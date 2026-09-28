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

package argocd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestGenerate_CRDsApplyEverySync pins the two properties that make Argo CD
// upgrade a chart's CRDs without consulting the registry's ownsCRDs flag.
//
// Argo CD renders a Helm source with `helm template --include-crds` unless the
// Application sets `helm.skipCrds: true`, so CRDs are ordinary manifests in
// the sync set and are re-applied on every sync. `ServerSideApply=true` is
// what lets that succeed for charts whose CRDs exceed kubectl's 262144-byte
// client-side annotation cap. Neither is conditional, and neither can be:
// skipCrds suppresses CRDs on *first install* too, so gating it on ownsCRDs
// would break a fresh install of every CRD-shipping chart.
//
// So argocd and argocd-helm need no code to honor ownsCRDs, which is why the
// properties are worth pinning here: they are load-bearing by omission, and
// nothing else would fail if a future change dropped them. Per-deployer CRD
// behavior is recorded in docs/user/component-catalog.md and #2264.
//
// Scoped to the upstream-chart source, which is the only shape with a `helm:`
// stanza to put skipCrds in. A vendored component becomes a path-based source
// at a wrapper chart, with no `helm:` block and therefore no knob to gate;
// Argo CD renders that path with `--include-crds` the same way, and Helm's
// CRDObjects() recurses into the wrapper's charts/<chart>-<version>.tgz
// dependency. Exercising the vendored path here would need a live chart pull,
// since the Generator takes no ChartPuller, and that is not worth a network
// dependency for a shape that structurally cannot carry the flag.
func TestGenerate_CRDsApplyEverySync(t *testing.T) {
	// k8s-aibom is an ownsCRDs component, so this is precisely the case the
	// issue claims is broken on this deployer.
	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{{
		Name:      "k8s-aibom",
		Namespace: "k8s-aibom-system",
		Chart:     "k8s-aibom",
		Version:   "1.3.0",
		Type:      recipe.ComponentTypeHelm,
		Source:    "oci://ghcr.io/googlecloudplatform/charts",
	}}
	recipeResult.DeploymentOrder = []string{"k8s-aibom"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"k8s-aibom": {"replicaCount": 1}},
		Version:         "v0.0.0-test",
		RepoURL:         "https://github.com/example/aicr-bundles.git",
		TargetRevision:  "main",
	}

	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(outputDir, "001-k8s-aibom", "application.yaml"))
	if err != nil {
		t.Fatalf("read application.yaml: %v", err)
	}
	app := string(raw)

	// The Application must actually carry a helm source; otherwise the
	// skipCrds assertion below passes vacuously.
	if !strings.Contains(app, "helm:") {
		t.Fatalf("Application has no helm: stanza, so the skipCrds assertion proves nothing\n%s", app)
	}
	// Any skipCrds at all, not just `true`: an explicit `false` is equally a
	// signal that someone started gating this and should read the comment
	// above first.
	if strings.Contains(app, "skipCrds") {
		t.Errorf("Application sets skipCrds; CRDs would stop reaching the cluster "+
			"on install as well as upgrade\n%s", app)
	}
	if !strings.Contains(app, "ServerSideApply=true") {
		t.Errorf("Application dropped ServerSideApply=true; CRDs over the 262144-byte "+
			"client-side annotation cap will fail to sync forever\n%s", app)
	}
}

// TestGenerate_NoApplyCRDsScript pins that the argocd bundle does NOT carry
// the helm/helmfile apply-crds.sh. Argo CD applies CRDs itself, so the script
// would be dead weight an operator might mistake for a required manual step.
//
// It shares localformat with the helm and helmfile deployers, which do emit
// it, so nothing structural keeps it out: only the argocd deployer's choice
// not to populate Component.OwnsCRDs.
func TestGenerate_NoApplyCRDsScript(t *testing.T) {
	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{{
		Name:      "k8s-aibom",
		Namespace: "k8s-aibom-system",
		Chart:     "k8s-aibom",
		Version:   "1.3.0",
		Type:      recipe.ComponentTypeHelm,
		Source:    "oci://ghcr.io/googlecloudplatform/charts",
	}}
	recipeResult.DeploymentOrder = []string{"k8s-aibom"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"k8s-aibom": {"replicaCount": 1}},
		Version:         "v0.0.0-test",
		RepoURL:         "https://github.com/example/aicr-bundles.git",
		TargetRevision:  "main",
	}

	outputDir := t.TempDir()
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	path := filepath.Join(outputDir, "001-k8s-aibom", "apply-crds.sh")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("argocd bundle emitted apply-crds.sh (stat err: %v); Argo CD applies "+
			"CRDs every sync, so the script implies a manual step that does not exist", err)
	}
}
