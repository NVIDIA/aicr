// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"

	stderrors "errors"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
)

const (
	// aicrModule is the main module path recorded in the build info of every
	// AICR executable.
	aicrModule = "aicr.run"

	// minStringRun matches the shortest credential the default secret rules
	// look for while keeping short runs of binary noise out of the strings
	// file.
	minStringRun = 8

	// sniffBytes is how much of a file is read to tell text from binary, the
	// same window git uses.
	sniffBytes = 8000

	layersDir  = "layers"
	stringsDir = "strings"

	// stringsHeader starts every strings file.
	stringsHeader = "image-unpack strings of"
)

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	elfMagic      = []byte("\x7fELF")

	indexMediaTypes = map[string]bool{
		"application/vnd.oci.image.index.v1+json":                   true,
		"application/vnd.docker.distribution.manifest.list.v2+json": true,
	}
	manifestMediaTypes = map[string]bool{
		"application/vnd.oci.image.manifest.v1+json":           true,
		"application/vnd.docker.distribution.manifest.v2+json": true,
	}
	gzipLayerMediaTypes = map[string]bool{
		"application/vnd.oci.image.layer.v1.tar+gzip":       true,
		"application/vnd.docker.image.rootfs.diff.tar.gzip": true,
	}
	tarLayerMediaTypes = map[string]bool{
		"application/vnd.oci.image.layer.v1.tar": true,
	}
)

// Options selects the image and where to unpack it.
type Options struct {
	// Layout is an OCI image layout whose index.json names exactly one image
	// index, the shape both `ko build --oci-layout-path` and
	// `crane pull --format=oci` write.
	Layout string
	// Platform is os/arch, with an optional /variant that must then match.
	Platform string
	// Index is the digest the layout's image index must have.
	Index string
	// Out is created by Unpack and must not exist.
	Out string
}

// Coverage records what was unpacked. Its counts let a scan summary state
// which files a scanner read and which it could not.
type Coverage struct {
	Index    string          `json:"index"`
	Manifest string          `json:"manifest"`
	Platform string          `json:"platform"`
	Config   string          `json:"config"`
	Layers   []LayerCoverage `json:"layers"`
	Totals   Counts          `json:"totals"`
}

// LayerCoverage is one layer's verified identity and what it held.
type LayerCoverage struct {
	Position int    `json:"position"`
	Digest   string `json:"digest"`
	DiffID   string `json:"diffID"`
	Dir      string `json:"dir"`
	Counts
}

// Counts tallies the entries of one layer, or of all of them. Files are the
// regular files written to disk and Empty the subset with no content, which
// file scanners do not count as scanned. Binaries is the subset that starts
// with the ELF magic or holds a NUL byte in its first sniffBytes, and
// AICRExecutables the subset of those built from the AICR module, each of
// which also gets a strings file.
// Symlinks, hardlinks, whiteouts and special files carry no content of their
// own and are counted, not written.
type Counts struct {
	Files           int   `json:"files"`
	Empty           int   `json:"empty"`
	Bytes           int64 `json:"bytes"`
	Binaries        int   `json:"binaries"`
	AICRExecutables int   `json:"aicrExecutables"`
	Symlinks        int   `json:"symlinks"`
	Hardlinks       int   `json:"hardlinks"`
	Whiteouts       int   `json:"whiteouts"`
	Special         int   `json:"special"`
}

func (c *Counts) add(o Counts) {
	c.Files += o.Files
	c.Empty += o.Empty
	c.Bytes += o.Bytes
	c.Binaries += o.Binaries
	c.AICRExecutables += o.AICRExecutables
	c.Symlinks += o.Symlinks
	c.Hardlinks += o.Hardlinks
	c.Whiteouts += o.Whiteouts
	c.Special += o.Special
}

type platformSpec struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type descriptor struct {
	MediaType string        `json:"mediaType"`
	Digest    string        `json:"digest"`
	Size      int64         `json:"size"`
	Platform  *platformSpec `json:"platform,omitempty"`
}

type imageIndex struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
}

type imageManifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}

