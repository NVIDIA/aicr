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

func TestBuildCRENCCLWorkloadRunEKSUsesEFAImage(t *testing.T) {
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8, Namespace: "ns"}
	obj := buildCRENCCLWorkloadRun("ns", cfg, map[string]string{"foo": "bar"})
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec is not a map")
	}
	if spec["image"] != creEFANCCLImage {
		t.Errorf("image = %v, want EFA nccl-tests image", spec["image"])
	}
	fw := spec["framework"].(map[string]any)
	mpi := fw["mpi"].(map[string]any)
	if mpi["mpirunPath"] != creEFAMpirun {
		t.Errorf("mpirunPath = %v, want %s", mpi["mpirunPath"], creEFAMpirun)
	}
	if mpi["binary"] != creEFANCCLBin {
		t.Errorf("binary = %v, want %s", mpi["binary"], creEFANCCLBin)
	}
	res := spec["resources"].(map[string]any)
	limits := res["limits"].(map[string]any)
	if limits[creEFAResource] != creEFACountH100 {
		t.Errorf("efa limit = %v, want %s", limits[creEFAResource], creEFACountH100)
	}
	if spec["enableMNNVL"] != false {
		t.Errorf("enableMNNVL = %v, want false", spec["enableMNNVL"])
	}
}

func TestUnstructuredConditionTrue(t *testing.T) {
	obj := buildCRENCCLWorkloadRun("ns", &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}, nil)
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
	obj := buildCRENCCLWorkloadRun("ns", &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}, nil)
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
