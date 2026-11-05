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
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestDetectPlatform(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       string
	}{
		{"eks", "aws://us-east-1a/i-0123456789", "eks"},
		{"gke", "gce://my-project/us-central1-a/gke-node-1", "gke"},
		{"aks", "azure:///subscriptions/000/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu/virtualMachines/0", "aks"},
		{"unknown", "kind://docker/kind/kind-control-plane", ""},
		{"bare metal", "nico://rack1/node-07", ""},
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
			got, pid, err := detectPlatform(vctx)
			if err != nil {
				t.Fatalf("detectPlatform() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("detectPlatform() = %q, want %q", got, tt.want)
			}
			if pid != tt.providerID {
				t.Errorf("detectPlatform() providerID = %q, want %q", pid, tt.providerID)
			}
		})
	}
}

func TestProviderScheme(t *testing.T) {
	tests := []struct {
		providerID string
		want       string
	}{
		{"nico://rack1/node-07", "nico"},
		{"kind://docker/kind/kind-control-plane", "kind"},
		{"aws://us-east-1a/i-0123456789", "aws"},
		{"", "none"},
		{"://missing-scheme", "none"},
		{"no-scheme-at-all", "none"},
	}
	for _, tt := range tests {
		t.Run(tt.providerID, func(t *testing.T) {
			if got := providerScheme(tt.providerID); got != tt.want {
				t.Errorf("providerScheme(%q) = %q, want %q", tt.providerID, got, tt.want)
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
	got, _, err := detectPlatform(vctx)
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
			got, _, err := detectPlatform(vctx)
			if got != "" {
				t.Errorf("detectPlatform() platform = %q, want empty on error", got)
			}
			if err == nil {
				t.Fatal("detectPlatform() = nil error, want a blocking failure (#2122)")
			}
			if validators.IsSkip(err) {
				t.Fatalf("detectPlatform() = %v, want a blocking failure but got a Skip — an infra error must never masquerade as inapplicable (#2122)", err)
			}
			if errorstest.ReportedCode(err) != tt.wantCode {
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
	if strings.Contains(err.Error(), "no autoscaler probe") {
		t.Errorf("checkPlatformAutoscaling() masqueraded infra error as missing-probe skip: %v", err)
	}
	if errorstest.ReportedCode(err) != errors.ErrCodeUnauthorized {
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

func TestCheckAKSAutoscaling(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []corev1.Node
		wantErr bool
	}{
		{
			name:    "no GPU nodes",
			nodes:   []corev1.Node{},
			wantErr: true,
		},
		{
			name: "GPU node with agent pool label",
			nodes: []corev1.Node{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "aks-gpu-node-1",
					Labels: map[string]string{
						"nvidia.com/gpu.present":           "true",
						"node.kubernetes.io/instance-type": "Standard_ND96isr_H100_v5",
						"kubernetes.azure.com/agentpool":   "gpuworker",
						"topology.kubernetes.io/zone":      "1",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/000/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu/virtualMachines/0"},
				Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				}},
			}},
			wantErr: false,
		},
		{
			name: "GPU node without agent pool label",
			nodes: []corev1.Node{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "aks-gpu-node-1",
					Labels: map[string]string{
						"nvidia.com/gpu.present": "true",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/000/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu/virtualMachines/0"},
				Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				}},
			}},
			wantErr: true,
		},
		{
			name: "mixed GPU nodes require every agent pool label",
			nodes: []corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "aks-gpu-labeled", Labels: map[string]string{
						"nvidia.com/gpu.present": "true", "kubernetes.azure.com/agentpool": "gpuworker",
					}},
					Status: corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "aks-gpu-unlabeled", Labels: map[string]string{
						"nvidia.com/gpu.present": "true",
					}},
					Status: corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			for i := range tt.nodes {
				_, err := client.CoreV1().Nodes().Create(
					context.Background(), &tt.nodes[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create node: %v", err)
				}
			}
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: client,
			}
			err := checkAKSAutoscaling(vctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkAKSAutoscaling() error = %v, wantErr %v", err, tt.wantErr)
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

// TestCheckPlatformAutoscalingSkipsUnknown pins the missing-probe outcome
// (#3428): a providerID no probe recognizes skips — never passes, never
// fails — and the reason names the scheme and says the skip is a missing
// probe, not product nonapplicability, which only configuration declares.
func TestCheckPlatformAutoscalingSkipsUnknown(t *testing.T) {
	client := k8sfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec:       corev1.NodeSpec{ProviderID: "nico://rack1/node-07"},
	})
	vctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: client,
	}
	var err error
	out := captureStdout(t, func() { err = checkPlatformAutoscaling(vctx) })
	if err == nil || !validators.IsSkip(err) {
		t.Fatalf("checkPlatformAutoscaling() = %v, want a skip for a provider without a probe", err)
	}
	for _, want := range []string{"no autoscaler probe for this provider", `"nico"`, "not product nonapplicability", "notApplicable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("skip reason %q should contain %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "not recognized") {
		t.Errorf("skip reason must not read as platform nonapplicability: %q", err.Error())
	}
	if !strings.Contains(out, "Cluster Autoscaling Observation") || !strings.Contains(out, "Provider scheme: nico") {
		t.Errorf("missing-probe observation artifact not recorded:\n%s", out)
	}
}

// TestPlatformPathsRecordObservationsNotPass pins the constrained claim
// (#3428): the EKS, AKS and GKE fallback paths record what they observed and
// state that no scale event was driven; none of them records a PASS verdict,
// and each records the autoscaler status ConfigMap and events observations.
func TestPlatformPathsRecordObservationsNotPass(t *testing.T) {
	gpuNode := func(name, providerID string, labels map[string]string) *corev1.Node {
		labels["nvidia.com/gpu.present"] = "true"
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Spec:       corev1.NodeSpec{ProviderID: providerID},
			Status:     corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
		}
	}
	statusCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler-status", Namespace: "kube-system"},
		Data:       map[string]string{"status": "Cluster-autoscaler status: Healthy"},
	}
	tests := []struct {
		name     string
		objects  []runtime.Object
		check    func(*validators.Context) error
		wantPool string
		wantAuto string
	}{
		{
			name:     "EKS node group with autoscaler status",
			objects:  []runtime.Object{gpuNode("eks-1", "aws://us-east-1a/i-1", map[string]string{"eks.amazonaws.com/nodegroup": "gpu-workers"}), statusCM},
			check:    checkEKSAutoscaling,
			wantPool: `managed node group "gpu-workers"`,
			wantAuto: "cluster-autoscaler-status ConfigMap present",
		},
		{
			name:     "AKS agent pool without autoscaler status",
			objects:  []runtime.Object{gpuNode("aks-1", "azure:///subscriptions/0/vmss/0", map[string]string{"kubernetes.azure.com/agentpool": "gpuworker"})},
			check:    checkAKSAutoscaling,
			wantPool: `agent pool(s) "gpuworker"`,
			wantAuto: "autoscaler enablement not observable",
		},
		{
			name:     "GKE node pool without autoscaler status",
			objects:  []runtime.Object{gpuNode("gke-1", "gce://proj/us-central1-a/gke-1", map[string]string{"cloud.google.com/gke-nodepool": "gpu-pool"})},
			check:    checkGKEAutoscaling,
			wantPool: "node pool(s) gpu-pool",
			wantAuto: "may not be enabled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(tt.objects...)}
			var err error
			out := captureStdout(t, func() { err = tt.check(vctx) })
			if err != nil {
				t.Fatalf("check error = %v", err)
			}
			if strings.Contains(out, "PASS") {
				t.Errorf("platform path must record observations, not a PASS verdict:\n%s", out)
			}
			for _, want := range []string{
				"Cluster Autoscaling Observation", "OBSERVED", "No scale event was driven or observed",
				"Cluster Autoscaler Status", "Autoscaler Events", tt.wantPool, tt.wantAuto,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
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
	// Should skip (missing probe) rather than fail with "not found"
	if err == nil || !validators.IsSkip(err) || !strings.Contains(err.Error(), "no autoscaler probe") {
		t.Errorf("expected missing-probe skip, got: %v", err)
	}
}

func TestCheckClusterAutoscaling_KarpenterNotFound_FallsBackToAKS(t *testing.T) {
	client := k8sfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "aks-gpu-node-1",
			Labels: map[string]string{
				"nvidia.com/gpu.present":         "true",
				"kubernetes.azure.com/agentpool": "gpuworker",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/000/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-gpu/virtualMachines/0"},
		Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
			"nvidia.com/gpu": resource.MustParse("8"),
		}},
	})
	vctx := &validators.Context{Ctx: context.Background(), Clientset: client}
	if err := CheckClusterAutoscaling(vctx); err != nil {
		t.Fatalf("CheckClusterAutoscaling() unexpected AKS fallback error: %v", err)
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
	if err == nil || strings.Contains(err.Error(), "no autoscaler probe") {
		t.Errorf("expected API error (not fallback skip), got: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to search for Karpenter deployment") {
		t.Errorf("expected wrapped API error, got: %v", err)
	}
}

// TestValidateClusterAutoscaling_NamespaceCreateFailure pins that the
// cluster-autoscaling check hands its namespace to the shared cleanup,
// registered before the create (#3648): a Create whose error may have
// persisted the namespace — before the error returned, or only after
// cleanup's first lookup answered NotFound — still ends with the namespace
// gone, a definitive rejection issues no delete, and a failing delete leaves
// the create error as the result. The cleanup status is recorded as the
// "Delete test namespace" artifact on this failed path. Sequential:
// shortenCleanupTimeout touches package state and captureStdout swaps
// os.Stdout.
func TestValidateClusterAutoscaling_NamespaceCreateFailure(t *testing.T) {
	shortenCleanupTimeout(t)
	for _, tt := range namespaceCreateFailureCases {
		t.Run(tt.name, func(t *testing.T) {
			c := k8sfake.NewClientset()
			createdName := failNamespaceCreate(c, tt.persists, tt.surfacesLate, tt.createErr)
			if tt.deleteErr != nil {
				failNamespaceDelete(c, tt.deleteErr)
			}

			var (
				report *clusterAutoscalingReport
				err    error
			)
			out := captureStdout(t, func() {
				report, err = validateClusterAutoscaling(&validators.Context{Ctx: context.Background(), Clientset: c}, "gpu-pool")
			})
			if report != nil {
				t.Errorf("report = %+v, want nil when the namespace create fails", report)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if err == nil || !strings.Contains(err.Error(), "failed to create cluster autoscaling test namespace") {
				t.Errorf("error = %v, want the namespace create failure as the result", err)
			}
			if !strings.Contains(out, "--- Delete test namespace ---") {
				t.Error("evidence missing the \"Delete test namespace\" artifact on the failed-create path")
			}
			if !strings.Contains(out, tt.wantStatus) {
				t.Errorf("cleanup artifact lacks status %q in output:\n%s", tt.wantStatus, out)
			}
			nsName := createdName()
			if nsName == "" {
				t.Fatal("the namespace create was never attempted")
			}
			if got := namespaceDeleteIssued(c.Actions(), nsName); got != tt.wantDeleted {
				t.Errorf("namespace delete issued = %t, want %t", got, tt.wantDeleted)
			}
			if (tt.persists || tt.surfacesLate) && tt.deleteErr == nil {
				if _, getErr := c.CoreV1().Namespaces().Get(context.Background(), nsName, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
					t.Errorf("namespace %s leaked after an ambiguous create (get err = %v)", nsName, getErr)
				}
			}
		})
	}
}

// TestValidateClusterAutoscaling_DoneContextIssuesNoCreate pins the fast
// path for a context that is already done before the namespace Create: the
// Create is never sent, so cleanup treats the namespace as never created
// (no Delete, no settle wait) and the artifact says so.
func TestValidateClusterAutoscaling_DoneContextIssuesNoCreate(t *testing.T) {
	shortenCleanupTimeout(t)
	c := k8sfake.NewClientset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error
	out := captureStdout(t, func() {
		_, err = validateClusterAutoscaling(&validators.Context{Ctx: ctx, Clientset: c}, "gpu-pool") //nolint:contextcheck // cleanup runs on its own bounded context
	})
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	for _, action := range c.Actions() {
		if action.GetResource().Resource == "namespaces" {
			t.Errorf("unexpected %s on namespaces with a done context", action.GetVerb())
		}
	}
	if !strings.Contains(out, "--- Delete test namespace ---") || !strings.Contains(out, "never created") {
		t.Errorf("cleanup artifact missing or not \"never created\" in output:\n%s", out)
	}
}

// TestCheckClusterAutoscaling_DoneContextTriesNoNodePool pins that the
// NodePool loop stops once the check's context is done: with two GPU
// NodePools and a canceled context no namespace Create is issued and the
// check reports the canceled budget instead of paying a cleanup budget per
// pool.
func TestCheckClusterAutoscaling_DoneContextTriesNoNodePool(t *testing.T) {
	client := k8sfake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "karpenter",
			Namespace: "karpenter",
			Labels:    map[string]string{"app.kubernetes.io/name": "karpenter"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "karpenter"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "karpenter"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	})
	npGVR := schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	nodePool := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "karpenter.sh/v1",
			"kind":       "NodePool",
			"metadata":   map[string]any{"name": name},
			"spec":       map[string]any{"limits": map[string]any{resourceNVIDIAGPU: "16"}},
		}}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{npGVR: "NodePoolList"}, nodePool("gpu-a"), nodePool("gpu-b"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error
	captureStdout(t, func() {
		err = CheckClusterAutoscaling(&validators.Context{Ctx: ctx, Clientset: client, DynamicClient: dyn}) //nolint:contextcheck // cleanup runs on its own bounded context
	})
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if err == nil || !strings.Contains(err.Error(), "before every GPU NodePool was tried") {
		t.Errorf("error = %v, want the exhausted-budget message", err)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "namespaces" {
			t.Errorf("unexpected %s on namespaces with a done context", action.GetVerb())
		}
	}
}

