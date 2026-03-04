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

package recipe

import (
	"context"
	"slices"
	"testing"
)

func TestDeploymentOrderGuards(t *testing.T) {
	tests := []struct {
		name             string
		criteria         func() *Criteria
		requiredDeps     map[string][]string
		requiredOrdering [][2]string
	}{
		{
			name: "h100-eks-inference",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceEKS
				c.Accelerator = CriteriaAcceleratorH100
				c.Intent = CriteriaIntentInference
				return c
			},
			requiredDeps: map[string][]string{
				"gpu-operator": {"cert-manager", "kube-prometheus-stack", "skyhook-customizations"},
			},
			requiredOrdering: [][2]string{
				{"skyhook-customizations", "gpu-operator"},
				{"gpu-operator", "nvsentinel"},
			},
		},
		{
			name: "h100-eks-training",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceEKS
				c.Accelerator = CriteriaAcceleratorH100
				c.Intent = CriteriaIntentTraining
				return c
			},
			requiredDeps: map[string][]string{
				"gpu-operator": {"cert-manager", "kube-prometheus-stack", "skyhook-customizations"},
			},
			requiredOrdering: [][2]string{
				{"skyhook-customizations", "gpu-operator"},
				{"gpu-operator", "nvsentinel"},
			},
		},
		{
			name: "h100-eks-ubuntu-inference-dynamo",
			criteria: func() *Criteria {
				c := NewCriteria()
				c.Service = CriteriaServiceEKS
				c.Accelerator = CriteriaAcceleratorH100
				c.Intent = CriteriaIntentInference
				c.OS = CriteriaOSUbuntu
				c.Platform = CriteriaPlatformDynamo
				return c
			},
			requiredDeps: map[string][]string{
				"dynamo-platform": {"dynamo-crds", "cert-manager", "kube-prometheus-stack", "kai-scheduler"},
			},
			requiredOrdering: [][2]string{
				{"kai-scheduler", "dynamo-platform"},
				{"gpu-operator", "nvsentinel"},
			},
		},
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
			requiredDeps: map[string][]string{
				"dynamo-platform": {"dynamo-crds", "cert-manager", "kube-prometheus-stack", "kai-scheduler"},
			},
			requiredOrdering: [][2]string{
				{"kai-scheduler", "dynamo-platform"},
				{"gpu-operator", "nvsentinel"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewBuilder()
			result, err := builder.BuildFromCriteria(context.Background(), tt.criteria())
			if err != nil {
				t.Fatalf("BuildFromCriteria failed: %v", err)
			}

			orderIndex := make(map[string]int, len(result.DeploymentOrder))
			for i, name := range result.DeploymentOrder {
				orderIndex[name] = i
			}

			for compName, deps := range tt.requiredDeps {
				comp := result.GetComponentRef(compName)
				if comp == nil {
					t.Fatalf("required component %q not found", compName)
				}
				for _, dep := range deps {
					if !slices.Contains(comp.DependencyRefs, dep) {
						t.Errorf("component %q missing dependency %q (got %v)", compName, dep, comp.DependencyRefs)
					}
				}
			}

			for _, pair := range tt.requiredOrdering {
				before, after := pair[0], pair[1]
				beforeIdx, ok := orderIndex[before]
				if !ok {
					t.Fatalf("component %q not found in deploymentOrder (%v)", before, result.DeploymentOrder)
				}
				afterIdx, ok := orderIndex[after]
				if !ok {
					t.Fatalf("component %q not found in deploymentOrder (%v)", after, result.DeploymentOrder)
				}
				if beforeIdx >= afterIdx {
					t.Errorf("deployment order regression: %q (idx=%d) must be before %q (idx=%d); order=%v",
						before, beforeIdx, after, afterIdx, result.DeploymentOrder)
				}
			}
		})
	}
}
