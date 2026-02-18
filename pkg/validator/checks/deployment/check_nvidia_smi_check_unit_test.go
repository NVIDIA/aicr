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

package deployment

import (
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidateCheckNvidiaSmi(t *testing.T) {
	tests := []struct {
		name    string
		setup   func() *checks.ValidationContext
		wantErr bool
	}{
		{
			name: "fails with fake clientset - cannot simulate pod lifecycle",
			setup: func() *checks.ValidationContext {
				// Create a fake node with GPU resources
				node := &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-1",
					},
					Spec: v1.NodeSpec{
						Unschedulable: false,
					},
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
						},
					},
				}
				// Use a short timeout context for unit tests to fail fast
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = cancel // Will be called by test cleanup
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				return &checks.ValidationContext{
					Context:   ctx,
					Namespace: "default",
					Clientset: fake.NewSimpleClientset(node),
					RecipeData: map[string]interface{}{
						"accelerator": recipe.CriteriaAcceleratorH100,
					},
				}
			},
			wantErr: true, // Fake clientset cannot simulate pod completion
		},
		// TODO: Add failure test cases when implementation is complete
		// {
		// 	name: "failure case - missing resource",
		// 	setup: func() *checks.ValidationContext {
		// 		return &checks.ValidationContext{
		// 			Context: context.Background(),
		// 			// Setup context that should cause failure
		// 		}
		// 	},
		// 	wantErr: true,
		// },
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup()
			err := validateCheckNvidiaSmi(ctx, t)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateCheckNvidiaSmi() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateCheckNvidiaSmiRegistration(t *testing.T) {
	// Verify the check is registered
	check, ok := checks.GetCheck("check-nvidia-smi")
	if !ok {
		t.Fatal("check-nvidia-smi check not registered")
	}

	if check.Name != "check-nvidia-smi" {
		t.Errorf("Name = %v, want check-nvidia-smi", check.Name)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.TestName == "" {
		t.Error("TestName is empty")
	}
}