// passClusterAutoscalingFakes makes validateClusterAutoscaling succeed against
// the fake control plane: the HPA reports scaling intent on create, the
// first node list is the baseline and every later one shows a new Karpenter
// node, and the test namespace lists two Running pods.
func passClusterAutoscalingFakes(c *k8sfake.Clientset, poolName string) {
	c.PrependReactor("create", "horizontalpodautoscalers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		hpa := action.(k8stesting.CreateAction).GetObject().(*autoscalingv2.HorizontalPodAutoscaler)
		hpa.Status = autoscalingv2.HorizontalPodAutoscalerStatus{DesiredReplicas: 2, CurrentReplicas: 1}
		return false, nil, nil
	})
	nodeLists := 0
	c.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		nodeLists++
		if nodeLists == 1 {
			return false, nil, nil // baseline: no pool nodes yet
		}
		return true, &corev1.NodeList{Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{
			Name: "kwok-gpu-0", Labels: map[string]string{karpenterNodePoolLabel: poolName}}}}}, nil
	})
	c.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.GetNamespace()
		running := func(name string) corev1.Pod {
			return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		}
		return true, &corev1.PodList{Items: []corev1.Pod{running("w-0"), running("w-1")}}, nil
	})
}

// TestValidateClusterAutoscaling_CleanupFailureFailsPassingRun pins the
// named-return contract on the SUCCESS path: a run whose behavioral test
// passed must still fail when its namespace cleanup terminally fails
// (every Delete rejected), otherwise the check reports PASS while the
// namespace, the workload, and its GPU request remain. The passing control
// row proves the fakes reach success. Sequential: shortenCleanupTimeout
// touches package state and captureStdout swaps os.Stdout.
func TestValidateClusterAutoscaling_CleanupFailureFailsPassingRun(t *testing.T) {
	shortenCleanupTimeout(t)
	tests := []struct {
		name       string
		deleteErr  error
		wantStatus string
	}{
		{name: "cleanup succeeds, run passes", wantStatus: "confirmed absent"},
		{
			name: "cleanup terminally fails, run fails with the cleanup error",
			deleteErr: k8serrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "",
				stderrors.New("namespaces is forbidden")),
			wantStatus: "CLEANUP FAILED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := k8sfake.NewClientset()
			passClusterAutoscalingFakes(c, "gpu-pool")
			if tt.deleteErr != nil {
				failNamespaceDelete(c, tt.deleteErr)
			}

			var (
				report *clusterAutoscalingReport
				err    error
			)
			out := captureStdout(t, func() {
				report, err = validateClusterAutoscaling(&validators.Context{Ctx: context.Background(), Clientset: c}, "gpu-pool")
			})
			if tt.deleteErr == nil {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				if report == nil || report.HPADesiredReplicas != 2 || report.ObservedNodeCount != 1 || report.ScheduledPodCount != 2 {
					t.Errorf("report = %+v, want desired=2 nodes=1 scheduled=2", report)
				}
				if strings.Contains(out, "--- Delete test namespace ---") {
					t.Error("the success path must leave the cleanup artifact to the caller")
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if err == nil || !strings.Contains(err.Error(), "CLEANUP FAILED") {
				t.Errorf("error = %v, want the cleanup failure to fail the passing run", err)
			}
			if !strings.Contains(out, "--- Delete test namespace ---") || !strings.Contains(out, tt.wantStatus) {
				t.Errorf("cleanup artifact missing or lacks %q in output:\n%s", tt.wantStatus, out)
			}
			if report == nil {
				t.Error("report = nil, want the behavioral result preserved beside the cleanup error")
			}
		})
	}
}

