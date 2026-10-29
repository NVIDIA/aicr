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
	"context"
	"encoding/json"
	"fmt"
	"path"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
)

// MirrorOCIChart copies a chart from an upstream OCI registry into
// target/<chart>. The upstream reference is the chart repository
// (oci://host/path/<chart>) and the version is its tag. The upstream
// manifest must be a Helm chart with exactly one chart layer; the layer is
// verified against its descriptor digest and recorded as the archive digest.
// A runtime-registry tag that already holds another digest for the same
// version is a conflict: the upstream content changed after it was mirrored.
func (c *Client) MirrorOCIChart(ctx context.Context, up UpstreamChart, target Repository) (*MirroredChart, error) {
	src, err := ParseRepository(up.Chart)
	if err != nil {
		return nil, invalid("upstream chart %q is not an OCI repository", up.Chart)
	}
	if up.Version == "" {
		return nil, invalid("upstream chart %s has no version", up.Chart)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(src)
	if err != nil {
		return nil, err
	}
	tag := (&ChartMeta{Version: up.Version}).Tag()
	desc, raw, err := repo.FetchReference(ctx, tag)
	if err != nil {
		return nil, classify(err, fmt.Sprintf("fetch upstream chart %s:%s", src, tag))
	}
	defer raw.Close()
	if desc.Size > maxManifestBytes {
		return nil, invalid("upstream chart manifest is %d bytes", desc.Size)
	}
	data, err := content.ReadAll(raw, desc)
	if err != nil {
		return nil, classify(err, "read upstream chart manifest")
	}
	var manifest ocispec.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return nil, invalid("upstream %s:%s is not an OCI image manifest: %v", src, tag, err)
	}
	if manifest.Config.MediaType != ChartConfigMediaType {
		return nil, invalid("upstream %s:%s is not a Helm chart (config %q)", src, tag, manifest.Config.MediaType)
	}
	var layer *ocispec.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType != ChartLayerMediaType {
			continue
		}
		if layer != nil {
			return nil, invalid("upstream %s:%s has more than one chart layer", src, tag)
		}
		layer = &manifest.Layers[i]
	}
	switch {
	case layer == nil:
		return nil, invalid("upstream %s:%s has no chart layer", src, tag)
	case layer.Size > defaults.MaxChartArchiveBytes:
		return nil, invalid("upstream chart layer is %d bytes, over %d", layer.Size, defaults.MaxChartArchiveBytes)
	}
	archive, err := content.FetchAll(ctx, repo, *layer)
	if err != nil {
		return nil, classify(err, fmt.Sprintf("fetch upstream chart layer %s@%s", src, layer.Digest))
	}
	meta, err := ReadChartMeta(archive)
	if err != nil {
		return nil, err
	}
	name := path.Base(src.Path)
	if meta.Name != name || meta.Version != up.Version {
		return nil, invalid("upstream %s:%s holds chart %s %s", src, tag, meta.Name, meta.Version)
	}
	dst := target.Child(name)
	digest, err := c.pushChart(ctx, dst, archive, meta, meta.Tag())
	if err != nil {
		return nil, err
	}
	return &MirroredChart{Repository: dst, Digest: digest, ArchiveDigest: layer.Digest.String(), Archive: archive}, nil
}
