// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
)

// BundleArtifactType is the artifactType of an AICR bundle at v1.0.0: one
// gzip-compressed tar layer of the bundle directory.
const BundleArtifactType = "application/vnd.nvidia.aicr.artifact"

// Bundle is a rendered AICR bundle: its regular files by slash-separated path
// relative to the bundle root.
type Bundle struct {
	Digest string
	Files  map[string][]byte
}

// Dir returns the files below dir, keyed by their path relative to dir.
func (b *Bundle) Dir(dir string) map[string][]byte {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	out := map[string][]byte{}
	for p, data := range b.Files {
		if rel, ok := strings.CutPrefix(p, prefix); ok {
			out[rel] = data
		}
	}
	return out
}

// Paths returns the bundle's file paths in sorted order.
func (b *Bundle) Paths() []string {
	out := make([]string, 0, len(b.Files))
	for p := range b.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// PullBundle fetches the bundle a render Job pushed, by manifest digest, and
// unpacks it in memory. The manifest, the layer and the archive are all
// bounded; links, devices and paths that leave the bundle are rejected.
func (c *Client) PullBundle(ctx context.Context, r Repository, digest string) (*Bundle, error) {
	if !digestPattern.MatchString(digest) {
		return nil, invalid("bundle digest %q is not a sha256 digest", digest)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return nil, err
	}
	desc, raw, err := repo.FetchReference(ctx, digest)
	if err != nil {
		return nil, classify(err, "fetch bundle manifest "+r.String()+"@"+digest)
	}
	defer raw.Close()
	if desc.Size > maxManifestBytes {
		return nil, invalid("bundle manifest is %d bytes", desc.Size)
	}
	data, err := content.ReadAll(raw, desc)
	if err != nil {
		return nil, classify(err, "read bundle manifest")
	}
	var manifest ocispec.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return nil, invalid("bundle manifest is not an OCI manifest: %v", err)
	}
	switch {
	case manifest.ArtifactType != BundleArtifactType:
		return nil, invalid("artifact %s@%s has type %q, want an AICR bundle", r, digest, manifest.ArtifactType)
	case len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != ocispec.MediaTypeImageLayerGzip:
		return nil, invalid("AICR bundle %s@%s must have exactly one tar+gzip layer", r, digest)
	case manifest.Layers[0].Size > defaults.MaxBundleLayerBytes:
		return nil, invalid("bundle layer is %d bytes, over %d", manifest.Layers[0].Size, defaults.MaxBundleLayerBytes)
	}
	layer, err := content.FetchAll(ctx, repo, manifest.Layers[0])
	if err != nil {
		return nil, classify(err, "fetch bundle layer")
	}
	files, err := untar(layer)
	if err != nil {
		return nil, err
	}
	return &Bundle{Digest: digest, Files: files}, nil
}

// maxManifestBytes bounds an OCI manifest; real manifests are a few KiB.
const maxManifestBytes = 4 << 20

// untar unpacks a gzip-compressed tar into memory within the bundle limits.
func untar(layer []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(layer))
	if err != nil {
		return nil, invalid("bundle layer is not gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, invalid("bundle layer is not a tar archive: %v", err)
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		switch {
		case h.Typeflag == tar.TypeDir:
			continue
		case h.Typeflag != tar.TypeReg:
			return nil, invalid("bundle entry %q is not a regular file", h.Name)
		case name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../"):
			return nil, invalid("bundle entry %q leaves the bundle", h.Name)
		case len(files) >= defaults.MaxBundleFiles:
			return nil, invalid("bundle has more than %d files", defaults.MaxBundleFiles)
		case h.Size < 0 || total+h.Size > defaults.MaxBundleBytes:
			return nil, invalid("bundle content exceeds %d bytes", defaults.MaxBundleBytes)
		}
		if _, dup := files[name]; dup {
			return nil, invalid("bundle entry %q appears twice", name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if err != nil || int64(len(data)) != h.Size {
			return nil, invalid("bundle entry %q is truncated", h.Name)
		}
		total += h.Size
		files[name] = data
	}
}