// TestRecordClusterAutoscalerStatus_ReadErrorIsNotAbsence pins that a failed
// ConfigMap read (RBAC here) is recorded with the absence rather than read
// as "not found", while a clean NotFound records a plain absence: the
// artifact must not claim an absence it did not observe.
func TestRecordClusterAutoscalerStatus_ReadErrorIsNotAbsence(t *testing.T) {
	tests := []struct {
		name       string
		getErr     error
		wantUnread bool
	}{
		{"RBAC-denied read is recorded as unread", k8serrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "cluster-autoscaler-status", fmt.Errorf("rbac denied")), true},
		{"clean NotFound is a plain absence", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset()
			if tt.getErr != nil {
				client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}
			vctx := &validators.Context{Ctx: context.Background(), Clientset: client}
			var found bool
			var unread string
			out := captureStdout(t, func() { found, unread = recordClusterAutoscalerStatus(vctx) })
			if found {
				t.Fatal("recordClusterAutoscalerStatus() = true without a ConfigMap")
			}
			if (unread != "") != tt.wantUnread || strings.Contains(out, "absence is NOT established") != tt.wantUnread {
				t.Errorf("unread=%q wantUnread=%v; artifact:\n%s", unread, tt.wantUnread, out)
			}
			if tt.wantUnread && !strings.Contains(out, "rbac denied") {
				t.Errorf("artifact must carry the read error:\n%s", out)
			}
		})
	}
}

