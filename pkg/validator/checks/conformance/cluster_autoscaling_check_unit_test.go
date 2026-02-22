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

package conformance

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckClusterAutoscaling(t *testing.T) {
	tests := []struct {
		name           string
		k8sObjects     []runtime.Object
		dynamicObjects []runtime.Object
		clientset      bool
		wantErr        bool
		errContains    string
	}{
		{
			name: "all healthy",
			k8sObjects: []runtime.Object{
				createDeployment("karpenter", "karpenter", 1),
			},
			dynamicObjects: []runtime.Object{
				createNodePool("gpu-pool", true),
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name:        "no clientset",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name:       "Karpenter not deployed",
			k8sObjects: []runtime.Object{
				// No karpenter deployment
			},
			clientset:   true,
			wantErr:     true,
			errContains: "Karpenter controller check failed",
		},
		{
			name: "Karpenter not available",
			k8sObjects: []runtime.Object{
				createDeployment("karpenter", "karpenter", 0),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "Karpenter controller check failed",
		},
		{
			name: "no NodePools",
			k8sObjects: []runtime.Object{
				createDeployment("karpenter", "karpenter", 1),
			},
			dynamicObjects: nil,
			clientset:      true,
			wantErr:        true,
			errContains:    "no NodePool with nvidia.com/gpu limits found",
		},
		{
			name: "NodePool without GPU limits",
			k8sObjects: []runtime.Object{
				createDeployment("karpenter", "karpenter", 1),
			},
			dynamicObjects: []runtime.Object{
				createNodePool("cpu-pool", false),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "no NodePool with nvidia.com/gpu limits found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				scheme := runtime.NewScheme()
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}: "NodePoolList",
					},
					tt.dynamicObjects...)

				ctx = &checks.ValidationContext{
					Context:       context.Background(),
					Clientset:     clientset,
					DynamicClient: dynClient,
				}
			} else {
				ctx = &checks.ValidationContext{
					Context: context.Background(),
				}
			}

			err := CheckClusterAutoscaling(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckClusterAutoscaling() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckClusterAutoscaling() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckClusterAutoscalingRegistration(t *testing.T) {
	check, ok := checks.GetCheck("cluster-autoscaling")
	if !ok {
		t.Fatal("cluster-autoscaling check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// createNodePool creates an unstructured Karpenter NodePool for testing.
func createNodePool(name string, hasGPULimits bool) *unstructured.Unstructured {
	limits := map[string]interface{}{
		"cpu": "100",
	}
	if hasGPULimits {
		limits["nvidia.com/gpu"] = "8"
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"limits": limits,
			},
		},
	}
}
