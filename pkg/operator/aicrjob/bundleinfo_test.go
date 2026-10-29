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

package aicrjob

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const bundleFixture = "bundle-h100-eks-ubuntu-training-kubeflow/"

// The Helm layout records no phase; it is inferred from release names.
func TestParseBundleInfoFixture(t *testing.T) {
	t.Parallel()
	for _, version := range fixtureVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			parseBundleInfoFixture(t, version)
		})
	}
}

func parseBundleInfoFixture(t *testing.T, version string) {
	t.Helper()
	fixture := func(t *testing.T, name string) []byte { t.Helper(); return fixtureAt(t, version, name) }
	b, err := ParseBundleInfo(fixture(t, bundleFixture+BundleInfoFile), version)
	if err != nil {
		t.Fatalf("ParseBundleInfo() = %v", err)
	}
	phases := map[string]plan.Phase{}
	for _, r := range b.Layout.Releases {
		phases[r.Name] = r.Phase
	}
	want := map[string]plan.Phase{
		"gpu-operator":           plan.PhasePrimary,
		"gpu-operator-readiness": plan.PhaseReadiness,
		"kubeflow-trainer":       plan.PhasePrimary,
		"kubeflow-trainer-post":  plan.PhasePost,
		"cert-manager":           plan.PhasePrimary,
	}
	for name, phase := range want {
		if phases[name] != phase {
			t.Errorf("release %s phase = %q, want %q", name, phases[name], phase)
		}
	}
	if len(b.Layout.Releases) != 17 {
		t.Errorf("releases = %d, want 17", len(b.Layout.Releases))
	}

	r, err := b.ParseBundleRecipe(fixture(t, bundleFixture+"recipe.yaml"), version)
	if err != nil {
		t.Fatalf("ParseBundleRecipe() = %v", err)
	}
	// AICR drops dra-node-labeler at bundle time when no DRA eviction label
	// is set; the bundle's recipe reflects that, the recipe Job's does not.
	full, err := ParseRecipeResult(fixture(t, "recipe-h100-eks-ubuntu-training-kubeflow.json"), version)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(full.DeploymentOrder, "dra-node-labeler") || slices.Contains(r.DeploymentOrder, "dra-node-labeler") {
		t.Errorf("bundle recipe %v, resolved recipe %v", r.DeploymentOrder, full.DeploymentOrder)
	}
	if _, err = b.ParseBundleRecipe(fixture(t, "recipe-h100-eks-ubuntu-training-kubeflow.json"), version); err == nil {
		t.Error("ParseBundleRecipe() accepted a recipe that does not match build.recipe.digest")
	}
}

// editBundleInfo returns the fixture with f applied to its decoded form.
func editBundleInfo(t *testing.T, f func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(fixture(t, bundleFixture+BundleInfoFile), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func section(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

func releases(m map[string]any) []any { return section(m, "layout")["releases"].([]any) }

func release(m map[string]any, i int) map[string]any { return releases(m)[i].(map[string]any) }

func TestParseBundleInfoRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(map[string]any)
		raw  string
		code operrors.ErrorCode
	}{
		{name: "other kind", edit: func(m map[string]any) { m["kind"] = "Bundle" }},
		{name: "other apiVersion", edit: func(m map[string]any) { m["apiVersion"] = "aicr.run/v2" }},
		{name: "missing layout", edit: func(m map[string]any) { delete(m, "layout") }},
		{name: "version mismatch", edit: func(m map[string]any) { section(m, "metadata")["version"] = "1.0.1" }},
		{name: "missing version", edit: func(m map[string]any) { delete(section(m, "metadata"), "version") }},
		{name: "argocd layout", edit: func(m map[string]any) { section(m, "build")["deployer"] = "argocd" }},
		{name: "no readiness gates", edit: func(m map[string]any) { section(m, "build", "settings")["readinessHooks"] = false }},
		{name: "component filter", edit: func(m map[string]any) { section(m, "build", "settings")["components"] = []any{"gpu-operator"} }},
		{name: "no recipe digest", edit: func(m map[string]any) { delete(section(m, "build", "recipe"), "digest") }},
		{name: "escaping entrypoint", edit: func(m map[string]any) { section(m, "layout")["entrypoint"] = "../deploy.sh" }},
		{name: "absolute release path", edit: func(m map[string]any) { release(m, 0)["path"] = "/etc" }},
		{name: "unnamed release", edit: func(m map[string]any) { release(m, 0)["name"] = "" }},
		{name: "repeated path", edit: func(m map[string]any) { release(m, 1)["path"] = release(m, 0)["path"] }},
		{name: "unknown phase", edit: func(m map[string]any) { release(m, 9)["name"] = "gpu-operator-gate" }},
		{name: "readiness before primary", edit: func(m map[string]any) {
			rs := releases(m)
			rs[8], rs[9] = rs[9], rs[8]
		}},
		{name: "pre after primary", edit: func(m map[string]any) { release(m, 9)["name"] = "gpu-operator-pre" }},
		{name: "second primary", edit: func(m map[string]any) {
			release(m, 1)["component"], release(m, 1)["name"] = "aws-ebs-csi-driver", "aws-ebs-csi-driver"
		}},
		{name: "orphan injected release", edit: func(m map[string]any) {
			release(m, 13)["component"], release(m, 13)["name"] = "nvsentinel-x", "nvsentinel-x-post"
		}},
		{name: "suffix collision", edit: func(m map[string]any) {
			release(m, 13)["component"], release(m, 13)["name"] = "kubeflow-trainer-post", "kubeflow-trainer-post"
		}},
		{name: "two documents", raw: "---\nkind: BundleInfo\n---\nkind: BundleInfo\n"},
		{name: "duplicate key", raw: "kind: BundleInfo\nkind: BundleInfo\n"},
		{name: "oversized", raw: "kind: " + strings.Repeat("x", maxArtifactBytes)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := []byte(tt.raw)
			if tt.edit != nil {
				data = editBundleInfo(t, tt.edit)
			}
			if _, err := ParseBundleInfo(data, "v1.0.0"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("ParseBundleInfo() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

// Added fields are ignored, matching AICR's additive rules.
func TestParseBundleInfoIgnoresAddedFields(t *testing.T) {
	t.Parallel()
	data := editBundleInfo(t, func(m map[string]any) {
		m["provenance"] = map[string]any{"future": true}
		release(m, 0)["discriminator"] = "primary"
	})
	if _, err := ParseBundleInfo(data, "v1.0.0"); err != nil {
		t.Fatalf("ParseBundleInfo() = %v", err)
	}
}

// The bundle recipe must list exactly the primary releases, in order.
func TestParseBundleRecipeChecksOrder(t *testing.T) {
	t.Parallel()
	data := editBundleInfo(t, func(m map[string]any) {
		rs := releases(m)
		rs[0], rs[1] = rs[1], rs[0]
	})
	b, err := ParseBundleInfo(data, "v1.0.0")
	if err != nil {
		t.Fatalf("ParseBundleInfo() = %v", err)
	}
	if _, err = b.ParseBundleRecipe(fixture(t, bundleFixture+"recipe.yaml"), "v1.0.0"); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Fatalf("ParseBundleRecipe() = %v, want INVALID_REQUEST", err)
	}
}
