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
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.yaml.in/yaml/v3"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
)

const demoValues = `# Installation values.

aicr:
  # Declared AICR release.
  version: v1.0.0
  image: ""
  validatorImages: []

operator:
  image: ""
  logLevel: info
`

var demoAICR = AICR{
	Version:         "v1.1.0",
	Image:           "ghcr.io/nvidia/aicr:v1.1.0@sha256:aa",
	ValidatorImages: []string{"ghcr.io/nvidia/aicr-validators/deployment:v1.1.0@sha256:bb"},
}

func chartFS() fstest.MapFS {
	return fstest.MapFS{
		"Chart.yaml":             {Data: []byte("apiVersion: v2\nname: demo\nversion: 0.1.0\nappVersion: 0.1.0\n")},
		"values.yaml":            {Data: []byte(demoValues)},
		"templates/cm.yaml":      {Data: []byte("kind: ConfigMap\n")},
		"files/crds/x.yaml":      {Data: []byte("kind: CRD\n")},
		".helmignore":            {Data: []byte("# comment\n.claude/\n*.swp\nNOTES.md\n")},
		".claude/settings.json":  {Data: []byte("{}")},
		"templates/cm.yaml.swp":  {Data: []byte("x")},
		"NOTES.md":               {Data: []byte("x")},
		"templates/.claude/keep": {Data: []byte("x")},
	}
}

func entries(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header carries name %q, time %v", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.ModTime.Unix() != 0 || h.Mode != 0o644 || h.Uid != 0 || h.Uname != "" {
			t.Errorf("%s: mtime %v mode %o owner %d %q", h.Name, h.ModTime, h.Mode, h.Uid, h.Uname)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
}

// The chart archive is the same bytes every time, stamped with the
// candidate version, without ignored files.
func TestPackageChart(t *testing.T) {
	t.Parallel()
	name, first, err := packageChart(chartFS(), "v0.2.0-rc.1", demoAICR, "ghcr.io/nvidia/aicr-operator@sha256:cc")
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := packageChart(chartFS(), "v0.2.0-rc.1", demoAICR, "ghcr.io/nvidia/aicr-operator@sha256:cc")
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("packaging is not deterministic: %v", err)
	}
	got := entries(t, first)
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{"demo/Chart.yaml", "demo/files/crds/x.yaml", "demo/templates/cm.yaml", "demo/values.yaml"}
	if name != "demo" || !slices.Equal(names, want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	if c := got["demo/Chart.yaml"]; !strings.Contains(c, "\nversion: 0.2.0-rc.1\n") || !strings.Contains(c, "\nappVersion: v0.2.0-rc.1\n") {
		t.Fatalf("Chart.yaml = %q", c)
	}
	wantValues := `# Installation values.

aicr:
  # Declared AICR release.
  version: v1.1.0
  image: "ghcr.io/nvidia/aicr:v1.1.0@sha256:aa"
  validatorImages:
    - "ghcr.io/nvidia/aicr-validators/deployment:v1.1.0@sha256:bb"

operator:
  image: "ghcr.io/nvidia/aicr-operator@sha256:cc"
  logLevel: info
`
	if v := got["demo/values.yaml"]; v != wantValues {
		t.Fatalf("values.yaml = %q, want %q", v, wantValues)
	}
	if _, other, _ := packageChart(chartFS(), "v0.2.0", demoAICR, "ghcr.io/nvidia/aicr-operator@sha256:cc"); bytes.Equal(first, other) {
		t.Fatal("another version packaged to the same bytes")
	}
	broken := chartFS()
	broken["Chart.yaml"] = &fstest.MapFile{Data: []byte("name: demo\nversion: 1\n")}
	if _, _, err = packageChart(broken, "v0.2.0", demoAICR, "ghcr.io/nvidia/aicr-operator@sha256:cc"); err == nil {
		t.Fatal("a Chart.yaml without appVersion was packaged")
	}
	for name, values := range map[string]string{
		"a pinned operator image":   strings.Replace(demoValues, "operator:\n  image: \"\"", "operator:\n  image: \"x\"", 1),
		"no AICR section":           strings.Replace(demoValues, "aicr:", "other:", 1),
		"two operator image fields": demoValues + "  image: \"\"\n",
	} {
		broken = chartFS()
		broken["values.yaml"] = &fstest.MapFile{Data: []byte(values)}
		if _, _, err = packageChart(broken, "v0.2.0", demoAICR, "ghcr.io/nvidia/aicr-operator@sha256:cc"); err == nil {
			t.Errorf("%s: values.yaml was stamped", name)
		}
	}
}

