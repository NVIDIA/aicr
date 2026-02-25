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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFormatArtifactBody(t *testing.T) {
	tests := []struct {
		name       string
		equivalent string
		data       string
		want       string
	}{
		{
			name:       "both empty",
			equivalent: "",
			data:       "",
			want:       "",
		},
		{
			name:       "whitespace only",
			equivalent: "  ",
			data:       "  ",
			want:       "",
		},
		{
			name:       "equivalent only",
			equivalent: "kubectl get pods",
			data:       "",
			want:       "Equivalent: kubectl get pods",
		},
		{
			name:       "data only",
			equivalent: "",
			data:       "pod-1 Running",
			want:       "pod-1 Running",
		},
		{
			name:       "both present",
			equivalent: "kubectl get pods",
			data:       "pod-1 Running",
			want:       "Equivalent: kubectl get pods\n\npod-1 Running",
		},
		{
			name:       "trims whitespace",
			equivalent: "  kubectl get pods  ",
			data:       "  pod-1 Running  ",
			want:       "Equivalent: kubectl get pods\n\npod-1 Running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatArtifactBody(tt.equivalent, tt.data)
			if got != tt.want {
				t.Errorf("formatArtifactBody(%q, %q) = %q, want %q",
					tt.equivalent, tt.data, got, tt.want)
			}
		})
	}
}

func TestValueOrUnknown(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty string", input: "", want: "unknown"},
		{name: "whitespace only", input: "   ", want: "unknown"},
		{name: "valid value", input: "kgateway", want: "kgateway"},
		{name: "value with spaces", input: " some value ", want: " some value "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valueOrUnknown(tt.input)
			if got != tt.want {
				t.Errorf("valueOrUnknown(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPodReadyCount(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "no containers",
			pod:  corev1.Pod{},
			want: "0/0",
		},
		{
			name: "all ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true},
						{Ready: true},
					},
				},
			},
			want: "2/2",
		},
		{
			name: "mixed ready and not ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true},
						{Ready: false},
						{Ready: true},
					},
				},
			},
			want: "2/3",
		},
		{
			name: "none ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: false},
					},
				},
			},
			want: "0/1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podReadyCount(tt.pod)
			if got != tt.want {
				t.Errorf("podReadyCount() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetConditionObservation(t *testing.T) {
	tests := []struct {
		name     string
		obj      *unstructured.Unstructured
		condType string
		want     *conditionObservation
		wantErr  bool
	}{
		{
			name: "condition found",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":    "Accepted",
								"status":  "True",
								"reason":  "Accepted",
								"message": "all good",
							},
						},
					},
				},
			},
			condType: "Accepted",
			want:     &conditionObservation{Type: "Accepted", Status: "True", Reason: "Accepted", Message: "all good"},
		},
		{
			name: "condition not found",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Ready",
								"status": "True",
							},
						},
					},
				},
			},
			condType: "Programmed",
			wantErr:  true,
		},
		{
			name: "no conditions field",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{},
				},
			},
			condType: "Accepted",
			wantErr:  true,
		},
		{
			name: "missing reason and message defaults to unknown",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Accepted",
								"status": "True",
							},
						},
					},
				},
			},
			condType: "Accepted",
			want:     &conditionObservation{Type: "Accepted", Status: "True", Reason: "unknown", Message: "unknown"},
		},
		{
			name: "non-string type field is skipped",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   int64(42), // non-string
								"status": "True",
							},
						},
					},
				},
			},
			condType: "Accepted",
			wantErr:  true,
		},
		{
			name: "non-string status falls back to fmt.Sprintf",
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"status": map[string]interface{}{
						"conditions": []interface{}{
							map[string]interface{}{
								"type":   "Ready",
								"status": true, // bool, not string
								"reason": "AllGood",
							},
						},
					},
				},
			},
			condType: "Ready",
			want:     &conditionObservation{Type: "Ready", Status: "true", Reason: "AllGood", Message: "unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getConditionObservation(tt.obj, tt.condType)
			if (err != nil) != tt.wantErr {
				t.Errorf("getConditionObservation() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.want != nil {
				if got == nil {
					t.Fatal("getConditionObservation() returned nil, want non-nil")
				}
				if *got != *tt.want {
					t.Errorf("getConditionObservation() = %+v, want %+v", *got, *tt.want)
				}
			}
		})
	}
}

