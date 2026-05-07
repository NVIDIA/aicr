// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package config_test

import (
	"reflect"
	"strings"
	"testing"

	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// === RecipeSpec.ResolveCriteria ===

func TestResolveCriteria_NilReceiver(t *testing.T) {
	var r *config.RecipeSpec
	got, err := r.ResolveCriteria()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result for nil receiver")
	}
	want := &recipe.Criteria{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected zero-valued Criteria, got %+v", got)
	}
}

func TestResolveCriteria_NilCriteria(t *testing.T) {
	r := &config.RecipeSpec{}
	got, err := r.ResolveCriteria()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Service != "" || got.Nodes != 0 {
		t.Errorf("expected zero-valued, got %+v", got)
	}
}

func TestResolveCriteria_AllFieldsPopulated(t *testing.T) {
	r := &config.RecipeSpec{
		Criteria: &config.CriteriaSpec{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
			OS:          "ubuntu",
			Platform:    "kubeflow",
			Nodes:       4,
		},
	}
	got, err := r.ResolveCriteria()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Service != recipe.CriteriaServiceEKS {
		t.Errorf("Service: got %q, want eks", got.Service)
	}
	if got.Accelerator != recipe.CriteriaAcceleratorH100 {
		t.Errorf("Accelerator: got %q", got.Accelerator)
	}
	if got.Intent != recipe.CriteriaIntentTraining {
		t.Errorf("Intent: got %q", got.Intent)
	}
	if got.OS != recipe.CriteriaOSUbuntu {
		t.Errorf("OS: got %q", got.OS)
	}
	if got.Platform != recipe.CriteriaPlatformKubeflow {
		t.Errorf("Platform: got %q", got.Platform)
	}
	if got.Nodes != 4 {
		t.Errorf("Nodes: got %d, want 4", got.Nodes)
	}
}

func TestResolveCriteria_PartialFields_UnsetStayZero(t *testing.T) {
	r := &config.RecipeSpec{
		Criteria: &config.CriteriaSpec{Service: "gke"},
	}
	got, err := r.ResolveCriteria()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Service != recipe.CriteriaServiceGKE {
		t.Errorf("Service: got %q, want gke", got.Service)
	}
	if got.Accelerator != "" {
		t.Errorf("Accelerator: expected empty (unset), got %q", got.Accelerator)
	}
	if got.Intent != "" || got.OS != "" || got.Platform != "" {
		t.Errorf("expected unset enum fields to remain zero, got %+v", got)
	}
}

func TestResolveCriteria_InvalidEnums(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.CriteriaSpec)
		wantSub string
	}{
		{
			name:    "service",
			mutate:  func(c *config.CriteriaSpec) { c.Service = "bogus" },
			wantSub: "spec.recipe.criteria.service",
		},
		{
			name:    "accelerator",
			mutate:  func(c *config.CriteriaSpec) { c.Accelerator = "h99999" },
			wantSub: "spec.recipe.criteria.accelerator",
		},
		{
			name:    "intent",
			mutate:  func(c *config.CriteriaSpec) { c.Intent = "mining" },
			wantSub: "spec.recipe.criteria.intent",
		},
		{
			name:    "os",
			mutate:  func(c *config.CriteriaSpec) { c.OS = "windows" },
			wantSub: "spec.recipe.criteria.os",
		},
		{
			name:    "platform",
			mutate:  func(c *config.CriteriaSpec) { c.Platform = "spark" },
			wantSub: "spec.recipe.criteria.platform",
		},
		{
			name:    "negative nodes",
			mutate:  func(c *config.CriteriaSpec) { c.Nodes = -1 },
			wantSub: "spec.recipe.criteria.nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &config.RecipeSpec{Criteria: &config.CriteriaSpec{}}
			tt.mutate(r.Criteria)
			_, err := r.ResolveCriteria()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q must contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// === BundleSpec.Resolve ===

func TestBundleResolve_NilReceiver(t *testing.T) {
	var b *config.BundleSpec
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil result for nil receiver")
	}
	want := &config.BundleResolved{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected zero BundleResolved, got %+v", got)
	}
}

func TestBundleResolve_EmptySpec(t *testing.T) {
	b := &config.BundleSpec{}
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil")
	}
	if got.Deployer != "" || got.RecipeInput != "" {
		t.Errorf("expected zero, got %+v", got)
	}
}

