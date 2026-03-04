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

package conformance

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckPlatformHealth(t *testing.T) {
	tests := []struct {
		name        string
		objects     []runtime.Object
		recipe      *recipe.RecipeResult
		clientset   bool
		wantErr     bool
		errContains string
	}{
		{
			name: "all healthy - namespace active and deployment available",
			objects: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"},
					Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
				},
				createDeployment("gpu-operator", "gpu-operator", 1),
			},
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "gpu-operator",
						Namespace: "gpu-operator",
						ExpectedResources: []recipe.ExpectedResource{
							{Kind: "Deployment", Namespace: "gpu-operator", Name: "gpu-operator"},
						},
					},
				},
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name:        "no clientset available",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name:      "no recipe available",
			objects:   []runtime.Object{},
			clientset: true,
			recipe:    nil,
			wantErr:   true,

			errContains: "recipe is not available",
		},
		{
			name:    "missing namespace",
			objects: []runtime.Object{},
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "gpu-operator",
						Namespace: "gpu-operator",
					},
				},
			},
			clientset:   true,
			wantErr:     true,
			errContains: "namespace gpu-operator: not found",
		},
		{
			name: "deployment not available",
			objects: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"},
					Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
				},
				createDeployment("gpu-operator", "gpu-operator", 0),
			},
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "gpu-operator",
						Namespace: "gpu-operator",
						ExpectedResources: []recipe.ExpectedResource{
							{Kind: "Deployment", Namespace: "gpu-operator", Name: "gpu-operator"},
						},
					},
				},
			},
			clientset:   true,
			wantErr:     true,
			errContains: "not healthy",
		},
		{
			name:    "empty recipe with no components - passes",
			objects: []runtime.Object{},
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{},
			},
			clientset: true,
			wantErr:   false,
		},
		{
			name: "namespace terminating",
			objects: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"},
					Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating},
				},
			},
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "gpu-operator",
						Namespace: "gpu-operator",
					},
				},
			},
			clientset:   true,
			wantErr:     true,
			errContains: "phase=Terminating",
		},
		{
			name: "daemonset not ready",
			objects: []runtime.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator"},
					Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
				},
				&appsv1.DaemonSet{
					ObjectMeta: metav1.ObjectMeta{Name: "nvidia-dcgm-exporter", Namespace: "gpu-operator"},
					Status: appsv1.DaemonSetStatus{
						DesiredNumberScheduled: 2,
						NumberReady:            0,
					},
				},
			},
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "dcgm-exporter",
						Namespace: "gpu-operator",
						ExpectedResources: []recipe.ExpectedResource{
							{Kind: "DaemonSet", Namespace: "gpu-operator", Name: "nvidia-dcgm-exporter"},
						},
					},
				},
			},
			clientset:   true,
			wantErr:     true,
			errContains: "not healthy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.objects...)
				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    tt.recipe,
				}
			} else {
				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: nil,
					Recipe:    tt.recipe,
				}
			}

			err := CheckPlatformHealth(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckPlatformHealth() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckPlatformHealth() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckPlatformHealthRegistration(t *testing.T) {
	check, ok := checks.GetCheck("platform-health")
	if !ok {
		t.Fatal("platform-health check not registered")
	}

	if check.Name != "platform-health" {
		t.Errorf("Name = %v, want platform-health", check.Name)
	}

	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// createDeployment creates a test Deployment with 1 desired replica and the given available count.
func createDeployment(namespace, name string, available int32) *appsv1.Deployment {
	var replicas int32 = 1
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: available,
		},
	}
}