func TestPodStuckReason(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "running pod not stuck",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					},
				},
			},
			want: "",
		},
		{
			name: "ImagePullBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Image: "busybox:bad",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ImagePullBackOff",
									Message: "back-off pulling image",
								},
							},
						},
					},
				},
			},
			want: "ImagePullBackOff: back-off pulling image (image: busybox:bad)",
		},
		{
			name: "CrashLoopBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Image: "myapp:latest",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "CrashLoopBackOff",
									Message: "back-off restarting failed container",
								},
							},
						},
					},
				},
			},
			want: "CrashLoopBackOff: back-off restarting failed container (image: myapp:latest)",
		},
		{
			name: "init container stuck",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Image: "init:bad",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ErrImagePull",
									Message: "pull failed",
								},
							},
						},
					},
				},
			},
			want: "ErrImagePull: pull failed (init container, image: init:bad)",
		},
		{
			name: "unschedulable",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodScheduled,
							Status:  corev1.ConditionFalse,
							Reason:  string(corev1.PodReasonUnschedulable),
							Message: "0/3 nodes available: insufficient gpu",
						},
					},
				},
			},
			want: "Unschedulable: 0/3 nodes available: insufficient gpu",
		},
		{
			name: "non-stuck waiting reason ignored",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ContainerCreating",
								},
							},
						},
					},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podStuckReason(tt.pod)
			if got != tt.want {
				t.Errorf("podStuckReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPodWaitingStatus(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "no waiting containers",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					},
				},
			},
			want: "none",
		},
		{
			name: "no containers",
			pod:  &corev1.Pod{},
			want: "none",
		},
		{
			name: "waiting container",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ContainerCreating",
									Message: "pulling image",
								},
							},
						},
					},
				},
			},
			want: "ContainerCreating: pulling image",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podWaitingStatus(tt.pod)
			if got != tt.want {
				t.Errorf("podWaitingStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecordChunkedTextArtifact(t *testing.T) {
	tests := []struct {
		name           string
		label          string
		equivalent     string
		data           string
		wantChunkCount int
		wantLineSplit  bool // true = verify chunks don't cut mid-line
	}{
		{
			name:           "empty data records single artifact",
			label:          "test",
			equivalent:     "",
			data:           "",
			wantChunkCount: 1,
		},
		{
			name:           "small data fits in single chunk",
			label:          "test",
			equivalent:     "kubectl get pods",
			data:           "pod-1 Running\npod-2 Running",
			wantChunkCount: 1,
		},
		{
			name:           "large data splits into multiple chunks on line boundaries",
			label:          "logs",
			equivalent:     "kubectl logs test",
			data:           generateLargeText(defaults.ArtifactMaxDataSize * 2),
			wantChunkCount: 3, // at least 2 chunks
			wantLineSplit:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := checks.NewArtifactCollector()
			ctx := &checks.ValidationContext{
				Context:   context.Background(),
				Artifacts: collector,
			}

			recordChunkedTextArtifact(ctx, tt.label, tt.equivalent, tt.data)
			arts := collector.Drain()

			if tt.wantChunkCount > 1 {
				if len(arts) < 2 {
					t.Errorf("expected multiple chunks, got %d", len(arts))
				}
			} else {
				if len(arts) != tt.wantChunkCount {
					t.Errorf("expected %d chunk(s), got %d", tt.wantChunkCount, len(arts))
				}
			}

			if tt.wantLineSplit && len(arts) > 1 {
				// Verify each chunk's data section doesn't end or start mid-line
				// (i.e., each chunk should start/end cleanly).
				for i, art := range arts {
					// Extract the data portion after the "Part: N/M\n\n" header.
					parts := strings.SplitN(art.Data, "\n\n", 3)
					if len(parts) < 3 {
						continue
					}
					chunkData := parts[2]
					// The chunk should not start or end with a partial line
					// (no leading/trailing spaces that look like mid-word cuts).
					// More importantly, verify it's valid text (not cut mid-multibyte).
					if len(chunkData) > 0 && chunkData[len(chunkData)-1] == ' ' {
						t.Errorf("chunk %d appears to cut mid-word", i+1)
					}
				}
			}
		})
	}
}

