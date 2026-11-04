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

// TestH100TrainingCREAdoption pins which H100 training recipes gate NCCL on a
// CRE Certification. Only the EKS kubeflow leaf does, and it runs the CRE check
// next to the inherited TrainJob check, because an overlay cannot drop an
// inherited check. It is the leaf the AWS UAT lane validates, and it already
// carries kubeflow-trainer through the platform-kubeflow mixin that nvcre
// needs. Attaching CRE to a shared base
// would install nvcre's CRDs and cluster-scoped RBAC for every consumer of
// that base, including the Slurm and Kueue leaves, and AICR has no mechanism
// for a user-selectable optional component. GKE and AKS stay on the TrainJob
// check until their CRE qualification gaps close (#3591).
func TestH100TrainingCREAdoption(t *testing.T) {
	const creCheck = "nccl-cre-all-reduce-bw"
	const trainJobCheck = "nccl-all-reduce-bw"

	tests := []struct {
		name         string
		criteria     *Criteria
		wantCRE      bool
		wantCREValue string
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
			wantCRE:      true,
			wantCREValue: ">= 300",
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
		},
		{
			name: "h100-gke-cos-training",
			criteria: &Criteria{
				Service:     CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSCOS,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantTrainJob: true,
			wantValue:    ">= 300",
		},
		{
			name: "h100-aks-training",
			criteria: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantTrainJob: true,
			wantValue:    ">= 150",
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

			hasNVCRE := result.GetComponentRef("nvcre") != nil
			if hasNVCRE != tt.wantCRE {
				t.Errorf("nvcre componentRef present = %v, want %v", hasNVCRE, tt.wantCRE)
			}
			if got := performanceCheckPresent(result.Validation, creCheck); got != tt.wantCRE {
				t.Errorf("performance check %q present = %v, want %v", creCheck, got, tt.wantCRE)
			}
			if got := performanceCheckPresent(result.Validation, trainJobCheck); got != tt.wantTrainJob {
				t.Errorf("performance check %q present = %v, want %v", trainJobCheck, got, tt.wantTrainJob)
			}

			wants := []struct {
				check string
				want  bool
				value string
			}{
				{creCheck, tt.wantCRE, tt.wantCREValue},
				{trainJobCheck, tt.wantTrainJob, tt.wantValue},
			}
			for _, w := range wants {
				got, found := findPerformanceConstraint(result.Validation, w.check)
				if found != w.want {
					t.Errorf("performance constraint %q present = %v (value %q), want %v", w.check, found, got, w.want)
					continue
				}
				if w.want && got != w.value {
					t.Errorf("%s = %q, want %q", w.check, got, w.value)
				}
			}
		})
	}
}