// writeLayout writes an OCI layout as ko does: an index of one image index
// whose platform manifests' last layer holds the binary.
func writeLayout(t *testing.T, dir string, binary []byte) {
	t.Helper()
	blobs := filepath.Join(dir, "image", "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	// put stores a blob and returns its descriptor fields.
	put := func(mediaType string, data []byte) string {
		d := sha256Of(data)
		if err := os.WriteFile(filepath.Join(blobs, strings.TrimPrefix(d, "sha256:")), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf(`"mediaType":%q,"digest":%q,"size":%d`, mediaType, d, len(data))
	}
	var layer bytes.Buffer
	gz := gzip.NewWriter(&layer)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "/ko-app/aicr-operator", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(binary)
	_ = tw.Close()
	_ = gz.Close()
	const layerType = "application/vnd.oci.image.layer.v1.tar+gzip"
	manifest := put("application/vnd.oci.image.manifest.v1+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",`+
		`"config":{`+put("application/vnd.oci.image.config.v1+json", []byte(`{"architecture":"amd64","os":"linux"}`))+`},`+
		`"layers":[{`+put(layerType, []byte("base"))+`},{`+put(layerType, layer.Bytes())+`}]}`))
	index := put("application/vnd.oci.image.index.v1+json", []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
		`{`+manifest+`,"platform":{"os":"linux","architecture":"amd64"}},`+
		`{`+manifest+`,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}`))
	top := `{"schemaVersion":2,"manifests":[{` + index + `}]}`
	for name, data := range map[string]string{"index.json": top, "oci-layout": `{"imageLayoutVersion":"1.0.0"}`} {
		if err := os.WriteFile(filepath.Join(dir, "image", name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// build records every identity, its deployment inputs and a digest over
// them; extract recovers the shipped binary from the image.
func TestBuildAndExtract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeLayout(t, dir, []byte("operator-binary"))
	for _, d := range []string{"plugin", "sbom"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin", "kubectl-aicr-linux-amd64"), []byte("plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"build", "-dir", dir, "-version", "v0.1.0-rc.1", "-repository", "ghcr.io/nvidia/aicr-operator",
		"-commit", "abc", "-commit-date", "2026-10-15T00:00:00Z", "-go-version", "go1.27.2",
		"-chart", "../../charts/aicr-operator", "-settings", "../../.settings.yaml", "-ko", "../../.ko.yaml", "-go-sum", "../../go.sum"}
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatalf("build: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "candidate.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{}
	if err = json.Unmarshal(data, m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "v0.1.0-rc.1" || len(m.Operator.Platforms) != 2 || m.Operator.Platforms["linux/arm64/v8"] == "" ||
		m.Chart.Name != "aicr-operator-0.1.0-rc.1.tgz" || m.Plugin["kubectl-aicr-linux-amd64"] != sha256Of([]byte("plugin")) ||
		!strings.Contains(m.AICR.Image, "@sha256:") || len(m.AICR.ValidatorImages) == 0 || m.Inputs["go.sum"] == "" ||
		!strings.Contains(m.Toolchain.BaseImage, "@sha256:") || !strings.Contains(out.String(), m.Digest) {

		t.Fatalf("candidate.json = %s", data)
	}
	digest := m.Digest
	if err = m.seal(); err != nil || m.Digest != digest {
		t.Fatalf("the recorded digest does not cover the manifest: %s != %s", m.Digest, digest)
	}
	m.Plugin["kubectl-aicr-linux-amd64"] = sha256Of([]byte("other"))
	if err = m.seal(); err != nil || m.Digest == digest {
		t.Fatal("a changed artifact keeps the candidate digest")
	}
	var vals struct {
		AICR     struct{ Version, Image string } `yaml:"aicr"`
		Operator struct{ Image string }          `yaml:"operator"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "candidate-values.yaml"))
	if err != nil || yaml.Unmarshal(raw, &vals) != nil || vals.Operator.Image != "ghcr.io/nvidia/aicr-operator@"+m.Operator.Digest ||
		vals.AICR.Image != m.AICR.Image {

		t.Fatalf("candidate-values.yaml = %s (%v)", raw, err)
	}

	archive, err := os.ReadFile(filepath.Join(dir, "chart", m.Chart.Name))
	if err != nil {
		t.Fatal(err)
	}
	if v := entries(t, archive)["aicr-operator/values.yaml"]; !strings.Contains(v, fmt.Sprintf("  image: %q\n", m.Operator.Ref())) ||
		!strings.Contains(v, fmt.Sprintf("  image: %q\n", m.AICR.Image)) {

		t.Fatalf("the packaged chart does not default to the candidate's images: %s", v)
	}

	bin := filepath.Join(dir, "aicr-operator")
	if err = run([]string{"extract", "-dir", dir, "-platform", "linux/amd64", "-out", bin}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(bin); string(got) != "operator-binary" {
		t.Fatalf("extracted %q", got)
	}
	if err = run([]string{"extract", "-dir", dir, "-platform", "windows/amd64", "-out", bin}, io.Discard); err == nil {
		t.Fatal("extracted a platform the candidate lacks")
	}
}

func TestBuildRefuses(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"development version": {"build", "-version", "v0.0.0-dev", "-repository", "r", "-commit", "c", "-commit-date", "d", "-go-version", "g"},
		"no repository":       {"build", "-version", "v0.1.0", "-commit", "c", "-commit-date", "d", "-go-version", "g"},
		"no command":          nil,
		"unknown command":     {"publish"},
		"extract without out": {"extract"},
	} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("%s: run() succeeded", name)
		}
	}
}

