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
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"testing"

	stderrors "errors"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

const (
	ociIndex    = "application/vnd.oci.image.index.v1+json"
	ociManifest = "application/vnd.oci.image.manifest.v1+json"
	ociConfig   = "application/vnd.oci.image.config.v1+json"
	ociGzip     = "application/vnd.oci.image.layer.v1.tar+gzip"
	ociTar      = "application/vnd.oci.image.layer.v1.tar"
)

// entry is one tar member of a test layer.
type entry struct {
	name     string
	typeflag byte
	body     []byte
	linkname string
}

func file(name, body string) entry {
	return entry{name: name, typeflag: tar.TypeReg, body: []byte(body)}
}

// testLayer is a layer to build, with knobs that break it.
type testLayer struct {
	entries   []entry
	mediaType string
	// badDiffID records a wrong diff_id for this layer in the config.
	badDiffID bool
	// record pads the uncompressed tar with zeros to a multiple of record
	// bytes, as GNU tar does after the end-of-archive marker.
	record int
}

// testImage is one platform image of a test layout.
type testImage struct {
	platform platformSpec
	layers   []testLayer
	// extraDiffID appends a diff_id with no layer behind it.
	extraDiffID bool
	// rootfsType overrides the config's rootfs.type ("layers").
	rootfsType string
	// configSizeDelta misstates the config's size in the manifest.
	configSizeDelta int64
}

// layoutBuilder writes an OCI layout of the shape ko and crane pull produce:
// index.json names one image index, which lists the platform manifests.
type layoutBuilder struct {
	t   *testing.T
	dir string
}

func newLayout(t *testing.T) *layoutBuilder {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o750); err != nil {
		t.Fatal(err)
	}
	return &layoutBuilder{t: t, dir: dir}
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (b *layoutBuilder) blob(mediaType string, data []byte) descriptor {
	b.t.Helper()
	d := descriptor{MediaType: mediaType, Digest: sha(data), Size: int64(len(data))}
	if err := os.WriteFile(blobPath(b.dir, d.Digest), data, 0o600); err != nil {
		b.t.Fatal(err)
	}
	return d
}

func (b *layoutBuilder) jsonBlob(mediaType string, v any) descriptor {
	b.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		b.t.Fatal(err)
	}
	return b.blob(mediaType, data)
}

func tarBytes(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: 0o644, Linkname: e.linkname}
		if e.typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// image writes one platform image and returns its manifest descriptor.
func (b *layoutBuilder) image(img testImage) descriptor {
	b.t.Helper()
	var layers []descriptor
	var diffIDs []string
	for _, l := range img.layers {
		raw := tarBytes(b.t, l.entries)
		if l.record > 0 && len(raw)%l.record != 0 {
			raw = append(raw, make([]byte, l.record-len(raw)%l.record)...)
		}
		diffID := sha(raw)
		if l.badDiffID {
			diffID = sha([]byte("not this layer"))
		}
		diffIDs = append(diffIDs, diffID)
		switch l.mediaType {
		case "", ociGzip:
			layers = append(layers, b.blob(ociGzip, gzipBytes(b.t, raw)))
		default:
			layers = append(layers, b.blob(l.mediaType, raw))
		}
	}
	if img.extraDiffID {
		diffIDs = append(diffIDs, sha([]byte("extra")))
	}
	rootfsType := "layers"
	if img.rootfsType != "" {
		rootfsType = img.rootfsType
	}
	config := map[string]any{
		"architecture": img.platform.Architecture,
		"os":           img.platform.OS,
		"config":       map[string]any{"Env": []string{"PATH=/usr/bin"}},
		"rootfs":       map[string]any{"type": rootfsType, "diff_ids": diffIDs},
	}
	cfg := b.jsonBlob(ociConfig, config)
	cfg.Size += img.configSizeDelta
	m := b.jsonBlob(ociManifest, imageManifest{MediaType: ociManifest, Config: cfg, Layers: layers})
	p := img.platform
	m.Platform = &p
	return m
}

