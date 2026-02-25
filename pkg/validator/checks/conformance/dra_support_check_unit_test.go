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
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckDRASupport(t *testing.T) {
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
				createDeployment("nvidia-dra-driver", "nvidia-dra-driver-gpu-controller", 1),
				createDaemonSet("nvidia-dra-driver", "nvidia-dra-driver-gpu-kubelet-plugin", 1),
			},
			dynamicObjects: []runtime.Object{
				createResourceSlice("gpu-slice-0"),
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
			name: "controller deployment not available",
			k8sObjects: []runtime.Object{
				createDeployment("nvidia-dra-driver", "nvidia-dra-driver-gpu-controller", 0),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "DRA driver controller check failed",
		},
		{
			name: "controller deployment missing",
			k8sObjects: []runtime.Object{
				// No controller deployment
				createDaemonSet("nvidia-dra-driver", "nvidia-dra-driver-gpu-kubelet-plugin", 1),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "DRA driver controller check failed",
		},
		{
			name: "kubelet plugin not ready",
			k8sObjects: []runtime.Object{
				createDeployment("nvidia-dra-driver", "nvidia-dra-driver-gpu-controller", 1),
				createDaemonSet("nvidia-dra-driver", "nvidia-dra-driver-gpu-kubelet-plugin", 0),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "DRA kubelet plugin check failed",
		},
		{
			name: "no ResourceSlices",
			k8sObjects: []runtime.Object{
				createDeployment("nvidia-dra-driver", "nvidia-dra-driver-gpu-controller", 1),
				createDaemonSet("nvidia-dra-driver", "nvidia-dra-driver-gpu-kubelet-plugin", 1),
			},
			dynamicObjects: nil, // empty but registered via custom list kinds
			clientset:      true,
			wantErr:        true,
			errContains:    "no ResourceSlices found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				// Discovery API resources for resource.k8s.io/v1.
				if fd, ok := clientset.Discovery().(*discoveryfake.FakeDiscovery); ok {
					fd.Resources = []*metav1.APIResourceList{
						{
							GroupVersion: "resource.k8s.io/v1",
							APIResources: []metav1.APIResource{
								{Name: "deviceclasses", Kind: "DeviceClass", Namespaced: false},
								{Name: "resourceclaims", Kind: "ResourceClaim", Namespaced: true},
								{Name: "resourceclaimtemplates", Kind: "ResourceClaimTemplate", Namespaced: true},
								{Name: "resourceslices", Kind: "ResourceSlice", Namespaced: false},
							},
						},
					}
				}

				// Behavioral test pod status stub: pods created with dra test prefix
				// immediately appear completed to avoid poll timeouts in unit tests.
				podDeleted := false
				clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					ga, ok := action.(k8stesting.GetAction)
					if !ok {
						// Let non-standard pod gets (for example log subresource plumbing)
						// fall back to the default fake behavior.
						return false, nil, nil
					}
					if strings.HasPrefix(ga.GetName(), draTestPrefix) && ga.GetNamespace() == draTestNamespace {
						if podDeleted {
							return true, nil, k8serrors.NewNotFound(
								schema.GroupResource{Resource: "pods"}, ga.GetName())
						}
						run := &draTestRun{podName: ga.GetName(), claimName: draClaimPrefix + ga.GetName()[len(draTestPrefix):]}
						return true, &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name:      run.podName,
								Namespace: draTestNamespace,
							},
							Spec: *buildDRATestPod(run).Spec.DeepCopy(),
							Status: corev1.PodStatus{
								Phase: corev1.PodSucceeded,
							},
						}, nil
					}
					return false, nil, nil
				})
				clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					da := action.(k8stesting.DeleteAction)
					if strings.HasPrefix(da.GetName(), draTestPrefix) && da.GetNamespace() == draTestNamespace {
						podDeleted = true
						return true, nil, nil
					}
					return false, nil, nil
				})

				scheme := runtime.NewScheme()
				// Always register custom list kinds so List() works even with 0 objects.
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices"}: "ResourceSliceList",
						{Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims"}: "ResourceClaimList",
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

			err := CheckDRASupport(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckDRASupport() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckDRASupport() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckDRASupportRegistration(t *testing.T) {
	check, ok := checks.GetCheck("dra-support")
	if !ok {
		t.Fatal("dra-support check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

// createResourceSlice creates an unstructured ResourceSlice for testing.
func createResourceSlice(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceSlice",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"driver":   "gpu.nvidia.com",
				"nodeName": "gpu-node-0",
			},
		},
	}
}

// Note: createDeployment is defined in platform_health_check_unit_test.go,
// and createDaemonSet is in gpu_operator_health_check_unit_test.go.
// They all live in the same test package so they're accessible here.
