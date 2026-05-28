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

package v1

import (
	stderrors "errors"
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildOrchestratorAffinity_NoDeps(t *testing.T) {
	got, err := BuildOrchestratorAffinity(nil, nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == nil || got.NodeAffinity == nil {
		t.Fatal("expected non-nil affinity with prefer-CPU NodeAffinity")
	}
	if got.PodAffinity != nil {
		t.Errorf("expected nil PodAffinity when no deps, got %+v", got.PodAffinity)
	}
}

func TestBuildOrchestratorAffinity_RequiredResolved(t *testing.T) {
	deps := []DependencyAffinity{{
		ComponentRef:     "kube-prometheus-stack",
		PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
		Requirement:      DependencyRequirementRequired,
	}}
	refs := []recipe.ComponentRef{{Name: "kube-prometheus-stack", Namespace: "monitoring"}}

	got, err := BuildOrchestratorAffinity(deps, refs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.PodAffinity == nil {
		t.Fatal("expected PodAffinity to be set")
	}
	required := got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if len(required) != 1 {
		t.Fatalf("expected 1 required term, got %d", len(required))
	}
	term := required[0]
	if len(term.Namespaces) != 1 || term.Namespaces[0] != "monitoring" {
		t.Errorf("expected term.Namespaces = [monitoring], got %v", term.Namespaces)
	}
	if term.TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("expected hostname topology key, got %q", term.TopologyKey)
	}
	if got.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("expected no preferred terms when only required deps, got %+v",
			got.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution)
	}
}

func TestBuildOrchestratorAffinity_PreferredResolved(t *testing.T) {
	deps := []DependencyAffinity{{
		ComponentRef:     "kube-prometheus-stack",
		PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
		Requirement:      DependencyRequirementPreferred,
	}}
	refs := []recipe.ComponentRef{{Name: "kube-prometheus-stack", Namespace: "monitoring"}}

	got, err := BuildOrchestratorAffinity(deps, refs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	preferred := got.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(preferred) != 1 {
		t.Fatalf("expected 1 preferred term, got %d", len(preferred))
	}
	if preferred[0].Weight != preferredAffinityWeight {
		t.Errorf("expected weight %d, got %d", preferredAffinityWeight, preferred[0].Weight)
	}
	if len(preferred[0].PodAffinityTerm.Namespaces) != 1 ||
		preferred[0].PodAffinityTerm.Namespaces[0] != "monitoring" {

		t.Errorf("expected term namespace [monitoring], got %v",
			preferred[0].PodAffinityTerm.Namespaces)
	}
	if got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Errorf("expected no required terms when only preferred deps")
	}
}

func TestBuildOrchestratorAffinity_RequiredMissingComponent(t *testing.T) {
	deps := []DependencyAffinity{{
		ComponentRef:     "kube-prometheus-stack",
		PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
		Requirement:      DependencyRequirementRequired,
	}}
	_, err := BuildOrchestratorAffinity(deps, nil)
	if err == nil {
		t.Fatal("expected error for missing required component")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("expected ErrCodeInvalidRequest, got %v", err)
	}
}

func TestBuildOrchestratorAffinity_PreferredMissingComponent_Skipped(t *testing.T) {
	deps := []DependencyAffinity{{
		ComponentRef:     "kube-prometheus-stack",
		PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
		Requirement:      DependencyRequirementPreferred,
	}}
	got, err := BuildOrchestratorAffinity(deps, nil)
	if err != nil {
		t.Fatalf("expected no error for missing preferred dep, got %v", err)
	}
	if got.PodAffinity != nil {
		t.Errorf("expected no PodAffinity when preferred dep is unresolved, got %+v", got.PodAffinity)
	}
	if got.NodeAffinity == nil {
		t.Fatal("NodeAffinity (prefer-CPU) must still be present")
	}
}

func TestBuildOrchestratorAffinity_MultipleDeps(t *testing.T) {
	deps := []DependencyAffinity{
		{
			ComponentRef:     "kube-prometheus-stack",
			PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
			Requirement:      DependencyRequirementRequired,
		},
		{
			ComponentRef:     "dcgm-exporter",
			PodLabelSelector: map[string]string{"app": "dcgm-exporter"},
			Requirement:      DependencyRequirementPreferred,
		},
	}
	refs := []recipe.ComponentRef{
		{Name: "kube-prometheus-stack", Namespace: "monitoring"},
		{Name: "dcgm-exporter", Namespace: "gpu-operator"},
	}
	got, err := BuildOrchestratorAffinity(deps, refs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("expected 1 required term, got %d",
			len(got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution))
	}
	if len(got.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Errorf("expected 1 preferred term, got %d",
			len(got.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution))
	}
}

func TestBuildOrchestratorAffinity_NamespaceEmptyTreatedAsMissing(t *testing.T) {
	deps := []DependencyAffinity{{
		ComponentRef:     "kube-prometheus-stack",
		PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
		Requirement:      DependencyRequirementRequired,
	}}
	// Component is present but unresolved (empty namespace).
	refs := []recipe.ComponentRef{{Name: "kube-prometheus-stack"}}

	_, err := BuildOrchestratorAffinity(deps, refs)
	if err == nil {
		t.Fatal("expected error when required dep's component has empty namespace")
	}
}

func TestBuildOrchestratorAffinity_ExplicitTopologyKeyPreserved(t *testing.T) {
	deps := []DependencyAffinity{{
		ComponentRef:     "kube-prometheus-stack",
		PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
		Requirement:      DependencyRequirementRequired,
		TopologyKey:      "topology.kubernetes.io/zone",
	}}
	refs := []recipe.ComponentRef{{Name: "kube-prometheus-stack", Namespace: "monitoring"}}

	got, err := BuildOrchestratorAffinity(deps, refs)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	term := got.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	if term.TopologyKey != "topology.kubernetes.io/zone" {
		t.Errorf("expected explicit topology key preserved, got %q", term.TopologyKey)
	}
}

// TestAffinityTypeFieldCountInvariant guards the hand-rolled
// affinityToApplyConfig walker in job_plan.go against silent field drops on
// k8s API bumps. If a k8s vendor bump adds a field to one of these structs
// (e.g., MatchLabelKeys / MismatchLabelKeys were added to PodAffinityTerm in
// v1.29-v1.30), this test fails and forces an audit of the apply-config
// converter so the new field is either propagated or intentionally excluded.
//
// Update the expected count only after confirming the converter handles (or
// has deliberately skipped) every new field.
func TestAffinityTypeFieldCountInvariant(t *testing.T) {
	cases := []struct {
		name string
		t    reflect.Type
		want int
	}{
		{"Affinity", reflect.TypeOf(corev1.Affinity{}), 3},
		{"NodeAffinity", reflect.TypeOf(corev1.NodeAffinity{}), 2},
		{"NodeSelectorTerm", reflect.TypeOf(corev1.NodeSelectorTerm{}), 2},
		{"PodAffinity", reflect.TypeOf(corev1.PodAffinity{}), 2},
		{"PodAntiAffinity", reflect.TypeOf(corev1.PodAntiAffinity{}), 2},
		{"PodAffinityTerm", reflect.TypeOf(corev1.PodAffinityTerm{}), 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.t.NumField(); got != c.want {
				t.Fatalf("corev1.%s has %d fields, expected %d. K8s API has added/removed a field; audit the apply-config converter in pkg/api/validator/v1/job_plan.go and update this test only after confirming the new field is handled or intentionally excluded.",
					c.name, got, c.want)
			}
		})
	}
}

