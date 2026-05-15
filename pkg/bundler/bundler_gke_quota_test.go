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

package bundler

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"gopkg.in/yaml.v3"
)

// TestComputeGKECriticalPriorityQuotaPods pins the pods cap arithmetic:
// the floor (32) applies when the recipe declared no node count or a
// non-positive count; otherwise nodeCount × 32.
func TestComputeGKECriticalPriorityQuotaPods(t *testing.T) {
	tests := []struct {
		name      string
		nodeCount int
		want      int
	}{
		{"unset (zero) → floor", 0, 32},
		{"negative → floor", -1, 32},
		{"one node → 32 (floor matches)", 1, 32},
		{"small cluster", 8, 256},
		{"medium cluster", 100, 3200},
		{"large cluster", 2000, 64000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeGKECriticalPriorityQuotaPods(tt.nodeCount)
			if got != tt.want {
				t.Errorf("computeGKECriticalPriorityQuotaPods(%d) = %d, want %d",
					tt.nodeCount, got, tt.want)
			}
		})
	}
}

// TestRenderGKECriticalPriorityQuota verifies the YAML shape so callers
// can rely on a stable manifest (helmfile / argocd / flux re-apply uses
// name + namespace as the identity key).
func TestRenderGKECriticalPriorityQuota(t *testing.T) {
	bytes, err := renderGKECriticalPriorityQuota("gpu-operator", 1024)
	if err != nil {
		t.Fatalf("renderGKECriticalPriorityQuota: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(bytes, &doc); err != nil {
		t.Fatalf("rendered manifest is not valid YAML: %v", err)
	}
	if doc["apiVersion"] != "v1" {
		t.Errorf("apiVersion = %v, want v1", doc["apiVersion"])
	}
	if doc["kind"] != "ResourceQuota" {
		t.Errorf("kind = %v, want ResourceQuota", doc["kind"])
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta["name"] != gkeCriticalPriorityQuotaName {
		t.Errorf("metadata.name = %v, want %q", meta["name"], gkeCriticalPriorityQuotaName)
	}
	if meta["namespace"] != "gpu-operator" {
		t.Errorf("metadata.namespace = %v, want gpu-operator", meta["namespace"])
	}
	spec, _ := doc["spec"].(map[string]any)
	hard, _ := spec["hard"].(map[string]any)
	// pods is emitted as a string ("1024") so yaml.Unmarshal returns a
	// string; matches the upstream ResourceQuota quantity convention.
	if hard["pods"] != "1024" {
		t.Errorf("spec.hard.pods = %v, want \"1024\"", hard["pods"])
	}
	scopes, _ := spec["scopeSelector"].(map[string]any)
	matches, _ := scopes["matchExpressions"].([]any)
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 matchExpression, got %d", len(matches))
	}
	expr, _ := matches[0].(map[string]any)
	if expr["scopeName"] != "PriorityClass" || expr["operator"] != "In" {
		t.Errorf("matchExpression operator/scopeName = %v/%v, want In/PriorityClass",
			expr["operator"], expr["scopeName"])
	}
	values, _ := expr["values"].([]any)
	if len(values) != 2 || values[0] != "system-node-critical" || values[1] != "system-cluster-critical" {
		t.Errorf("scope values = %v, want [system-node-critical system-cluster-critical]", values)
	}
}

// TestRenderGKECriticalPriorityQuota_Deterministic guards the
// serializer.MarshalYAMLDeterministic call: the bundle is checksummed
// and optionally attested, so two renders of the same inputs must
// produce byte-identical output. yaml.v3 walks randomized Go map
// order, which would silently regress this if the call ever flipped
// back to the stdlib yaml.Marshal.
func TestRenderGKECriticalPriorityQuota_Deterministic(t *testing.T) {
	// 50 iterations is generous; map-order non-determinism typically
	// surfaces within ~5 with a multi-key spec.
	first, err := renderGKECriticalPriorityQuota("gpu-operator", 320)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	for i := range 50 {
		got, err := renderGKECriticalPriorityQuota("gpu-operator", 320)
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Fatalf("non-deterministic render at iter %d:\nfirst:\n%s\ngot:\n%s",
				i, string(first), string(got))
		}
	}
}

