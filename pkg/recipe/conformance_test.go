// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// conformance_test.go verifies that conformance-critical recipes contain
// all CNCF-required components and conformance checks. These tests catch
// regressions at PR time (no cluster needed) — e.g., a component removal
// or a conformance check accidentally dropped from an overlay.
//
// Area of Concern: Recipe-level conformance guarantees
// - Required components present in resolved recipe
// - Conformance validation checks declared in overlay chain
// - DRA constraint (K8s >= 1.34) present for all conformance recipes

package recipe

import (
	"context"
	"strings"
	"testing"
)

func TestConformanceRecipeInvariants(t *testing.T) {
	tests := []struct {
		name               string
		criteria           func() *Criteria
		requiredComponents []string
		requiredChecks     []string
	}{
		{
			name: "h100-kind-inference-dynamo",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceKind
				c.Accelerator = CriteriaAcceleratorH100
				c.Intent = CriteriaIntentInference
				c.Platform = CriteriaPlatformDynamo
				return c
			},
			requiredComponents: []string{
				"cert-manager",
				"gpu-operator",
				"kube-prometheus-stack",
				"prometheus-adapter",
				"nvidia-dra-driver-gpu",
				"kai-scheduler",
				"kgateway-crds",
				"kgateway",
				"dynamo-crds",
				"dynamo-platform",
			},
			requiredChecks: []string{
				"platform-health",
				"gpu-operator-health",
				"dra-support",
				"accelerator-metrics",
				"ai-service-metrics",
				"inference-gateway",
				"robust-controller",
				"secure-accelerator-access",
				"pod-autoscaling",
				"cluster-autoscaling",
			},
		},
		{
			name: "h100-kind-training",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceKind
				c.Accelerator = CriteriaAcceleratorH100
				c.Intent = CriteriaIntentTraining
				return c
			},
			requiredComponents: []string{
				"cert-manager",
				"gpu-operator",
				"kube-prometheus-stack",
				"prometheus-adapter",
				"nvidia-dra-driver-gpu",
				"kai-scheduler",
				"dynamo-crds",
				"dynamo-platform",
			},
			requiredChecks: []string{
				"platform-health",
				"gpu-operator-health",
				"dra-support",
				"accelerator-metrics",
				"ai-service-metrics",
				"gang-scheduling",
				"robust-controller",
				"pod-autoscaling",
				"cluster-autoscaling",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			builder := NewBuilder()
			criteria := tt.criteria()

			result, err := builder.BuildFromCriteria(ctx, criteria)
			if err != nil {
				t.Fatalf("BuildFromCriteria failed: %v", err)
			}
			if result == nil {
				t.Fatal("Recipe result is nil")
			}

			// 1. All required components present
			for _, name := range tt.requiredComponents {
				if comp := result.GetComponentRef(name); comp == nil {
					t.Errorf("Required component %q not found in resolved recipe", name)
				}
			}

			// 2. Conformance validation configured
			if result.Validation == nil {
				t.Fatal("result.Validation is nil")
			}
			if result.Validation.Conformance == nil {
				t.Fatal("result.Validation.Conformance is nil")
			}

			// 3. All required conformance checks present
			checkSet := make(map[string]bool)
			for _, c := range result.Validation.Conformance.Checks {
				checkSet[c] = true
			}
			for _, check := range tt.requiredChecks {
				if !checkSet[check] {
					t.Errorf("Required conformance check %q not found (have: %v)",
						check, result.Validation.Conformance.Checks)
				}
			}

			// 4. No fewer checks than expected (guards against accidental removal)
			if len(result.Validation.Conformance.Checks) < len(tt.requiredChecks) {
				t.Errorf("Conformance checks count = %d, want >= %d",
					len(result.Validation.Conformance.Checks), len(tt.requiredChecks))
			}

			// 5. DRA constraint present (K8s >= 1.34 required for DRA GA)
			var hasDRAConstraint bool
			for _, c := range result.Constraints {
				if c.Name == "K8s.server.version" && strings.Contains(c.Value, "1.34") {
					hasDRAConstraint = true
					break
				}
			}
			if !hasDRAConstraint {
				t.Error("Missing K8s >= 1.34 constraint (required for DRA GA)")
			}

			t.Logf("Recipe %s: %d components, %d conformance checks",
				tt.name, len(result.ComponentRefs), len(result.Validation.Conformance.Checks))
		})
	}
}