type imageConfig struct {
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// Unpack verifies the layout against opts.Index, selects opts.Platform and
// unpacks its layers, config, manifest and index under opts.Out.
func Unpack(ctx context.Context, opts Options) (*Coverage, error) {
	want, err := parsePlatform(opts.Platform)
	if err != nil {
		return nil, err
	}
	if !digestPattern.MatchString(opts.Index) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "index digest must be sha256:<64 hex>")
	}
	top, err := readRootIndex(opts.Layout)
	if err != nil {
		return nil, err
	}
	if top.Digest != opts.Index {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("layout holds image index %s, want %s", top.Digest, opts.Index))
	}
	indexRaw, err := readMetadataBlob(opts.Layout, top)
	if err != nil {
		return nil, err
	}
	var index imageIndex
	if err = json.Unmarshal(indexRaw, &index); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse the image index", err)
	}
	target, err := selectPlatform(index, want)
	if err != nil {
		return nil, err
	}
	manifestRaw, err := readMetadataBlob(opts.Layout, target)
	if err != nil {
		return nil, err
	}
	var manifest imageManifest
	if err = json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse the platform manifest", err)
	}
	if len(manifest.Layers) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("manifest %s has no layers", target.Digest))
	}
	configRaw, err := readMetadataBlob(opts.Layout, manifest.Config)
	if err != nil {
		return nil, err
	}
	var config imageConfig
	if err = json.Unmarshal(configRaw, &config); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse the image config", err)
	}
	if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"config %s lists %d diff_ids of type %q for %d layers",
			manifest.Config.Digest, len(config.RootFS.DiffIDs), config.RootFS.Type, len(manifest.Layers)))
	}

	if err = os.Mkdir(opts.Out, 0o750); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to create %s", opts.Out), err)
	}
	for name, raw := range map[string][]byte{"index.json": indexRaw, "manifest.json": manifestRaw, "config.json": configRaw} {
		if err = os.WriteFile(filepath.Join(opts.Out, name), raw, 0o600); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to write %s", name), err)
		}
	}

	coverage := &Coverage{
		Index:    opts.Index,
		Manifest: target.Digest,
		Platform: opts.Platform,
		Config:   manifest.Config.Digest,
		Layers:   make([]LayerCoverage, 0, len(manifest.Layers)),
	}
	for i, layer := range manifest.Layers {
		if err = ctx.Err(); err != nil {
			return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "unpack interrupted")
		}
		dir := fmt.Sprintf("%03d", i)
		u := &layerUnpacker{
			ctx:     ctx,
			root:    filepath.Join(opts.Out, layersDir, dir),
			strings: filepath.Join(opts.Out, stringsDir, dir),
			written: map[string]bool{},
		}
		if err = u.unpack(opts.Layout, layer, config.RootFS.DiffIDs[i]); err != nil {
			return nil, err
		}
		coverage.Layers = append(coverage.Layers, LayerCoverage{
			Position: i,
			Digest:   layer.Digest,
			DiffID:   config.RootFS.DiffIDs[i],
			Dir:      path.Join(layersDir, dir),
			Counts:   u.counts,
		})
		coverage.Totals.add(u.counts)
	}
	return coverage, nil
}

func parsePlatform(s string) (platformSpec, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return platformSpec{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("platform %q must be os/arch[/variant]", s))
	}
	p := platformSpec{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		if parts[2] == "" {
			return platformSpec{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("platform %q has an empty variant", s))
		}
		p.Variant = parts[2]
	}
	return p, nil
}

// readRootIndex returns the one image index descriptor index.json must hold.
func readRootIndex(layout string) (descriptor, error) {
	raw, err := readCapped(filepath.Join(layout, "index.json"))
	if err != nil {
		return descriptor{}, err
	}
	var root imageIndex
	if err = json.Unmarshal(raw, &root); err != nil {
		return descriptor{}, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse index.json", err)
	}
	if len(root.Manifests) != 1 || !indexMediaTypes[root.Manifests[0].MediaType] {
		return descriptor{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s/index.json holds %d entries; want one image index", layout, len(root.Manifests)))
	}
	return root.Manifests[0], nil
}

// selectPlatform returns the one manifest that matches want. A variant is
// matched only when want names one.
func selectPlatform(index imageIndex, want platformSpec) (descriptor, error) {
	var matches []descriptor
	for _, m := range index.Manifests {
		p := m.Platform
		if p == nil || p.OS != want.OS || p.Architecture != want.Architecture {
			continue
		}
		if want.Variant != "" && p.Variant != want.Variant {
			continue
		}
		matches = append(matches, m)
	}
	label := want.OS + "/" + want.Architecture
	if want.Variant != "" {
		label += "/" + want.Variant
	}
	if len(matches) != 1 {
		return descriptor{}, errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("image index has %d %s manifests; want exactly one", len(matches), label))
	}
	if !manifestMediaTypes[matches[0].MediaType] {
		return descriptor{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s manifest has media type %q", label, matches[0].MediaType))
	}
	return matches[0], nil
}

func blobPath(layout, digest string) string {
	return filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
}

// readCapped reads a metadata file under MaxImageMetadataBytes.
func readCapped(file string) ([]byte, error) {
	f, err := os.Open(file) // #nosec G304 -- release-controlled layout path, size-bounded below
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to open %s", file), err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, defaults.MaxImageMetadataBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read %s", file), err)
	}
	if int64(len(data)) > defaults.MaxImageMetadataBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s exceeds %d bytes", file, defaults.MaxImageMetadataBytes))
	}
	return data, nil
}

