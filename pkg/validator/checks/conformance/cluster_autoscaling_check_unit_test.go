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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCheckClusterAutoscaling(t *testing.T) {
	tests := []struct {
		name           string
		k8sObjects     []runtime.Object
		dynamicObjects []runtime.Object
		clientset      bool
		reactorPool    string // NodePool name for behavioral chain reactors (empty = no reactors)
		wantErr        bool
		errContains    string
	}{
		{
			name: "all healthy with behavioral chain",
			k8sObjects: []runtime.Object{
				createDeployment("karpenter", "karpenter", 1),
			},
			dynamicObjects: []runtime.Object{
				createNodePool("gpu-pool", true),
			},
			clientset:   true,
			reactorPool: "gpu-pool",
			wantErr:     false,
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
		{
			name: "multiple GPU NodePools (first viable)",
			k8sObjects: []runtime.Object{
				createDeployment("karpenter", "karpenter", 1),
			},
			dynamicObjects: []runtime.Object{
				createNodePool("gpu-pool-a100", true),
				createNodePool("gpu-pool-h100", true),
			},
			clientset:   true,
			reactorPool: "gpu-pool-a100",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				if tt.reactorPool != "" {
					addClusterAutoReactors(clientset, tt.reactorPool)
				}

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

// addClusterAutoReactors adds fake reactors to simulate the full behavioral chain:
// HPA with scaling intent, Karpenter KWOK nodes, Deployment scale-up, and scheduled pods.
func addClusterAutoReactors(clientset *fake.Clientset, nodePoolName string) {
	// HPA Get reactor: return HPA with desiredReplicas > currentReplicas.
	clientset.PrependReactor("get", "horizontalpodautoscalers",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			hpa := &autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      action.(k8stesting.GetAction).GetName(),
					Namespace: action.GetNamespace(),
				},
				Status: autoscalingv2.HorizontalPodAutoscalerStatus{
					DesiredReplicas: 3,
					CurrentReplicas: 1,
				},
			}
			return true, hpa, nil
		})

	// Deployment Get reactor: return Deployment with scaled replicas.
	// Only intercept Gets in test namespaces (not the Karpenter controller check).
	clientset.PrependReactor("get", "deployments",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			if !strings.HasPrefix(action.GetNamespace(), clusterAutoTestPrefix) {
				return false, nil, nil
			}
			deploy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      action.(k8stesting.GetAction).GetName(),
					Namespace: action.GetNamespace(),
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 3,
				},
			}
			return true, deploy, nil
		})

	// Node List reactor: return a KWOK node with discovered NodePool label.
	clientset.PrependReactor("list", "nodes",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.NodeList{
				Items: []corev1.Node{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "kwok-gpu-node-1",
							Labels: map[string]string{
								karpenterNodePoolLabel: nodePoolName,
							},
						},
					},
				},
			}, nil
		})

	// Pod List reactor: return 3 Running pods (match unique namespace by prefix).
	clientset.PrependReactor("list", "pods",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			listAction := action.(k8stesting.ListAction)
			ns := listAction.GetNamespace()
			if !strings.HasPrefix(ns, clusterAutoTestPrefix) {
				return false, nil, nil
			}
			return true, &corev1.PodList{
				Items: []corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: ns},
						Status:     corev1.PodStatus{Phase: corev1.PodRunning},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: ns},
						Status:     corev1.PodStatus{Phase: corev1.PodRunning},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "pod-3", Namespace: ns},
						Status:     corev1.PodStatus{Phase: corev1.PodRunning},
					},
				},
			}, nil
		})
}

func TestValidateClusterAutoscaling(t *testing.T) {
	const testNodePool = "test-gpu-pool"

	tests := []struct {
		name            string
		hpaDesired      int32
		hpaCurrent      int32
		deployReplicas  int32
		kwokNodes       int
		podCount        int
		podPhase        corev1.PodPhase
		useShortTimeout bool
		wantErr         bool
		errContains     string
	}{
		{
			name:           "full chain succeeds",
			hpaDesired:     3,
			hpaCurrent:     1,
			deployReplicas: 3,
			kwokNodes:      1,
			podCount:       3,
			podPhase:       corev1.PodRunning,
			wantErr:        false,
		},
		{
			name:            "HPA does not scale",
			hpaDesired:      1,
			hpaCurrent:      1,
			useShortTimeout: true,
			wantErr:         true,
			errContains:     "HPA did not report scaling intent",
		},
		{
			name:            "no Karpenter nodes",
			hpaDesired:      3,
			hpaCurrent:      1,
			deployReplicas:  3,
			kwokNodes:       0,
			useShortTimeout: true,
			wantErr:         true,
			errContains:     "Karpenter did not provision GPU nodes",
		},
		{
			name:           "pods not scheduled",
			hpaDesired:     3,
			hpaCurrent:     1,
			deployReplicas: 3,
			kwokNodes:      1,
			podCount:       3,
			podPhase:       corev1.PodPending,
			useShortTimeout: true,
			wantErr:         true,
			errContains:    "test pods not scheduled within timeout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset()

			// HPA Get reactor.
			clientset.PrependReactor("get", "horizontalpodautoscalers",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					hpa := &autoscalingv2.HorizontalPodAutoscaler{
						ObjectMeta: metav1.ObjectMeta{
							Name:      action.(k8stesting.GetAction).GetName(),
							Namespace: action.GetNamespace(),
						},
						Status: autoscalingv2.HorizontalPodAutoscalerStatus{
							DesiredReplicas: tt.hpaDesired,
							CurrentReplicas: tt.hpaCurrent,
						},
					}
					return true, hpa, nil
				})

			// Deployment Get reactor: return deployment with scaled replicas.
			clientset.PrependReactor("get", "deployments",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					deploy := &appsv1.Deployment{
						ObjectMeta: metav1.ObjectMeta{
							Name:      action.(k8stesting.GetAction).GetName(),
							Namespace: action.GetNamespace(),
						},
						Status: appsv1.DeploymentStatus{
							Replicas: tt.deployReplicas,
						},
					}
					return true, deploy, nil
				})

			// Node List reactor.
			nodes := make([]corev1.Node, tt.kwokNodes)
			for i := range nodes {
				nodes[i] = corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("kwok-gpu-node-%d", i),
						Labels: map[string]string{
							karpenterNodePoolLabel: testNodePool,
						},
					},
				}
			}
			clientset.PrependReactor("list", "nodes",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					return true, &corev1.NodeList{Items: nodes}, nil
				})

			// Pod List reactor: match unique namespace by prefix.
			pods := make([]corev1.Pod, tt.podCount)
			for i := range pods {
				pods[i] = corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d", i),
						Namespace: "test-ns",
					},
					Status: corev1.PodStatus{Phase: tt.podPhase},
				}
			}
			clientset.PrependReactor("list", "pods",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					listAction := action.(k8stesting.ListAction)
					ns := listAction.GetNamespace()
					if !strings.HasPrefix(ns, clusterAutoTestPrefix) {
						return false, nil, nil
					}
					return true, &corev1.PodList{Items: pods}, nil
				})

			ctx := context.Background()
			if tt.useShortTimeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 1*time.Second)
				defer cancel()
			}

			err := validateClusterAutoscaling(ctx, clientset, testNodePool)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateClusterAutoscaling() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("validateClusterAutoscaling() error = %v, should contain %q", err, tt.errContains)
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
