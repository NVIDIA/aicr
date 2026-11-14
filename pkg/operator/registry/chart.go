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
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v4"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Helm's OCI media types.
const (
	ChartConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	ChartLayerMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// maxChartYAMLBytes bounds Chart.yaml inside an archive, and
// maxChartConfigBytes bounds the chart's config blob, Chart.yaml as JSON,
// which escaping can make larger than the YAML.
const (
	maxChartYAMLBytes   = 1 << 20
	maxChartConfigBytes = 1 << 20
)

// chartNamePattern is Helm's chart naming rule; it also keeps a name safe as
// an archive path prefix.
var chartNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

// ChartMeta identifies a chart archive.
type ChartMeta struct {
	Name    string
	Version string
	// config is Chart.yaml as canonical JSON, Helm's OCI config blob.
	config []byte
}

// ReadChartMeta reads Chart.yaml from a chart archive. Helm archives hold
// exactly one top-level directory, the chart's.
func ReadChartMeta(archive []byte) (*ChartMeta, error) {
	if len(archive) > defaults.MaxChartArchiveBytes {
		return nil, invalid("chart archive is %d bytes, over %d", len(archive), defaults.MaxChartArchiveBytes)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, invalid("chart archive is not gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, invalid("chart archive has no Chart.yaml")
		}
		if err != nil {
			return nil, invalid("chart archive is not a tar archive: %v", err)
		}
		dir, file := path.Split(path.Clean(h.Name))
		if file != "Chart.yaml" || strings.Count(strings.Trim(dir, "/"), "/") != 0 || dir == "" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, maxChartYAMLBytes+1))
		if err != nil || len(raw) > maxChartYAMLBytes {
			return nil, invalid("chart archive Chart.yaml is unreadable or too large")
		}
		return parseChartYAML(raw)
	}
}

func parseChartYAML(raw []byte) (*ChartMeta, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, invalid("Chart.yaml is not valid YAML: %v", err)
	}
	name, _ := doc["name"].(string)
	version, _ := doc["version"].(string)
	if name == "" || version == "" {
		return nil, invalid("Chart.yaml has no name or version")
	}
	if !chartNamePattern.MatchString(name) {
		return nil, invalid("Chart.yaml name %q is not a valid chart name", name)
	}
	config, err := json.Marshal(doc)
	if err != nil {
		return nil, invalid("Chart.yaml cannot be represented as JSON: %v", err)
	}
	if len(config) > maxChartConfigBytes {
		return nil, invalid("Chart.yaml is %d bytes as JSON, over %d", len(config), maxChartConfigBytes)
	}
	return &ChartMeta{Name: name, Version: version, config: config}, nil
}

// Tag is the OCI tag Helm uses for a chart version ("+" is not allowed).
func (m *ChartMeta) Tag() string { return strings.ReplaceAll(m.Version, "+", "_") }

// Config returns Chart.yaml as JSON, the config blob of the chart's OCI
// manifest.
func (m *ChartMeta) Config() []byte { return m.config }

// chartManifest builds the chart's OCI manifest. It carries no timestamp, so
// the same archive always yields the same manifest digest.
func chartManifest(m *ChartMeta, archive []byte) (config, layer ocispec.Descriptor, manifest []byte, err error) {
	config = ocispec.Descriptor{MediaType: ChartConfigMediaType, Digest: digest.FromBytes(m.config), Size: int64(len(m.config))}
	layer = ocispec.Descriptor{MediaType: ChartLayerMediaType, Digest: digest.FromBytes(archive), Size: int64(len(archive))}
	manifest, err = json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
		Annotations: map[string]string{
			ocispec.AnnotationTitle:   m.Name,
			ocispec.AnnotationVersion: m.Version,
		},
	})
	return config, layer, manifest, err
}