// readMetadataBlob reads an index, manifest or config blob and checks it
// against its descriptor.
func readMetadataBlob(layout string, d descriptor) ([]byte, error) {
	if !digestPattern.MatchString(d.Digest) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported digest %q", d.Digest))
	}
	data, err := readCapped(blobPath(layout, d.Digest))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != d.Size {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("blob %s is %d bytes, descriptor says %d", d.Digest, len(data), d.Size))
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != d.Digest {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("blob %s hashes to %s", d.Digest, got))
	}
	return data, nil
}

// verifyLayerBlob hashes a layer blob in full before any of it is unpacked,
// so a tampered blob is rejected before a single file reaches a scanner.
func verifyLayerBlob(file string, d descriptor) error {
	f, err := os.Open(file) // #nosec G304 -- release-controlled layout path
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to open layer %s", d.Digest), err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read layer %s", d.Digest), err)
	}
	if n != d.Size {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("layer %s is %d bytes, descriptor says %d", d.Digest, n, d.Size))
	}
	if got := digestOf(h); got != d.Digest {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("layer %s hashes to %s", d.Digest, got))
	}
	return nil
}

func digestOf(h hash.Hash) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

type layerUnpacker struct {
	ctx     context.Context
	root    string
	strings string
	written map[string]bool
	counts  Counts
}

func (u *layerUnpacker) unpack(layout string, layer descriptor, diffID string) error {
	gzipped := gzipLayerMediaTypes[layer.MediaType]
	if !gzipped && !tarLayerMediaTypes[layer.MediaType] {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("layer %s has unsupported media type %q", layer.Digest, layer.MediaType))
	}
	if !digestPattern.MatchString(layer.Digest) || !digestPattern.MatchString(diffID) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("layer %s or its diff_id %s is not sha256:<64 hex>", layer.Digest, diffID))
	}
	file := blobPath(layout, layer.Digest)
	if err := verifyLayerBlob(file, layer); err != nil {
		return err
	}
	if err := os.MkdirAll(u.root, 0o750); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create %s", u.root), err)
	}
	f, err := os.Open(file) // #nosec G304 -- verified above
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to open layer %s", layer.Digest), err)
	}
	defer func() { _ = f.Close() }()

	var stream io.Reader = bufio.NewReader(f)
	if gzipped {
		gz, gzErr := gzip.NewReader(stream)
		if gzErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("layer %s is not gzip", layer.Digest), gzErr)
		}
		defer func() { _ = gz.Close() }()
		stream = gz
	}
	h := sha256.New()
	tee := io.TeeReader(stream, h)
	tr := tar.NewReader(tee)
	for {
		if err = u.ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "unpack interrupted")
		}
		header, nextErr := tr.Next()
		if stderrors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to read layer %s", layer.Digest), nextErr)
		}
		if err = u.entry(header, tr); err != nil {
			code := errors.ErrCodeInvalidRequest
			if coded, ok := stderrors.AsType[*errors.StructuredError](err); ok {
				code = coded.Code
			}
			return errors.Wrap(code, fmt.Sprintf("layer %s entry %q", layer.Digest, header.Name), err)
		}
	}
	// The tar reader stops at the end-of-archive marker; the diff_id covers the
	// padding after it too.
	if _, err = io.Copy(io.Discard, tee); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to read layer %s", layer.Digest), err)
	}
	if got := digestOf(h); got != diffID {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("layer %s uncompresses to %s, config says %s", layer.Digest, got, diffID))
	}
	return nil
}

// entryPath maps a tar entry name to a path inside the layer directory. A
// name that would leave it is an error, not something to clean up silently.
func entryPath(name string) (string, error) {
	rel := path.Clean(strings.TrimLeft(name, "/"))
	if rel == "." {
		return "", nil
	}
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", errors.New(errors.ErrCodeInvalidRequest, "path leaves the layer root")
	}
	return rel, nil
}

