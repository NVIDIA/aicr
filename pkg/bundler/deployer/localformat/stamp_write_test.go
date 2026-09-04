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

package localformat_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/defaults"
)

// chartMetadata mirrors the subset of Helm's chart.Metadata the generated
// wrappers populate. Helm's Go SDK is not a dependency, so tests assert
// against a local shape.
type chartMetadata struct {
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	AppVersion  string            `yaml:"appVersion"`
	Annotations map[string]string `yaml:"annotations"`
}

// parseChartMetadata unmarshals a generated Chart.yaml and asserts the two
// invariants Helm enforces at load time: the document parses, and `version:`
// is SemVer. Helm's chart.Metadata.Validate rejects the chart outright
// otherwise, so a generated wrapper failing either check is undeployable.
func parseChartMetadata(t *testing.T, content []byte) chartMetadata {
	t.Helper()
	var md chartMetadata
	if err := yaml.Unmarshal(content, &md); err != nil {
		t.Fatalf("generated Chart.yaml does not parse: %v\n--- got:\n%s", err, content)
	}
	if _, err := semver.NewVersion(md.Version); err != nil {
		t.Fatalf("Chart.yaml version %q is not SemVer, so Helm refuses to load the chart: %v\n--- got:\n%s",
			md.Version, err, content)
	}
	return md
}

// readChartMetadata is parseChartMetadata against a Chart.yaml on disk.
func readChartMetadata(t *testing.T, path string) chartMetadata {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	md := parseChartMetadata(t, content)
	if md.Name == "" {
		t.Fatalf("%s has no chart name:\n%s", path, content)
	}
	return md
}

// stampScenario is the component set exercised by both tests below: it emits
// one folder of every generated-wrapper shape Write produces.
func stampScenario(outDir, aicrVersion string) localformat.Options {
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	return localformat.Options{
		OutputDir:   outDir,
		AICRVersion: aicrVersion,
		Components: []localformat.Component{
			{
				// Manifest-only: no upstream pin at all.
				Name:      "skyhook-customizations",
				Namespace: "skyhook",
			},
			{
				// Mixed + vendored: emits a vendored primary, an injected
				// -pre and -post wrapper, and a readiness gate.
				Name:       "gpu-operator",
				Namespace:  "gpu-operator",
				Repository: "https://nvidia.github.io/gpu-operator",
				ChartName:  "gpu-operator",
				Version:    "v25.3.0",
			},
		},
		ComponentPreManifests:  map[string]map[string][]byte{"gpu-operator": {"ns.yaml": manifest}},
		ComponentPostManifests: map[string]map[string][]byte{"skyhook-customizations": {"cm.yaml": manifest}, "gpu-operator": {"cm.yaml": manifest}},
		ComponentReadiness:     map[string]map[string][]byte{"gpu-operator": {"gate.yaml": manifest}},
		VendorCharts:           true,
		Puller:                 &fakePuller{},
	}
}

// TestWrite_StampsGeneratedWrappers pins the ADR-021 Decision 7 contract at
// the writer boundary: every generated wrapper reports the AICR version in
// `version:` and its own payload version in aicr.run/component-version, so a
// reader can tell what an installed release actually carries. Before this,
// all of them reported a fictional 0.1.0 for both.
func TestWrite_StampsGeneratedWrappers(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), stampScenario(outDir, "v1.4.0"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Payload version by folder. The injected -pre / -post / -readiness
	// wrappers accompany their parent component, so they inherit its pin;
	// the manifest-only component has none and reports AICR's version.
	wantPayload := map[string]string{
		"001-skyhook-customizations": "1.4.0",
		"002-gpu-operator-pre":       "v25.3.0",
		"003-gpu-operator":           "v25.3.0",
		"004-gpu-operator-post":      "v25.3.0",
		"005-gpu-operator-readiness": "v25.3.0",
	}
	if len(res.Folders) != len(wantPayload) {
		t.Fatalf("got %d folders, want %d: %+v", len(res.Folders), len(wantPayload), res.Folders)
	}

	for _, f := range res.Folders {
		want, ok := wantPayload[f.Dir]
		if !ok {
			t.Errorf("unexpected folder %q", f.Dir)
			continue
		}
		md := readChartMetadata(t, filepath.Join(outDir, f.Dir, "Chart.yaml"))
		if md.Version != "1.4.0" {
			t.Errorf("%s: version = %q, want the AICR version 1.4.0", f.Dir, md.Version)
		}
		if md.Annotations[localformat.AnnotationGeneratedBy] != "1.4.0" {
			t.Errorf("%s: %s = %q, want 1.4.0",
				f.Dir, localformat.AnnotationGeneratedBy, md.Annotations[localformat.AnnotationGeneratedBy])
		}
		if got := md.Annotations[localformat.AnnotationComponentVersion]; got != want {
			t.Errorf("%s: %s = %q, want %q",
				f.Dir, localformat.AnnotationComponentVersion, got, want)
		}
		if md.AppVersion != want {
			t.Errorf("%s: appVersion = %q, want %q", f.Dir, md.AppVersion, want)
		}
	}
}

