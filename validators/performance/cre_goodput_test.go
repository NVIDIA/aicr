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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCheckCRETrainingGoodputSkipsWithoutConstraint(t *testing.T) {
	err := checkCRETrainingGoodput(&validators.Context{ValidationInput: v1.NewValidationInput()})
	if !validators.IsSkip(err) {
		t.Fatalf("expected Skip, got %v", err)
	}
}

func TestBuildCRETrainingCertification(t *testing.T) {
	obj := buildCRETrainingCertification("ns", creTrainingRunName, twoNodeGPUConfig())
	if obj.GetKind() != "Certification" {
		t.Fatalf("kind = %q, want Certification", obj.GetKind())
	}
	spec := obj.Object["spec"].(map[string]any)
	category := spec["categories"].([]any)[0].(map[string]any)
	if category["domain"] != creTrainingDomain || category["variant"] != creTrainingVariant {
		t.Errorf("category = %#v", category)
	}
	if spec["nodesPerJob"] != int64(2) {
		t.Errorf("nodesPerJob = %#v, want 2", spec["nodesPerJob"])
	}
	names := spec["target"].(map[string]any)["nodeNames"].([]any)
	if len(names) != 2 {
		t.Errorf("nodeNames = %#v", names)
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
		{
			name: "nil reason and message",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"status": map[string]any{
					"conditions": []any{
						map[string]any{"type": "Failed", "status": "True", "reason": nil, "message": nil},
					},
				},
			}},
			want: "Failed",
		},
		{
			name: "nil reason and message",
			obj: &unstructured.Unstructured{Object: map[string]any{
				"status": map[string]any{
					"conditions": []any{
						map[string]any{"type": "Failed", "status": "True", "reason": nil, "message": nil},
					},
				},
			}},
			want: "Failed",
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
