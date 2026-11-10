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
	"bytes"
	"encoding/json"
	"io"
	"log"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v4"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/registry/registrytest"
)

func TestLatestStable(t *testing.T) {
	t.Parallel()
	tags := []string{"1.2.0-rc1", "1.2.0", "1.10.0", "1.3.0", "1.3.0-rc2", "sha256-abc", "latest", "v1.9.0"}
	tests := []struct {
		name    string
		tags    []string
		below   string
		want    string
		wantErr string
	}{
		{name: "no bound: the newest stable release", tags: tags, below: "", want: "1.10.0"},
		{name: "newest stable below a later release", tags: tags, below: "1.10.0", want: "1.3.0"},
		{name: "a prerelease bound excludes its own release", tags: tags, below: "1.3.0-rc1", want: "1.2.0"},
		{name: "numeric, not lexical, order", tags: tags, below: "2.0.0", want: "1.10.0"},
		{name: "a bound with a leading v", tags: tags, below: "v1.3.0", want: "1.2.0"},
		{name: "only prereleases", tags: []string{"1.2.0-rc1"}, below: "9.0.0", wantErr: "no stable release"},
		{name: "nothing below the bound", tags: tags, below: "1.2.0", wantErr: "no stable release"},
		{name: "an invalid bound", tags: tags, below: "next", wantErr: "not a semantic version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := latestStable(tt.tags, tt.below)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("latestStable() = %q, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("latestStable() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestChartPinsRefusesUnpinnedImages(t *testing.T) {
	t.Parallel()
	const pinned = "ghcr.io/nvidia/aicr-operator@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"
	values := func(operator, registry, aicr string) []byte {
		return []byte("aicr:\n  version: v1.1.2\n  image: \"" + aicr + "\"\n  validatorImages: []\n" +
			"operator:\n  image: \"" + operator + "\"\n" +
			"registry:\n  inCluster:\n    image: \"" + registry + "\"\n")
	}
	const aicr = "ghcr.io/nvidia/aicr:v1.1.2@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	tests := []struct {
		name    string
		values  []byte
		wantErr string
	}{
		{name: "every image pinned", values: values(pinned, pinned, aicr)},
		{name: "operator by tag", values: values("ghcr.io/nvidia/aicr-operator:v1.2.0", pinned, aicr), wantErr: "operator.image"},
		{name: "registry by tag", values: values(pinned, "ghcr.io/nvidia/aicr-registry:v1.2.0", pinned), wantErr: "registry.inCluster.image"},
		{name: "AICR by tag", values: values(pinned, pinned, "ghcr.io/nvidia/aicr:v1.1.2"), wantErr: "aicr.image"},
		{name: "operator empty", values: values("", pinned, aicr), wantErr: "operator.image"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := chartPins(tt.values)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("chartPins() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "not pinned by digest") {
				t.Fatalf("chartPins() = %v; want an error naming %s as not pinned by digest", err, tt.wantErr)
			}
		})
	}
}

// archiveFile finds the same top-level chart files registry.ReadChartMeta
// does, so a chart push accepts is one pull can read.
func TestArchiveFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string]string
		want    string
		wantErr string
	}{
		{name: "the chart's top-level file", files: map[string]string{"demo/values.yaml": "a: 1"}, want: "a: 1"},
		{name: "a name with a leading ./", files: map[string]string{"./demo/values.yaml": "a: 1"}, want: "a: 1"},
		{name: "a subchart's file is skipped", files: map[string]string{"demo/charts/sub/values.yaml": "a: 1"}, wantErr: "has no values.yaml"},
		{name: "a file outside the chart directory is skipped", files: map[string]string{"values.yaml": "a: 1"}, wantErr: "has no values.yaml"},
		{name: "a file at the limit", files: map[string]string{"demo/values.yaml": strings.Repeat("a", maxArchiveFileBytes)}, want: strings.Repeat("a", maxArchiveFileBytes)},
		{name: "a file over the limit is refused, not cut short", files: map[string]string{"demo/values.yaml": strings.Repeat("a", maxArchiveFileBytes+1)}, wantErr: "over"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := archiveFile(registrytest.TarGz(t, tt.files), "values.yaml")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("archiveFile() = %q, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Fatalf("archiveFile() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// pull reads back exactly what push published: the chart archive, both image
// indexes as OCI layouts, and candidate.json and candidate-values.yaml in the
// shape a candidate build writes, so the qualify suite can install it.
func TestPullWritesCandidateLayout(t *testing.T) {
	t.Parallel()
	host := testRegistry(t)
	repository := host + "/nvidia/aicr-operator"
	src, m := writeCandidateAICR(t, repository, []byte("operator-binary"), pinnedAICR)
	if err := run([]string{"push", "-dir", src, "-chart-registry", host + "/nvidia/charts", "-plain-http"}, io.Discard); err != nil {
		t.Fatalf("push: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "published")
	var out bytes.Buffer
	if err := run([]string{"pull", "-chart-registry", host + "/nvidia/charts", "-chart", "demo",
		"-version", "0.1.0-rc.1", "-dir", dir, "-plain-http"}, &out); err != nil {
		t.Fatalf("pull: %v", err)
	}

	printed := pushed(t, out.String())
	for k, v := range map[string]string{"version": "0.1.0-rc.1", "image-ref": m.Operator.Ref(), "registry-ref": m.Registry.Ref()} {
		if printed[k] != v {
			t.Errorf("pull printed %s=%q, want %q", k, printed[k], v)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := &Manifest{}
	if err = json.Unmarshal(data, got); err != nil {
		t.Fatal(err)
	}
	if got.Operator.Digest != m.Operator.Digest || !maps.Equal(got.Operator.Platforms, m.Operator.Platforms) {
		t.Errorf("candidate.json operator = %+v, want %+v", got.Operator, m.Operator)
	}
	if got.Registry.Digest != m.Registry.Digest || got.Registry.Repository != m.Registry.Repository {
		t.Errorf("candidate.json registry = %+v, want %+v", got.Registry, m.Registry)
	}
	if got.Version == "" {
		t.Error("candidate.json names no version")
	}
	for layout, want := range map[string]Image{"image": m.Operator, "registry-image": m.Registry} {
		img, readErr := readImage(filepath.Join(dir, layout), want.Repository)
		if readErr != nil || img.Digest != want.Digest {
			t.Errorf("%s layout holds %s (%v), want %s", layout, img.Digest, readErr, want.Digest)
		}
	}
	archive, err := os.ReadFile(filepath.Join(dir, "chart", "demo-0.1.0-rc.1.tgz"))
	if err != nil || sha256Of(archive) != m.Chart.Digest {
		t.Errorf("chart/demo-0.1.0-rc.1.tgz is not the published archive %s (%v)", m.Chart.Digest, err)
	}

	data, err = os.ReadFile(filepath.Join(dir, "candidate-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var vals struct {
		AICR     AICR `yaml:"aicr"`
		Operator struct {
			Image string `yaml:"image"`
		} `yaml:"operator"`
	}
	if err = yaml.Unmarshal(data, &vals); err != nil {
		t.Fatal(err)
	}
	if vals.AICR.Version != pinnedAICR.Version || vals.AICR.Image != pinnedAICR.Image || vals.Operator.Image != m.Operator.Ref() {
		t.Errorf("candidate-values.yaml = %+v, want AICR %s %s and operator %s", vals, pinnedAICR.Version, pinnedAICR.Image, m.Operator.Ref())
	}
}

// Asking for the newest stable release when only prereleases are published is
// an error that names the repository, never a silent pick of a prerelease.
func TestPullLatestRefusesNoStableRelease(t *testing.T) {
	t.Parallel()
	host := testRegistry(t)
	src, _ := writeCandidateAICR(t, host+"/nvidia/aicr-operator", []byte("operator-binary"), pinnedAICR)
	if err := run([]string{"push", "-dir", src, "-chart-registry", host + "/nvidia/charts", "-plain-http"}, io.Discard); err != nil {
		t.Fatalf("push: %v", err)
	}
	err := run([]string{"pull", "-chart-registry", host + "/nvidia/charts", "-chart", "demo",
		"-version", "latest", "-below", "9.0.0", "-dir", t.TempDir(), "-plain-http"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no stable release") || !strings.Contains(err.Error(), host+"/nvidia/charts/demo") {
		t.Fatalf("pull -version latest = %v; want an error naming %s/nvidia/charts/demo", err, host)
	}
}

// pull never writes over an existing directory's content, and a version the
// registry does not have is an error naming it.
func TestPullRefuses(t *testing.T) {
	t.Parallel()
	host := testRegistry(t)
	src, _ := writeCandidateAICR(t, host+"/nvidia/aicr-operator", []byte("operator-binary"), pinnedAICR)
	if err := run([]string{"push", "-dir", src, "-chart-registry", host + "/nvidia/charts", "-plain-http"}, io.Discard); err != nil {
		t.Fatalf("push: %v", err)
	}
	used := t.TempDir()
	if err := os.WriteFile(filepath.Join(used, "keep.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, version, dir, wantErr string
	}{
		{name: "a non-empty directory", version: "0.1.0-rc.1", dir: used, wantErr: "is not empty"},
		{name: "an unpublished version", version: "9.9.9", dir: filepath.Join(t.TempDir(), "out"), wantErr: "demo:9.9.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := run([]string{"pull", "-chart-registry", host + "/nvidia/charts", "-chart", "demo",
				"-version", tt.version, "-dir", tt.dir, "-plain-http"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("pull = %v; want an error containing %q", err, tt.wantErr)
			}
			if entries, _ := os.ReadDir(used); tt.dir == used && len(entries) != 1 {
				t.Fatalf("pull wrote into the non-empty directory: %d entries", len(entries))
			}
		})
	}
}

// Only versions with a published release are candidates for latest: a chart
// tag pushed by a release that has not finished (or failed) is skipped.
func TestOnlyReleased(t *testing.T) {
	t.Parallel()
	tags := []string{"1.2.0", "1.3.0", "1.3.0-rc1", "sha256-abc"}
	tests := []struct {
		name     string
		released string
		want     []string
	}{
		{name: "an empty list keeps nothing", released: "", want: nil},
		{name: "a release still in progress is skipped", released: "v1.2.0,v1.1.2", want: []string{"1.2.0"}},
		{name: "with or without the leading v", released: "1.2.0, v1.3.0", want: []string{"1.2.0", "1.3.0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := onlyReleased(tags, tt.released); !slices.Equal(got, tt.want) {
				t.Fatalf("onlyReleased() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A chart tag that names another chart or version than its Chart.yaml is
// refused: the tag chosen is the release qualified and reported.
func TestPullRefusesMismatchedChart(t *testing.T) {
	t.Parallel()
	host := testRegistry(t)
	src, _ := writeCandidateAICR(t, host+"/nvidia/aicr-operator", []byte("operator-binary"), pinnedAICR)
	if err := run([]string{"push", "-dir", src, "-chart-registry", host + "/nvidia/charts", "-plain-http"}, io.Discard); err != nil {
		t.Fatalf("push: %v", err)
	}
	charts, err := remote.NewRepository(host + "/nvidia/charts/demo")
	if err != nil {
		t.Fatal(err)
	}
	charts.PlainHTTP = true
	desc, err := charts.Resolve(t.Context(), "0.1.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = charts.Tag(t.Context(), desc, "9.0.0"); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"pull", "-chart-registry", host + "/nvidia/charts", "-chart", "demo",
		"-version", "9.0.0", "-dir", filepath.Join(t.TempDir(), "out"), "-plain-http"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "0.1.0-rc.1") {
		t.Fatalf("pull of a tag naming another version = %v; want an error naming the chart's own version", err)
	}
}

// pinnedAICR is demoAICR with full digests, as a published chart pins them.
var pinnedAICR = AICR{
	Version:         "v1.1.0",
	Image:           "ghcr.io/nvidia/aicr:v1.1.0@sha256:" + strings.Repeat("a", 64),
	ValidatorImages: []string{"ghcr.io/nvidia/aicr-validators/deployment:v1.1.0@sha256:" + strings.Repeat("b", 64)},
}

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// A -released list that is given but empty, as when no GitHub release is
// published, selects nothing rather than every chart tag.
func TestPullLatestRefusesEmptyReleasedList(t *testing.T) {
	t.Parallel()
	host := testRegistry(t)
	src, _ := writeCandidateAICR(t, host+"/nvidia/aicr-operator", []byte("operator-binary"), pinnedAICR)
	if err := run([]string{"push", "-dir", src, "-chart-registry", host + "/nvidia/charts", "-plain-http"}, io.Discard); err != nil {
		t.Fatalf("push: %v", err)
	}
	charts, err := remote.NewRepository(host + "/nvidia/charts/demo")
	if err != nil {
		t.Fatal(err)
	}
	charts.PlainHTTP = true
	desc, err := charts.Resolve(t.Context(), "0.1.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = charts.Tag(t.Context(), desc, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"pull", "-chart-registry", host + "/nvidia/charts", "-chart", "demo",
		"-version", "latest", "-released", "", "-dir", filepath.Join(t.TempDir(), "out"), "-plain-http"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no stable release") {
		t.Fatalf("pull -released \"\" = %v; want no stable release, not the newest chart tag", err)
	}
}

// A chart whose archive layer declares more than a chart may hold is refused
// before its archive is read into memory.
func TestPullRefusesOversizedChart(t *testing.T) {
	t.Parallel()
	host := testRegistry(t)
	charts, err := remote.NewRepository(host + "/nvidia/charts/demo")
	if err != nil {
		t.Fatal(err)
	}
	charts.PlainHTTP = true
	config, err := oras.PushBytes(t.Context(), charts, registry.ChartConfigMediaType, []byte(`{"name":"demo","version":"1.0.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers: []ocispec.Descriptor{{
			MediaType: registry.ChartLayerMediaType,
			Digest:    digest.FromString("absent"),
			Size:      defaults.HelmChartArtifactLimit + 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = oras.TagBytes(t.Context(), charts, ocispec.MediaTypeImageManifest, manifest, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"pull", "-chart-registry", host + "/nvidia/charts", "-chart", "demo",
		"-version", "1.0.0", "-dir", filepath.Join(t.TempDir(), "out"), "-plain-http"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("pull of an oversized chart = %v; want it refused by size before the archive is read", err)
	}
}
