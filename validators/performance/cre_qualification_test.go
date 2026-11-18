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
	"os"
	"regexp"
	"strings"
	"testing"

	aicrErrors "aicr.run/pkg/errors"
	"aicr.run/pkg/recipe"
	"aicr.run/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
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

func buildCRETrainingCertification(t *testing.T, namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	t.Helper()
	return mustBuildCRECertification(t, namespace, name, gpuConfig, creQualifiedEntries[checkNameCRETrainingGoodput][creEKSH100])
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
				t.Errorf("MaxNodes = %d, want at least the 2 nodes a multi-node run needs", entry.MaxNodes)
			}
		})
	}
}

// creEKSGB300 is the second combination qualified for nccl-cre-all-reduce-bw.
var creEKSGB300 = creCombination{
	Service:     recipe.CriteriaServiceEKS,
	Accelerator: recipe.CriteriaAcceleratorGB300,
}

// nvcreClosureFor names the committed NVCRE workload closure each qualified
// combination pins its image from, with the platform and gpuArchitecture
// selectors the closure was derived for. A combination absent here has no
// closure to pin from.
var nvcreClosureFor = map[creCombination]struct {
	path, platform, architecture string
}{
	creEKSH100:  {"../../recipes/components/nvcre/workload-images.yaml", "aws", "h100"},
	creEKSGB300: {"../../recipes/components/nvcre/workload-images-gb300.yaml", "aws", "gb300"},
}

// A qualified entry pins its workload image to the digest the closure records
// for the pinned CRE version. A chart bump re-derives the closures, so a pin
// left behind fails here instead of running bytes the closure no longer
// describes.
func TestCREQualifiedImagesMatchWorkloadClosure(t *testing.T) {
	type closureFile struct {
		Platform        string `json:"platform"`
		GPUArchitecture string `json:"gpuArchitecture"`
		Images          []struct {
			Image  string `json:"image"`
			Digest string `json:"digest"`
		} `json:"images"`
	}
	pinPattern := regexp.MustCompile(`^([^@\s]+:[^@\s]+)@(sha256:[0-9a-f]{64})$`)

	for checkName, combinations := range creQualifiedEntries {
		for combination, entry := range combinations {
			t.Run(checkName+"/"+string(combination.Service)+"-"+string(combination.Accelerator), func(t *testing.T) {
				source, ok := nvcreClosureFor[combination]
				if !ok {
					t.Fatalf("%s is qualified on %s x %s, which no committed closure covers; "+
						"derive one for its platform and pin its image from it",
						checkName, combination.Service, combination.Accelerator)
				}
				data, err := os.ReadFile(source.path)
				if err != nil {
					t.Fatalf("read %s: %v", source.path, err)
				}
				var closure closureFile
				if err := yaml.Unmarshal(data, &closure); err != nil {
					t.Fatalf("parse %s: %v", source.path, err)
				}
				if closure.Platform != source.platform || closure.GPUArchitecture != source.architecture {
					t.Fatalf("%s resolves platform %q, gpuArchitecture %q; %s x %s needs %s, %s",
						source.path, closure.Platform, closure.GPUArchitecture,
						combination.Service, combination.Accelerator, source.platform, source.architecture)
				}
				recorded := make(map[string]string, len(closure.Images))
				for _, img := range closure.Images {
					recorded[img.Image] = img.Digest
				}
				match := pinPattern.FindStringSubmatch(entry.Image)
				if match == nil {
					t.Fatalf("Image = %q, want <repository>:<tag>@sha256:<64 hex>", entry.Image)
				}
				digest, found := recorded[match[1]]
				if !found {
					t.Fatalf("%s is not in %s; re-derive the pin from the closure", match[1], source.path)
				}
				if digest != match[2] {
					t.Errorf("%s pins %s, the closure records %s", match[1], match[2], digest)
				}
			})
		}
	}
}

