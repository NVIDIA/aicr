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

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v4"
	"golang.org/x/mod/semver"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// pull writes a published release into dir in the layout a candidate build
// writes: the chart archive, the operator and in-cluster registry image indexes
// the chart defaults to as OCI layouts, candidate.json and candidate-values.yaml.
// tools/operator-qualify then installs it like any candidate. Only images the
// chart pins by digest are pulled, so the layout is exactly what the chart
// installs. It prints what it pulled as key=value lines.
func pull(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	chartRegistry := fs.String("chart-registry", "ghcr.io/nvidia/charts", "Registry path the chart is published under.")
	chart := fs.String("chart", "aicr-operator", "Chart name.")
	version := fs.String("version", "latest", "Chart version without the leading v, or latest: the newest stable release, older than -below when set.")
	below := fs.String("below", "", "With -version latest, a version the release must be older than, such as the release being built.")
	released := fs.String("released", "", "With -version latest, the comma-separated versions with a published release; others are skipped, and an empty list selects none.")
	dir := fs.String("dir", "dist/published", "Directory to write the release into; it must not exist or be empty.")
	plainHTTP := fs.Bool("plain-http", false, "Pull over plain HTTP; test registries only.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var releasedList *string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "released" {
			releasedList = released
		}
	})
	if entries, err := os.ReadDir(*dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty; remove it first", *dir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	ref := *chartRegistry + "/" + *chart
	charts, err := repository(ref, *plainHTTP)
	if err != nil {
		return err
	}
	tag := *version
	if tag == "latest" {
		if tag, err = latestTag(ctx, charts, ref, releasedList, *below); err != nil {
			return err
		}
	}

	published, err := fetchChart(ctx, charts, ref, tag, *chart)
	if err != nil {
		return err
	}
	archive, chartVersion := published.archive, published.version
	valuesYAML, err := archiveFile(archive, "values.yaml")
	if err != nil {
		return err
	}
	operatorRef, registryRef, aicr, err := chartPins(valuesYAML)
	if err != nil {
		return fmt.Errorf("%s:%s: %w", ref, tag, err)
	}

	operator, err := pullImage(ctx, operatorRef, filepath.Join(*dir, "image"), *plainHTTP)
	if err != nil {
		return err
	}
	registry, err := pullImage(ctx, registryRef, filepath.Join(*dir, "registry-image"), *plainHTTP)
	if err != nil {
		return err
	}
	archiveName := fmt.Sprintf("%s-%s.tgz", published.name, chartVersion)
	if err = os.MkdirAll(filepath.Join(*dir, "chart"), 0o755); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*dir, "chart", archiveName), archive, 0o600); err != nil {
		return err
	}

	m := &Manifest{
		SchemaVersion: 1, Version: published.appVersion,
		Operator: operator,
		Chart:    File{Name: archiveName, Digest: sha256Of(archive)},
		AICR:     aicr,
		Registry: registry,
	}
	if err = m.seal(); err != nil {
		return err
	}
	if err = writeManifest(*dir, m); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "version=%s\nchart-repository=%s\nchart-digest=%s\nimage-ref=%s\nregistry-ref=%s\n",
		chartVersion, ref, published.desc.Digest, operator.Ref(), registry.Ref())
	return err
}

// publishedChart is a chart fetched from a registry and checked against the
// tag it was fetched by.
type publishedChart struct {
	desc                      ocispec.Descriptor
	archive                   []byte
	name, version, appVersion string
}

