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
	"context"
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// ctxWithCriteriaAndPerfConstraints builds a validators.Context whose recipe
// carries both criteria and performance constraints — the shape an external
// --data recipe presents to the NCCL checks.
func ctxWithCriteriaAndPerfConstraints(service recipe.CriteriaServiceType,
	accelerator recipe.CriteriaAcceleratorType, cs ...recipe.Constraint) *validators.Context {

	return &validators.Context{ValidationInput: validatorv1.ToValidationInput(&recipe.RecipeResult{
		Criteria: &recipe.Criteria{Service: service, Accelerator: accelerator},
		Validation: &recipe.ValidationConfig{
			Performance: &recipe.ValidationPhase{Constraints: cs},
		},
	})}
}

func profileConstraint(v string) recipe.Constraint {
	return recipe.Constraint{Name: perfConstraintNCCLBenchmarkProfile, Value: v}
}

func TestResolveNCCLBenchmarkProfile(t *testing.T) {
	tests := []struct {
		name       string
		ctx        *validators.Context
		want       *ncclBenchmarkTarget
		wantErr    bool
		wantErrSub string
	}{
		{
			name: "absent constraint → no profile",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200"),
			want: nil,
		},
		{
			name: "blank value → no profile",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("   ")),
			want: nil,
		},
		{
			name: "valid pair",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("gb200/eks")),
			want: &ncclBenchmarkTarget{
				accelerator: recipe.CriteriaAcceleratorGB200,
				service:     recipe.CriteriaServiceEKS,
				fromProfile: true,
			},
		},
		{
			name: "case and whitespace normalized",
			ctx:  ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("  GB200 / EKS  ")),
			want: &ncclBenchmarkTarget{
				accelerator: recipe.CriteriaAcceleratorGB200,
				service:     recipe.CriteriaServiceEKS,
				fromProfile: true,
			},
		},
		{
			name:       "missing service segment",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("gb200")),
			wantErr:    true,
			wantErrSub: "{accelerator}/{service}",
		},
		{
			name:       "empty accelerator segment",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("/eks")),
			wantErr:    true,
			wantErrSub: "{accelerator}/{service}",
		},
		{
			name:       "empty service segment",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("gb200/")),
			wantErr:    true,
			wantErrSub: "{accelerator}/{service}",
		},
		{
			name:       "too many segments",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("gb200/eks/net")),
			wantErr:    true,
			wantErrSub: "{accelerator}/{service}",
		},
		{
			name:       "unknown pair fails closed with the valid set",
			ctx:        ctxWithCriteriaAndPerfConstraints("custom-svc", "gb200", profileConstraint("gb200/aks")),
			wantErr:    true,
			wantErrSub: "available profiles: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveNCCLBenchmarkProfile(tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("profile = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNCCLCombinationSupported(t *testing.T) {
	target := func(a recipe.CriteriaAcceleratorType, s recipe.CriteriaServiceType) ncclBenchmarkTarget {
		return ncclBenchmarkTarget{accelerator: a, service: s}
	}
	tests := []struct {
		name    string
		variant ncclVariant
		fabric  ncclFabricType
		target  ncclBenchmarkTarget
		want    bool
	}{
		{"default H100 EKS", variantDefault, fabricEFA, target(recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceEKS), true},
		{"default H200 EKS", variantDefault, fabricEFA, target(recipe.CriteriaAcceleratorH200, recipe.CriteriaServiceEKS), true},
		{"default H100 GKE", variantDefault, fabricEFA, target(recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE), true},
		{"default H100 AKS", variantDefault, fabricEFA, target(recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceAKS), true},
		{"default B200 any", variantDefault, fabricEFA, target(recipe.CriteriaAcceleratorB200, recipe.CriteriaServiceAny), true},
		{"default GB200 EKS not covered", variantDefault, fabricEFA, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS), false},
		{"NET GB200 EKS", variantNET, fabricEFA, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS), true},
		{"NET GB200 OKE not covered", variantNET, fabricEFA, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceOKE), false},
		{"NVLS GB200 EKS", variantNVLS, fabricEFA, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS), true},
		{"NVLS GB200 OKE", variantNVLS, fabricEFA, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceOKE), true},
		{"unknown service", variantNVLS, fabricEFA, target(recipe.CriteriaAcceleratorGB200, "custom-svc"), false},
		{"unknown accelerator", variantNET, fabricEFA, target("gb300", recipe.CriteriaServiceEKS), false},
		// RoCE NET is service-keyed and accelerator-agnostic.
		{"RoCE NET EKS any accelerator", variantNET, fabricRoCE, target("gb300", recipe.CriteriaServiceEKS), true},
		{"RoCE NET GKE not covered", variantNET, fabricRoCE, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceGKE), false},
		// RoCE only reroutes NET; NVLS keeps the accelerator-keyed matrix.
		{"RoCE NVLS falls back to matrix", variantNVLS, fabricRoCE, target(recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceEKS), true},
		{"RoCE NVLS unknown accelerator", variantNVLS, fabricRoCE, target("gb300", recipe.CriteriaServiceEKS), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ncclCombinationSupported(tt.variant, tt.fabric, tt.target); got != tt.want {
				t.Errorf("ncclCombinationSupported(%s, %s, %s) = %v, want %v",
					tt.variant, tt.fabric, tt.target.String(), got, tt.want)
			}
		})
	}
}

