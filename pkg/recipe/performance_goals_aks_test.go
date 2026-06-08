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

	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestAKSDynamoInferencePerformanceGoalsFollowPattern(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()

	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	overlay, ok := store.GetRecipeByName("h100-aks-ubuntu-inference-dynamo")
	if !ok {
		t.Fatal("overlay h100-aks-ubuntu-inference-dynamo not found in store")
	}
	result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
	if err != nil {
		t.Fatalf("BuildRecipeResult failed: %v", err)
	}
	if result.Validation == nil || result.Validation.Performance == nil {
		t.Fatal("performance phase missing from resolved recipe")
	}

	performance := result.Validation.Performance
	wantChecks := []string{"inference-perf"}
	if !slices.Equal(performance.Checks, wantChecks) {
		t.Errorf("performance.checks = %v, want %v", performance.Checks, wantChecks)
	}

	wantConstraints := map[string]string{
		"inference-model":               "Qwen/Qwen3-8B",
		"inference-concurrency-per-gpu": "256",
		"inference-throughput":          ">= 50000",
		"inference-ttft-p99":            "<= 2000",
	}
	if len(performance.Constraints) != len(wantConstraints) {
		t.Errorf("performance.constraints count = %d, want %d: %v",
			len(performance.Constraints), len(wantConstraints), performance.Constraints)
	}
	for wantName, wantValue := range wantConstraints {
		found := slices.ContainsFunc(performance.Constraints, func(c Constraint) bool {
			return c.Name == wantName && c.Value == wantValue
		})
		if !found {
			t.Errorf("performance.constraints missing %q=%q: %v", wantName, wantValue, performance.Constraints)
		}
	}
}