// index writes the image index and index.json, returning the index digest.
func (b *layoutBuilder) index(manifests ...descriptor) string {
	b.t.Helper()
	idx := b.jsonBlob(ociIndex, imageIndex{MediaType: ociIndex, Manifests: manifests})
	b.writeRoot(idx)
	return idx.Digest
}

func (b *layoutBuilder) writeRoot(manifests ...descriptor) {
	b.t.Helper()
	data, err := json.Marshal(imageIndex{MediaType: ociIndex, Manifests: manifests})
	if err != nil {
		b.t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(b.dir, "index.json"), data, 0o600); err != nil {
		b.t.Fatal(err)
	}
}

var (
	amd64 = platformSpec{OS: "linux", Architecture: "amd64"}
	arm64 = platformSpec{OS: "linux", Architecture: "arm64", Variant: "v8"}
)

// aicrBinary returns the running test binary, whose build info names the
// AICR module, so it stands in for an AICR executable.
var aicrBinary = sync.OnceValues(func() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(exe)
})

func unpackOptions(layout, index, platform string, t *testing.T) Options {
	return Options{Layout: layout, Platform: platform, Index: index, Out: filepath.Join(t.TempDir(), "out")}
}

func TestUnpackKeepsEveryLayer(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	base := testLayer{entries: []entry{
		{name: "etc/", typeflag: tar.TypeDir},
		file("etc/app.conf", "token = placeholder\n"),
		file("/usr/share/zoneinfo/UTC", "TZif\x00\x00binary"),
		{name: "usr/bin/sh", typeflag: tar.TypeSymlink, linkname: "busybox"},
		{name: "dev/null", typeflag: tar.TypeChar},
	}}
	top := testLayer{entries: []entry{
		{name: "etc/.wh.app.conf", typeflag: tar.TypeReg},
		file("opt/app/run.sh", "#!/bin/sh\n"),
		{name: "opt/app/run2.sh", typeflag: tar.TypeLink, linkname: "opt/app/run.sh"},
		file("opt/app/dup.txt", "first"),
		file("opt/app/dup.txt", "second"),
		file("opt/app/.keep", ""),
	}}
	m := b.image(testImage{platform: amd64, layers: []testLayer{base, top}})
	other := b.image(testImage{platform: arm64, layers: []testLayer{{entries: []entry{file("x", "y")}}}})
	index := b.index(m, other)

	opts := unpackOptions(b.dir, index, "linux/amd64", t)
	coverage, err := Unpack(context.Background(), opts)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// The file the top layer deletes is still on disk, under the layer that
	// shipped it.
	for rel, want := range map[string]string{
		"layers/000/etc/app.conf":         "token = placeholder\n",
		"layers/001/opt/app/dup.txt":      "first",
		"layers/001/opt/app/dup.txt.dup1": "second",
	} {
		got, readErr := os.ReadFile(filepath.Join(opts.Out, rel))
		if readErr != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", rel, got, readErr, want)
		}
	}
	for _, absent := range []string{"layers/000/usr/bin/sh", "layers/000/dev/null", "layers/001/etc/.wh.app.conf", "layers/001/opt/app/run2.sh"} {
		if _, statErr := os.Lstat(filepath.Join(opts.Out, absent)); !stderrors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s was written; links, devices and whiteouts carry no content of their own", absent)
		}
	}
	for name, digest := range map[string]string{"index.json": index, "manifest.json": m.Digest, "config.json": coverage.Config} {
		got, readErr := os.ReadFile(filepath.Join(opts.Out, name))
		want, blobErr := os.ReadFile(blobPath(b.dir, digest))
		if readErr != nil || blobErr != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is not the %s blob (%v, %v)", name, digest, readErr, blobErr)
		}
	}

	if coverage.Index != index || coverage.Manifest != m.Digest || coverage.Platform != "linux/amd64" {
		t.Errorf("coverage identity = %s %s %s", coverage.Index, coverage.Manifest, coverage.Platform)
	}
	if len(coverage.Layers) != 2 || coverage.Layers[1].Dir != "layers/001" {
		t.Fatalf("coverage layers = %+v", coverage.Layers)
	}
	want := Counts{Files: 6, Empty: 1, Binaries: 1, Symlinks: 1, Hardlinks: 1, Whiteouts: 1, Special: 1,
		Bytes: int64(len("token = placeholder\n") + len("TZif\x00\x00binary") + len("#!/bin/sh\n") + len("first") + len("second"))}
	if coverage.Totals != want {
		t.Errorf("totals = %+v, want %+v", coverage.Totals, want)
	}
}

