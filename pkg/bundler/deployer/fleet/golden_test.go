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

package fleet

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

const nodePrepDaemonSet = "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: node-prep\nspec: {}\n"

// TestGenerate_Scenarios is the golden-file suite for Generator.Generate:
// each row supplies a Generator configuration, the testdata/<name>/ dir
// holding the expected output, and the files to byte-compare. Error paths
// and mode-specific behavior live in the focused tests in fleet_test.go.
func TestGenerate_Scenarios(t *testing.T) {
	scenarios := []struct {
		// name is the t.Run subtest label and the testdata/<name>/ dir.
		name string
		// gen is the configured Generator to invoke.
		gen *Generator
		// goldens lists paths under testdata/<name>/ to byte-compare.
		goldens []string
	}{
		{
			// Two upstream charts, one HTTPS and one OCI: the OCI chart is a
			// full reference in helm.repo, the second bundle dependsOn the
			// first, and the GitRepo lists both paths in install order.
			name: "gitrepo_upstream_helm",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
					ref("k8s-aibom", "k8s-aibom-system", "k8s-aibom", "1.5.1", "oci://ghcr.io/googlecloudplatform/charts"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}},
					"k8s-aibom":    {"replicaCount": 1},
				},
				Version: testBundlerVersion,
				RepoURL: "https://example.com/fleet.git",
			},
			goldens: []string{
				fileGitRepo,
				fileReadme,
				filepath.Join("001-cert-manager", fileFleetYAML),
				filepath.Join("001-cert-manager", fileFleetIgnore),
				filepath.Join("002-k8s-aibom", fileFleetYAML),
			},
		},
		{
			// A manifest-only component is wrapped as a local chart, so its
			// bundle names the folder itself as the chart. The generated
			// Chart.yaml is pinned too: it is the only artifact carrying
			// testBundlerVersion.
			name: "gitrepo_manifest_only",
			gen: &Generator{
				RecipeResult: recipeWith(recipe.ComponentRef{
					Name:      "node-prep",
					Namespace: "kube-system",
					Type:      recipe.ComponentTypeHelm,
				}),
				ComponentValues: map[string]map[string]any{"node-prep": {}},
				ComponentPostManifests: map[string]map[string][]byte{
					"node-prep": {"daemonset.yaml": []byte(nodePrepDaemonSet)},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{
				fileGitRepo,
				filepath.Join("001-node-prep", fileFleetYAML),
				filepath.Join("001-node-prep", "Chart.yaml"),
			},
		},
		{
			// Upstream chart plus recipe-side manifests: the -post local
			// chart is its own bundle and dependsOn the primary.
			name: "gitrepo_mixed_post",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
				),
				ComponentValues: map[string]map[string]any{
					"gpu-operator": {"driver": map[string]any{"enabled": true}},
				},
				ComponentPostManifests: map[string]map[string][]byte{
					"gpu-operator": {
						"nvidia-runtime-class.yaml": []byte("apiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata:\n  name: nvidia\nhandler: nvidia\n"),
					},
				},
				Version: testBundlerVersion,
			},
			goldens: []string{
				fileGitRepo,
				filepath.Join("001-gpu-operator", fileFleetYAML),
				filepath.Join("002-gpu-operator-post", fileFleetYAML),
			},
		},
		{
			// Dynamic paths move into cluster-values.yaml, which fleet.yaml
			// must list after values.yaml so it wins. The README switches to
			// its per-cluster customization section.
			name: "gitrepo_dynamic_values",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}, "replicaCount": 3},
				},
				DynamicValues: map[string][]string{"cert-manager": {"replicaCount"}},
				Version:       testBundlerVersion,
			},
			goldens: []string{
				fileReadme,
				filepath.Join("001-cert-manager", fileFleetYAML),
				filepath.Join("001-cert-manager", fileValues),
				filepath.Join("001-cert-manager", fileClusterValues),
			},
		},
		{
			// A vendored chart is a local chart whose templates read .Values.
			// helm.chart "." is what makes Fleet parse valuesFiles for it;
			// without it every value is dropped from the Bundle.
			name: "gitrepo_vendored_chart",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}, "replicaCount": 3},
				},
				DynamicValues: map[string][]string{"cert-manager": {"replicaCount"}},
				Version:       testBundlerVersion,
				VendorCharts:  true,
				Puller:        &stubChartPuller{},
			},
			goldens: []string{
				fileReadme,
				filepath.Join("001-cert-manager", fileFleetYAML),
				filepath.Join("001-cert-manager", fileFleetIgnore),
			},
		},
		{
			// HelmOp mode writes a single helmops.yaml with values inlined
			// and no fleet.yaml, .fleetignore or gitrepo.yaml.
			name: "helmop_upstream_helm",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
					ref("k8s-aibom", "k8s-aibom-system", "k8s-aibom", "1.5.1", "oci://ghcr.io/googlecloudplatform/charts"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}},
					"k8s-aibom":    {"replicaCount": 1},
				},
				Version: testBundlerVersion,
				Mode:    ModeHelmOp,
			},
			goldens: []string{fileHelmOps, fileReadme},
		},
		{
			// Dynamic values are merged into spec.helm.values, with
			// cluster-values.yaml winning over values.yaml.
			name: "helmop_dynamic_values",
			gen: &Generator{
				RecipeResult: recipeWith(
					ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
				),
				ComponentValues: map[string]map[string]any{
					"cert-manager": {"crds": map[string]any{"enabled": true}, "replicaCount": 3},
				},
				DynamicValues: map[string][]string{"cert-manager": {"replicaCount"}},
				Version:       testBundlerVersion,
				Mode:          ModeHelmOp,
			},
			goldens: []string{fileHelmOps},
		},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			outputDir := t.TempDir()
			out, err := tc.gen.Generate(context.Background(), outputDir)
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if out == nil || len(out.Files) == 0 {
				t.Fatal("Generate() returned no files")
			}
			for _, rel := range tc.goldens {
				assertGolden(t, outputDir, filepath.Join("testdata", tc.name), rel)
			}
		})
	}
}

func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err = os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err = os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", relPath, got, want)
	}
}
