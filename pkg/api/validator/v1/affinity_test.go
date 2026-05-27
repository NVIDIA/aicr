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
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
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