func TestUnpackWritesStringsForAICRExecutables(t *testing.T) {
	t.Parallel()
	exe, err := aicrBinary()
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	b := newLayout(t)
	m := b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{
		{name: "ko-app/aicr", typeflag: tar.TypeReg, body: exe},
		{name: "usr/lib/libother.so", typeflag: tar.TypeReg, body: []byte("\x7fELF\x02\x01\x01 not go")},
	}}}})
	opts := unpackOptions(b.dir, b.index(m), "linux/amd64", t)
	coverage, err := Unpack(context.Background(), opts)
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if coverage.Totals.Binaries != 2 || coverage.Totals.AICRExecutables != 1 {
		t.Fatalf("totals = %+v; want 2 binaries, 1 AICR executable", coverage.Totals)
	}
	strs, err := os.ReadFile(filepath.Join(opts.Out, "strings", "000", "ko-app", "aicr.strings"))
	if err != nil {
		t.Fatalf("strings file: %v", err)
	}
	if !bytes.HasPrefix(strs, []byte(stringsHeader+" ko-app/aicr\n")) {
		t.Errorf("strings file does not start with its header: %q", strs[:min(len(strs), 60)])
	}
	if !bytes.Contains(strs, []byte(aicrModule)) {
		t.Error("strings file does not hold the module path from the build info")
	}
	if _, err = os.Stat(filepath.Join(opts.Out, "strings", "000", "usr", "lib", "libother.so.strings")); !stderrors.Is(err, os.ErrNotExist) {
		t.Error("a binary from outside the AICR module got a strings file")
	}
}

func TestUnpackSelectsPlatform(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	m1 := b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a", "amd64")}}}})
	m2 := b.image(testImage{platform: arm64, layers: []testLayer{{entries: []entry{file("a", "arm64")}}}})
	index := b.index(m1, m2)

	tests := []struct {
		platform string
		want     string
		wantErr  bool
	}{
		{platform: "linux/amd64", want: m1.Digest},
		{platform: "linux/arm64", want: m2.Digest},
		{platform: "linux/arm64/v8", want: m2.Digest},
		{platform: "linux/arm64/v7", wantErr: true},
		{platform: "linux/s390x", wantErr: true},
		{platform: "linux", wantErr: true},
		{platform: "linux/arm64/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			t.Parallel()
			coverage, err := Unpack(context.Background(), unpackOptions(b.dir, index, tt.platform, t))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && coverage.Manifest != tt.want {
				t.Errorf("manifest = %s, want %s", coverage.Manifest, tt.want)
			}
		})
	}
}

// rewriteBlob replaces the blob at digest with content of the same size, so
// only the digest check can tell.
func rewriteBlob(t *testing.T, layout, digest string, edit func([]byte) []byte) {
	t.Helper()
	data, err := os.ReadFile(blobPath(layout, digest))
	if err != nil {
		t.Fatal(err)
	}
	edited := edit(bytes.Clone(data))
	if len(edited) != len(data) || bytes.Equal(edited, data) {
		t.Fatalf("rewrite of %s must change the content and keep the size", digest)
	}
	if err = os.WriteFile(blobPath(layout, digest), edited, 0o600); err != nil {
		t.Fatal(err)
	}
}

