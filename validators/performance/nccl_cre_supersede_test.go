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
	"slices"
	"testing"

	"aicr.run/pkg/recipe"
	v1 "aicr.run/pkg/validator/v1"
	"aicr.run/validators"
)

// supersedeCtx lists a check for every constraint, as a shipped recipe does.
func supersedeCtx(
	service recipe.CriteriaServiceType,
	accelerator recipe.CriteriaAcceleratorType,
	constraints ...recipe.Constraint,
) *validators.Context {

	checks := make([]string, 0, len(constraints))
	for _, c := range constraints {
		checks = append(checks, c.Name)
	}
	return &validators.Context{
		ValidationInput: &v1.ValidationInput{
			Criteria: recipe.Criteria{Service: service, Accelerator: accelerator},
			Config: v1.ValidationConfig{
				Performance: &v1.ValidationPhase{Constraints: constraints, Checks: checks},
			},
		},
	}
}

// withCheck lists a check without declaring its constraint, the shape
// --skip-check leaves for a skipped CRE check.
func withCheck(ctx *validators.Context, name string) *validators.Context {
	perf := ctx.ValidationInput.Config.Performance
	perf.Checks = append(perf.Checks, name)
	return ctx
}

// withoutChecks drops the named checks from ctx while keeping their constraints.
func withoutChecks(ctx *validators.Context, names ...string) *validators.Context {
	perf := ctx.ValidationInput.Config.Performance
	perf.Checks = slices.DeleteFunc(perf.Checks, func(c string) bool { return slices.Contains(names, c) })
	return ctx
}

func TestCRESupersedesTrainJobNCCL(t *testing.T) {
	cre := recipe.Constraint{Name: checkNameCRENCCLAllReduceBW, Value: ">= 700"}
	creNet := recipe.Constraint{Name: checkNameCRENCCLAllReduceBWNET, Value: ">= 30"}
	nvls := recipe.Constraint{Name: constraintNameForVariant(variantNVLS), Value: ">= 700"}
	net := recipe.Constraint{Name: constraintNameForVariant(variantNET), Value: ">= 30"}

	tests := []struct {
		name    string
		ctx     *validators.Context
		variant ncclVariant
		want    bool
	}{
		{
			name:    "gb300 supersedes the nvls variant when the CRE constraint is declared",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls, cre),
			variant: variantNVLS,
			want:    true,
		},
		{
			name:    "gb300 keeps the EFA-only net variant on TrainJob",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls, cre),
			variant: variantNET,
			want:    false,
		},
		{
			name:    "gb300 supersedes the net variant when the CRE net constraint is declared",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls, net, cre, creNet),
			variant: variantNET,
			want:    true,
		},
		{
			name:    "the CRE net constraint alone leaves the nvls variant on TrainJob",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls, net, creNet),
			variant: variantNVLS,
			want:    false,
		},
		{
			name:    "CRE net constraint on a combination it is not qualified on",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorH100, net, creNet),
			variant: variantNET,
			want:    false,
		},
		{
			name:    "gb300 does not supersede the default variant",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls, cre),
			variant: variantDefault,
			want:    false,
		},
		{
			name:    "CRE constraint not declared",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls),
			variant: variantNVLS,
			want:    false,
		},
		{
			name:    "combination CRE is not qualified on keeps the TrainJob check",
			ctx:     supersedeCtx(recipe.CriteriaServiceGKE, recipe.CriteriaAcceleratorGB300, nvls, cre),
			variant: variantNVLS,
			want:    false,
		},
		{
			name:    "eks h100 supersedes the default variant",
			ctx:     supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorH100, cre),
			variant: variantDefault,
			want:    true,
		},
		{
			name:    "gke h100 supersedes the default variant",
			ctx:     supersedeCtx(recipe.CriteriaServiceGKE, recipe.CriteriaAcceleratorH100, cre),
			variant: variantDefault,
			want:    true,
		},
		{
			name: "gb300 CRE constraint without its check keeps the nvls variant on TrainJob",
			ctx: withoutChecks(supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls, cre),
				checkNameCRENCCLAllReduceBW),
			variant: variantNVLS,
			want:    false,
		},
		{
			name: "gb300 CRE net constraint without its check keeps the net variant on TrainJob",
			ctx: withoutChecks(supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, net, creNet),
				checkNameCRENCCLAllReduceBWNET),
			variant: variantNET,
			want:    false,
		},
		{
			name: "eks h100 CRE constraint without its check keeps the default variant on TrainJob",
			ctx: withoutChecks(supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorH100, cre),
				checkNameCRENCCLAllReduceBW),
			variant: variantDefault,
			want:    false,
		},
		{
			name:    "gb300 CRE check listed with its constraint withheld keeps the nvls variant on TrainJob",
			ctx:     withCheck(supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300, nvls), checkNameCRENCCLAllReduceBW),
			variant: variantNVLS,
			want:    false,
		},
		{
			name:    "no performance phase",
			ctx:     &validators.Context{ValidationInput: &v1.ValidationInput{}},
			variant: variantNVLS,
			want:    false,
		},
		{
			name:    "no validation input",
			ctx:     &validators.Context{},
			variant: variantNVLS,
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

// A nil clientset and dynamic client would panic if the check went on to run,
// so a skip here proves the verdict is reached before any cluster access.
func TestCheckNCCLAllReduceBWSkipsWhenSuperseded(t *testing.T) {
	ctx := supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300,
		recipe.Constraint{Name: constraintNameForVariant(variantNVLS), Value: ">= 700"},
		recipe.Constraint{Name: checkNameCRENCCLAllReduceBW, Value: ">= 700"})
	ctx.Ctx = t.Context()

	err := checkNCCLAllReduceBWVariant(ctx, variantNVLS)
	if !validators.IsSkip(err) {
		t.Fatalf("expected a skip when the CRE check supersedes the nvls TrainJob check, got %v", err)
	}
}

func TestCheckNCCLAllReduceBWNETSkipsWhenSuperseded(t *testing.T) {
	ctx := supersedeCtx(recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB300,
		recipe.Constraint{Name: constraintNameForVariant(variantNET), Value: ">= 30"},
		recipe.Constraint{Name: checkNameCRENCCLAllReduceBWNET, Value: ">= 30"})
	ctx.Ctx = t.Context()

	err := checkNCCLAllReduceBWVariant(ctx, variantNET)
	if !validators.IsSkip(err) {
		t.Fatalf("expected a skip when the CRE net check supersedes the net TrainJob check, got %v", err)
	}
}