func TestBundleResolve_AllFieldsPopulated(t *testing.T) {
	b := &config.BundleSpec{
		Input:  &config.BundleInputSpec{Recipe: "recipe.yaml"},
		Output: &config.BundleOutputSpec{Target: "./out", ImageRefs: "refs.txt"},
		Deployment: &config.DeploymentSpec{
			Deployer: "argocd",
			Repo:     "https://repo.example",
			Set:      []string{"gpuoperator:driver.version=570.0.0"},
			Dynamic:  []string{"gpuoperator:storage.class"},
		},
		Scheduling: &config.SchedulingSpec{
			SystemNodeSelector:         map[string]string{"sys": "true"},
			SystemNodeTolerations:      []string{"sys-only=yes:NoSchedule"},
			AcceleratedNodeSelector:    map[string]string{"gpu": "true"},
			AcceleratedNodeTolerations: []string{"gpu-only=yes:NoSchedule"},
			WorkloadGate:               "k=v:NoSchedule",
			WorkloadSelector:           map[string]string{"workload": "true"},
			Nodes:                      8,
			StorageClass:               "fast-ssd",
		},
		Attestation: &config.AttestationSpec{
			Enabled:                   true,
			CertificateIdentityRegexp: ".+",
			OIDCDeviceFlow:            true,
		},
		Registry: &config.RegistrySpec{InsecureTLS: true, PlainHTTP: true},
	}
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RecipeInput != "recipe.yaml" {
		t.Errorf("RecipeInput: got %q", got.RecipeInput)
	}
	if got.OutputTargetRaw != "./out" {
		t.Errorf("OutputTargetRaw: got %q", got.OutputTargetRaw)
	}
	if got.OutputTarget == nil {
		t.Errorf("OutputTarget should be non-nil for non-empty target")
	}
	if got.ImageRefs != "refs.txt" {
		t.Errorf("ImageRefs: got %q", got.ImageRefs)
	}
	if got.Deployer != bundlercfg.DeployerArgoCD {
		t.Errorf("Deployer: got %q", got.Deployer)
	}
	if got.Repo != "https://repo.example" {
		t.Errorf("Repo: got %q", got.Repo)
	}
	if len(got.ValueOverrides) != 1 {
		t.Errorf("ValueOverrides count: got %d, want 1", len(got.ValueOverrides))
	}
	if len(got.DynamicValues) != 1 {
		t.Errorf("DynamicValues count: got %d, want 1", len(got.DynamicValues))
	}
	if got.SystemNodeSelector["sys"] != "true" {
		t.Errorf("SystemNodeSelector: got %v", got.SystemNodeSelector)
	}
	if len(got.SystemNodeTolerations) != 1 {
		t.Errorf("SystemNodeTolerations count: got %d, want 1", len(got.SystemNodeTolerations))
	}
	if len(got.AcceleratedNodeTolerations) != 1 {
		t.Errorf("AcceleratedNodeTolerations count: got %d", len(got.AcceleratedNodeTolerations))
	}
	if got.WorkloadGate == nil || got.WorkloadGate.Key != "k" {
		t.Errorf("WorkloadGate: got %+v", got.WorkloadGate)
	}
	if got.WorkloadSelector["workload"] != "true" {
		t.Errorf("WorkloadSelector: got %v", got.WorkloadSelector)
	}
	if got.Nodes != 8 {
		t.Errorf("Nodes: got %d", got.Nodes)
	}
	if got.StorageClass != "fast-ssd" {
		t.Errorf("StorageClass: got %q", got.StorageClass)
	}
	if !got.Attest || got.CertIDRegexp != ".+" || !got.OIDCDeviceFlow {
		t.Errorf("Attestation fields: got attest=%v cert=%q oidc=%v",
			got.Attest, got.CertIDRegexp, got.OIDCDeviceFlow)
	}
	if !got.InsecureTLS || !got.PlainHTTP {
		t.Errorf("Registry: got insecure=%v plain=%v", got.InsecureTLS, got.PlainHTTP)
	}
}