// TestInjectGKECriticalPriorityQuotas covers the integration of the
// synthesizer with collectComponentPreManifests. Each case asserts both
// presence/absence and (where applicable) the namespace + pods cap that
// landed in the synthesized manifest.
func TestInjectGKECriticalPriorityQuotas(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const markedComponent = "gpu-operator" // registry has gkeCriticalPriority: true

	tests := []struct {
		name             string
		service          recipe.CriteriaServiceType
		nodes            int
		refs             []recipe.ComponentRef
		wantSynthesized  bool   // expect the synthesized manifest in the map
		wantPodsContains string // substring to look for in the rendered YAML
	}{
		{
			name:    "gke + marked component + node count → synthesized with computed cap",
			service: recipe.CriteriaServiceGKE,
			nodes:   10,
			refs: []recipe.ComponentRef{
				{Name: markedComponent, Namespace: "gpu-operator"},
			},
			wantSynthesized:  true,
			wantPodsContains: "pods: \"320\"",
		},
		{
			name:    "gke + marked component + zero nodes → floor (32)",
			service: recipe.CriteriaServiceGKE,
			nodes:   0,
			refs: []recipe.ComponentRef{
				{Name: markedComponent, Namespace: "gpu-operator"},
			},
			wantSynthesized:  true,
			wantPodsContains: "pods: \"32\"",
		},
		{
			name:    "non-gke (eks) + marked component → no synthesis",
			service: recipe.CriteriaServiceEKS,
			nodes:   100,
			refs: []recipe.ComponentRef{
				{Name: markedComponent, Namespace: "gpu-operator"},
			},
			wantSynthesized: false,
		},
		{
			name:    "gke + unmarked component → no synthesis",
			service: recipe.CriteriaServiceGKE,
			nodes:   10,
			refs: []recipe.ComponentRef{
				{Name: "cert-manager", Namespace: "cert-manager"},
			},
			wantSynthesized: false,
		},
		{
			name:    "gke + marked component but empty namespace → skipped (defensive)",
			service: recipe.CriteriaServiceGKE,
			nodes:   10,
			refs: []recipe.ComponentRef{
				{Name: markedComponent, Namespace: ""},
			},
			wantSynthesized: false,
		},
		{
			name:    "gke + mixed: marked + unmarked → only marked is synthesized",
			service: recipe.CriteriaServiceGKE,
			nodes:   5,
			refs: []recipe.ComponentRef{
				{Name: markedComponent, Namespace: "gpu-operator"},
				{Name: "cert-manager", Namespace: "cert-manager"},
			},
			wantSynthesized:  true,
			wantPodsContains: "pods: \"160\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := &recipe.RecipeResult{
				Criteria:      &recipe.Criteria{Service: tt.service, Nodes: tt.nodes},
				ComponentRefs: tt.refs,
			}
			pre, err := bundler.collectComponentPreManifests(context.Background(), rr)
			if err != nil {
				t.Fatalf("collectComponentPreManifests: %v", err)
			}

			got, ok := pre[markedComponent][gkeCriticalPriorityQuotaFilename]
			if !tt.wantSynthesized {
				if ok {
					t.Errorf("expected no synthesized quota, got:\n%s", string(got))
				}
				return
			}
			if !ok {
				t.Fatalf("expected synthesized quota at %q, got map=%v",
					gkeCriticalPriorityQuotaFilename, pre)
			}
			if !strings.Contains(string(got), tt.wantPodsContains) {
				t.Errorf("rendered quota missing %q:\n%s", tt.wantPodsContains, string(got))
			}
			if !strings.Contains(string(got), "namespace: gpu-operator") {
				t.Errorf("rendered quota missing namespace line:\n%s", string(got))
			}
		})
	}
}

// TestInjectGKECriticalPriorityQuotas_CoexistsWithExistingPreManifests
// pins the additive behavior: the synthesized manifest sits alongside
// any manifests already declared via PreManifestFiles on the same
// component, not replacing them.
func TestInjectGKECriticalPriorityQuotas_CoexistsWithExistingPreManifests(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Pre-populate the per-component pre-manifest map as
	// collectComponentManifestsByPhase would, then run the injector
	// directly so we exercise the merge logic without needing a real
	// PreManifestFiles path on disk.
	pre := map[string]map[string][]byte{
		"gpu-operator": {
			"existing/pre/manifest.yaml": []byte("kind: ConfigMap\n"),
		},
	}
	rr := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{
			Service: recipe.CriteriaServiceGKE,
			Nodes:   4,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator"},
		},
	}
	out, err := bundler.injectGKECriticalPriorityQuotas(pre, rr)
	if err != nil {
		t.Fatalf("injectGKECriticalPriorityQuotas: %v", err)
	}

	gpu := out["gpu-operator"]
	if _, ok := gpu["existing/pre/manifest.yaml"]; !ok {
		t.Error("pre-existing manifest entry was dropped")
	}
	if _, ok := gpu[gkeCriticalPriorityQuotaFilename]; !ok {
		t.Error("synthesized manifest not added")
	}
	if len(gpu) != 2 {
		t.Errorf("expected exactly 2 entries (existing + synthesized), got %d: %v",
			len(gpu), gpu)
	}
}

// TestInjectGKECriticalPriorityQuotas_NilInputs documents the
// nil-tolerant contract: a nil RecipeResult or nil Criteria returns
// the input unchanged (no panic, no synthesis).
func TestInjectGKECriticalPriorityQuotas_NilInputs(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tests := []struct {
		name string
		in   *recipe.RecipeResult
	}{
		{"nil recipe result", nil},
		{"nil criteria", &recipe.RecipeResult{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := bundler.injectGKECriticalPriorityQuotas(nil, tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(out) != 0 {
				t.Errorf("expected empty map, got %v", out)
			}
		})
	}
}
