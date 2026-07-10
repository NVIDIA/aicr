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

// nccl_bandwidth_floor_test.go pins the production-resolved value of the
// H100 GKE COS `nccl-all-reduce-bw` performance gate. The K8s-native check
// runs on the parent leaf and its kubeflow sibling at a fixed busBW floor;
// the Slinky (slurm) leaf intentionally clears the performance phase
// (checks/constraints empty) because the K8s-launched check bypasses
// slurmd and measures the wrong path. Exact-value coverage so a future
// floor edit cannot silently drift these three overlays apart.

package recipe

import (
	"context"
	"testing"
)

// findPerformanceConstraint returns the value of the named constraint in the
// resolved performance phase, plus whether it was found.
func findPerformanceConstraint(v *ValidationConfig, name string) (string, bool) {
	if v == nil || v.Performance == nil {
		return "", false
	}
	for _, c := range v.Performance.Constraints {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

// performanceCheckPresent reports whether the named check appears in the
// resolved performance phase's checks list.
func performanceCheckPresent(v *ValidationConfig, name string) bool {
	if v == nil || v.Performance == nil {
		return false
	}
	for _, ch := range v.Performance.Checks {
		if ch == name {
			return true
		}
	}
	return false
}

// TestH100GKENCCLBandwidthFloor asserts the production resolver yields the
// expected nccl-all-reduce-bw floor for the H100 GKE COS training family.
func TestH100GKENCCLBandwidthFloor(t *testing.T) {
	const checkName = "nccl-all-reduce-bw"

	tests := []struct {
		name string
		// criteria selects the overlay through the production resolver.
		criteria *Criteria
		// wantValue is the expected resolved constraint value when wantPerf
		// is true.
		wantValue string
		// wantPerf is true when the overlay should resolve the K8s-native
		// nccl-all-reduce-bw check + constraint; false when the performance
		// phase is cleared (no check, no constraint).
		wantPerf bool
	}{
		{
			name: "h100-gke-cos-training",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantValue: ">= 300",
			wantPerf:  true,
		},
		{
			name: "h100-gke-cos-training-kubeflow",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformKubeflow,
			},
			wantValue: ">= 300",
			wantPerf:  true,
		},
		{
			name: "h100-gke-cos-training-slurm",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformSlurm,
			},
			wantPerf: false,
		},
	}

	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, tt.criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult: %v", err)
			}

			gotValue, found := findPerformanceConstraint(result.Validation, checkName)
			checkPresent := performanceCheckPresent(result.Validation, checkName)

			if tt.wantPerf {
				if !checkPresent {
					t.Errorf("performance check %q not present in resolved checks", checkName)
				}
				if !found {
					t.Fatalf("performance constraint %q not found; expected value %q", checkName, tt.wantValue)
				}
				if gotValue != tt.wantValue {
					t.Errorf("nccl-all-reduce-bw = %q, want %q", gotValue, tt.wantValue)
				}
			} else {
				if checkPresent {
					t.Errorf("performance check %q should be cleared but is present", checkName)
				}
				if found {
					t.Errorf("performance constraint %q should be cleared but resolved to %q", checkName, gotValue)
				}
			}
		})
	}
}