// TestWrite_DevBuildProducesHelmValidCharts is the regression guard the ADR
// calls out explicitly: an unstamped build reports defaults.DevVersion
// ("dev"), which is not SemVer 2. Stamping it verbatim into `version:` would
// make Helm reject every generated chart, breaking `make dev-env` and Tilt.
// The payload version must survive the fold untouched.
func TestWrite_DevBuildProducesHelmValidCharts(t *testing.T) {
	outDir := t.TempDir()

	if _, err := localformat.Write(context.Background(), stampScenario(outDir, defaults.DevVersion)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Walk rather than iterate res.Folders: a Chart.yaml Helm cannot load is
	// fatal wherever it lands, including in a folder shape added later.
	seen := 0
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "Chart.yaml" {
			return nil
		}
		seen++
		md := readChartMetadata(t, path) // fails the test if not SemVer
		if md.Version != defaults.DevChartVersion {
			t.Errorf("%s: version = %q, want %q", path, md.Version, defaults.DevChartVersion)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", outDir, err)
	}
	if seen == 0 {
		t.Fatal("no Chart.yaml found; the scenario emitted no generated wrappers")
	}

	// The dev fold applies to the chart version only — the payload pin is
	// free-form and must be reported as the recipe declares it.
	md := readChartMetadata(t, filepath.Join(outDir, "003-gpu-operator", "Chart.yaml"))
	if md.AppVersion != "v25.3.0" {
		t.Errorf("appVersion = %q, want the payload version v25.3.0 unmodified", md.AppVersion)
	}
}

// TestAnnotationKeysMatchTemplates keeps the exported constants and the
// literal keys in the Chart.yaml templates from drifting. The constants derive
// from header.Domain (ADR-013); the templates spell the keys out, because a
// templated key reads worse than the value it labels. A domain migration that
// updates only the constants must fail here rather than ship annotations no
// consumer can find.
func TestAnnotationKeysMatchTemplates(t *testing.T) {
	outDir := t.TempDir()
	if _, err := localformat.Write(context.Background(), stampScenario(outDir, "v1.4.0")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 001 is the plain local-helm chart.yaml.tmpl; 003 is the vendored
	// wrapper-chart.yaml.tmpl. Both templates must be covered.
	for _, dir := range []string{"001-skyhook-customizations", "003-gpu-operator"} {
		path := filepath.Join(outDir, dir, "Chart.yaml")
		content, err := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, key := range []string{
			localformat.AnnotationComponentVersion,
			localformat.AnnotationGeneratedBy,
		} {
			if !strings.Contains(string(content), key+":") {
				t.Errorf("%s does not spell annotation key %q; template and constant have drifted\n%s",
					path, key, content)
			}
		}
	}
}

func TestRenderWrapperChartYAML(t *testing.T) {
	got, err := localformat.RenderWrapperChartYAML(localformat.WrapperChartInput{
		Name:           "gpu-operator",
		Parent:         "gpu-operator",
		ChartName:      "gpu-operator",
		ChartVersion:   "v25.3.0",
		AICRVersion:    "v1.4.0",
		PayloadVersion: "v25.3.0",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"name: gpu-operator",
		"- name: gpu-operator",
		"version: v25.3.0",
		`repository: ""`,
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("Chart.yaml missing %q\n--- got:\n%s", want, got)
		}
	}

	md := parseChartMetadata(t, got)
	if md.Version != "1.4.0" {
		t.Errorf("version = %q, want the AICR version 1.4.0", md.Version)
	}
	if md.AppVersion != "v25.3.0" {
		t.Errorf("appVersion = %q, want the payload version v25.3.0", md.AppVersion)
	}
	if md.Annotations[localformat.AnnotationComponentVersion] != "v25.3.0" {
		t.Errorf("%s = %q, want v25.3.0",
			localformat.AnnotationComponentVersion, md.Annotations[localformat.AnnotationComponentVersion])
	}
	if md.Annotations[localformat.AnnotationGeneratedBy] != "1.4.0" {
		t.Errorf("%s = %q, want 1.4.0",
			localformat.AnnotationGeneratedBy, md.Annotations[localformat.AnnotationGeneratedBy])
	}
}

// TestRenderWrapperChartYAML_DevBuild covers the case that breaks
// `make dev-env` and Tilt if the normalization regresses: an unstamped build
// reports defaults.DevVersion ("dev"), which Helm rejects for `version:`.
// The payload version must survive untouched — only the chart version is
// constrained to SemVer.
func TestRenderWrapperChartYAML_DevBuild(t *testing.T) {
	got, err := localformat.RenderWrapperChartYAML(localformat.WrapperChartInput{
		Name:           "gpu-operator",
		Parent:         "gpu-operator",
		ChartName:      "gpu-operator",
		ChartVersion:   "25.3.0",
		AICRVersion:    defaults.DevVersion,
		PayloadVersion: "v25.3.0",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	md := parseChartMetadata(t, got)
	if md.Version != defaults.DevChartVersion {
		t.Errorf("version = %q, want %q", md.Version, defaults.DevChartVersion)
	}
	if md.AppVersion != "v25.3.0" {
		t.Errorf("appVersion = %q, want the payload version v25.3.0 unmodified", md.AppVersion)
	}
}

// TestRenderWrapperChartYAML_EmptyPayloadVersion pins the fallback the
// exported entry point owns: a caller with no upstream pin still gets a
// populated aicr.run/component-version, so consumers never have to
// distinguish "absent" from "empty".
func TestRenderWrapperChartYAML_EmptyPayloadVersion(t *testing.T) {
	got, err := localformat.RenderWrapperChartYAML(localformat.WrapperChartInput{
		Name:         "local-thing",
		Parent:       "local-thing",
		ChartName:    "local-thing",
		ChartVersion: "0.1.0",
		AICRVersion:  "v1.4.0",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	md := parseChartMetadata(t, got)
	if md.Annotations[localformat.AnnotationComponentVersion] != "1.4.0" {
		t.Errorf("%s = %q, want the AICR version 1.4.0",
			localformat.AnnotationComponentVersion, md.Annotations[localformat.AnnotationComponentVersion])
	}
	if md.AppVersion != "1.4.0" {
		t.Errorf("appVersion = %q, want the AICR version 1.4.0", md.AppVersion)
	}
}

// TestRenderWrapperChartYAML_QuotesHostileVersion proves the `q` template
// function is load-bearing: a version string carrying a quote character must
// not break out of its YAML scalar and corrupt the document. Recipe pins are
// trusted today, but a generated Chart.yaml that stops parsing is a failure
// mode worth closing at the template.
func TestRenderWrapperChartYAML_QuotesHostileVersion(t *testing.T) {
	hostile := `1.0"` + "\nname: hijacked"
	got, err := localformat.RenderWrapperChartYAML(localformat.WrapperChartInput{
		Name:           "gpu-operator",
		Parent:         "gpu-operator",
		ChartName:      "gpu-operator",
		ChartVersion:   "25.3.0",
		AICRVersion:    "v1.4.0",
		PayloadVersion: hostile,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	md := parseChartMetadata(t, got)
	if md.Name != "gpu-operator" {
		t.Fatalf("name = %q, want gpu-operator — the payload version escaped its scalar", md.Name)
	}
	// The hostile value must survive as one scalar, not merely fail to
	// inject: `q` has to round-trip it, or the annotation stops matching
	// the recipe pin it is supposed to mirror.
	if md.AppVersion != hostile {
		t.Errorf("appVersion = %q, want the input round-tripped verbatim %q", md.AppVersion, hostile)
	}
	if md.Annotations[localformat.AnnotationComponentVersion] != hostile {
		t.Errorf("%s = %q, want %q", localformat.AnnotationComponentVersion,
			md.Annotations[localformat.AnnotationComponentVersion], hostile)
	}
}
