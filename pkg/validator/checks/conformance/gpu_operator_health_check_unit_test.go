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
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckGPUOperatorHealth(t *testing.T) {
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
				createDeployment("gpu-operator", "gpu-operator", 1),
				createDaemonSet("gpu-operator", "nvidia-dcgm-exporter", 1),
			},
			dynamicObjects: []runtime.Object{
				createClusterPolicy("ready"),
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
			name: "operator deployment not available",
			k8sObjects: []runtime.Object{
				createDeployment("gpu-operator", "gpu-operator", 0),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "not available",
		},
		{
			name: "ClusterPolicy not ready",
			k8sObjects: []runtime.Object{
				createDeployment("gpu-operator", "gpu-operator", 1),
			},
			dynamicObjects: []runtime.Object{
				createClusterPolicy("notReady"),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "ClusterPolicy state=notReady",
		},
		{
			name: "DCGM exporter not ready",
			k8sObjects: []runtime.Object{
				createDeployment("gpu-operator", "gpu-operator", 1),
				createDaemonSet("gpu-operator", "nvidia-dcgm-exporter", 0),
			},
			dynamicObjects: []runtime.Object{
				createClusterPolicy("ready"),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				scheme := runtime.NewScheme()
				var dynClient *dynamicfake.FakeDynamicClient
				if len(tt.dynamicObjects) > 0 {
					dynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
						map[schema.GroupVersionResource]string{
							{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}: "ClusterPolicyList",
						},
						tt.dynamicObjects...)
				} else {
					dynClient = dynamicfake.NewSimpleDynamicClient(scheme)
				}

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

			err := CheckGPUOperatorHealth(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckGPUOperatorHealth() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckGPUOperatorHealth() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckGPUOperatorHealthRegistration(t *testing.T) {
	check, ok := checks.GetCheck("gpu-operator-health")
	if !ok {
		t.Fatal("gpu-operator-health check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// createDaemonSet creates a test DaemonSet with 1 desired and the given ready count.
func createDaemonSet(namespace, name string, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 1,
			NumberReady:            ready,
		},
	}
}

// createClusterPolicy creates an unstructured ClusterPolicy with the given state.
func createClusterPolicy(state string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "nvidia.com/v1",
			"kind":       "ClusterPolicy",
			"metadata": map[string]interface{}{
				"name": "cluster-policy",
			},
			"status": map[string]interface{}{
				"state": state,
			},
		},
	}
}
