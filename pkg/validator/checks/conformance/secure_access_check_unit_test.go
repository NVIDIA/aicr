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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckSecureAcceleratorAccess(t *testing.T) {
	tests := []struct {
		name           string
		k8sObjects     []runtime.Object
		dynamicObjects []runtime.Object
		clientset      bool
		wantErr        bool
		errContains    string
	}{
		{
			name: "valid DRA pod succeeded with claim",
			k8sObjects: []runtime.Object{
				createDRAPod(true, false, false, corev1.PodSucceeded),
			},
			dynamicObjects: []runtime.Object{
				createResourceClaim("dra-test", "gpu-claim"),
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
			name:        "pod not found",
			k8sObjects:  []runtime.Object{},
			clientset:   true,
			wantErr:     true,
			errContains: "DRA test pod not found",
		},
		{
			name: "pod without resourceClaims",
			k8sObjects: []runtime.Object{
				createDRAPod(false, false, false, corev1.PodSucceeded),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "does not use DRA resourceClaims",
		},
		{
			name: "pod uses device plugin",
			k8sObjects: []runtime.Object{
				createDRAPod(true, true, false, corev1.PodSucceeded),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "uses device plugin",
		},
		{
			name: "pod has hostPath to GPU device",
			k8sObjects: []runtime.Object{
				createDRAPod(true, false, true, corev1.PodSucceeded),
			},
			clientset:   true,
			wantErr:     true,
			errContains: "hostPath volume to /dev/nvidia0",
		},
		{
			name: "ResourceClaim not found",
			k8sObjects: []runtime.Object{
				createDRAPod(true, false, false, corev1.PodSucceeded),
			},
			dynamicObjects: []runtime.Object{
				// No ResourceClaim
			},
			clientset:   true,
			wantErr:     true,
			errContains: "ResourceClaim gpu-claim not found",
		},
		{
			name: "pod not succeeded",
			k8sObjects: []runtime.Object{
				createDRAPod(true, false, false, corev1.PodFailed),
			},
			dynamicObjects: []runtime.Object{
				createResourceClaim("dra-test", "gpu-claim"),
			},
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
				clientset := fake.NewSimpleClientset(tt.k8sObjects...)

				scheme := runtime.NewScheme()
				dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
					map[schema.GroupVersionResource]string{
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

// createDRAPod creates a test pod simulating DRA-based GPU access.
func createDRAPod(hasResourceClaims, hasDevicePlugin, hasHostPath bool, phase corev1.PodPhase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dra-gpu-test",
			Namespace: "dra-test",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "gpu-workload",
					Image: "nvidia/cuda:12.0-base",
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}

	if hasResourceClaims {
		pod.Spec.ResourceClaims = []corev1.PodResourceClaim{
			{
				Name:              "gpu",
				ResourceClaimName: strPtr("gpu-claim"),
			},
		}
	}

	if hasDevicePlugin {
		pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse("1"),
			},
		}
	}

	if hasHostPath {
		hostPathType := corev1.HostPathCharDev
		pod.Spec.Volumes = []corev1.Volume{
			{
				Name: "gpu-device",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/dev/nvidia0",
						Type: &hostPathType,
					},
				},
			},
		}
	}

	return pod
}

func strPtr(s string) *string {
	return &s
}

// createResourceClaim creates an unstructured ResourceClaim.
func createResourceClaim(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceClaim",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
			},
		},
	}
}