// writeCandidate writes a candidate with the given image binary, whose chart
// defaults to the given AICR image, as candidate.json names them.
func writeCandidate(t *testing.T, repository string, binary []byte, aicrImage string) (string, *Manifest) {
	t.Helper()
	dir := t.TempDir()
	writeLayout(t, dir, binary)
	image, err := readImage(filepath.Join(dir, "image"), repository)
	if err != nil {
		t.Fatal(err)
	}
	aicr := demoAICR
	aicr.Image = aicrImage
	name, archive, err := packageChart(chartFS(), "v0.1.0-rc.1", aicr, image.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(dir, "chart"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "chart", name), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Version:  "v0.1.0-rc.1",
		Source:   Source{CommitDate: "2026-10-16T00:00:00Z"},
		Operator: image,
		Chart:    File{Name: name, Digest: sha256Of(archive)},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "candidate.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, m
}

// pushed parses push's key=value output.
func pushed(t *testing.T, out string) map[string]string {
	t.Helper()
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("push printed %q", line)
		}
		kv[k] = v
	}
	return kv
}

// push publishes the candidate unchanged: the image index under the
// candidate version and the chart archive as the one layer of a Helm OCI
// chart under the chart version. Publishing it again is a no-op, and a
// different candidate under the same version is refused.
func TestPush(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	repository := host + "/nvidia/aicr-operator"
	push := func(dir string) (string, error) {
		var out bytes.Buffer
		err := run([]string{"push", "-dir", dir, "-chart-registry", host + "/nvidia/charts", "-plain-http"}, &out)
		return out.String(), err
	}

	dir, m := writeCandidate(t, repository, []byte("operator-binary"), demoAICR.Image)
	out, err := push(dir)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	got := pushed(t, out)
	want := map[string]string{
		"image-repository": repository,
		"image-digest":     m.Operator.Digest,
		"chart-repository": host + "/nvidia/charts/demo",
		"chart-version":    "0.1.0-rc.1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	ctx := t.Context()
	images, err := remote.NewRepository(repository)
	if err != nil {
		t.Fatal(err)
	}
	images.PlainHTTP = true
	tagged, err := images.Resolve(ctx, m.Version)
	if err != nil || tagged.Digest.String() != m.Operator.Digest {
		t.Fatalf("%s:%s resolves to %v (%v), want %s", repository, m.Version, tagged.Digest, err, m.Operator.Digest)
	}
	charts, err := remote.NewRepository(got["chart-repository"])
	if err != nil {
		t.Fatal(err)
	}
	charts.PlainHTTP = true
	desc, data, err := oras.FetchBytes(ctx, charts, "0.1.0-rc.1", oras.DefaultFetchBytesOptions)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Digest.String() != got["chart-digest"] {
		t.Fatalf("the chart tag resolves to %s, push printed %s", desc.Digest, got["chart-digest"])
	}
	var manifest ocispec.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Config.MediaType != helmConfigMediaType || len(manifest.Layers) != 1 ||
		manifest.Layers[0].MediaType != helmChartMediaType || manifest.Layers[0].Digest.String() != m.Chart.Digest {

		t.Fatalf("the chart manifest is %+v; want a Helm chart whose one layer is %s", manifest, m.Chart.Digest)
	}
	config, err := content.FetchAll(ctx, charts, manifest.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `"name":"demo"`) || !strings.Contains(string(config), `"version":"0.1.0-rc.1"`) {
		t.Fatalf("the chart config is %s", config)
	}

	if out, err = push(dir); err != nil {
		t.Fatalf("pushing the same candidate again: %v", err)
	}
	if again := pushed(t, out); again["chart-digest"] != got["chart-digest"] {
		t.Fatalf("the same candidate packed to chart %s, then %s", got["chart-digest"], again["chart-digest"])
	}

	otherImage, _ := writeCandidate(t, repository, []byte("other-binary"), demoAICR.Image)
	if _, err = push(otherImage); err == nil || !strings.Contains(err.Error(), "a version names one candidate") {
		t.Fatalf("a different image under the same version: %v", err)
	}
	otherChart, _ := writeCandidate(t, repository, []byte("operator-binary"), "ghcr.io/nvidia/aicr:v1.1.1@sha256:dd")
	if _, err = push(otherChart); err == nil || !strings.Contains(err.Error(), "a version names one candidate") {
		t.Fatalf("a different chart under the same version: %v", err)
	}
	if err = run([]string{"push", "-dir", t.TempDir()}, io.Discard); err == nil {
		t.Fatal("pushed without a candidate")
	}
}
