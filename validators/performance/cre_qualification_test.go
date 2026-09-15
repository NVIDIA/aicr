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
	"strings"
	"testing"

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
func buildCRENCCLCertification(namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	return buildCRECertification(namespace, name, gpuConfig, creQualifiedEntries[checkNameCRENCCLAllReduceBW][creEKSH100])
}

func buildCRETrainingCertification(namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	return buildCRECertification(namespace, name, gpuConfig, creQualifiedEntries[checkNameCRETrainingGoodput][creEKSH100])
}

// The namespace a check runs in reaches the Certification through metadata, so
// a dropped namespace would put the run somewhere the validator is not looking.
func TestBuildCRECertificationCarriesNamespace(t *testing.T) {
	obj := buildCRENCCLCertification("cre-system", creNCCLRunName, twoNodeGPUConfig())
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
		{"goodput on eks h100", checkNameCRETrainingGoodput, creTrainingDomain, creTrainingVariant},
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

// Every check that consults the record must appear in it, or it silently skips
// on every combination.
func TestCREQualifiedEntriesCoverBothChecks(t *testing.T) {
	for _, checkName := range []string{checkNameCRENCCLAllReduceBW, checkNameCRETrainingGoodput} {
		if len(creQualifiedEntries[checkName]) == 0 {
			t.Errorf("%s has no qualified combinations", checkName)
		}
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
