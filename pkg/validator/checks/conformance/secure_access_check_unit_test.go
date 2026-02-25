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
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckSecureAcceleratorAccess(t *testing.T) {
	tests := []struct {
		name        string
		podPhase    corev1.PodPhase
		clientset   bool
		wantErr     bool
		errContains string
	}{
		{
			name:      "DRA allocation succeeds",
			podPhase:  corev1.PodSucceeded,
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
			name:        "DRA allocation fails",
			podPhase:    corev1.PodFailed,
			clientset:   true,
			wantErr:     true,
			errContains: "GPU allocation may have failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				podDeleted := false

				// Reactor: match any pod with the DRA test prefix.
				// Returns the pod with desired phase, or NotFound after deletion.
				clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					ga, ok := action.(k8stesting.GetAction)
					if !ok {
						// Let log subresource plumbing use default fake behavior.
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
								Phase: tt.podPhase,
							},
						}, nil
					}
					return false, nil, nil
				})

				// Reactor: mark pod as deleted so subsequent Gets return NotFound.
				clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					da := action.(k8stesting.DeleteAction)
					if strings.HasPrefix(da.GetName(), draTestPrefix) && da.GetNamespace() == draTestNamespace {
						podDeleted = true
						return true, nil, nil
					}
					return false, nil, nil
				})

				// Reactor: return no-claim pod with Succeeded phase for isolation test.
				noClaimPodDeleted := false
				clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					ga, ok := action.(k8stesting.GetAction)
					if !ok {
						// Let log subresource plumbing use default fake behavior.
						return false, nil, nil
					}
					if strings.HasPrefix(ga.GetName(), draNoClaimPrefix) && ga.GetNamespace() == draTestNamespace {
						if noClaimPodDeleted {
							return true, nil, k8serrors.NewNotFound(
								schema.GroupResource{Resource: "pods"}, ga.GetName())
						}
						return true, &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name:      ga.GetName(),
								Namespace: draTestNamespace,
							},
							Spec: *buildNoClaimTestPod(&draTestRun{noClaimPodName: ga.GetName()}, "test-node").Spec.DeepCopy(),
							Status: corev1.PodStatus{
								Phase: corev1.PodSucceeded,
							},
						}, nil
					}
					return false, nil, nil
				})

				// Reactor: mark no-claim pod as deleted.
				clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					da := action.(k8stesting.DeleteAction)
					if strings.HasPrefix(da.GetName(), draNoClaimPrefix) && da.GetNamespace() == draTestNamespace {
						noClaimPodDeleted = true
						return true, nil, nil
					}
					return false, nil, nil
				})

				scheme := runtime.NewScheme()
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims"}: "ResourceClaimList",
						{Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices"}: "ResourceSliceList",
					})

				// Reactor: match any ResourceClaim with the DRA claim prefix.
				dynClient.PrependReactor("get", "resourceclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
					ga := action.(k8stesting.GetAction)
					if strings.HasPrefix(ga.GetName(), draClaimPrefix) && ga.GetNamespace() == draTestNamespace {
						return true, &unstructured.Unstructured{
							Object: map[string]interface{}{
								"apiVersion": "resource.k8s.io/v1",
								"kind":       "ResourceClaim",
								"metadata": map[string]interface{}{
									"name":      ga.GetName(),
									"namespace": draTestNamespace,
								},
							},
						}, nil
					}
					return false, nil, nil
				})

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

			err := CheckSecureAcceleratorAccess(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckSecureAcceleratorAccess() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckSecureAcceleratorAccess() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckSecureAcceleratorAccessRegistration(t *testing.T) {
	check, ok := checks.GetCheck("secure-accelerator-access")
	if !ok {
		t.Fatal("secure-accelerator-access check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}
