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

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestValidateExpectedResources(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() *checks.ValidationContext
		wantErr     bool
		errContains string
	}{
		{
			name: "all resources present and healthy",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDeployment("gpu-operator", "gpu-operator", 1, 1),
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 2, 2),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr: false,
		},
		{
			name: "missing deployment",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 2, 2),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "gpu-operator",
		},
		{
			name: "deployment exists but not healthy",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDeployment("gpu-operator", "gpu-operator", 1, 0),
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 2, 2),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "not healthy",
		},
		{
			name: "daemonset exists but not ready",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDeployment("gpu-operator", "gpu-operator", 1, 1),
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 2, 0),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "not healthy",
		},
		{
			name: "deployment partially available",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDeployment("gpu-operator", "gpu-operator", 3, 2),
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 2, 2),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "2/3 replicas available",
		},
		{
			name: "daemonset partially ready",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDeployment("gpu-operator", "gpu-operator", 1, 1),
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 4, 3),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "3/4 pods ready",
		},
		{
			name: "no expected resources",
			setup: func() *checks.ValidationContext {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{Name: "gpu-operator", Type: "Helm"},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "nil recipe",
			setup: func() *checks.ValidationContext {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    nil,
				}
			},
			wantErr:     true,
			errContains: "recipe is not available",
		},
		{
			name: "nil clientset",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: nil,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name: "API error",
			setup: func() *checks.ValidationContext {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, &fakeAPIError{message: "simulated API error"}
				})
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe:    sampleRecipeWithExpectedResources(),
				}
			},
			wantErr:     true,
			errContains: "not found",
		},
		{
			name: "multiple components with expected resources",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					createDeployment("gpu-operator", "gpu-operator", 1, 1),
					createDaemonSet("nvidia-driver-daemonset", "gpu-operator", 2, 2),
					createDaemonSet("nvidia-device-plugin-daemonset", "gpu-operator", 2, 2),
					createDeployment("network-operator", "nvidia-network-operator", 1, 1),
					createDaemonSet("mofed-driver", "nvidia-network-operator", 2, 2),
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "gpu-operator",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "Deployment", Name: "gpu-operator", Namespace: "gpu-operator"},
									{Kind: "DaemonSet", Name: "nvidia-driver-daemonset", Namespace: "gpu-operator"},
									{Kind: "DaemonSet", Name: "nvidia-device-plugin-daemonset", Namespace: "gpu-operator"},
								},
							},
							{
								Name: "network-operator",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "Deployment", Name: "network-operator", Namespace: "nvidia-network-operator"},
									{Kind: "DaemonSet", Name: "mofed-driver", Namespace: "nvidia-network-operator"},
								},
							},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "statefulset healthy",
			setup: func() *checks.ValidationContext {
				replicas := int32(3)
				objects := []runtime.Object{
					&appsv1.StatefulSet{
						ObjectMeta: metav1.ObjectMeta{Name: "prometheus", Namespace: "monitoring"},
						Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
						Status:     appsv1.StatefulSetStatus{ReadyReplicas: 3},
					},
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "monitoring",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "StatefulSet", Name: "prometheus", Namespace: "monitoring"},
								},
							},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "service exists",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					&corev1.Service{
						ObjectMeta: metav1.ObjectMeta{Name: "gpu-operator", Namespace: "gpu-operator"},
					},
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "gpu-operator",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "Service", Name: "gpu-operator", Namespace: "gpu-operator"},
								},
							},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "configmap exists",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					&corev1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "gpu-config", Namespace: "gpu-operator"},
					},
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "gpu-operator",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "ConfigMap", Name: "gpu-config", Namespace: "gpu-operator"},
								},
							},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "secret exists",
			setup: func() *checks.ValidationContext {
				objects := []runtime.Object{
					&corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "gpu-credentials", Namespace: "gpu-operator"},
					},
				}
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(objects...)
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "gpu-operator",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "Secret", Name: "gpu-credentials", Namespace: "gpu-operator"},
								},
							},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "secret missing",
			setup: func() *checks.ValidationContext {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "gpu-operator",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "Secret", Name: "nonexistent-secret", Namespace: "gpu-operator"},
								},
							},
						},
					},
				}
			},
			wantErr:     true,
			errContains: "not found",
		},
		{
			name: "unsupported kind",
			setup: func() *checks.ValidationContext {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				return &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "test",
								Type: "Helm",
								ExpectedResources: []recipe.ExpectedResource{
									{Kind: "CronJob", Name: "test", Namespace: "default"},
								},
							},
						},
					},
				}
			},
			wantErr:     true,
			errContains: "unsupported resource kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup()
			err := validateExpectedResources(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateExpectedResources() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !containsString(err.Error(), tt.errContains) {
					t.Errorf("validateExpectedResources() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestValidateExpectedResourcesRegistration(t *testing.T) {
	// Verify the check is registered
	check, ok := checks.GetCheck("expected-resources")
	if !ok {
		t.Fatal("expected-resources check not registered")
	}

	if check.Name != "expected-resources" {
		t.Errorf("Name = %v, want expected-resources", check.Name)
	}

	if check.Phase != "deployment" {
		t.Errorf("Phase = %v, want deployment", check.Phase)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// sampleRecipeWithExpectedResources creates a RecipeResult with gpu-operator expectedResources.
func sampleRecipeWithExpectedResources() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:   "gpu-operator",
				Type:   "Helm",
				Source: "https://helm.ngc.nvidia.com/nvidia",
				ExpectedResources: []recipe.ExpectedResource{
					{Kind: "Deployment", Name: "gpu-operator", Namespace: "gpu-operator"},
					{Kind: "DaemonSet", Name: "nvidia-driver-daemonset", Namespace: "gpu-operator"},
					{Kind: "DaemonSet", Name: "nvidia-device-plugin-daemonset", Namespace: "gpu-operator"},
				},
			},
		},
	}
}

// createDeployment creates a Deployment with the specified replica counts.
func createDeployment(name, namespace string, replicas, available int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			Replicas:          replicas,
			AvailableReplicas: available,
		},
	}
}

// createDaemonSet creates a DaemonSet with the specified scheduling counts.
func createDaemonSet(name, namespace string, desired, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: desired,
			NumberReady:            ready,
		},
	}
}
