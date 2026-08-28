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

	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
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
		cfg,
		map[string]string{"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3"},
	)
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
	if config["train.sh"] != creTrainingScript {
		t.Error("training script does not match the pinned CRE EKS H100 workload")
	}
	goodput := spec["goodputMeasurement"].(map[string]any)
	if goodput["logProfileRef"] != creLogProfileTrainingGoodput {
		t.Errorf("logProfileRef = %v, want %s", goodput["logProfileRef"], creLogProfileTrainingGoodput)
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
