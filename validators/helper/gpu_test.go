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

package helper

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// gpuTestNode builds a node with `gpu` allocatable nvidia.com/gpu (gpu < 0
// omits the resource entirely, producing a non-GPU node). unschedulable
// marks the node cordoned.
func gpuTestNode(name string, gpu int64, unschedulable bool) *v1.Node {
	alloc := v1.ResourceList{}
	if gpu >= 0 {
		alloc[v1.ResourceName(GpuResourceName)] = *resource.NewQuantity(gpu, resource.DecimalSI)
	}
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1.NodeSpec{Unschedulable: unschedulable},
		Status:     v1.NodeStatus{Allocatable: alloc},
	}
}

func TestLoadPodFromTemplate(t *testing.T) {
	validTemplate := `apiVersion: v1
kind: Pod
metadata:
  name: ${POD_NAME}
  namespace: ${NAMESPACE}
spec:
  containers:
  - name: worker
    image: ${IMAGE}
    command: ["echo", "hello"]
`

	tests := []struct {
		name        string
		template    string
		data        map[string]string
		wantPodName string
		wantNS      string
		wantImage   string
		wantErr     bool
	}{
		{
			name:     "valid template with substitutions",
			template: validTemplate,
			data: map[string]string{
				"POD_NAME":  "test-pod",
				"NAMESPACE": "test-ns",
				"IMAGE":     "nvidia/cuda:12.0",
			},
			wantPodName: "test-pod",
			wantNS:      "test-ns",
			wantImage:   "nvidia/cuda:12.0",
		},
		{
			name:     "no substitution data leaves placeholders",
			template: validTemplate,
			data:     map[string]string{},
			// Placeholders remain as literal strings in the YAML.
			wantPodName: "${POD_NAME}",
			wantNS:      "${NAMESPACE}",
			wantImage:   "${IMAGE}",
		},
		{
			name:        "nil data map",
			template:    validTemplate,
			data:        nil,
			wantPodName: "${POD_NAME}",
			wantNS:      "${NAMESPACE}",
			wantImage:   "${IMAGE}",
		},
		{
			name:     "invalid YAML",
			template: "not: [valid: yaml: {",
			data:     map[string]string{},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "pod.yaml")
			if err := os.WriteFile(tmpFile, []byte(tt.template), 0600); err != nil {
				t.Fatalf("failed to write temp file: %v", err)
			}

			pod, err := LoadPodFromTemplate(tmpFile, tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadPodFromTemplate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if pod.Name != tt.wantPodName {
				t.Errorf("pod.Name = %q, want %q", pod.Name, tt.wantPodName)
			}
			if pod.Namespace != tt.wantNS {
				t.Errorf("pod.Namespace = %q, want %q", pod.Namespace, tt.wantNS)
			}
			if len(pod.Spec.Containers) == 0 {
				t.Fatal("pod has no containers")
			}
			if pod.Spec.Containers[0].Image != tt.wantImage {
				t.Errorf("container image = %q, want %q", pod.Spec.Containers[0].Image, tt.wantImage)
			}
		})
	}
}

func TestLoadPodFromTemplateFileNotFound(t *testing.T) {
	_, err := LoadPodFromTemplate("/nonexistent/path/pod.yaml", nil)
	if err == nil {
		t.Error("LoadPodFromTemplate() expected error for missing file, got nil")
	}
}

func TestCheckPodRunningOrTerminal(t *testing.T) {
	tests := []struct {
		name     string
		phase    v1.PodPhase
		wantDone bool
		wantErr  bool
	}{
		{"running", v1.PodRunning, true, false},
		{"succeeded", v1.PodSucceeded, true, false},
		{"failed", v1.PodFailed, true, true},
		{"pending", v1.PodPending, false, false},
		{"unknown", v1.PodPhase("Unknown"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{Status: v1.PodStatus{Phase: tt.phase}}
			done, err := checkPodRunningOrTerminal(pod)
			if done != tt.wantDone {
				t.Errorf("checkPodRunningOrTerminal() done = %v, want %v", done, tt.wantDone)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("checkPodRunningOrTerminal() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestFindGpuNodes proves FindGpuNodes reports every GPU node — schedulable
// and cordoned alike — tagged with its cordon state, so callers that must
// disclose cordoned nodes (rather than silently drop them, issue #1668) have
// the full set to work from.
func TestFindGpuNodes(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewSimpleClientset(
		gpuTestNode("schedulable-1", 8, false),
		gpuTestNode("cordoned-1", 8, true),
		gpuTestNode("non-gpu", -1, false),
		gpuTestNode("zero-gpu", 0, false),
	)

	got, err := FindGpuNodes(context.Background(), clientset)
	if err != nil {
		t.Fatalf("FindGpuNodes() error = %v", err)
	}

	// want tracks expected presence; non-gpu (resource absent) and zero-gpu
	// (allocatable quantity 0, not IsZero()-excluded) must both be absent.
	want := map[string]bool{"schedulable-1": false, "cordoned-1": true}
	seen := make(map[string]bool, len(got))
	for _, n := range got {
		// A duplicate result could mask a missing expected node if only
		// checked by membership, so duplicates are caught explicitly here
		// rather than relying on a bare length comparison.
		if seen[n.Node.Name] {
			t.Fatalf("FindGpuNodes() returned duplicate node %q", n.Node.Name)
		}
		seen[n.Node.Name] = true

		wantCordoned, ok := want[n.Node.Name]
		if !ok {
			t.Errorf("FindGpuNodes() returned unexpected node %q", n.Node.Name)
			continue
		}
		if n.Cordoned != wantCordoned {
			t.Errorf("FindGpuNodes() node %q Cordoned = %v, want %v", n.Node.Name, n.Cordoned, wantCordoned)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("FindGpuNodes() missing expected node %q", name)
		}
	}
}

// TestFindSchedulableGpuNodes proves the schedulable-only view still excludes
// cordoned GPU nodes, preserving existing callers' behavior after
// FindSchedulableGpuNodes was rebuilt on top of FindGpuNodes.
func TestFindSchedulableGpuNodes(t *testing.T) {
	t.Parallel()

	clientset := k8sfake.NewSimpleClientset(
		gpuTestNode("schedulable-1", 8, false),
		gpuTestNode("schedulable-2", 8, false),
		gpuTestNode("cordoned-1", 8, true),
	)

	got, err := FindSchedulableGpuNodes(context.Background(), clientset)
	if err != nil {
		t.Fatalf("FindSchedulableGpuNodes() error = %v", err)
	}

	want := map[string]bool{"schedulable-1": true, "schedulable-2": true}
	seen := make(map[string]bool, len(got))
	for _, n := range got {
		if seen[n.Name] {
			t.Fatalf("FindSchedulableGpuNodes() returned duplicate node %q", n.Name)
		}
		seen[n.Name] = true

		if n.Name == "cordoned-1" {
			t.Errorf("FindSchedulableGpuNodes() must not include cordoned node %q", n.Name)
		} else if !want[n.Name] {
			t.Errorf("FindSchedulableGpuNodes() returned unexpected node %q", n.Name)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("FindSchedulableGpuNodes() missing expected node %q", name)
		}
	}
}
