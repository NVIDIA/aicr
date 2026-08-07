// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDetectPlatform(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       string
	}{
		{"eks", "aws://us-east-1a/i-0123456789", "eks"},
		{"gke", "gce://my-project/us-central1-a/gke-node-1", "gke"},
		{"unknown", "azure:///subscriptions/...", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(&corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
				Spec:       corev1.NodeSpec{ProviderID: tt.providerID},
			})
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
			}
			got, err := detectPlatform(vctx)
			if err != nil {
				t.Fatalf("detectPlatform() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("detectPlatform() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectPlatformNoNodes(t *testing.T) {
	// A genuinely empty node list (Kind CI, KWOK) is legitimately unrecognized,
	// not an infrastructure failure: ("", nil) so the caller can Skip.
	client := k8sfake.NewClientset()
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	got, err := detectPlatform(vctx)
	if err != nil {
		t.Fatalf("detectPlatform() unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("detectPlatform() = %q, want empty string", got)
	}
}

// TestDetectPlatformListError_FailsClosed proves the #2122 fix: a Nodes().List
// error (RBAC denial, apiserver timeout, transport failure) must NOT be
// flattened to "" (which the caller would turn into an inapplicable Skip and a
// false PASS). It must fail closed with a classified pkg/errors code and never
// a Skip. Each case fails against the pre-fix code, which returned ("") with no
// error for any List failure.
func TestDetectPlatformListError_FailsClosed(t *testing.T) {
	gr := schema.GroupResource{Group: "", Resource: "nodes"}
	tests := []struct {
		name     string
		listErr  error
		wantCode errors.ErrorCode
	}{
		{
			name:     "Forbidden → Unauthorized",
			listErr:  k8serrors.NewForbidden(gr, "", fmt.Errorf("rbac denied")),
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name:     "apiserver ServerTimeout → Timeout",
			listErr:  k8serrors.NewServerTimeout(gr, "list", 1),
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name:     "context deadline → Timeout",
			listErr:  context.DeadlineExceeded,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name:     "ServiceUnavailable → Unavailable",
			listErr:  k8serrors.NewServiceUnavailable("aggregated apiserver down"),
			wantCode: errors.ErrCodeUnavailable,
		},
		{
			// A NotFound on a *collection* List is an apiserver/aggregation-layer
			// anomaly, never the clean absence of a single object — so it must
			// block, not Skip. This case fails against a path that routes the List
			// error through Capability.Require (which would Skip on NotFound for an
			// undeclared capability); RequireList keeps it a blocking Internal.
			name:     "collection NotFound → Internal (never Skip)",
			listErr:  k8serrors.NewNotFound(gr, ""),
			wantCode: errors.ErrCodeInternal,
		},
		{
			name:     "generic error → Internal",
			listErr:  stderrors.New("boom"),
			wantCode: errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.listErr
			})
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
			}
			got, err := detectPlatform(vctx)
			if got != "" {
				t.Errorf("detectPlatform() platform = %q, want empty on error", got)
			}
			if err == nil {
				t.Fatal("detectPlatform() = nil error, want a blocking failure (#2122)")
			}
			if validators.IsSkip(err) {
				t.Fatalf("detectPlatform() = %v, want a blocking failure but got a Skip — an infra error must never masquerade as inapplicable (#2122)", err)
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("detectPlatform() code = %v, want %v", err, tt.wantCode)
			}
		})
	}
}

// TestCheckPlatformAutoscalingListError_FailsClosed is the integration-level
// #2122 guard: a node-list RBAC denial reaching the platform fallback must
// block the gate, not fall through to the unrecognized-platform Skip.
func TestCheckPlatformAutoscalingListError_FailsClosed(t *testing.T) {
	client := k8sfake.NewClientset()
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(
			schema.GroupResource{Group: "", Resource: "nodes"}, "", fmt.Errorf("rbac denied"))
	})
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	err := checkPlatformAutoscaling(vctx)
	if err == nil {
		t.Fatal("checkPlatformAutoscaling() = nil, want a blocking failure (#2122)")
	}
	if validators.IsSkip(err) {
		t.Fatalf("checkPlatformAutoscaling() = %v, want a blocking failure but got a Skip (#2122)", err)
	}
	if strings.Contains(err.Error(), "not recognized") {
		t.Errorf("checkPlatformAutoscaling() masqueraded infra error as unrecognized-platform skip: %v", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnauthorized, "")) {
		t.Errorf("checkPlatformAutoscaling() code = %v, want Unauthorized", err)
	}
}