func manifestOf(t *testing.T, layout, digest string) imageManifest {
	t.Helper()
	data, err := os.ReadFile(blobPath(layout, digest))
	if err != nil {
		t.Fatal(err)
	}
	var m imageManifest
	if err = json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUnpackRejects(t *testing.T) {
	t.Parallel()
	good := testLayer{entries: []entry{file("a", "b")}}
	tests := []struct {
		name string
		// build returns the layout dir and the index digest to pass.
		build    func(t *testing.T) (string, string)
		wantCode errors.ErrorCode
		wantMsg  string
		// noFiles asserts the check runs before any file is written.
		noFiles bool
	}{
		{
			name: "wrong expected index",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				b.index(b.image(testImage{platform: amd64, layers: []testLayer{good}}))
				return b.dir, sha([]byte("another index"))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "layout holds image index",
		},
		{
			name: "index.json with two entries",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				idx := b.jsonBlob(ociIndex, imageIndex{MediaType: ociIndex, Manifests: []descriptor{m}})
				b.writeRoot(idx, idx)
				return b.dir, idx.Digest
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "want one image index",
		},
		{
			name: "index.json naming a manifest, not an index",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				b.writeRoot(m)
				return b.dir, m.Digest
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "want one image index",
		},
		{
			name: "index.json over the size cap",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{good}}))
				data, _ := os.ReadFile(filepath.Join(b.dir, "index.json"))
				data = append(data, bytes.Repeat([]byte(" "), int(defaults.MaxImageMetadataBytes))...)
				if err := os.WriteFile(filepath.Join(b.dir, "index.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				return b.dir, index
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "exceeds",
		},
		{
			name: "platform manifest rewritten",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				index := b.index(m)
				rewriteBlob(t, b.dir, m.Digest, func(d []byte) []byte {
					return bytes.Replace(d, []byte("manifest.v1+json"), []byte("manifest.v9+json"), 1)
				})
				return b.dir, index
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "hashes to",
		},
		{
			name: "config rewritten",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				index := b.index(m)
				rewriteBlob(t, b.dir, manifestOf(t, b.dir, m.Digest).Config.Digest, func(d []byte) []byte {
					return bytes.Replace(d, []byte("PATH=/usr/bin"), []byte("PATH=/usr/xyz"), 1)
				})
				return b.dir, index
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "hashes to",
		},
		{
			name: "config size misstated",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{good}, configSizeDelta: 1}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "descriptor says",
		},
		{
			name: "malformed manifest digest",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				m.Digest = "sha256:../../index.json"
				return b.dir, b.index(m)
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "unsupported digest",
		},
		{
			name: "layer blob rewritten",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				index := b.index(m)
				// The gzip header's MTIME: the stream stays valid and the
				// uncompressed bytes keep their diff_id.
				rewriteBlob(t, b.dir, manifestOf(t, b.dir, m.Digest).Layers[0].Digest, func(d []byte) []byte {
					d[4] ^= 0xff
					return d
				})
				return b.dir, index
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "hashes to", noFiles: true,
		},
		{
			name: "wrong diff_id",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: good.entries, badDiffID: true}}}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "uncompresses to",
		},
		{
			name: "more diff_ids than layers",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{good}, extraDiffID: true}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "diff_ids",
		},
		{
			name: "rootfs that is not layers",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{good}, rootfsType: "squashfs"}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `of type "squashfs"`,
		},
		{
			name: "manifest with no layers",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "has no layers",
		},
		{
			name: "zstd layer",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: good.entries, mediaType: "application/vnd.oci.image.layer.v1.tar+zstd"}}}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "unsupported media type",
		},
		{
			name: "entry escaping the layer root",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a/../../escape", "x")}}}}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "leaves the layer root",
		},
		{
			name: "file where a directory was",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				return b.dir, b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a/b", "x"), file("a", "y")}}}}))
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "failed to create a",
		},
		{
			name: "two manifests for one platform",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m1 := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				m2 := b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("c", "d")}}}})
				return b.dir, b.index(m1, m2)
			},
			wantCode: errors.ErrCodeNotFound, wantMsg: "want exactly one",
		},
		{
			name: "platform entry that is an index",
			build: func(t *testing.T) (string, string) {
				b := newLayout(t)
				m := b.image(testImage{platform: amd64, layers: []testLayer{good}})
				m.MediaType = ociIndex
				return b.dir, b.index(m)
			},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "has media type",
		},
		{
			name: "missing layout",
			build: func(t *testing.T) (string, string) {
				return filepath.Join(t.TempDir(), "absent"), sha([]byte("x"))
			},
			wantCode: errors.ErrCodeNotFound, wantMsg: "failed to open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			layout, index := tt.build(t)
			opts := unpackOptions(layout, index, "linux/amd64", t)
			_, err := Unpack(context.Background(), opts)
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want one naming %q", err, tt.wantMsg)
			}
			if _, statErr := os.Stat(filepath.Join(opts.Out, "layers", "000", "a")); tt.noFiles && !stderrors.Is(statErr, os.ErrNotExist) {
				t.Error("a tampered layer blob wrote a file before it was rejected")
			}
		})
	}
}

