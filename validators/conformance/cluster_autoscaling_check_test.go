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

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
			got := detectPlatform(vctx)
			if got != tt.want {
				t.Errorf("detectPlatform() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectPlatformNoNodes(t *testing.T) {
	client := k8sfake.NewClientset()
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	got := detectPlatform(vctx)
	if got != "" {
		t.Errorf("detectPlatform() = %q, want empty string", got)
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
