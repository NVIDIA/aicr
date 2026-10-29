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
	"go.yaml.in/yaml/v3"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Helm's OCI media types.
const (
	ChartConfigMediaType = "application/vnd.cncf.helm.config.v1+json"
	ChartLayerMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// maxChartYAMLBytes bounds Chart.yaml inside an archive.
const maxChartYAMLBytes = 1 << 20

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
	return &ChartMeta{Name: name, Version: version, config: config}, nil
}

// Tag is the OCI tag Helm uses for a chart version ("+" is not allowed).
func (m *ChartMeta) Tag() string { return strings.ReplaceAll(m.Version, "+", "_") }

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
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifest), Size: int64(len(manifest)),
	}
	want := manifestDesc.Digest.String()

	ctx, cancel := context.WithTimeout(ctx, defaults.RegistryTimeout)
	defer cancel()
	repo, err := c.repository(r)
	if err != nil {
		return "", err
	}
	switch existing, rerr := repo.Resolve(ctx, tag); {
	case rerr == nil && existing.Digest.String() == want:
		return want, nil
	case rerr == nil:
		return "", operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("%s:%s already holds %s, not %s", r, tag, existing.Digest, want))
	case operrors.CodeOf(classify(rerr, "")) != operrors.ErrCodeNotFound:
		return "", classify(rerr, fmt.Sprintf("resolve %s:%s", r, tag))
	}
	for _, b := range []struct {
		desc ocispec.Descriptor
		data []byte
	}{{configDesc, meta.config}, {layerDesc, archive}} {
		exists, eerr := repo.Exists(ctx, b.desc)
		if eerr != nil {
			return "", classify(eerr, "check chart blob")
		}
		if exists {
			continue
		}
		if perr := repo.Push(ctx, b.desc, bytes.NewReader(b.data)); perr != nil {
			return "", classify(perr, "push chart blob to "+r.String())
		}
	}
	if err = repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifest), tag); err != nil {
		return "", classify(err, fmt.Sprintf("push chart manifest %s:%s", r, tag))
	}
	return want, nil
}