// TestRecordClusterAutoscalerDeployment_UnavailableIsNotAbsence pins that a
// Cluster Autoscaler Deployment that exists with no available replica is
// recorded as present-but-unavailable, not as absent.
func TestRecordClusterAutoscalerDeployment_UnavailableIsNotAbsence(t *testing.T) {
	client := k8sfake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler", Namespace: "kube-system"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptrInt32(1)},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 0},
	})
	vctx := &validators.Context{Ctx: context.Background(), Clientset: client}
	var found bool
	var unread string
	out := captureStdout(t, func() { found, unread = recordClusterAutoscalerDeployment(vctx) })
	if found {
		t.Fatal("recordClusterAutoscalerDeployment() = true for an unavailable Deployment")
	}
	if unread == "" || !strings.Contains(out, "exists but is not available") {
		t.Errorf("artifact must record the unavailable Deployment (unread=%q):\n%s", unread, out)
	}
}

// TestPlatformSummariesDoNotTurnUnreadIntoAbsence pins the summary line of
// each platform path when the status ConfigMap read fails: it must say the
// ConfigMap could not be read, never that it is absent or the autoscaler
// disabled, and the check still records an observation (nil).
func TestPlatformSummariesDoNotTurnUnreadIntoAbsence(t *testing.T) {
	gpuNode := func(providerID string, labels map[string]string) *corev1.Node {
		labels["nvidia.com/gpu.present"] = "true"
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: labels},
			Spec:       corev1.NodeSpec{ProviderID: providerID},
			Status:     corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
		}
	}
	tests := []struct {
		name      string
		node      *corev1.Node
		check     func(*validators.Context) error
		mustNotBe []string
	}{
		{"EKS", gpuNode("aws://us-east-1a/i-1", map[string]string{"eks.amazonaws.com/nodegroup": "gpu-workers"}), checkEKSAutoscaling,
			[]string{"no cluster-autoscaler-status ConfigMap", "no in-cluster Cluster Autoscaler Deployment;"}},
		{"AKS", gpuNode("azure:///subscriptions/0/vmss/0", map[string]string{"kubernetes.azure.com/agentpool": "gpuworker"}), checkAKSAutoscaling,
			[]string{"no cluster-autoscaler-status ConfigMap", "enablement not observable"}},
		{"GKE", gpuNode("gce://proj/us-central1-a/gke-1", map[string]string{"cloud.google.com/gke-nodepool": "gpu-pool"}), checkGKEAutoscaling,
			[]string{"no cluster-autoscaler-status ConfigMap", "may not be enabled"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(tt.node)
			denied := func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "x"}, "", fmt.Errorf("rbac denied"))
			}
			client.PrependReactor("get", "configmaps", denied)
			client.PrependReactor("get", "deployments", denied)
			vctx := &validators.Context{Ctx: context.Background(), Clientset: client}
			var err error
			out := captureStdout(t, func() { err = tt.check(vctx) })
			if err != nil {
				t.Fatalf("check error = %v", err)
			}
			summary := out[strings.LastIndex(out, "OBSERVED"):]
			if !strings.Contains(summary, "cluster-autoscaler-status ConfigMap could not be read") {
				t.Errorf("summary must say the ConfigMap could not be read:\n%s", summary)
			}
			for _, bad := range tt.mustNotBe {
				if strings.Contains(summary, bad) {
					t.Errorf("summary turns an unread ConfigMap into absence (%q):\n%s", bad, summary)
				}
			}
			if tt.name != "GKE" && !strings.Contains(summary, "Cluster Autoscaler Deployment could not be confirmed") {
				t.Errorf("summary must say the Deployment could not be confirmed:\n%s", summary)
			}
		})
	}
}