func TestCheckEKSAutoscaling(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []corev1.Node
		wantErr bool
		errMsg  string
	}{
		{
			name:    "no GPU nodes",
			nodes:   []corev1.Node{},
			wantErr: true,
			errMsg:  "no GPU nodes found",
		},
		{
			name: "GPU nodes with node group label",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gpu-node-1",
						Labels: map[string]string{
							"nvidia.com/gpu.present":           "true",
							"node.kubernetes.io/instance-type": "p5.48xlarge",
							"eks.amazonaws.com/nodegroup":      "gpu-workers",
							"topology.kubernetes.io/region":    "us-east-1",
							"topology.kubernetes.io/zone":      "us-east-1a",
						},
					},
					Spec: corev1.NodeSpec{ProviderID: "aws://us-east-1a/i-abc123"},
					Status: corev1.NodeStatus{
						Capacity: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			for i := range tt.nodes {
				_, err := client.CoreV1().Nodes().Create(context.Background(), &tt.nodes[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create node: %v", err)
				}
			}
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
			}
			err := checkEKSAutoscaling(vctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkEKSAutoscaling() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckGKEAutoscaling(t *testing.T) {
	tests := []struct {
		name       string
		nodes      []corev1.Node
		configMaps []corev1.ConfigMap
		wantErr    bool
	}{
		{
			name:    "no GPU nodes",
			nodes:   []corev1.Node{},
			wantErr: true,
		},
		{
			name: "GPU nodes with autoscaler ConfigMap",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gke-gpu-node-1",
						Labels: map[string]string{
							"nvidia.com/gpu.present":           "true",
							"node.kubernetes.io/instance-type": "a3-megagpu-8g",
							"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb",
							"cloud.google.com/gke-nodepool":    "gpu-pool",
						},
					},
					Spec: corev1.NodeSpec{ProviderID: "gce://my-project/us-central1-a/gke-gpu-node-1"},
					Status: corev1.NodeStatus{
						Capacity: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
			configMaps: []corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cluster-autoscaler-status",
						Namespace: "kube-system",
					},
					Data: map[string]string{
						"status": "Cluster-autoscaler status at 2026-03-19: Healthy",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "GPU nodes without autoscaler ConfigMap still passes",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gke-gpu-node-1",
						Labels: map[string]string{
							"nvidia.com/gpu.present":        "true",
							"cloud.google.com/gke-nodepool": "gpu-pool",
						},
					},
					Spec: corev1.NodeSpec{ProviderID: "gce://my-project/us-central1-a/gke-gpu-node-1"},
					Status: corev1.NodeStatus{
						Capacity: corev1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("8"),
						},
					},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			for i := range tt.nodes {
				_, err := client.CoreV1().Nodes().Create(context.Background(), &tt.nodes[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create node: %v", err)
				}
			}
			for i := range tt.configMaps {
				_, err := client.CoreV1().ConfigMaps(tt.configMaps[i].Namespace).Create(
					context.Background(), &tt.configMaps[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create configmap: %v", err)
				}
			}
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
			}
			err := checkGKEAutoscaling(vctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkGKEAutoscaling() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckPlatformAutoscalingSkipsUnknown(t *testing.T) {
	client := k8sfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"},
	})
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	err := checkPlatformAutoscaling(vctx)
	if err == nil || !strings.Contains(err.Error(), "not recognized") {
		t.Errorf("checkPlatformAutoscaling() should skip for unknown platform, got: %v", err)
	}
}

func TestCheckClusterAutoscaling_KarpenterNotFound_FallsBack(t *testing.T) {
	// No karpenter deployment → K8s returns NotFound → should fall back to platform detection.
	// Node has unknown providerID → platform fallback returns skip.
	client := k8sfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{ProviderID: "kind://docker/kind/kind-control-plane"},
	})
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	err := CheckClusterAutoscaling(vctx)
	// Should skip (platform not recognized) rather than fail with "not found"
	if err == nil || !strings.Contains(err.Error(), "not recognized") {
		t.Errorf("expected platform skip, got: %v", err)
	}
}

func TestCheckClusterAutoscaling_KarpenterUnhealthy_Fails(t *testing.T) {
	// Karpenter deployment exists but has 0 available replicas → should fail, not fall back.
	client := k8sfake.NewClientset()
	replicas := int32(1)
	_, err := client.AppsV1().Deployments("karpenter").Create(context.Background(),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "karpenter",
				Namespace: "karpenter",
				Labels:    map[string]string{"app.kubernetes.io/name": "karpenter"},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "karpenter"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "karpenter"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
				},
			},
			Status: appsv1.DeploymentStatus{AvailableReplicas: 0},
		}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	err = CheckClusterAutoscaling(vctx)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("expected unhealthy error, got: %v", err)
	}
}

func TestCheckClusterAutoscaling_APIError_DoesNotFallBack(t *testing.T) {
	// Non-NotFound API error (e.g., RBAC forbidden) on list → should fail, not fall back.
	client := k8sfake.NewClientset()
	client.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"},
			"karpenter", fmt.Errorf("forbidden"))
	})
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	err := CheckClusterAutoscaling(vctx)
	if err == nil || strings.Contains(err.Error(), "not recognized") {
		t.Errorf("expected API error (not fallback skip), got: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to search for Karpenter deployment") {
		t.Errorf("expected wrapped API error, got: %v", err)
	}
}
