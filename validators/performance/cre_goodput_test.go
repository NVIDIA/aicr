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
	"strings"
	"testing"

	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCheckCRETrainingGoodputSkipsWithoutConstraint(t *testing.T) {
	err := checkCRETrainingGoodput(&validators.Context{ValidationInput: v1.NewValidationInput()})
	if !validators.IsSkip(err) {
		t.Fatalf("expected Skip, got %v", err)
	}
}

func TestBuildCRETrainingWorkloadRun(t *testing.T) {
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}
	obj := buildCRETrainingWorkloadRun(
		"ns",
		creTrainingRunName,
		cfg,
		map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
	)
	if obj.GetName() != creTrainingRunName {
		t.Errorf("name = %q, want %s", obj.GetName(), creTrainingRunName)
	}
	spec := obj.Object["spec"].(map[string]any)
	if spec["image"] != creTrainingImage {
		t.Errorf("image = %v, want %s", spec["image"], creTrainingImage)
	}
	framework := spec["framework"].(map[string]any)
	exec := framework["exec"].(map[string]any)
	command := exec["command"].([]any)
	if len(command) != 2 || command[1] != "/config/train.sh" {
		t.Errorf("command = %v, want /bin/bash /config/train.sh", command)
	}
	config := spec["config"].(map[string]any)["inline"].(map[string]any)
	script, _ := config["train.sh"].(string)
	if script != creTrainingScript {
		t.Error("training script does not match the pinned CRE EKS H100 workload")
	}
	for _, want := range []string{
		"--mock-data",
		"--tensor-model-parallel-size",
		"${TENSOR_PARALLELISM:-2}",
		"--num-layers 32",
		"--hidden-size 4096",
		"--log-throughput",
		"--use-mcore-models",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("training script missing %q", want)
		}
	}
	if strings.Contains(script, "--num-layers 79") {
		t.Error("2-node H100 check must use Nemotron-5 8B, not the 56B sample")
	}
	if strings.Contains(script, "${TENSOR_PARALLELISM:-4}") {
		t.Error("H100 8B check must not default TP to the GB200/GB300 56B value 4")
	}
	env := spec["env"].([]any)
	foundTP := false
	for _, raw := range env {
		m := raw.(map[string]any)
		if m[keyName] == "TENSOR_PARALLELISM" {
			foundTP = true
			if m[keyValue] != "2" {
				t.Errorf("TENSOR_PARALLELISM = %v, want 2", m[keyValue])
			}
		}
	}
	if !foundTP {
		t.Error("missing TENSOR_PARALLELISM=2 env (CRE catalog parallelism.h100 for nemotron5-8b)")
	}
	goodput := spec["goodputMeasurement"].(map[string]any)
	if goodput["logProfileRef"] != creLogProfileTrainingGoodput {
		t.Errorf("logProfileRef = %v, want %s", goodput["logProfileRef"], creLogProfileTrainingGoodput)
	}
}

func TestCRETerminalConditionSummary(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want string
	}{
		{
			name: "missing conditions",
			obj:  &unstructured.Unstructured{Object: map[string]any{}},
			want: "no status.conditions",
		},
		{
			name: "Failed with reason and message",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"status": map[string]any{
					"conditions": []any{
						map[string]any{
							"type":    "Failed",
							"status":  "True",
							"reason":  "WorkloadFailed",
							"message": "JobSet FailedJobs",
						},
					},
				},
			}},
			want: "Failed (WorkloadFailed): JobSet FailedJobs",
		},
		{
			name: "ignores False conditions",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"status": map[string]any{
					"conditions": []any{
						map[string]any{"type": "InProgress", "status": "False"},
						map[string]any{"type": "Failed", "status": "True", "reason": "WorkloadFailed"},
					},
				},
			}},
			want: "Failed (WorkloadFailed)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := creTerminalConditionSummary(tt.obj); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseGoodputRatio(t *testing.T) {
	tests := []struct {
		name    string
		status  map[string]any
		want    float64
		wantErr bool
	}{
		{name: "string", status: map[string]any{"result": "0.95"}, want: 0.95},
		{name: "JSON number", status: map[string]any{"result": float64(0.9)}, want: 0.9},
		{name: "missing", status: map[string]any{}, wantErr: true},
		{name: "invalid", status: map[string]any{"result": "bad"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGoodputRatio(tt.status)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
