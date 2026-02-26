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

package performance

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidateNcclAllReduceBw(t *testing.T) {
	tests := []struct {
		name       string
		setup      func() *checks.ValidationContext
		constraint recipe.Constraint
		wantActual string
		wantPassed bool
		wantErr    bool
	}{
		{
			name: "skipped when recipe is nil",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
				}
			},
			constraint: recipe.Constraint{
				Name:  "nccl-all-reduce-bw",
				Value: "450 GB/s",
			},
			wantActual: "skipped - requires Service + Accelerator",
			wantPassed: true,
			wantErr:    false,
		},
		{
			name: "skipped when service is not EKS",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Recipe: &recipe.RecipeResult{
						Criteria: &recipe.Criteria{
							Service:     recipe.CriteriaServiceGKE,
							Accelerator: recipe.CriteriaAcceleratorH100,
						},
					},
				}
			},
			constraint: recipe.Constraint{
				Name:  "nccl-all-reduce-bw",
				Value: "450 GB/s",
			},
			wantActual: "skipped - requires Service + Accelerator to be implemented",
			wantPassed: true,
			wantErr:    false,
		},
		{
			name: "skipped when accelerator is not H100",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Recipe: &recipe.RecipeResult{
						Criteria: &recipe.Criteria{
							Service:     recipe.CriteriaServiceEKS,
							Accelerator: recipe.CriteriaAcceleratorA100,
						},
					},
				}
			},
			constraint: recipe.Constraint{
				Name:  "nccl-all-reduce-bw",
				Value: "450 GB/s",
			},
			wantActual: "skipped - requires Service + Accelerator to be implemented",
			wantPassed: true,
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup()
			actual, passed, err := validateNcclAllReduceBw(ctx, tt.constraint)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateNcclAllReduceBw() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if actual != tt.wantActual {
				t.Errorf("validateNcclAllReduceBw() actual = %v, want %v", actual, tt.wantActual)
			}

			if passed != tt.wantPassed {
				t.Errorf("validateNcclAllReduceBw() passed = %v, want %v", passed, tt.wantPassed)
			}
		})
	}
}

func TestSupportedNCCLCombinations(t *testing.T) {
	tests := []struct {
		name        string
		service     recipe.CriteriaServiceType
		accelerator recipe.CriteriaAcceleratorType
		wantFound   bool
	}{
		{
			name:        "EKS + H100 is supported",
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorH100,
			wantFound:   true,
		},
		{
			name:        "GKE + H100 is not supported",
			service:     recipe.CriteriaServiceGKE,
			accelerator: recipe.CriteriaAcceleratorH100,
			wantFound:   false,
		},
		{
			name:        "EKS + A100 is not supported",
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorA100,
			wantFound:   false,
		},
		{
			name:        "EKS + GB200 is not supported",
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorGB200,
			wantFound:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found := false
			if accelerators, ok := supportedNCCLCombinations[tt.service]; ok {
				for _, a := range accelerators {
					if a == tt.accelerator {
						found = true
						break
					}
				}
			}
			if found != tt.wantFound {
				t.Errorf("combination %s+%s: found=%v, want %v", tt.service, tt.accelerator, found, tt.wantFound)
			}
		})
	}
}

func TestDetermineGPUConfig(t *testing.T) {
	tests := []struct {
		name            string
		nodes           []v1.Node
		wantWorkerCount int
		wantGPUPerNode  int
		wantTotalGPU    int
		wantErr         bool
	}{
		{
			name: "single GPU node yields WorkerCount=1 (triggers < 2 node skip)",
			nodes: []v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
			wantWorkerCount: 1,
			wantGPUPerNode:  8,
			wantTotalGPU:    8,
			wantErr:         false,
		},
		{
			name: "two GPU nodes yields WorkerCount=2",
			nodes: []v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-1"},
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-node-2"},
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
			wantWorkerCount: 2,
			wantGPUPerNode:  8,
			wantTotalGPU:    16,
			wantErr:         false,
		},
		{
			name:    "no GPU nodes returns error",
			nodes:   []v1.Node{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.nodes))
			for i := range tt.nodes {
				objects = append(objects, &tt.nodes[i])
			}

			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset(objects...)

			ctx := &checks.ValidationContext{
				Context:   context.Background(),
				Clientset: clientset,
				Namespace: "default",
			}

			gpuConfig, err := determineGPUConfig(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("determineGPUConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if gpuConfig.WorkerCount != tt.wantWorkerCount {
				t.Errorf("WorkerCount = %d, want %d", gpuConfig.WorkerCount, tt.wantWorkerCount)
			}
			if gpuConfig.GPUCountPerNode != tt.wantGPUPerNode {
				t.Errorf("GPUCountPerNode = %d, want %d", gpuConfig.GPUCountPerNode, tt.wantGPUPerNode)
			}
			if gpuConfig.TotalGPUCount != tt.wantTotalGPU {
				t.Errorf("TotalGPUCount = %d, want %d", gpuConfig.TotalGPUCount, tt.wantTotalGPU)
			}
		})
	}
}

func TestValidateNcclAllReduceBwRegistration(t *testing.T) {
	// Verify the constraint validator is registered
	validator, ok := checks.GetConstraintValidator("nccl-all-reduce-bw")
	if !ok {
		t.Fatal("nccl-all-reduce-bw constraint validator not registered")
	}

	if validator.Pattern != "nccl-all-reduce-bw" {
		t.Errorf("Pattern = %v, want nccl-all-reduce-bw", validator.Pattern)
	}

	if validator.Description == "" {
		t.Error("Description is empty")
	}

	if validator.TestName == "" {
		t.Error("TestName is empty")
	}
}