func TestBundleResolve_NilVsExplicitlyEmptyMaps(t *testing.T) {
	tests := []struct {
		name           string
		scheduling     *config.SchedulingSpec
		wantSysSel     map[string]string
		wantSysSelNil  bool
		wantSysSelLen0 bool
	}{
		{
			name:          "nil scheduling: selector nil",
			scheduling:    nil,
			wantSysSelNil: true,
		},
		{
			name:          "scheduling present, selector nil: stays nil",
			scheduling:    &config.SchedulingSpec{},
			wantSysSelNil: true,
		},
		{
			name:           "scheduling present, selector explicitly empty: non-nil empty",
			scheduling:     &config.SchedulingSpec{SystemNodeSelector: map[string]string{}},
			wantSysSelLen0: true,
		},
		{
			name:       "scheduling present, selector populated",
			scheduling: &config.SchedulingSpec{SystemNodeSelector: map[string]string{"k": "v"}},
			wantSysSel: map[string]string{"k": "v"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &config.BundleSpec{Scheduling: tt.scheduling}
			got, err := b.Resolve()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantSysSelNil {
				if got.SystemNodeSelector != nil {
					t.Errorf("expected nil, got %v", got.SystemNodeSelector)
				}
				return
			}
			if tt.wantSysSelLen0 {
				if got.SystemNodeSelector == nil {
					t.Errorf("expected non-nil empty map")
				}
				if len(got.SystemNodeSelector) != 0 {
					t.Errorf("expected empty, got %v", got.SystemNodeSelector)
				}
				return
			}
			if got.SystemNodeSelector["k"] != "v" {
				t.Errorf("got %v, want %v", got.SystemNodeSelector, tt.wantSysSel)
			}
		})
	}
}

func TestBundleResolve_NilVsExplicitlyEmptySlices(t *testing.T) {
	tests := []struct {
		name           string
		deployment     *config.DeploymentSpec
		wantOverNil    bool
		wantOverLen0   bool
		wantDynamicNil bool
	}{
		{
			name:           "nil deployment: both nil",
			deployment:     nil,
			wantOverNil:    true,
			wantDynamicNil: true,
		},
		{
			name:           "deployment present, set nil: nil",
			deployment:     &config.DeploymentSpec{},
			wantOverNil:    true,
			wantDynamicNil: true,
		},
		{
			name:         "deployment present, set explicitly empty: non-nil empty",
			deployment:   &config.DeploymentSpec{Set: []string{}, Dynamic: []string{}},
			wantOverLen0: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &config.BundleSpec{Deployment: tt.deployment}
			got, err := b.Resolve()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantOverNil {
				if got.ValueOverrides != nil {
					t.Errorf("ValueOverrides: expected nil, got %v", got.ValueOverrides)
				}
			} else if tt.wantOverLen0 {
				if got.ValueOverrides == nil {
					t.Errorf("ValueOverrides: expected non-nil empty, got nil")
				}
				if len(got.ValueOverrides) != 0 {
					t.Errorf("ValueOverrides: expected len 0, got %d", len(got.ValueOverrides))
				}
				if got.DynamicValues == nil {
					t.Errorf("DynamicValues: expected non-nil empty, got nil")
				}
			}
			if tt.wantDynamicNil && got.DynamicValues != nil {
				t.Errorf("DynamicValues: expected nil, got %v", got.DynamicValues)
			}
		})
	}
}

func TestBundleResolve_NilVsExplicitlyEmptyTolerations(t *testing.T) {
	// nil tolerations: resolved field stays nil so CLI can apply DefaultTolerations.
	b1 := &config.BundleSpec{Scheduling: &config.SchedulingSpec{}}
	r1, err := b1.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r1.SystemNodeTolerations != nil {
		t.Errorf("nil input → expected nil output, got %v", r1.SystemNodeTolerations)
	}

	// Explicitly empty tolerations: parser conflates nil/empty and returns
	// DefaultTolerations — Resolve preserves that parser semantics.
	b2 := &config.BundleSpec{Scheduling: &config.SchedulingSpec{SystemNodeTolerations: []string{}}}
	r2, err := b2.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r2.SystemNodeTolerations) == 0 {
		t.Errorf("explicit empty input → expected DefaultTolerations, got empty")
	}
}