// Fabric and Supersedes are what separate the two NCCL combinations; a dropped
// one would run the wrong transport or skip the wrong TrainJob check silently.
func TestCRENCCLQualificationSelectsFabricAndSupersededVariant(t *testing.T) {
	tests := []struct {
		combination    creCombination
		wantFabric     string
		wantSupersedes ncclVariant
	}{
		{creEKSH100, "", variantDefault},
		{creEKSGB300, "efa", variantNVLS},
	}
	for _, tt := range tests {
		t.Run(string(tt.combination.Accelerator), func(t *testing.T) {
			entry, ok := lookupCREQualification(checkNameCRENCCLAllReduceBW,
				tt.combination.Service, tt.combination.Accelerator)
			if !ok {
				t.Fatalf("%s x %s is unqualified", tt.combination.Service, tt.combination.Accelerator)
			}
			if entry.Fabric != tt.wantFabric {
				t.Errorf("Fabric = %q, want %q", entry.Fabric, tt.wantFabric)
			}
			if entry.Supersedes != tt.wantSupersedes {
				t.Errorf("Supersedes = %q, want %q", entry.Supersedes, tt.wantSupersedes)
			}
		})
	}
}

// spec.fabric reaches the Certification only when the entry names one, so the
// H100 object stays byte-for-byte what it was qualified with.
func TestBuildCRECertificationFabric(t *testing.T) {
	gpuConfig := twoNodeGPUConfig()
	tests := []struct {
		name       string
		entry      creCatalogEntry
		wantFabric string
		wantSet    bool
	}{
		{"fabric named", creCatalogEntry{Domain: "d", Variant: "v", MaxNodes: 2, Fabric: "efa"}, "efa", true},
		{"fabric unset", creCatalogEntry{Domain: "d", Variant: "v", MaxNodes: 2}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := mustBuildCRECertification(t, "ns", "name", gpuConfig, tt.entry)
			got, found, err := unstructured.NestedString(obj.Object, "spec", "fabric")
			if err != nil {
				t.Fatalf("spec.fabric: %v", err)
			}
			if found != tt.wantSet || got != tt.wantFabric {
				t.Errorf("spec.fabric = %q (found %v), want %q (found %v)", got, found, tt.wantFabric, tt.wantSet)
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

// TestCREChecksRejectBadConstraintBeforeQualification pins the ordering that
// makes the documented promise true: a constraint the check cannot honor is
// refused even on a combination that would otherwise skip. Validating it after
// the qualification lookup would hide every malformed constraint behind a skip
// on any combination the check was not measured on.
func TestCREChecksRejectBadConstraintBeforeQualification(t *testing.T) {
	for _, value := range []string{"<= 0.5", ">= NaN"} {
		t.Run("nccl "+value, func(t *testing.T) {
			ctx := ctxWithCriteriaAndPerfConstraints(
				recipe.CriteriaServiceEKS,
				recipe.CriteriaAcceleratorGB200,
				recipe.Constraint{Name: checkNameCRENCCLAllReduceBW, Value: value},
			)
			constraint, found := findPerformanceConstraint(ctx, checkNameCRENCCLAllReduceBW)
			if !found {
				t.Fatal("constraint not found in the test context")
			}
			_, passed, err := validateCRENcclAllReduceBw(ctx, constraint)
			if err == nil {
				t.Fatalf("error = nil, want a refusal for constraint %q", value)
			}
			if passed {
				t.Error("passed = true, want false")
			}
			if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error %v, want ErrCodeInvalidRequest", err)
			}
		})

		t.Run("goodput "+value, func(t *testing.T) {
			ctx := ctxWithCriteriaAndPerfConstraints(
				recipe.CriteriaServiceEKS,
				recipe.CriteriaAcceleratorGB200,
				recipe.Constraint{Name: checkNameCRETrainingGoodput, Value: value},
			)
			err := checkCRETrainingGoodput(ctx)
			if validators.IsSkip(err) {
				t.Fatalf("error = %v, want a refusal rather than a skip for constraint %q", err, value)
			}
			if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
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
		{name: "NaN is refused", value: ">= NaN", wantErr: true},
		{name: "positive infinity is refused", value: ">= +Inf", wantErr: true},
		{name: "negative infinity is refused", value: ">= -Inf", wantErr: true},
		{name: "spelled-out infinity is refused", value: ">= infinity", wantErr: true},
		{name: "bare NaN is refused", value: "NaN", wantErr: true},
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