// TestKnownBenchmarkProfiles pins the profile inventory to the compiled
// matrix, mirroring how TestSupportedNCCLCombinations_Variants pins the matrix
// itself. When a tuple is added to supportedNCCLCombinations, this list — and
// the docs that enumerate valid nccl-benchmark-profile values — must follow.
func TestKnownBenchmarkProfiles(t *testing.T) {
	want := []string{
		"b200/any",
		"gb200/any",
		"gb200/eks",
		"gb200/oke",
		"h100/aks",
		"h100/eks",
		"h100/gke",
		"h200/eks",
	}
	if got := knownBenchmarkProfiles(); !reflect.DeepEqual(got, want) {
		t.Errorf("knownBenchmarkProfiles() = %v, want %v", got, want)
	}
}

// TestRoCEServicesHaveMatrixTuples guards the profile-validation assumption
// documented on benchmarkProfileKnown: every RoCE NET service must also carry
// at least one accelerator-keyed tuple in supportedNCCLCombinations, or no
// nccl-benchmark-profile value could name it. When this fails, extend
// benchmarkProfileKnown to consult roceNETSupportedServices as part of adding
// the RoCE-only service.
func TestRoCEServicesHaveMatrixTuples(t *testing.T) {
	for service := range roceNETSupportedServices {
		found := false
		for _, byService := range supportedNCCLCombinations {
			if len(byService[service]) > 0 {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("RoCE NET service %q has no accelerator-keyed tuple in supportedNCCLCombinations — no benchmark profile can name it (see benchmarkProfileKnown doc comment)", service)
		}
	}
}

// TestValidateNcclAllReduceBwProfileGate exercises validateNcclAllReduceBw up
// to (and just past) the applicability gate without a cluster: skip paths
// return before any client access, and gate passage is detected via the
// threshold-parse error that immediately follows the gate (cases with an
// intentionally malformed "not-a-number" threshold).
func TestValidateNcclAllReduceBwProfileGate(t *testing.T) {
	t.Setenv(ncclFabricEnv, "") // isolate from ambient AICR_NCCL_FABRIC

	thresholdC := func(name, v string) recipe.Constraint { return recipe.Constraint{Name: name, Value: v} }

	tests := []struct {
		name        string
		service     recipe.CriteriaServiceType
		accelerator recipe.CriteriaAcceleratorType
		profile     string            // nccl-benchmark-profile value; "" omits the constraint
		constraint  recipe.Constraint // check constraint, passed to validateNcclAllReduceBw
		variant     ncclVariant
		wantMsg     string // exact skip message; empty means an error is expected instead
		wantErrCode bool   // expect ErrCodeInvalidRequest
		wantErrSub  string // substring the error must contain
	}{
		{
			name:        "new service without profile skips",
			service:     "custom-svc",
			accelerator: "gb200",
			constraint:  thresholdC("nccl-all-reduce-bw-net", ">= 40"),
			variant:     variantNET,
			wantMsg:     "skipped - requires Service + Accelerator to be implemented",
		},
		{
			name:        "profile valid for another variant skips with profile message",
			service:     "custom-svc",
			accelerator: "gb200",
			profile:     "gb200/eks",
			constraint:  thresholdC(checkNameNCCLAllReduceBW, ">= 40"),
			variant:     variantDefault,
			wantMsg:     "skipped - benchmark profile gb200/eks does not implement the nccl-all-reduce-bw NCCL variant",
		},
		{
			name:        "malformed profile fails closed before cluster access",
			service:     "custom-svc",
			accelerator: "gb200",
			profile:     "gb200",
			constraint:  thresholdC("nccl-all-reduce-bw-net", ">= 40"),
			variant:     variantNET,
			wantErrCode: true,
		},
		{
			name:        "unknown profile fails closed before cluster access",
			service:     "custom-svc",
			accelerator: "gb200",
			profile:     "gb200/aks",
			constraint:  thresholdC("nccl-all-reduce-bw-net", ">= 40"),
			variant:     variantNET,
			wantErrCode: true,
		},
		{
			name:        "profile admits a new service past the gate",
			service:     "custom-svc",
			accelerator: "gb200",
			profile:     "gb200/eks",
			constraint:  thresholdC("nccl-all-reduce-bw-net", "not-a-number"),
			variant:     variantNET,
			wantErrSub:  "invalid threshold",
		},
		{
			name:        "criteria-supported combination is unaffected by absent profile",
			service:     recipe.CriteriaServiceEKS,
			accelerator: recipe.CriteriaAcceleratorH100,
			constraint:  thresholdC(checkNameNCCLAllReduceBW, "not-a-number"),
			variant:     variantDefault,
			wantErrSub:  "invalid threshold",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := []recipe.Constraint{tt.constraint}
			if tt.profile != "" {
				cs = append(cs, profileConstraint(tt.profile))
			}
			ctx := ctxWithCriteriaAndPerfConstraints(tt.service, tt.accelerator, cs...)
			msg, passed, err := validateNcclAllReduceBw(ctx, tt.constraint, tt.variant)

			if tt.wantMsg != "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !passed || msg != tt.wantMsg {
					t.Errorf("got (%q, %v), want (%q, true)", msg, passed, tt.wantMsg)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error, got (%q, %v)", msg, passed)
			}
			if tt.wantErrCode && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
			if tt.wantErrSub != "" && !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
			}
		})
	}
}

