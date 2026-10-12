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
	stderrors "errors"
	"strings"
	"testing"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// creEKSH100 is the only combination qualified today; see creQualifiedEntries.
var creEKSH100 = creCombination{
	Service:     recipe.CriteriaServiceEKS,
	Accelerator: recipe.CriteriaAcceleratorH100,
}

// buildCRENCCLCertification and buildCRETrainingCertification pin a
// qualification entry so the certification-shape tests stay about the shape.
// Production callers resolve the entry from recipe criteria instead.
func buildCRENCCLCertification(t *testing.T, namespace string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	t.Helper()
	return mustBuildCRECertification(t, namespace, creNCCLRunName, gpuConfig, creQualifiedEntries[checkNameCRENCCLAllReduceBW][creEKSH100])
}

// The entry is spelled out rather than read from creQualifiedEntries, which
// holds no training combination while the upstream catalog entry is broken.
// What the driver renders for the training domain is independent of whether a
// combination is qualified to run it, and stays worth asserting.
func buildCRETrainingCertification(t *testing.T, namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	t.Helper()
	return mustBuildCRECertification(t, namespace, name, gpuConfig, creCatalogEntry{
		Domain:   creTrainingDomain,
		Variant:  creTrainingVariant,
		MaxNodes: 2,
	})
}

func mustBuildCRECertification(
	t *testing.T, namespace, name string, gpuConfig *gpuConfiguration, entry creCatalogEntry,
) *unstructured.Unstructured {

	t.Helper()
	obj, err := buildCRECertification(namespace, name, gpuConfig, entry)
	if err != nil {
		t.Fatalf("buildCRECertification: %v", err)
	}
	return obj
}

// The namespace a check runs in reaches the Certification through metadata, so
// a dropped namespace would put the run somewhere the validator is not looking.
func TestBuildCRECertificationCarriesNamespace(t *testing.T) {
	obj := buildCRENCCLCertification(t, "cre-system", twoNodeGPUConfig())
	if got := obj.GetNamespace(); got != "cre-system" {
		t.Errorf("namespace = %q, want cre-system", got)
	}
	if got := obj.GetName(); got != creNCCLRunName {
		t.Errorf("name = %q, want %q", got, creNCCLRunName)
	}
}

func TestCREQualifiedEntries(t *testing.T) {
	tests := []struct {
		name        string
		checkName   string
		wantDomain  string
		wantVariant string
	}{
		{"nccl on eks h100", checkNameCRENCCLAllReduceBW, creNCCLDomain, creNCCLVariant},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := lookupCREQualification(tt.checkName, creEKSH100.Service, creEKSH100.Accelerator)
			if !ok {
				t.Fatalf("lookupCREQualification(%q, eks, h100) reported unqualified", tt.checkName)
			}
			if entry.Domain != tt.wantDomain || entry.Variant != tt.wantVariant {
				t.Errorf("entry = %s/%s, want %s/%s", entry.Domain, entry.Variant, tt.wantDomain, tt.wantVariant)
			}
			// An uncapped footprint fans a certification out across the whole
			// GPU pool, so a qualified entry without a cap is a leak.
			if entry.MaxNodes < 2 {
				t.Errorf("MaxNodes = %d, want at least the 2 nodes an all-reduce needs", entry.MaxNodes)
			}
		})
	}
}

// Every check that consults the record must appear in it. Presence is what is
// asserted, not population: a check keyed to an empty set skips everywhere,
// which is a deliberate state, while a check missing from the record entirely
// is an authoring slip that looks identical from the outside.
func TestCREQualifiedEntriesCoverBothChecks(t *testing.T) {
	for _, checkName := range []string{checkNameCRENCCLAllReduceBW, checkNameCRETrainingGoodput} {
		if _, ok := creQualifiedEntries[checkName]; !ok {
			t.Errorf("%s is absent from the qualification record", checkName)
		}
	}
}

// cre-training-goodput has no qualified combination while the nemotron5-8b
// catalog entry does not run on the CRE version the registry pins; see
// creQualifiedEntries for the upstream cause. This asserts the consequence
// that matters — the check skips rather than failing a healthy cluster. When
// the entry is fixed and re-measured, this test is what says to delete it.
func TestCRETrainingGoodputIsUnqualifiedPendingUpstreamFix(t *testing.T) {
	if _, ok := lookupCREQualification(
		checkNameCRETrainingGoodput, creEKSH100.Service, creEKSH100.Accelerator,
	); ok {
		t.Fatal("cre-training-goodput reports qualified on eks x h100; " +
			"if it has been re-measured on the pinned CRE version, remove this test")
	}

	ctx := ctxWithCriteriaAndPerfConstraints(
		recipe.CriteriaServiceEKS,
		recipe.CriteriaAcceleratorH100,
		recipe.Constraint{Name: checkNameCRETrainingGoodput, Value: ">= 0.5"},
	)
	err := checkCRETrainingGoodput(ctx)
	if !validators.IsSkip(err) {
		t.Fatalf("error = %v, want Skip while the check is unqualified", err)
	}
}

