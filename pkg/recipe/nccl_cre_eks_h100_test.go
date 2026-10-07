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
	"testing"
)

// TestH100EKSTrainingCREStaysOptIn pins the CRE rollout gate: the `nvcre`
// component and its `nccl-cre-all-reduce-bw` Certification check are defined
// in the registry and the validator catalog, but no shipped overlay may
// reference them. AICR has no mechanism for a user-selectable optional
// component — a profile cannot introduce a componentRef
// (applyEffectiveProfile rejects one absent from the composition) and
// `overrides.enabled: false` is one-way (the bundler refuses to re-enable a
// recipe-disabled component). Until that mechanism lands, attaching CRE to an
// overlay would make it mandatory for every EKS H100 training consumer and
// would retire the TrainJob path before its results are correlated.
func TestH100EKSTrainingCREStaysOptIn(t *testing.T) {
	const creCheck = "nccl-cre-all-reduce-bw"
	const trainJobCheck = "nccl-all-reduce-bw"

	tests := []struct {
		name         string
		criteria     *Criteria
		wantTrainJob bool
		wantValue    string
	}{
		{
			name: "h100-eks-training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantTrainJob: true,
			wantValue:    ">= 300",
		},
		{
			name: "h100-eks-ubuntu-training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantTrainJob: true,
			wantValue:    ">= 300",
		},
		{
			name: "h100-eks-ubuntu-training-kubeflow",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformKubeflow,
			},
			wantTrainJob: true,
			wantValue:    ">= 300",
		},
		{
			name: "h100-eks-ubuntu-training-slurm",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformSlurm,
			},
			wantTrainJob: false,
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

			if performanceCheckPresent(result.Validation, creCheck) {
				t.Errorf("performance check %q is attached to a shipped overlay; CRE must stay opt-in", creCheck)
			}
			if value, found := findPerformanceConstraint(result.Validation, creCheck); found {
				t.Errorf("performance constraint %q resolved to %q; CRE must stay opt-in", creCheck, value)
			}
			for _, ref := range result.ComponentRefs {
				if ref.Name == "nvcre" {
					t.Error("resolved recipe declares the nvcre componentRef; CRE must stay opt-in")
					break
				}
			}

			gotValue, found := findPerformanceConstraint(result.Validation, trainJobCheck)
			if !tt.wantTrainJob {
				if performanceCheckPresent(result.Validation, trainJobCheck) || found {
					t.Errorf("performance check %q should be cleared on this leaf", trainJobCheck)
				}
				return
			}
			if !performanceCheckPresent(result.Validation, trainJobCheck) {
				t.Errorf("performance check %q not present in resolved checks", trainJobCheck)
			}
			if !found {
				t.Fatalf("performance constraint %q not found; expected value %q", trainJobCheck, tt.wantValue)
			}
			if gotValue != tt.wantValue {
				t.Errorf("%s = %q, want %q", trainJobCheck, gotValue, tt.wantValue)
			}
		})
	}
}