// TestValidateNcclAllReduceBwProfileClusterPath drives the profile-supported
// path past the applicability gate and into determineGPUConfig against a fake
// cluster, proving the mixed-source contract: the profile's service keys the
// EKS instance-type narrowing while node identification stays on the recipe's
// own criteria accelerator.
//
// The cluster has two schedulable GPU nodes whose GFD product (NVIDIA-GB300)
// matches no compiled accelerator matcher, spread across two different EC2
// instance types:
//   - If the profile's accelerator (gb200) leaked into node identification,
//     narrowByAccelerator would hard-fail with zero product matches; with the
//     criteria accelerator (gb300, no matcher) the set passes through intact.
//   - Because the profile's service (eks) drives narrowByInstanceType, the
//     two-node set narrows to the first node's instance type, leaving a
//     single worker — observable as the "requires at least 2 GPU nodes" skip,
//     which a criteria-derived non-EKS service would not produce.
func TestValidateNcclAllReduceBwProfileClusterPath(t *testing.T) {
	t.Setenv(ncclFabricEnv, "") // isolate from ambient AICR_NCCL_FABRIC

	node := func(name, instanceType string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					gpuProductLabel:   "NVIDIA-GB300",
					instanceTypeLabel: instanceType,
				},
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					"nvidia.com/gpu": resource.MustParse("8"),
				},
			},
		}
	}

	threshold := recipe.Constraint{Name: "nccl-all-reduce-bw-net", Value: ">= 40"}
	ctx := ctxWithCriteriaAndPerfConstraints("custom-svc", "gb300",
		threshold, profileConstraint("gb200/eks"))
	ctx.Ctx = context.Background()
	ctx.Clientset = fake.NewClientset(
		node("gpu-node-a", "p5.48xlarge"),
		node("gpu-node-b", "p5e.48xlarge"),
	)

	msg, passed, err := validateNcclAllReduceBw(ctx, threshold, variantNET)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "skipped - requires at least 2 GPU nodes for EW fabric test"
	if !passed || msg != want {
		t.Errorf("got (%q, %v), want (%q, true)", msg, passed, want)
	}
}