// GNU tar pads a layer to a 10240-byte record after the end-of-archive
// marker; the diff_id covers that padding.
func TestUnpackLayerPadding(t *testing.T) {
	t.Parallel()
	for _, mediaType := range []string{ociGzip, ociTar} {
		t.Run(mediaType, func(t *testing.T) {
			t.Parallel()
			b := newLayout(t)
			index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("etc/x", "y")}, mediaType: mediaType, record: 10240}}}))
			if _, err := Unpack(context.Background(), unpackOptions(b.dir, index, "linux/amd64", t)); err != nil {
				t.Fatalf("Unpack: %v", err)
			}
		})
	}
}

// Index entries with no platform, such as an attestation manifest a builder
// adds, are not candidates.
func TestUnpackSkipsEntriesWithoutPlatform(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	m := b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a", "b")}}}})
	unplaced := m
	unplaced.Platform = nil
	coverage, err := Unpack(context.Background(), unpackOptions(b.dir, b.index(unplaced, m), "linux/amd64", t))
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if coverage.Manifest != m.Digest {
		t.Errorf("manifest = %s, want %s", coverage.Manifest, m.Digest)
	}
}

func TestParsePlatform(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"linux", "linux/arm64/", "linux/arm64/v8/extra", "/amd64", "linux//v8", ""} {
		_, err := parsePlatform(bad)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	}
	got, err := parsePlatform("linux/arm64/v8")
	if err != nil || got != (platformSpec{OS: "linux", Architecture: "arm64", Variant: "v8"}) {
		t.Errorf("parsePlatform(linux/arm64/v8) = %+v, %v", got, err)
	}
}

func TestEntryTypes(t *testing.T) {
	t.Parallel()
	u := &layerUnpacker{ctx: context.Background(), root: t.TempDir(), strings: t.TempDir(), written: map[string]bool{}}
	for _, h := range []*tar.Header{
		{Name: "dev/sda", Typeflag: tar.TypeBlock},
		{Name: "run/pipe", Typeflag: tar.TypeFifo},
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader},
	} {
		if err := u.entry(h, strings.NewReader("")); err != nil {
			t.Fatalf("entry %q: %v", h.Name, err)
		}
	}
	if err := u.entry(&tar.Header{Name: "var/sparse", Typeflag: tar.TypeGNUSparse}, strings.NewReader("data")); err != nil {
		t.Fatalf("sparse entry: %v", err)
	}
	if u.counts != (Counts{Special: 2, Files: 1, Bytes: 4}) {
		t.Errorf("counts = %+v", u.counts)
	}
	err := u.entry(&tar.Header{Name: "x", Typeflag: tar.TypeCont}, strings.NewReader(""))
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestIsAICRModule(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		info *debug.BuildInfo
		want bool
	}{
		{info: nil},
		{info: &debug.BuildInfo{Main: debug.Module{Path: "aicr.run"}}, want: true},
		{info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/other/tool"}}},
		{info: &debug.BuildInfo{Main: debug.Module{Path: "aicr.run.evil"}}},
		{info: &debug.BuildInfo{Path: "aicr.run/cmd/aicr", Main: debug.Module{Path: "example.com/m"}}},
	} {
		if got := isAICRModule(tt.info); got != tt.want {
			t.Errorf("isAICRModule(%+v) = %v, want %v", tt.info, got, tt.want)
		}
	}
}

