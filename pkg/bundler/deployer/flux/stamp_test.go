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

package flux

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// chartMetadata mirrors the subset of Helm's chart.Metadata the generated
// charts populate. Helm's Go SDK is not a dependency, so tests assert against
// a local shape.
type chartMetadata struct {
	Name        string            `yaml:"name"`
	Version     string            `yaml:"version"`
	AppVersion  string            `yaml:"appVersion"`
	Annotations map[string]string `yaml:"annotations"`
}

// readChartMetadata parses a generated Chart.yaml and asserts the invariant
// Helm enforces at load time: `version:` must be SemVer, or helm-controller
// cannot reconcile the HelmRelease at all.
func readChartMetadata(t *testing.T, path string) chartMetadata {
	t.Helper()
	content, err := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var md chartMetadata
	if err := yaml.Unmarshal(content, &md); err != nil {
		t.Fatalf("%s does not parse: %v\n%s", path, err, content)
	}
	if _, err := semver.NewVersion(md.Version); err != nil {
		t.Fatalf("%s version %q is not SemVer, so Helm refuses to load the chart: %v",
			path, md.Version, err)
	}
	return md
}

// stampRecipe pairs a manifest-only component (no upstream pin) with a mixed
// Helm component (pinned, and carrying recipe-side manifests that become a
// -post chart), so one Generate covers every generated chart flux emits.
func stampRecipe() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.run/v1alpha2",
		ComponentRefs: []recipe.ComponentRef{
			{Name: "custom-manifests", Namespace: "default", Type: recipe.ComponentTypeHelm},
			{
				Name:      "gpu-operator",
				Namespace: "gpu-operator",
				Type:      recipe.ComponentTypeHelm,
				Chart:     "gpu-operator",
				Source:    "https://nvidia.github.io/gpu-operator",
				Version:   "v25.3.0",
			},
		},
		DeploymentOrder: []string{"custom-manifests", "gpu-operator"},
	}
}

func stampManifests() map[string]map[string][]byte {
	cm := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n")
	return map[string]map[string][]byte{
		"custom-manifests": {"configmap.yaml": cm},
		"gpu-operator":     {"tuning.yaml": cm},
	}
}

// TestGenerate_StampsGeneratedCharts pins ADR-021 Decision 7 for the flux
// deployer, whose generated charts previously reported a hardcoded 0.1.0 with
// no payload version anywhere. `version:` tracks the AICR build; the payload
// pin lands in appVersion and aicr.run/component-version, falling back to the
// AICR version for a component with no upstream pin.
func TestGenerate_StampsGeneratedCharts(t *testing.T) {
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult:       stampRecipe(),
		ComponentManifests: stampManifests(),
		Version:            "v1.4.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	assertStamped(t, outputDir, []stampCase{
		{"custom-manifests", "1.4.0"}, // no upstream pin: AICR's version is the payload
		{"gpu-operator-post", "v25.3.0"},
	})
}

// TestGenerate_StampsVendoredWrapper covers flux's second generated-chart
// path: the vendored wrapper, which flux builds itself via
// localformat.RenderWrapperChartYAML rather than through localformat.Write.
func TestGenerate_StampsVendoredWrapper(t *testing.T) {
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult:       stampRecipe(),
		ComponentManifests: stampManifests(),
		Version:            "v1.4.0",
		RepoURL:            "https://github.com/my-org/gitops.git",
		VendorCharts:       true,
		Puller:             &stubChartPuller{},
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertStamped(t, outputDir, []stampCase{{"gpu-operator", "v25.3.0"}})
}

// stampCase is one generated chart directory and the payload version its
// appVersion / aicr.run/component-version must report.
type stampCase struct {
	dir         string
	wantPayload string
}

// assertStamped checks each case against the AICR version "1.4.0" the tests
// above configure the Generator with.
func assertStamped(t *testing.T, outputDir string, tests []stampCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			md := readChartMetadata(t, filepath.Join(outputDir, tt.dir, "Chart.yaml"))
			if md.Version != "1.4.0" {
				t.Errorf("version = %q, want the AICR version 1.4.0", md.Version)
			}
			if md.Annotations[localformat.AnnotationGeneratedBy] != "1.4.0" {
				t.Errorf("%s = %q, want 1.4.0", localformat.AnnotationGeneratedBy,
					md.Annotations[localformat.AnnotationGeneratedBy])
			}
			if got := md.Annotations[localformat.AnnotationComponentVersion]; got != tt.wantPayload {
				t.Errorf("%s = %q, want %q", localformat.AnnotationComponentVersion, got, tt.wantPayload)
			}
			if md.AppVersion != tt.wantPayload {
				t.Errorf("appVersion = %q, want %q", md.AppVersion, tt.wantPayload)
			}
		})
	}
}

// TestGenerate_DevBuildProducesHelmValidCharts guards the case that breaks
// `make dev-env`: an unstamped build reports defaults.DevVersion ("dev"),
// which Helm rejects for Chart.yaml `version:`.
func TestGenerate_DevBuildProducesHelmValidCharts(t *testing.T) {
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult:       stampRecipe(),
		ComponentManifests: stampManifests(),
		Version:            defaults.DevVersion,
		RepoURL:            "https://github.com/my-org/gitops.git",
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, dir := range []string{"custom-manifests", "gpu-operator-post"} {
		md := readChartMetadata(t, filepath.Join(outputDir, dir, "Chart.yaml"))
		if md.Version != defaults.DevChartVersion {
			t.Errorf("%s: version = %q, want %q", dir, md.Version, defaults.DevChartVersion)
		}
	}

	// The fold applies to the chart version only; the payload pin is
	// free-form and reported as the recipe declares it.
	md := readChartMetadata(t, filepath.Join(outputDir, "gpu-operator-post", "Chart.yaml"))
	if md.AppVersion != "v25.3.0" {
		t.Errorf("appVersion = %q, want the payload version v25.3.0 unmodified", md.AppVersion)
	}
}