func (u *layerUnpacker) entry(h *tar.Header, r io.Reader) error {
	rel, err := entryPath(h.Name)
	if err != nil {
		return err
	}
	if rel == "" {
		return nil
	}
	if strings.HasPrefix(path.Base(rel), ".wh.") {
		u.counts.Whiteouts++
		return nil
	}
	switch h.Typeflag {
	case tar.TypeDir:
		if err = os.MkdirAll(filepath.Join(u.root, filepath.FromSlash(rel)), 0o750); err != nil {
			return fsError("create directory", rel, err)
		}
	case tar.TypeReg, tar.TypeGNUSparse:
		return u.writeFile(rel, r)
	case tar.TypeSymlink:
		u.counts.Symlinks++
	case tar.TypeLink:
		u.counts.Hardlinks++
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		u.counts.Special++
	case tar.TypeXGlobalHeader:
	default:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported tar entry type %q", h.Typeflag))
	}
	return nil
}

// writeFile writes one regular file. A path a layer writes twice keeps both
// contents: the second goes to <path>.dup<n>, because both are in the shipped
// bytes. O_EXCL means nothing is ever written through an existing path.
func (u *layerUnpacker) writeFile(rel string, r io.Reader) error {
	name := rel
	for n := 1; u.written[name]; n++ {
		name = fmt.Sprintf("%s.dup%d", rel, n)
	}
	u.written[name] = true
	dest := filepath.Join(u.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fsError("create the directory of", name, err)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- dest is inside the layer root (entryPath)
	if err != nil {
		return fsError("create", name, err)
	}
	sniff := &prefix{limit: sniffBytes}
	n, err := io.Copy(io.MultiWriter(f, sniff), r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fsError("write", name, err)
	}
	u.counts.Files++
	u.counts.Bytes += n
	if n == 0 {
		u.counts.Empty++
	}
	if !sniff.binary() {
		return nil
	}
	u.counts.Binaries++
	if !isAICRExecutable(dest) {
		return nil
	}
	u.counts.AICRExecutables++
	return writeStrings(dest, name, filepath.Join(u.strings, filepath.FromSlash(name)+".strings"))
}

// prefix keeps the first limit bytes written to it.
type prefix struct {
	limit int
	buf   []byte
}

func (p *prefix) Write(b []byte) (int, error) {
	if room := p.limit - len(p.buf); room > 0 {
		p.buf = append(p.buf, b[:min(room, len(b))]...)
	}
	return len(b), nil
}

func (p *prefix) binary() bool {
	return bytes.HasPrefix(p.buf, elfMagic) || bytes.IndexByte(p.buf, 0) >= 0
}

// fsError classifies a filesystem error. An existing path, or a file where a
// directory must be, means two entries of the layer conflict, which is the
// image's fault; anything else is the runner's.
func fsError(op, name string, err error) error {
	code := errors.ErrCodeInternal
	if stderrors.Is(err, fs.ErrExist) || stderrors.Is(err, syscall.ENOTDIR) || stderrors.Is(err, syscall.EISDIR) {
		code = errors.ErrCodeInvalidRequest
	}
	return errors.Wrap(code, fmt.Sprintf("failed to %s %s", op, name), err)
}

func isAICRExecutable(file string) bool {
	info, err := buildinfo.ReadFile(file)
	return err == nil && isAICRModule(info)
}

func isAICRModule(info *debug.BuildInfo) bool {
	return info != nil && info.Main.Path == aicrModule
}

// writeStrings writes each run of at least minStringRun printable ASCII bytes
// in src on its own line of dst, like strings(1) with -n 8, after a header
// line naming the file. Gitleaks skips a file whose first bytes match a binary
// format's magic number, and the first run is usually the random build ID, so
// the header keeps the start of the file fixed text.
func writeStrings(src, name, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- a file this tool just wrote
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to open %s", src), err)
	}
	defer func() { _ = in.Close() }()
	if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create the directory of %s", dst), err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- inside the strings root
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create %s", dst), err)
	}
	w := bufio.NewWriter(out)
	if _, err = fmt.Fprintf(w, "%s %s\n", stringsHeader, name); err == nil {
		err = extractStrings(bufio.NewReader(in), w)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to write %s", dst), err)
	}
	return nil
}

func extractStrings(in *bufio.Reader, out *bufio.Writer) error {
	var run []byte
	flush := func() error {
		if len(run) >= minStringRun {
			if _, err := out.Write(append(run, '\n')); err != nil {
				return err
			}
		}
		run = run[:0]
		return nil
	}
	for {
		b, err := in.ReadByte()
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if b == '\t' || (b >= 0x20 && b < 0x7f) {
			run = append(run, b)
			continue
		}
		if err = flush(); err != nil {
			return err
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return out.Flush()
}