// TestCheckEKSAutoscaling_NoAutoscalerIsAnObservation pins that an EKS
// cluster with self-managed node groups (no eks.amazonaws.com/nodegroup
// label) and no in-cluster Cluster Autoscaler records "no autoscaler
// observed" as an observation rather than failing — but only when every
// observation was a definitive absence: a present status ConfigMap is an
// autoscaler observation, and a failed ConfigMap read is unread, not absent.
func TestCheckEKSAutoscaling_NoAutoscalerIsAnObservation(t *testing.T) {
	const noAutoscaler = "no autoscaler observed: no managed node group label, no in-cluster Cluster Autoscaler, no Karpenter"
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: map[string]string{"nvidia.com/gpu.present": "true"}},
		Spec:       corev1.NodeSpec{ProviderID: "aws://us-east-1a/i-1"},
		Status:     corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}},
	}
	statusCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler-status", Namespace: "kube-system"},
		Data:       map[string]string{"status": "Cluster-autoscaler status: Healthy"},
	}
	tests := []struct {
		name           string
		objects        []runtime.Object
		statusReadErr  error
		wantNoAuto     bool
		wantInSummary  string
		wantNotSummary string
	}{
		{"nothing observed at all", []runtime.Object{node}, nil, true, "", ""},
		{"status ConfigMap present, no Deployment", []runtime.Object{node, statusCM}, nil, false, "cluster-autoscaler-status ConfigMap present", ""},
		{"status ConfigMap unread", []runtime.Object{node}, k8serrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", fmt.Errorf("rbac denied")), false, "cluster-autoscaler-status ConfigMap could not be read", "no cluster-autoscaler-status ConfigMap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := k8sfake.NewClientset(tt.objects...)
			if tt.statusReadErr != nil {
				client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.statusReadErr
				})
			}
			vctx := &validators.Context{Ctx: context.Background(), Clientset: client}
			var err error
			out := captureStdout(t, func() { err = checkEKSAutoscaling(vctx) })
			if err != nil {
				t.Fatalf("checkEKSAutoscaling() = %v, want an observation (nil)", err)
			}
			summary := out[strings.LastIndex(out, "OBSERVED"):]
			if strings.Contains(summary, noAutoscaler) != tt.wantNoAuto {
				t.Errorf("no-autoscaler observation present=%v, want %v:\n%s", !tt.wantNoAuto, tt.wantNoAuto, summary)
			}
			if tt.wantInSummary != "" && !strings.Contains(summary, tt.wantInSummary) {
				t.Errorf("summary missing %q:\n%s", tt.wantInSummary, summary)
			}
			if tt.wantNotSummary != "" && strings.Contains(summary, tt.wantNotSummary) {
				t.Errorf("summary turns an unread ConfigMap into absence (%q):\n%s", tt.wantNotSummary, summary)
			}
			if strings.Contains(out, "PASS") {
				t.Errorf("observation must not read as a pass:\n%s", out)
			}
		})
	}
}

