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

	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
)

func supersedeCtx(service recipe.CriteriaServiceType, constraints ...recipe.Constraint) *validators.Context {
	return &validators.Context{
		ValidationInput: &v1.ValidationInput{
			Criteria: recipe.Criteria{Service: service, Accelerator: recipe.CriteriaAcceleratorH100},
			Config: v1.ValidationConfig{
				Performance: &v1.ValidationPhase{Constraints: constraints},
			},
		},
	}
}

func TestCRESupersedesTrainJobNCCL(t *testing.T) {
	trainJob := recipe.Constraint{Name: checkNameNCCLAllReduceBW, Value: ">= 300"}
	cre := recipe.Constraint{Name: checkNameCRENCCLAllReduceBW, Value: ">= 300"}

	tests := []struct {
		name    string
		ctx     *validators.Context
		variant ncclVariant
		want    bool
	}{
		{
			name:    "qualified combination with the CRE constraint declared",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, trainJob, cre),
			variant: variantDefault,
			want:    true,
		},
		{
			name:    "CRE constraint not declared",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, trainJob),
			variant: variantDefault,
			want:    false,
		},
		{
			name:    "combination CRE is not qualified on keeps the TrainJob check",
			ctx:     supersedeCtx(recipe.CriteriaServiceGKE, trainJob, cre),
			variant: variantDefault,
			want:    false,
		},
		{
			name:    "net variant is never superseded",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, trainJob, cre),
			variant: variantNET,
			want:    false,
		},
		{
			name:    "no validation input",
			ctx:     &validators.Context{},
			variant: variantDefault,
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := creSupersedesTrainJobNCCL(tt.ctx, tt.variant); got != tt.want {
				t.Errorf("creSupersedesTrainJobNCCL() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCheckNCCLAllReduceBWSkipsWhenSuperseded pins the verdict a recipe sees:
// a skip carrying the superseded-by-cre reason, reached before any cluster
// access. A nil clientset and dynamic client would panic if the check went on
// to run.
func TestCheckNCCLAllReduceBWSkipsWhenSuperseded(t *testing.T) {
	ctx := supersedeCtx(recipe.CriteriaServiceEKS,
		recipe.Constraint{Name: checkNameNCCLAllReduceBW, Value: ">= 300"},
		recipe.Constraint{Name: checkNameCRENCCLAllReduceBW, Value: ">= 300"})

	err := checkNCCLAllReduceBWVariant(ctx, variantDefault)
	if !validators.IsSkip(err) {
		t.Fatalf("expected a skip when the CRE check supersedes the TrainJob check, got %v", err)
	}
}
