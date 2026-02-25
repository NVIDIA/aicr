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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

// createGPUResourceSlice creates an unstructured ResourceSlice with the given number of GPU devices.
func createGPUResourceSlice(name string, numDevices int) *unstructured.Unstructured {
	devices := make([]interface{}, numDevices)
	for i := range numDevices {
		devices[i] = map[string]interface{}{
			"name": fmt.Sprintf("gpu-%d", i),
			"basic": map[string]interface{}{
				"attributes": map[string]interface{}{},
			},
		}
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceSlice",
			"metadata": map[string]interface{}{
				"name": name,
			},
			"spec": map[string]interface{}{
				"driver":  gpuDriverName,
				"devices": devices,
			},
		},
	}
}

func TestCheckGangScheduling(t *testing.T) {
	// Build the full set of KAI scheduler deployments for the happy path.
	allDeployments := []runtime.Object{
		createDeployment("kai-scheduler", "kai-scheduler-default", 1),
		createDeployment("kai-scheduler", "admission", 1),
		createDeployment("kai-scheduler", "binder", 1),
		createDeployment("kai-scheduler", "kai-operator", 1),
		createDeployment("kai-scheduler", "pod-grouper", 1),
		createDeployment("kai-scheduler", "podgroup-controller", 1),
		createDeployment("kai-scheduler", "queue-controller", 1),
	}

	// Common dynamic objects: CRDs + ResourceSlice with 4 GPUs.
	fullDynamicObjects := []runtime.Object{
		createCRD("queues.scheduling.run.ai"),
		createCRD("podgroups.scheduling.run.ai"),
		createGPUResourceSlice("gpu-node-0", 4),
	}

	tests := []struct {
		name           string
		k8sObjects     []runtime.Object
		dynamicObjects []runtime.Object
		clientset      bool
		podPhase       corev1.PodPhase
		claimsListErr  error
		wantErr        bool
		errContains    string
	}{
		{
			name:           "all healthy with gang scheduling",
			k8sObjects:     allDeployments,
			dynamicObjects: fullDynamicObjects,
			clientset:      true,
			podPhase:       corev1.PodSucceeded,
			wantErr:        false,
		},
		{
			name:        "no clientset",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name: "missing one deployment",
			k8sObjects: []runtime.Object{
				// Only first 6 -- missing queue-controller
				createDeployment("kai-scheduler", "kai-scheduler-default", 1),
				createDeployment("kai-scheduler", "admission", 1),
				createDeployment("kai-scheduler", "binder", 1),
				createDeployment("kai-scheduler", "kai-operator", 1),
				createDeployment("kai-scheduler", "pod-grouper", 1),
				createDeployment("kai-scheduler", "podgroup-controller", 1),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "queue-controller check failed",
		},
		{
			name: "deployment not available",
			k8sObjects: []runtime.Object{
				createDeployment("kai-scheduler", "kai-scheduler-default", 0), // 0 available
				createDeployment("kai-scheduler", "admission", 1),
				createDeployment("kai-scheduler", "binder", 1),
				createDeployment("kai-scheduler", "kai-operator", 1),
				createDeployment("kai-scheduler", "pod-grouper", 1),
				createDeployment("kai-scheduler", "podgroup-controller", 1),
				createDeployment("kai-scheduler", "queue-controller", 1),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "kai-scheduler-default check failed",
		},
		{
			name:       "missing CRD",
			k8sObjects: allDeployments,
			dynamicObjects: []runtime.Object{
				createCRD("queues.scheduling.run.ai"),
				// Missing podgroups CRD
			},
			clientset:   true,
			wantErr:     true,
			errContains: "podgroups.scheduling.run.ai not found",
		},
		{
			name:       "insufficient GPUs",
			k8sObjects: allDeployments,
			dynamicObjects: []runtime.Object{
				createCRD("queues.scheduling.run.ai"),
				createCRD("podgroups.scheduling.run.ai"),
				createGPUResourceSlice("gpu-node-0", 1), // only 1 GPU, need 2
			},
			clientset:   true,
			wantErr:     true,
			errContains: "insufficient free GPUs",
		},
		{
			name:           "gang test pod fails",
			k8sObjects:     allDeployments,
			dynamicObjects: fullDynamicObjects,
			clientset:      true,
			podPhase:       corev1.PodFailed,
			wantErr:        true,
			errContains:    "gang scheduling may have failed",
		},
		{
			name:           "resourceclaim list fails",
			k8sObjects:     allDeployments,
			dynamicObjects: fullDynamicObjects,
			clientset:      true,
			claimsListErr:  fmt.Errorf("resourceclaims list failed"),
			wantErr:        true,
			errContains:    "failed to list ResourceClaims",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				// Track deleted pods to return NotFound on subsequent Gets.
				var mu sync.Mutex
				deletedPods := make(map[string]bool)

				// Co-scheduling timestamp: all gang pods share the same schedule time.
				scheduleTime := metav1.NewTime(time.Now())

				// Reactor: match any pod with the gang worker prefix.
				// Returns a pod with the desired phase, correct gang labels,
				// and PodScheduled condition with co-scheduling timestamp.
				clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					ga, ok := action.(k8stesting.GetAction)
					if !ok {
						// Non-standard pod gets (for example log subresource plumbing)
						// should use default fake behavior.
						return false, nil, nil
					}
					if strings.HasPrefix(ga.GetName(), gangPodPrefix) && ga.GetNamespace() == gangTestNamespace {
						mu.Lock()
						deleted := deletedPods[ga.GetName()]
						mu.Unlock()
						if deleted {
							return true, nil, k8serrors.NewNotFound(
								schema.GroupResource{Resource: "pods"}, ga.GetName())
						}
						// Extract suffix from pod name: "gang-worker-SUFFIX-N" -> SUFFIX
						nameAfterPrefix := ga.GetName()[len(gangPodPrefix):]
						lastDash := strings.LastIndex(nameAfterPrefix, "-")
						suffix := nameAfterPrefix[:lastDash]
						groupName := gangGroupPrefix + suffix

						return true, &corev1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name:      ga.GetName(),
								Namespace: gangTestNamespace,
								Labels: map[string]string{
									"pod-group.scheduling.run.ai/name":     groupName,
									"pod-group.scheduling.run.ai/group-id": groupName,
								},
							},
							Spec: corev1.PodSpec{
								SchedulerName: "kai-scheduler",
								RestartPolicy: corev1.RestartPolicyNever,
								ResourceClaims: []corev1.PodResourceClaim{
									{Name: "gpu", ResourceClaimName: strPtr("test-claim")},
								},
								Containers: []corev1.Container{
									{Name: "worker", Image: "nvidia/cuda:12.9.0-base-ubuntu24.04"},
								},
							},
							Status: corev1.PodStatus{
								Phase: tt.podPhase,
								Conditions: []corev1.PodCondition{
									{
										Type:               corev1.PodScheduled,
										Status:             corev1.ConditionTrue,
										LastTransitionTime: scheduleTime,
									},
								},
							},
						}, nil
					}
					return false, nil, nil
				})

				// Reactor: mark pod as deleted so subsequent Gets return NotFound.
				clientset.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					da := action.(k8stesting.DeleteAction)
					if strings.HasPrefix(da.GetName(), gangPodPrefix) && da.GetNamespace() == gangTestNamespace {
						mu.Lock()
						deletedPods[da.GetName()] = true
						mu.Unlock()
						return true, nil, nil
					}
					return false, nil, nil
				})

				scheme := runtime.NewScheme()
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
						{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
						{Group: "scheduling.run.ai", Version: "v2alpha2", Resource: "podgroups"}:              "PodGroupList",
						{Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims"}:                 "ResourceClaimList",
						{Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices"}:                 "ResourceSliceList",
					},
					tt.dynamicObjects...)
				if tt.claimsListErr != nil {
					dynClient.PrependReactor("list", "resourceclaims",
						func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
							return true, nil, tt.claimsListErr
						},
					)
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

			err := CheckGangScheduling(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckGangScheduling() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("CheckGangScheduling() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckGangSchedulingRegistration(t *testing.T) {
	check, ok := checks.GetCheck("gang-scheduling")
	if !ok {
		t.Fatal("gang-scheduling check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}