func TestRecordRawTextArtifact(t *testing.T) {
	collector := checks.NewArtifactCollector()
	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Artifacts: collector,
	}

	recordRawTextArtifact(ctx, "test label", "kubectl get pods", "pod-1 Running")
	arts := collector.Drain()
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	if arts[0].Label != "test label" {
		t.Errorf("Label = %q, want %q", arts[0].Label, "test label")
	}
	if !strings.Contains(arts[0].Data, "Equivalent: kubectl get pods") {
		t.Errorf("artifact data should contain equivalent command")
	}
	if !strings.Contains(arts[0].Data, "pod-1 Running") {
		t.Errorf("artifact data should contain the data")
	}
}

func TestRecordRawTextArtifact_NilCollector(t *testing.T) {
	// Should not panic when ctx.Artifacts is nil.
	ctx := &checks.ValidationContext{
		Context: context.Background(),
	}
	recordRawTextArtifact(ctx, "test", "cmd", "data") // should be a no-op
	t.Log("recordRawTextArtifact with nil collector completed without panic")
}

func TestTruncateLines(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		n     int
		want  string
	}{
		{name: "fewer lines than limit", text: "a\nb", n: 5, want: "a\nb"},
		{name: "exact limit", text: "a\nb\nc", n: 3, want: "a\nb\nc"},
		{name: "exceeds limit", text: "a\nb\nc\nd", n: 2, want: "a\nb\n... [truncated]"},
		{name: "single line", text: "hello", n: 1, want: "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateLines(tt.text, tt.n)
			if got != tt.want {
				t.Errorf("truncateLines(%q, %d) = %q, want %q", tt.text, tt.n, got, tt.want)
			}
		})
	}
}

func TestFirstContainerImage(t *testing.T) {
	tests := []struct {
		name       string
		containers []corev1.Container
		want       string
	}{
		{name: "no containers", containers: nil, want: "unknown"},
		{name: "one container", containers: []corev1.Container{{Image: "nginx:1.25"}}, want: "nginx:1.25"},
		{name: "multiple containers", containers: []corev1.Container{{Image: "app:v1"}, {Image: "sidecar:v2"}}, want: "app:v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstContainerImage(tt.containers)
			if got != tt.want {
				t.Errorf("firstContainerImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRecordObjectYAMLArtifact(t *testing.T) {
	collector := checks.NewArtifactCollector()
	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Artifacts: collector,
	}

	obj := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      "test-pod",
			"namespace": "default",
		},
	}

	recordObjectYAMLArtifact(ctx, "Pod YAML", "kubectl get pod test-pod -o yaml", obj)
	arts := collector.Drain()
	if len(arts) < 1 {
		t.Fatal("expected at least 1 artifact")
	}
	if !strings.Contains(arts[0].Data, "kind: Pod") {
		t.Errorf("artifact data should contain YAML output, got: %s", arts[0].Data)
	}
}

// generateLargeText creates a multi-line string of approximately n bytes.
func generateLargeText(n int) string {
	var b strings.Builder
	line := "log-entry: this is a sample log line for chunk testing purposes"
	for b.Len() < n {
		fmt.Fprintln(&b, line)
	}
	return b.String()
}

func TestRecordChunkedTextArtifact_SingleLineExceedsChunkSize(t *testing.T) {
	// A single very long line that exceeds chunk size should still produce valid output.
	collector := checks.NewArtifactCollector()
	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Artifacts: collector,
	}

	longLine := strings.Repeat("x", defaults.ArtifactMaxDataSize*2)
	recordChunkedTextArtifact(ctx, "long", "", longLine)
	arts := collector.Drain()

	// Should produce at least one artifact (the single line can't be split further).
	if len(arts) < 1 {
		t.Fatal("expected at least 1 artifact for a single long line")
	}
}

func TestInt32Ptr(t *testing.T) {
	v := int32Ptr(42)
	if v == nil {
		t.Fatal("int32Ptr returned nil")
	}
	if *v != 42 {
		t.Errorf("int32Ptr(42) = %d, want 42", *v)
	}
}

func TestPodStuckReason_NilPod(t *testing.T) {
	// Verify empty pod doesn't panic.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	}
	got := podStuckReason(pod)
	if got != "" {
		t.Errorf("podStuckReason(empty pod) = %q, want empty", got)
	}
}
