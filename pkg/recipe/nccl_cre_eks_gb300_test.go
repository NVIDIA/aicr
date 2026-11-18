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

// TestGB300EKSTrainingCREAdoption pins which GB300 EKS training recipes gate
// NVLS bandwidth on a CRE Certification. Only the kubeflow leaf does. It lists
// the CRE check next to the inherited TrainJob checks, because an overlay
// cannot drop an inherited check; the validator skips the nvls TrainJob check as
// superseded and keeps the EFA-only net check. The kubeflow leaf already
// carries kubeflow-trainer through the platform-kubeflow mixin that nvcre
// needs. Attaching CRE to a shared base would install nvcre's CRDs and
// cluster-scoped RBAC for every consumer of that base, including the Slurm and
// inference leaves, and AICR has no mechanism for a user-selectable optional
// component.
func TestGB300EKSTrainingCREAdoption(t *testing.T) {
	const (
		creCheck  = "nccl-cre-all-reduce-bw"
		netCheck  = "nccl-all-reduce-bw-net"
		nvlsCheck = "nccl-all-reduce-bw-nvls"
	)

	tests := []struct {
		name         string
		criteria     *Criteria
		wantCRE      bool
		wantTrainJob bool
	}{
		{
			name: "gb300-eks-training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB300,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantTrainJob: true,
		},
		{
			name: "gb300-eks-ubuntu-training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB300,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformAny,
			},
			wantTrainJob: true,
		},
		{
			name: "gb300-eks-ubuntu-training-kubeflow",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB300,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformKubeflow,
			},
			wantCRE:      true,
			wantTrainJob: true,
		},
		{
			name: "gb300-eks-ubuntu-training-slurm",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB300,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
				Platform:    CriteriaPlatformSlurm,
			},
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

			if got := result.GetComponentRef("nvcre") != nil; got != tt.wantCRE {
				t.Errorf("nvcre componentRef present = %v, want %v", got, tt.wantCRE)
			}
			if got := performanceCheckPresent(result.Validation, creCheck); got != tt.wantCRE {
				t.Errorf("performance check %q present = %v, want %v", creCheck, got, tt.wantCRE)
			}

			wants := []struct {
				check string
				want  bool
				value string
			}{
				{creCheck, tt.wantCRE, ">= 700"},
				{netCheck, tt.wantTrainJob, ">= 40"},
				{nvlsCheck, tt.wantTrainJob, ">= 700"},
			}
			for _, w := range wants {
				if got := performanceCheckPresent(result.Validation, w.check); got != w.want {
					t.Errorf("performance check %q present = %v, want %v", w.check, got, w.want)
				}
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
