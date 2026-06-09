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

package recipe

import (
	"context"
	"slices"
	"testing"
)

func TestOKEPerformanceGoalsFollowTrainingInferencePattern(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	tests := []struct {
		name            string
		wantChecks      []string
		wantConstraints map[string]string
	}{
		{
			name:       "gb200-oke-training",
			wantChecks: []string{"nccl-all-reduce-bw-nvls"},
			wantConstraints: map[string]string{
				"nccl-all-reduce-bw-nvls": ">= 500",
			},
		},
		{
			name:       "gb200-oke-ubuntu-training",
			wantChecks: []string{"nccl-all-reduce-bw-nvls"},
			wantConstraints: map[string]string{
				"nccl-all-reduce-bw-nvls": ">= 500",
			},
		},
		{
			name:       "gb200-oke-ubuntu-training-kubeflow",
			wantChecks: []string{"nccl-all-reduce-bw-nvls"},
			wantConstraints: map[string]string{
				"nccl-all-reduce-bw-nvls": ">= 500",
			},
		},
		{
			name:       "gb200-oke-ubuntu-inference-dynamo",
			wantChecks: []string{"inference-perf"},
			wantConstraints: map[string]string{
				"inference-model":               "Qwen/Qwen3-8B",
				"inference-concurrency-per-gpu": "256",
				"inference-throughput":          ">= 50000",
				"inference-ttft-p99":            "<= 2000",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay, ok := store.GetRecipeByName(tt.name)
			if !ok {
				t.Fatalf("overlay %q not found in store", tt.name)
			}
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			if result.Validation == nil || result.Validation.Performance == nil {
				t.Fatalf("performance phase missing from resolved recipe")
			}

			performance := result.Validation.Performance
			if !slices.Equal(performance.Checks, tt.wantChecks) {
				t.Errorf("performance.checks = %v, want %v", performance.Checks, tt.wantChecks)
			}
			if len(performance.Constraints) != len(tt.wantConstraints) {
				t.Errorf("performance.constraints count = %d, want %d: %v",
					len(performance.Constraints), len(tt.wantConstraints), performance.Constraints)
			}
			for wantName, wantValue := range tt.wantConstraints {
				found := slices.ContainsFunc(performance.Constraints, func(c Constraint) bool {
					return c.Name == wantName && c.Value == wantValue
				})
				if !found {
					t.Errorf("performance.constraints missing %q=%q: %v", wantName, wantValue, performance.Constraints)
				}
			}
		})
	}
}