// fetchChart fetches ref:tag, which must be one Helm archive no larger than a
// chart may be, holding chart at the version the tag names.
func fetchChart(ctx context.Context, charts *remote.Repository, ref, tag, chart string) (*publishedChart, error) {
	desc, data, err := oras.FetchBytes(ctx, charts, tag, oras.DefaultFetchBytesOptions)
	if err != nil {
		return nil, fmt.Errorf("fetch %s:%s: %w", ref, tag, err)
	}
	var manifest ocispec.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s:%s: %w", ref, tag, err)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != registry.ChartLayerMediaType {
		return nil, fmt.Errorf("%s:%s is not a Helm chart with one archive layer", ref, tag)
	}
	if size := manifest.Layers[0].Size; size > defaults.HelmChartArtifactLimit {
		return nil, fmt.Errorf("%s:%s archive is %d bytes, over the %d byte limit", ref, tag, size, defaults.HelmChartArtifactLimit)
	}
	archive, err := content.FetchAll(ctx, charts, manifest.Layers[0])
	if err != nil {
		return nil, fmt.Errorf("fetch the archive of %s:%s: %w", ref, tag, err)
	}
	meta, err := chartMetadata(archive)
	if err != nil {
		return nil, err
	}
	c := &publishedChart{desc: desc, archive: archive}
	c.name, _ = meta["name"].(string)
	c.version, _ = meta["version"].(string)
	c.appVersion, _ = meta["appVersion"].(string)
	if c.name == "" || c.version == "" || c.appVersion == "" {
		return nil, fmt.Errorf("%s:%s names no chart name, version or appVersion", ref, tag)
	}
	// Helm tags a chart with its version, "+" written as "_".
	if c.name != chart || strings.ReplaceAll(c.version, "+", "_") != tag {
		return nil, fmt.Errorf("%s:%s holds chart %s %s", ref, tag, c.name, c.version)
	}
	return c, nil
}

// chartMetadata reads the top-level Chart.yaml from a chart archive.
func chartMetadata(archive []byte) (map[string]any, error) {
	data, err := archiveFile(archive, "Chart.yaml")
	if err != nil {
		return nil, err
	}
	meta := map[string]any{}
	if err = yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse Chart.yaml: %w", err)
	}
	return meta, nil
}

// maxArchiveFileBytes bounds a file archiveFile reads.
const maxArchiveFileBytes = 1 << 20

// archiveFile reads the chart's top-level file name from a chart archive.
func archiveFile(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("read the chart archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the chart archive has no %s", name)
		}
		if err != nil {
			return nil, fmt.Errorf("read the chart archive: %w", err)
		}
		dir, file := path.Split(path.Clean(h.Name))
		if file != name || strings.Count(strings.Trim(dir, "/"), "/") != 0 || dir == "" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxArchiveFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("read %s from the chart archive: %w", name, err)
		}
		if len(data) > maxArchiveFileBytes {
			return nil, fmt.Errorf("%s in the chart archive is over %d bytes", name, maxArchiveFileBytes)
		}
		return data, nil
	}
}

// writeManifest writes candidate.json and candidate-values.yaml for m.
func writeManifest(dir string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "candidate.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	vals, err := values(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "candidate-values.yaml"), vals, 0o600)
}

// pullImage copies the image index ref names, pinned by digest, and
// everything it references into an OCI layout at layout.
func pullImage(ctx context.Context, ref, layout string, plainHTTP bool) (Image, error) {
	name, digest, _ := strings.Cut(ref, "@")
	repo, err := repository(name, plainHTTP)
	if err != nil {
		return Image{}, err
	}
	desc, err := repo.Resolve(ctx, digest)
	if err != nil {
		return Image{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	store, err := oci.New(layout)
	if err != nil {
		return Image{}, fmt.Errorf("create the layout %s: %w", layout, err)
	}
	if err = oras.CopyGraph(ctx, repo, store, desc, oras.DefaultCopyGraphOptions); err != nil {
		return Image{}, fmt.Errorf("pull %s: %w", ref, err)
	}
	// The store lists every manifest it wrote in index.json; a candidate's
	// layout lists only the image index, which readImage and
	// tools/operator-qualify read as manifests[0].
	root, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{desc}})
	if err != nil {
		return Image{}, err
	}
	if err = os.WriteFile(filepath.Join(layout, "index.json"), root, 0o600); err != nil {
		return Image{}, err
	}
	img, err := readImage(layout, name)
	if err != nil {
		return Image{}, err
	}
	if img.Digest != digest {
		return Image{}, fmt.Errorf("%s holds %s, not %s", layout, img.Digest, digest)
	}
	return img, nil
}