func ptrInt32(v int32) *int32 { return &v }

// TestRecordAutoscalerEvents_NewestFirst pins that the artifact selects the
// newest autoscaler events by timestamp (lastTimestamp, else eventTime)
// rather than by list position, and prints them in UTC.
func TestRecordAutoscalerEvents_NewestFirst(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	objects := make([]runtime.Object, 0, defaults.AutoscalerMaxEvents+6)
	for i := range defaults.AutoscalerMaxEvents + 3 {
		objects = append(objects, &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("ev-%02d", i), Namespace: "default"},
			Reason:         "TriggeredScaleUp",
			Message:        fmt.Sprintf("scale-up %d", i),
			InvolvedObject: corev1.ObjectReference{Name: fmt.Sprintf("pod-%d", i)},
			LastTimestamp:  metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
		})
	}
	// An events.k8s.io-style event with only eventTime, newer than all others.
	objects = append(objects, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "aa-eventtime", Namespace: "default"},
		Reason:         "ScaleDown",
		Message:        "newest by eventTime",
		InvolvedObject: corev1.ObjectReference{Name: "node-9"},
		EventTime:      metav1.NewMicroTime(base.Add(time.Hour)),
	}, &corev1.Event{
		// A repeated series event: its first observation (eventTime) is older
		// than everything, its last occurrence is second newest.
		ObjectMeta:     metav1.ObjectMeta{Name: "ab-series", Namespace: "default"},
		Reason:         "NotTriggerScaleUp",
		Message:        "repeated series",
		InvolvedObject: corev1.ObjectReference{Name: "pod-series"},
		EventTime:      metav1.NewMicroTime(base.Add(-time.Hour)),
		Series:         &corev1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(base.Add(50 * time.Minute))},
	}, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "zz-unrelated", Namespace: "default"},
		Reason:     "Scheduled", Message: "ignored",
	})
	vctx := &validators.Context{Ctx: context.Background(), Clientset: k8sfake.NewClientset(objects...)}
	out := captureStdout(t, func() { recordAutoscalerEvents(vctx) })
	if strings.Contains(out, "scale-up 0\n") || strings.Contains(out, "scale-up 1\n") || strings.Contains(out, "scale-up 2\n") {
		t.Errorf("oldest events must be dropped when more than %d match:\n%s", defaults.AutoscalerMaxEvents, out)
	}
	if strings.Contains(out, "0001-01-01") || strings.Contains(out, "ignored") {
		t.Errorf("zero timestamps and unrelated events must not appear:\n%s", out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	first := lines[len(lines)-defaults.AutoscalerMaxEvents]
	if !strings.Contains(first, "newest by eventTime") || !strings.HasPrefix(first, base.Add(time.Hour).Format(time.RFC3339)) {
		t.Errorf("newest event must be first and printed in UTC, got %q", first)
	}
	second := lines[len(lines)-defaults.AutoscalerMaxEvents+1]
	if !strings.Contains(second, "repeated series") || !strings.HasPrefix(second, base.Add(50*time.Minute).Format(time.RFC3339)) {
		t.Errorf("a series event must sort by its last occurrence, got %q", second)
	}
}