// PushChart pushes a chart archive to r, tagged with its version, and returns
// the manifest digest. An existing tag is reused when it points at the same
// manifest; a tag that points elsewhere is a conflict, never overwritten.
func (c *Client) PushChart(ctx context.Context, r Repository, archive []byte) (string, error) {
	meta, err := ReadChartMeta(archive)
	if err != nil {
		return "", err
	}
	return c.pushChart(ctx, r, archive, meta, meta.Tag())
}

func (c *Client) pushChart(ctx context.Context, r Repository, archive []byte, meta *ChartMeta, tag string) (string, error) {
	configDesc, layerDesc, manifest, err := chartManifest(meta, archive)
	if err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInternal, "encode chart manifest", err)
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return "", err
	}
	return pushManifest(ctx, repo, r, tag, manifest, blob{configDesc, meta.config}, blob{layerDesc, archive})
}

// ReuseChart returns the chart tagged version in r, without an upstream
// request, when the tag holds digest. A missing tag, or one holding another
// digest, is NOT_FOUND.
func (c *Client) ReuseChart(ctx context.Context, r Repository, version, digest string) (*MirroredChart, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return nil, err
	}
	m, _, err := pullChart(ctx, repo, r, version)
	if err != nil {
		return nil, err
	}
	if m.Digest != digest {
		// NOT_FOUND, so the caller mirrors from upstream, which reports a
		// moved tag as a conflict.
		return nil, operrors.New(operrors.ErrCodeNotFound, fmt.Sprintf("%s:%s holds %s, not %s", r, version, m.Digest, digest))
	}
	return m, nil
}

// pullChart returns the Helm chart m tagged version in src, and its metadata
// meta. The manifest must carry a Helm config and exactly one chart layer,
// both present and verified against their digests, and the layer must hold
// the chart src names at version.
func pullChart(ctx context.Context, repo *remote.Repository, src Repository, version string) (m *MirroredChart, meta *ChartMeta, err error) {
	tag := (&ChartMeta{Version: version}).Tag()
	desc, manifest, err := fetchManifest(ctx, repo, src, tag, maxManifestBytes)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case manifest.Config.MediaType != ChartConfigMediaType:
		return nil, nil, invalid("%s:%s is not a Helm chart (config %q)", src, tag, manifest.Config.MediaType)
	case manifest.Config.Size > maxChartConfigBytes:
		return nil, nil, invalid("chart config of %s:%s is %d bytes, over %d", src, tag, manifest.Config.Size, maxChartConfigBytes)
	}
	if _, err = content.FetchAll(ctx, repo, manifest.Config); err != nil {
		return nil, nil, classify(err, fmt.Sprintf("fetch chart config %s@%s", src, manifest.Config.Digest))
	}
	var layer *ocispec.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType != ChartLayerMediaType {
			continue
		}
		if layer != nil {
			return nil, nil, invalid("%s:%s has more than one chart layer", src, tag)
		}
		layer = &manifest.Layers[i]
	}
	switch {
	case layer == nil:
		return nil, nil, invalid("%s:%s has no chart layer", src, tag)
	case layer.Size > defaults.MaxChartArchiveBytes:
		return nil, nil, invalid("chart layer of %s:%s is %d bytes, over %d", src, tag, layer.Size, defaults.MaxChartArchiveBytes)
	}
	archive, err := content.FetchAll(ctx, repo, *layer)
	if err != nil {
		return nil, nil, classify(err, fmt.Sprintf("fetch chart layer %s@%s", src, layer.Digest))
	}
	meta, err = ReadChartMeta(archive)
	if err != nil {
		return nil, nil, err
	}
	if name := path.Base(src.Path); meta.Name != name || meta.Version != version {
		return nil, nil, invalid("%s:%s holds chart %s %s, want %s %s", src, tag, meta.Name, meta.Version, name, version)
	}
	return &MirroredChart{Repository: src, Digest: desc.Digest.String(), ArchiveDigest: layer.Digest.String(), Archive: archive}, meta, nil
}