func TestFSError(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err  error
		want errors.ErrorCode
	}{
		{err: fs.ErrExist, want: errors.ErrCodeInvalidRequest},
		{err: &fs.PathError{Op: "mkdir", Path: "a", Err: syscall.ENOTDIR}, want: errors.ErrCodeInvalidRequest},
		{err: syscall.EISDIR, want: errors.ErrCodeInvalidRequest},
		{err: syscall.ENOSPC, want: errors.ErrCodeInternal},
		{err: io.ErrShortWrite, want: errors.ErrCodeInternal},
	} {
		errorstest.WantReportedCode(t, fsError("write", "x", tt.err), tt.want)
	}
}

// Cancellation is honored between the entries of a layer, not only between
// layers, so a large layer does not hold a canceled run.
func TestUnpackLayerStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	m := b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a", "b")}}}})
	b.index(m)
	manifest := manifestOf(t, b.dir, m.Digest)
	configData, err := os.ReadFile(blobPath(b.dir, manifest.Config.Digest))
	if err != nil {
		t.Fatal(err)
	}
	var config imageConfig
	if err = json.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u := &layerUnpacker{ctx: ctx, root: t.TempDir(), strings: t.TempDir(), written: map[string]bool{}}
	err = u.unpack(b.dir, manifest.Layers[0], config.RootFS.DiffIDs[0])
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

func TestUnpackUncompressedLayer(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("etc/x", "y")}, mediaType: ociTar}}}))
	opts := unpackOptions(b.dir, index, "linux/amd64", t)
	if _, err := Unpack(context.Background(), opts); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(opts.Out, "layers", "000", "etc", "x")); err != nil {
		t.Errorf("file not unpacked: %v", err)
	}
}

func TestUnpackRefusesExistingOut(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a", "b")}}}}))
	opts := unpackOptions(b.dir, index, "linux/amd64", t)
	if err := os.Mkdir(opts.Out, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(context.Background(), opts); err == nil {
		t.Fatal("Unpack wrote into an existing directory")
	}
}

func TestUnpackStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("a", "b")}}}}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Unpack(ctx, unpackOptions(b.dir, index, "linux/amd64", t))
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

func TestEntryPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "usr/bin/x", want: "usr/bin/x"},
		{name: "./usr/bin/x", want: "usr/bin/x"},
		{name: "/etc/passwd", want: "etc/passwd"},
		{name: "a/../b", want: "b"},
		{name: "./", want: ""},
		{name: "../x", wantErr: true},
		{name: "a/../../x", wantErr: true},
		{name: "/../x", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := entryPath(tt.name)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("entryPath(%q) = %q, %v; want %q, wantErr %v", tt.name, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestExtractStrings(t *testing.T) {
	t.Parallel()
	in := "ab\x00abcdefgh\x01ijklm\tnopq\nshort\x00" + strings.Repeat("z", 8)
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	if err := extractStrings(bufio.NewReader(strings.NewReader(in)), w); err != nil {
		t.Fatal(err)
	}
	want := "abcdefgh\nijklm\tnopq\nzzzzzzzz\n"
	if out.String() != want {
		t.Errorf("strings = %q, want %q", out.String(), want)
	}
}

// A runner failure inside an entry keeps its code through the layer's wrap,
// so the log does not blame the image for it.
func TestUnpackKeepsEntryErrorCodes(t *testing.T) {
	t.Parallel()
	exe, err := aicrBinary()
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	b := newLayout(t)
	m := b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{
		{name: "ko-app/aicr", typeflag: tar.TypeReg, body: exe},
	}}}})
	b.index(m)
	manifest := manifestOf(t, b.dir, m.Digest)
	configData, err := os.ReadFile(blobPath(b.dir, manifest.Config.Digest))
	if err != nil {
		t.Fatal(err)
	}
	var config imageConfig
	if err = json.Unmarshal(configData, &config); err != nil {
		t.Fatal(err)
	}
	// A regular file where the strings directory must go makes writing the
	// strings fail for a reason that is not the image's.
	blocked := filepath.Join(t.TempDir(), "strings")
	if err = os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	u := &layerUnpacker{ctx: context.Background(), root: t.TempDir(), strings: blocked, written: map[string]bool{}}
	err = u.unpack(b.dir, manifest.Layers[0], config.RootFS.DiffIDs[0])
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}