// A recipe can declare a CRE constraint on a combination the check was never
// measured on. Both checks must skip there rather than run the benchmark and
// judge it against a threshold calibrated for other hardware.
func TestCREChecksSkipUnqualifiedCombination(t *testing.T) {
	t.Run("nccl", func(t *testing.T) {
		ctx := ctxWithCriteriaAndPerfConstraints(
			recipe.CriteriaServiceEKS,
			recipe.CriteriaAcceleratorGB200,
			recipe.Constraint{Name: checkNameCRENCCLAllReduceBW, Value: ">= 300"},
		)
		constraint, found := findPerformanceConstraint(ctx, checkNameCRENCCLAllReduceBW)
		if !found {
			t.Fatal("constraint not found in the test context")
		}
		actual, passed, err := validateCRENcclAllReduceBw(ctx, constraint)
		if err != nil {
			t.Fatalf("unexpected error = %v", err)
		}
		if !passed {
			t.Error("passed = false, want true: an unqualified combination skips, it does not fail")
		}
		if !strings.Contains(actual, "not qualified") {
			t.Errorf("actual = %q, want a not-qualified skip message", actual)
		}
	})

	t.Run("goodput", func(t *testing.T) {
		ctx := ctxWithCriteriaAndPerfConstraints(
			recipe.CriteriaServiceEKS,
			recipe.CriteriaAcceleratorGB200,
			recipe.Constraint{Name: checkNameCRETrainingGoodput, Value: ">= 0.5"},
		)
		err := checkCRETrainingGoodput(ctx)
		if !validators.IsSkip(err) {
			t.Fatalf("error = %v, want Skip on an unqualified combination", err)
		}
		if !strings.Contains(err.Error(), "not qualified") {
			t.Errorf("error = %q, want a not-qualified skip message", err)
		}
	})
}

func TestLookupCREQualificationUnqualified(t *testing.T) {
	tests := []struct {
		name        string
		checkName   string
		service     recipe.CriteriaServiceType
		accelerator recipe.CriteriaAcceleratorType
	}{
		{"qualified service, other accelerator", checkNameCRENCCLAllReduceBW, recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB200},
		{"other service, qualified accelerator", checkNameCRENCCLAllReduceBW, recipe.CriteriaServiceGKE, recipe.CriteriaAcceleratorH100},
		{"goodput on other accelerator", checkNameCRETrainingGoodput, recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorGB200},
		{"check absent from the record", checkNameNCCLAllReduceBW, recipe.CriteriaServiceEKS, recipe.CriteriaAcceleratorH100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := lookupCREQualification(tt.checkName, tt.service, tt.accelerator)
			if ok {
				t.Fatalf("lookupCREQualification(%q, %s, %s) reported qualified with %+v",
					tt.checkName, tt.service, tt.accelerator, entry)
			}
			if entry != (creCatalogEntry{}) {
				t.Errorf("entry = %+v, want zero value on an unqualified lookup", entry)
			}
		})
	}
}

// Both CRE checks compare measured >= threshold. parseThreshold strips the
// operator, so a recipe asking for anything else would have been evaluated as
// a floor while the trace line echoed the operator it was given — "<= 0.5"
// reported satisfied by a ratio above it. The refusal has to land before the
// Certification is created so no GPU work starts on a constraint the verdict
// cannot keep.
func TestCREThreshold(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{name: "greater or equal", value: ">= 300", want: 300},
		{name: "greater or equal without a space", value: ">=0.5", want: 0.5},
		{name: "bare number is a floor", value: "300", want: 300},
		{name: "units are tolerated", value: ">= 300 GB/s", want: 300},
		{name: "strictly greater is refused", value: "> 0.5", wantErr: true},
		{name: "at most is refused", value: "<= 0.5", wantErr: true},
		{name: "strictly less is refused", value: "< 0.5", wantErr: true},
		{name: "equality is refused", value: "== 0.5", wantErr: true},
		{name: "inequality is refused", value: "!= 0.5", wantErr: true},
		{name: "no number at all", value: ">=", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := creThreshold(checkNameCRETrainingGoodput, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if got != tt.want {
				t.Errorf("threshold = %v, want %v", got, tt.want)
			}
		})
	}
}
