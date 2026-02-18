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
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidateCheckNvidiaSmi(t *testing.T) {
	// This test requires a real Kubernetes cluster with GPU nodes.
	// The fake clientset does not simulate pod lifecycle (pods never transition
	// to Succeeded phase) and cannot provide real GPU log output, so this
	// validation must run as an integration/e2e test.
	t.Skip("requires a real Kubernetes cluster with GPU nodes; run via e2e tests")
}

func TestValidateCheckNvidiaSmiNoGpuNodes(t *testing.T) {
	// Verify that validateCheckNvidiaSmi skips gracefully when no GPU nodes exist
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Namespace: "default",
		Clientset: fake.NewSimpleClientset(),
		RecipeData: map[string]interface{}{
			"accelerator": recipe.CriteriaAcceleratorH100,
		},
	}

	err := validateCheckNvidiaSmi(ctx, t)
	if err != nil {
		t.Errorf("validateCheckNvidiaSmi() with no GPU nodes should skip, got error: %v", err)
	}
}

func TestGetLogSnippet(t *testing.T) {
	tests := []struct {
		name     string
		logs     string
		maxLines int
		expected string
	}{
		{
			name:     "fewer lines than max",
			logs:     "line1\nline2\nline3",
			maxLines: 5,
			expected: "line1\nline2\nline3",
		},
		{
			name:     "exact number of lines",
			logs:     "line1\nline2\nline3",
			maxLines: 3,
			expected: "line1\nline2\nline3",
		},
		{
			name:     "more lines than max",
			logs:     "line1\nline2\nline3\nline4\nline5",
			maxLines: 3,
			expected: "line1\nline2\nline3",
		},
		{
			name:     "empty logs",
			logs:     "",
			maxLines: 5,
			expected: "",
		},
		{
			name:     "single line",
			logs:     "only line",
			maxLines: 1,
			expected: "only line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getLogSnippet(tt.logs, tt.maxLines)
			if result != tt.expected {
				t.Errorf("getLogSnippet() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestVerifyLogContent(t *testing.T) {
	t.Run("all markers present", func(t *testing.T) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod",
				Namespace: "default",
			},
		}
		logs := "NVIDIA-SMI 550.00\nDriver Version: 550.00\nCUDA Version: 12.4\nGPU_CHECK_SUCCESS"
		err := verifyLogContent(t, logs, pod)
		if err != nil {
			t.Errorf("verifyLogContent() with all markers should pass, got error: %v", err)
		}
	})

	// Note: Error cases (missing markers) are not tested here because
	// verifyLogContent calls t.Errorf internally, which would mark the
	// test as failed even when we expect the error path.
}

func TestReportResults(t *testing.T) {
	tests := []struct {
		name       string
		results    map[string]error
		totalNodes int
		wantErr    bool
		errContain string
	}{
		{
			name:       "all nodes successful",
			results:    map[string]error{"node1": nil, "node2": nil},
			totalNodes: 2,
			wantErr:    false,
		},
		{
			name:       "one node failed",
			results:    map[string]error{"node1": nil, "node2": fmt.Errorf("gpu error")},
			totalNodes: 2,
			wantErr:    true,
			errContain: "GPU verification failed on 1/2 nodes",
		},
		{
			name:       "all nodes failed",
			results:    map[string]error{"node1": fmt.Errorf("error1"), "node2": fmt.Errorf("error2")},
			totalNodes: 2,
			wantErr:    true,
			errContain: "GPU verification failed on 2/2 nodes",
		},
		{
			name:       "count mismatch",
			results:    map[string]error{"node1": nil},
			totalNodes: 2,
			wantErr:    true,
			errContain: "verification count mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := reportResults(t, tt.results, tt.totalNodes)
			if (err != nil) != tt.wantErr {
				t.Errorf("reportResults() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && tt.errContain != "" && !strings.Contains(err.Error(), tt.errContain) {
				t.Errorf("reportResults() error = %q, want containing %q", err.Error(), tt.errContain)
			}
		})
	}
}

func TestGetGpuVerifyImage(t *testing.T) {
	tests := []struct {
		name     string
		ctx      *checks.ValidationContext
		expected string
	}{
		{
			name: "GB200 accelerator returns ubuntu24 image",
			ctx: &checks.ValidationContext{
				RecipeData: map[string]interface{}{
					"accelerator": recipe.CriteriaAcceleratorGB200,
				},
			},
			expected: "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu24.04",
		},
		{
			name: "H100 accelerator returns default image",
			ctx: &checks.ValidationContext{
				RecipeData: map[string]interface{}{
					"accelerator": recipe.CriteriaAcceleratorH100,
				},
			},
			expected: "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04",
		},
		{
			name: "A100 accelerator returns default image",
			ctx: &checks.ValidationContext{
				RecipeData: map[string]interface{}{
					"accelerator": recipe.CriteriaAcceleratorA100,
				},
			},
			expected: "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04",
		},
		{
			name: "nil RecipeData returns default image",
			ctx: &checks.ValidationContext{
				RecipeData: nil,
			},
			expected: "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04",
		},
		{
			name: "wrong type in accelerator returns default image",
			ctx: &checks.ValidationContext{
				RecipeData: map[string]interface{}{
					"accelerator": "not-a-type",
				},
			},
			expected: "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getGpuVerifyImage(tt.ctx)
			if result != tt.expected {
				t.Errorf("getGpuVerifyImage() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFindBusyNodes(t *testing.T) {
	tests := []struct {
		name     string
		nodes    []v1.Node
		pods     []v1.Pod
		wantBusy int
	}{
		{
			name: "no busy nodes",
			nodes: []v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			pods:     nil,
			wantBusy: 0,
		},
		{
			name: "node with GPU pod is busy",
			nodes: []v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-pod", Namespace: "default"},
					Spec: v1.PodSpec{
						NodeName: "node1",
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Limits: v1.ResourceList{
										v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
									},
								},
							},
						},
					},
					Status: v1.PodStatus{Phase: v1.PodRunning},
				},
			},
			wantBusy: 1,
		},
		{
			name: "completed GPU pod does not make node busy",
			nodes: []v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			pods: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "gpu-pod", Namespace: "default"},
					Spec: v1.PodSpec{
						NodeName: "node1",
						Containers: []v1.Container{
							{
								Resources: v1.ResourceRequirements{
									Limits: v1.ResourceList{
										v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
									},
								},
							},
						},
					},
					Status: v1.PodStatus{Phase: v1.PodSucceeded},
				},
			},
			wantBusy: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset()
			ctx := context.Background()
			for i := range tt.pods {
				_, err := clientset.CoreV1().Pods(tt.pods[i].Namespace).Create(ctx, &tt.pods[i], metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create pod: %v", err)
				}
			}

			valCtx := &checks.ValidationContext{
				Context:   ctx,
				Namespace: "default",
				Clientset: clientset,
			}

			busyNodes := findBusyNodes(valCtx, t, tt.nodes)
			if len(busyNodes) != tt.wantBusy {
				t.Errorf("findBusyNodes() returned %d busy nodes, want %d: %v", len(busyNodes), tt.wantBusy, busyNodes)
			}
		})
	}
}

func TestValidateCheckNvidiaSmiRegistration(t *testing.T) {
	// Verify the check is registered
	check, ok := checks.GetCheck("check-nvidia-smi")
	if !ok {
		t.Fatal("check-nvidia-smi check not registered")
	}

	if check.Name != "check-nvidia-smi" {
		t.Errorf("Name = %v, want check-nvidia-smi", check.Name)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.TestName == "" {
		t.Error("TestName is empty")
	}
}
