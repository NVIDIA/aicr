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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCheckCRENCCLSkipsWithoutConstraint(t *testing.T) {
	ctx := &validators.Context{
		ValidationInput: &v1.ValidationInput{
			Config: v1.ValidationConfig{
				Performance: &v1.ValidationPhase{
					Constraints: []recipe.Constraint{{Name: "nccl-all-reduce-bw", Value: ">= 300"}},
				},
			},
		},
	}
	err := checkCRENCCLAllReduceBW(ctx)
	if !validators.IsSkip(err) {
		t.Fatalf("expected Skip without CRE constraint, got %v", err)
	}
}

func TestMaxBusBandwidthGBps(t *testing.T) {
	tests := []struct {
		name    string
		results []any
		want    float64
		wantErr bool
	}{
		{
			name: "max of string busBW",
			results: []any{
				map[string]any{"sizeBytes": int64(8), "busBW": "12.5"},
				map[string]any{"sizeBytes": int64(16), "busBW": "400.25"},
				map[string]any{"sizeBytes": int64(32), "busBW": "90"},
			},
			want: 400.25,
		},
		{
			name: "float64 from JSON decoder",
			results: []any{
				map[string]any{"busBW": float64(300)},
			},
			want: 300,
		},
		{
			name:    "empty",
			results: nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := maxBusBandwidthGBps(tt.results)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildCRENCCLCertification(t *testing.T) {
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8, Namespace: "ns"}
	obj := buildCRENCCLCertification("ns", cfg, map[string]string{"foo": "bar"})
	if obj.GetAPIVersion() != "nvcre.nvidia.com/v1alpha1" {
		t.Errorf("apiVersion = %q", obj.GetAPIVersion())
	}
	if obj.GetKind() != "Certification" {
		t.Errorf("kind = %q", obj.GetKind())
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec is not a map")
	}
	categories := spec["categories"].([]any)
	category := categories[0].(map[string]any)
	if category["domain"] != "communication" || category["variant"] != "nccl-all-reduce" {
		t.Errorf("category = %#v", category)
	}
	target := spec["target"].(map[string]any)
	selector := target["nodeSelector"].(map[string]any)
	if selector["foo"] != "bar" {
		t.Errorf("nodeSelector = %#v", selector)
	}
	if spec["gpusPerNode"] != int64(8) {
		t.Errorf("gpusPerNode = %#v", spec["gpusPerNode"])
	}
}

func TestBuildCRENCCLCertificationCopiesSharedGPUTaints(t *testing.T) {
	taint := corev1.Taint{
		Key:    "dedicated",
		Value:  "worker-workload",
		Effect: corev1.TaintEffectNoSchedule,
	}
	cfg := &gpuConfiguration{
		WorkerCount:     2,
		GPUCountPerNode: 8,
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{taint}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{taint}}},
		},
	}
	obj := buildCRENCCLCertification("ns", cfg, nil)
	spec := obj.Object["spec"].(map[string]any)
	target := spec["target"].(map[string]any)
	sels, ok := target["taintSelectors"].([]any)
	if !ok || len(sels) != 1 {
		t.Fatalf("taintSelectors = %#v", target["taintSelectors"])
	}
	sel := sels[0].(map[string]any)
	if sel["key"] != "dedicated" || sel["value"] != "worker-workload" || sel["effect"] != string(corev1.TaintEffectNoSchedule) {
		t.Fatalf("selector = %#v", sel)
	}
}

func TestUnstructuredConditionTrue(t *testing.T) {
	obj := buildCRENCCLCertification("ns", &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}, nil)
	obj.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Succeeded", "status": "True"},
		},
	}
	if !unstructuredConditionTrue(obj, "Succeeded") {
		t.Fatal("expected Succeeded=True")
	}
	if unstructuredConditionTrue(obj, "Failed") {
		t.Fatal("did not expect Failed")
	}
}

func TestCertificationWorkflowName(t *testing.T) {
	obj := buildCRENCCLCertification("ns", &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}, nil)
	obj.Object["status"] = map[string]any{
		"categoryStatuses": []any{
			map[string]any{
				"domain":  "communication",
				"variant": "nccl-all-reduce",
				"workflowRef": map[string]any{
					"name": "aicr-cre-nccl-abcde",
				},
			},
		},
	}
	got, err := certificationWorkflowName(obj)
	if err != nil {
		t.Fatalf("certificationWorkflowName() error = %v", err)
	}
	if got != "aicr-cre-nccl-abcde" {
		t.Errorf("workflow name = %q", got)
	}
}

func TestYoungestLivePodSince(t *testing.T) {
	cutoff := metav1.NewTime(time.Unix(50, 0))
	older := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "old",
			CreationTimestamp: metav1.NewTime(time.Unix(1, 0)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	newer := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "new",
			CreationTimestamp: metav1.NewTime(time.Unix(100, 0)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	failed := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "failed",
			CreationTimestamp: metav1.NewTime(time.Unix(200, 0)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	got := youngestLivePodSince([]corev1.Pod{older, failed, newer}, cutoff)
	if got == nil || got.Name != "new" {
		t.Fatalf("got %#v, want new", got)
	}
}

func TestMeasurementBelongsToRun(t *testing.T) {
	createdAt := metav1.NewTime(time.Unix(100, 0))
	obj := buildCRENCCLCertification("ns", &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}, nil)
	obj.SetCreationTimestamp(metav1.NewTime(time.Unix(101, 0)))
	obj.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Workflow", Name: creNCCLRunName}})
	if !measurementBelongsToRun(obj, creNCCLRunName, createdAt) {
		t.Fatal("expected matching measurement")
	}
	if measurementBelongsToRun(obj, "aicr-cre-nemo", createdAt) {
		t.Fatal("measurement from another WorkloadRun must not match")
	}
	obj.SetCreationTimestamp(metav1.NewTime(time.Unix(99, 0)))
	if measurementBelongsToRun(obj, creNCCLRunName, createdAt) {
		t.Fatal("stale measurement must not match")
	}
}