func TestBundleResolve_InvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		spec    *config.BundleSpec
		wantSub string
	}{
		{
			name: "invalid deployer",
			spec: &config.BundleSpec{
				Deployment: &config.DeploymentSpec{Deployer: "fluxcd"},
			},
			wantSub: "spec.bundle.deployment.deployer",
		},
		{
			name: "invalid set value override",
			spec: &config.BundleSpec{
				Deployment: &config.DeploymentSpec{Set: []string{"no-equals-sign"}},
			},
			wantSub: "spec.bundle.deployment.set",
		},
		{
			name: "invalid dynamic declaration",
			spec: &config.BundleSpec{
				Deployment: &config.DeploymentSpec{Dynamic: []string{"with=equals"}},
			},
			wantSub: "spec.bundle.deployment.dynamic",
		},
		{
			name: "invalid system tolerations",
			spec: &config.BundleSpec{
				Scheduling: &config.SchedulingSpec{
					SystemNodeTolerations: []string{"malformed"},
				},
			},
			wantSub: "spec.bundle.scheduling.systemNodeTolerations",
		},
		{
			name: "invalid accelerated tolerations",
			spec: &config.BundleSpec{
				Scheduling: &config.SchedulingSpec{
					AcceleratedNodeTolerations: []string{"bad:taint:format"},
				},
			},
			wantSub: "spec.bundle.scheduling.acceleratedNodeTolerations",
		},
		{
			name: "invalid workload gate",
			spec: &config.BundleSpec{
				Scheduling: &config.SchedulingSpec{WorkloadGate: "no-effect"},
			},
			wantSub: "spec.bundle.scheduling.workloadGate",
		},
		{
			name: "invalid output target",
			spec: &config.BundleSpec{
				Output: &config.BundleOutputSpec{Target: "oci://"},
			},
			wantSub: "spec.bundle.output.target",
		},
		{
			name: "negative nodes",
			spec: &config.BundleSpec{
				Scheduling: &config.SchedulingSpec{Nodes: -3},
			},
			wantSub: "spec.bundle.scheduling.nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.spec.Resolve()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q must contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestBundleResolve_OutputTargetEmptyStringStaysNil(t *testing.T) {
	b := &config.BundleSpec{Output: &config.BundleOutputSpec{}}
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.OutputTarget != nil {
		t.Errorf("expected nil OutputTarget for empty raw, got %+v", got.OutputTarget)
	}
	if got.OutputTargetRaw != "" {
		t.Errorf("expected empty OutputTargetRaw, got %q", got.OutputTargetRaw)
	}
}

func TestBundleResolve_DeployerEmptyStaysZero(t *testing.T) {
	// Empty config-deployer means "config did not set" — Resolve does NOT
	// fabricate a default. The CLI is responsible for defaulting to Helm.
	b := &config.BundleSpec{Deployment: &config.DeploymentSpec{}}
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Deployer != "" {
		t.Errorf("expected zero deployer, got %q", got.Deployer)
	}
}

func TestBundleResolve_WorkloadGateEmptyStaysNil(t *testing.T) {
	b := &config.BundleSpec{Scheduling: &config.SchedulingSpec{WorkloadGate: ""}}
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.WorkloadGate != nil {
		t.Errorf("expected nil WorkloadGate, got %+v", got.WorkloadGate)
	}
}

func TestBundleResolve_DefensiveCloneOfMaps(t *testing.T) {
	// Ensures Resolve clones map fields so callers cannot mutate the
	// loaded config's backing map by mutating the resolved value.
	src := map[string]string{"k": "v"}
	b := &config.BundleSpec{Scheduling: &config.SchedulingSpec{SystemNodeSelector: src}}
	got, err := b.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got.SystemNodeSelector["k"] = "mutated"
	if src["k"] != "v" {
		t.Errorf("Resolve must defensively clone maps; source was mutated to %q", src["k"])
	}
}