// TestValidateDependencyAffinity_MatchesBuildSemantics ensures the
// pre-flight validator returns the same error class as the full build path
// for every resolution failure mode, since both share resolveDeps.
func TestValidateDependencyAffinity_MatchesBuildSemantics(t *testing.T) {
	cases := []struct {
		name    string
		deps    []DependencyAffinity
		refs    []recipe.ComponentRef
		wantErr bool
	}{
		{
			name:    "nil deps returns nil",
			wantErr: false,
		},
		{
			name: "valid required resolves",
			deps: []DependencyAffinity{{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      DependencyRequirementRequired,
			}},
			refs:    []recipe.ComponentRef{{Name: "kube-prometheus-stack", Namespace: "monitoring"}},
			wantErr: false,
		},
		{
			name: "required missing returns error",
			deps: []DependencyAffinity{{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      DependencyRequirementRequired,
			}},
			wantErr: true,
		},
		{
			name: "preferred missing returns nil",
			deps: []DependencyAffinity{{
				ComponentRef:     "kube-prometheus-stack",
				PodLabelSelector: map[string]string{"app.kubernetes.io/name": "prometheus"},
				Requirement:      DependencyRequirementPreferred,
			}},
			wantErr: false,
		},
		{
			name: "invalid dep (empty selector) returns error",
			deps: []DependencyAffinity{{
				ComponentRef: "kube-prometheus-stack",
				Requirement:  DependencyRequirementPreferred,
			}},
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			validateErr := ValidateDependencyAffinity(c.deps, c.refs)
			_, buildErr := BuildOrchestratorAffinity(c.deps, c.refs)
			if (validateErr != nil) != c.wantErr {
				t.Errorf("ValidateDependencyAffinity err=%v, wantErr=%v", validateErr, c.wantErr)
			}
			// Validate and Build must agree on error/no-error.
			if (validateErr == nil) != (buildErr == nil) {
				t.Errorf("validate/build disagree: validateErr=%v, buildErr=%v", validateErr, buildErr)
			}
			// When both error, they must share the same StructuredError.Code
			// so the pre-flight gate and the build path classify failures
			// consistently — a future refactor that downgrades one path's
			// code (e.g., to ErrCodeInternal) must not slip through.
			if validateErr != nil && buildErr != nil {
				var vErr, bErr *errors.StructuredError
				switch {
				case !stderrors.As(validateErr, &vErr):
					t.Errorf("validate err is not a *StructuredError: %T (%v)", validateErr, validateErr)
				case !stderrors.As(buildErr, &bErr):
					t.Errorf("build err is not a *StructuredError: %T (%v)", buildErr, buildErr)
				case vErr.Code != bErr.Code:
					t.Errorf("error code mismatch: validate=%s build=%s", vErr.Code, bErr.Code)
				}
			}
		})
	}
}
