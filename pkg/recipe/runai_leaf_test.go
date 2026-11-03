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

package recipe

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

const runaiClusterComponent = "runai-cluster"

// TestRunaiLeavesDisableAICRKaiScheduler pins the one invariant that makes a
// Run:ai leaf installable: the runai-cluster chart bundles its own KAI
// Scheduler, which ships the same CRDs and PriorityClasses as AICR's
// kai-scheduler, so the leaf must carry kai-scheduler disabled rather than
// absent (absent would break every dependencyRef on it) and must carry
// runai-cluster from the registry's NGC coordinates. A base-chain change that
// re-enables kai-scheduler, or a mixin refactor that drops the override, fails
// here instead of at helm install time with a CRD ownership conflict.
func TestRunaiLeavesDisableAICRKaiScheduler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		leaf     string
		criteria func() *Criteria
	}{
		{
			name: "eks ubuntu",
			leaf: "h100-eks-ubuntu-training-runai",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceEKS
				c.Accelerator = CriteriaAcceleratorH100
				c.OS = CriteriaOSUbuntu
				c.Intent = CriteriaIntentTraining
				c.Platform = CriteriaPlatformRunai
				return c
			},
		},
		{
			name: "gke cos",
			leaf: "h100-gke-cos-training-runai",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceGKE
				c.Accelerator = CriteriaAcceleratorH100
				c.OS = CriteriaOSCOS
				c.Intent = CriteriaIntentTraining
				c.Platform = CriteriaPlatformRunai
				return c
			},
		},
	}

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	runaiCfg := registry.Get(runaiClusterComponent)
	if runaiCfg == nil {
		t.Fatalf("registry has no %q component", runaiClusterComponent)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteria(context.Background(), tt.criteria())
			if err != nil {
				t.Fatalf("BuildFromCriteria: %v", err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, tt.leaf) {
				t.Fatalf("criteria resolved to %v, which does not include %s", result.Metadata.AppliedOverlays, tt.leaf)
			}

			kai := result.GetComponentRef("kai-scheduler")
			if kai == nil {
				t.Fatal("kai-scheduler is absent; it must be present and disabled so dependencyRefs on it still resolve")
			}
			if kai.IsEnabled() {
				t.Error("kai-scheduler is enabled; runai-cluster bundles its own KAI Scheduler and the two cannot coexist")
			}
			if slices.Contains(result.DeploymentOrder, "kai-scheduler") {
				t.Errorf("kai-scheduler is in deploymentOrder %v although disabled", result.DeploymentOrder)
			}

			runai := result.GetComponentRef(runaiClusterComponent)
			if runai == nil {
				t.Fatalf("%s is absent from the resolved recipe", runaiClusterComponent)
			}
			if !runai.IsEnabled() {
				t.Errorf("%s is disabled", runaiClusterComponent)
			}
			if runai.Source != runaiCfg.Helm.DefaultRepository {
				t.Errorf("%s source = %q, want the registry's %q", runaiClusterComponent, runai.Source, runaiCfg.Helm.DefaultRepository)
			}
			if runai.Version != runaiCfg.Helm.DefaultVersion {
				t.Errorf("%s version = %q, want the registry's %q", runaiClusterComponent, runai.Version, runaiCfg.Helm.DefaultVersion)
			}
			if runai.Namespace != "runai" {
				t.Errorf("%s namespace = %q, want %q (the chart hardcodes it)", runaiClusterComponent, runai.Namespace, "runai")
			}
			if runai.HealthCheckAsserts == "" {
				t.Errorf("%s resolved without its health check inlined", runaiClusterComponent)
			}
		})
	}
}

// TestRunaiPlatformNeedsAMatchingLeaf guards the other side of the coverage
// contract in RELEASE.md: platform=runai is a recognized enum value, and a
// coordinate it has no leaf for must still fail resolution naming the gap
// rather than silently fall back to the platform-neutral training recipe.
func TestRunaiPlatformNeedsAMatchingLeaf(t *testing.T) {
	t.Parallel()

	c := NewCriteria()
	c.Service = CriteriaServiceAKS
	c.Accelerator = CriteriaAcceleratorH100
	c.OS = CriteriaOSUbuntu
	c.Intent = CriteriaIntentTraining
	c.Platform = CriteriaPlatformRunai

	result, err := NewBuilder().BuildFromCriteria(context.Background(), c)
	if err == nil {
		t.Fatalf("aks/h100/ubuntu/training/runai resolved to %v; no Run:ai leaf exists for AKS", result.Metadata.AppliedOverlays)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if want := "no recipe provides platform 'runai'"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}