// chartPins reads the images a chart's values.yaml installs by default and
// refuses any not pinned by digest: the qualify suite asserts the running
// operator by digest, and a tag can move after the chart was published.
func chartPins(valuesYAML []byte) (operator, registry string, aicr AICR, err error) {
	var vals struct {
		AICR struct {
			Version         string   `yaml:"version"`
			Image           string   `yaml:"image"`
			ValidatorImages []string `yaml:"validatorImages"`
		} `yaml:"aicr"`
		Operator struct {
			Image string `yaml:"image"`
		} `yaml:"operator"`
		Registry struct {
			InCluster struct {
				Image string `yaml:"image"`
			} `yaml:"inCluster"`
		} `yaml:"registry"`
	}
	if err = yaml.Unmarshal(valuesYAML, &vals); err != nil {
		return "", "", AICR{}, fmt.Errorf("parse values.yaml: %w", err)
	}
	pins := []struct{ field, ref string }{
		{"operator.image", vals.Operator.Image},
		{"registry.inCluster.image", vals.Registry.InCluster.Image},
		{"aicr.image", vals.AICR.Image},
	}
	for i, v := range vals.AICR.ValidatorImages {
		pins = append(pins, struct{ field, ref string }{fmt.Sprintf("aicr.validatorImages[%d]", i), v})
	}
	for _, p := range pins {
		if !pinnedByDigest(p.ref) {
			return "", "", AICR{}, fmt.Errorf("%s %q is not pinned by digest", p.field, p.ref)
		}
	}
	return vals.Operator.Image, vals.Registry.InCluster.Image,
		AICR{Version: vals.AICR.Version, Image: vals.AICR.Image, ValidatorImages: vals.AICR.ValidatorImages}, nil
}

// pinnedByDigest reports a reference that ends in a sha256 digest.
func pinnedByDigest(ref string) bool {
	_, digest, ok := strings.Cut(ref, "@")
	return ok && strings.HasPrefix(digest, "sha256:") && len(digest) == len("sha256:")+64
}

// latestTag picks the newest stable tag of ref older than below, among the
// versions released lists when it is non-nil.
func latestTag(ctx context.Context, charts *remote.Repository, ref string, released *string, below string) (string, error) {
	var tags []string
	if err := charts.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	}); err != nil {
		return "", fmt.Errorf("list %s: %w", ref, err)
	}
	if released != nil {
		tags = onlyReleased(tags, *released)
	}
	tag, err := latestStable(tags, below)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ref, err)
	}
	return tag, nil
}

// onlyReleased keeps the tags released lists, with or without a leading v;
// an empty list keeps none. A release pushes its chart before it attests
// and publishes it, so a chart tag alone does not mean a finished release.
func onlyReleased(tags []string, released string) []string {
	set := map[string]bool{}
	for _, v := range strings.Split(released, ",") {
		if v = strings.TrimPrefix(strings.TrimSpace(v), "v"); v != "" {
			set[v] = true
		}
	}
	var out []string
	for _, t := range tags {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}

// latestStable returns the highest tag that is a full stable release
// (X.Y.Z, no prerelease or build), older than below unless below is empty.
// Tags carry no leading v, as Helm writes them; below may have one.
func latestStable(tags []string, below string) (string, error) {
	bound := ""
	if below != "" {
		bound = "v" + strings.TrimPrefix(below, "v")
		if !semver.IsValid(bound) {
			return "", fmt.Errorf("%q is not a semantic version", below)
		}
	}
	best := ""
	for _, t := range tags {
		v := "v" + t
		if !semver.IsValid(v) || semver.Canonical(v) != v || semver.Prerelease(v) != "" ||
			bound != "" && semver.Compare(v, bound) >= 0 {

			continue
		}
		if best == "" || semver.Compare(v, best) > 0 {
			best = v
		}
	}
	if best == "" && below == "" {
		return "", errors.New("no stable release")
	}
	if best == "" {
		return "", fmt.Errorf("no stable release below %s", below)
	}
	return strings.TrimPrefix(best, "v"), nil
}
