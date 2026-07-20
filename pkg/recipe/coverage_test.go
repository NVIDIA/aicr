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
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// covStore builds a synthetic store from (name, criteria, base) triples.
func covStore(t *testing.T, overlays ...*RecipeMetadata) *MetadataStore {
	t.Helper()
	base := &RecipeMetadata{}
	base.Metadata.Name = baseRecipeName
	m := map[string]*RecipeMetadata{}
	for _, o := range overlays {
		m[o.Metadata.Name] = o
	}
	return &MetadataStore{Base: base, Overlays: m}
}

func covOverlay(name string, criteria *Criteria, baseRef string) *RecipeMetadata {
	o := &RecipeMetadata{}
	o.Metadata.Name = name
	o.Spec.Criteria = criteria
	o.Spec.Base = baseRef
	return o
}

func TestUncoveredDimensions(t *testing.T) {
	eks := covOverlay("eks", &Criteria{Service: CriteriaServiceEKS}, "")
	h100Any := covOverlay("h100-any", &Criteria{Accelerator: CriteriaAcceleratorH100}, "")
	eksTraining := covOverlay("eks-training",
		&Criteria{Service: CriteriaServiceEKS, Intent: CriteriaIntentTraining}, "eks")
	store := covStore(t, eks, h100Any, eksTraining)

	tests := []struct {
		name     string
		criteria *Criteria
		applied  []string
		want     []string
	}{
		{
			name: "all stated dimensions covered",
			criteria: &Criteria{Service: CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining},
			applied: []string{baseRecipeName, "h100-any", "eks", "eks-training"},
			want:    []string{},
		},
		{
			name: "platform stated but covered by nothing",
			criteria: &Criteria{Service: CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining,
				Platform: CriteriaPlatformKubeflow},
			applied: []string{baseRecipeName, "h100-any", "eks", "eks-training"},
			want:    []string{"platform"},
		},
		{
			name: "multiple uncovered reported in fixed order",
			criteria: &Criteria{Service: CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining,
				OS: CriteriaOSUbuntu, Platform: CriteriaPlatformKubeflow},
			applied: []string{baseRecipeName, "h100-any", "eks", "eks-training"},
			want:    []string{"os", "platform"},
		},
		{
			name:     "covered by ancestor only (leaf wildcard in service)",
			criteria: &Criteria{Service: CriteriaServiceEKS, Intent: CriteriaIntentTraining},
			applied:  []string{baseRecipeName, "eks", "eks-training"},
			want:     []string{},
		},
		{
			name:     "unstated dimensions never uncovered (generic tier)",
			criteria: &Criteria{Service: CriteriaServiceEKS},
			applied:  []string{baseRecipeName, "eks"},
			want:     []string{},
		},
		{
			name:     "nodes exempt from coverage",
			criteria: &Criteria{Service: CriteriaServiceEKS, Nodes: 4},
			applied:  []string{baseRecipeName, "eks"},
			want:     []string{},
		},
		{
			name: "any and empty treated as unstated",
			criteria: &Criteria{Service: CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorAny, OS: ""},
			applied: []string{baseRecipeName, "eks"},
			want:    []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.uncoveredDimensions(tt.criteria, tt.applied)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uncoveredDimensions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompletionTuplesFor(t *testing.T) {
	// Catalog: kubeflow reachable on eks only via ubuntu leaf; on kind via
	// an OS-agnostic leaf that also needs service=kind.
	eks := covOverlay("eks", &Criteria{Service: CriteriaServiceEKS}, "")
	eksUbuKf := covOverlay("h100-eks-ubuntu-training-kubeflow", &Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining, OS: CriteriaOSUbuntu,
		Platform: CriteriaPlatformKubeflow}, "eks")
	kindKf := covOverlay("h100-kind-training-kubeflow", &Criteria{
		Service: CriteriaServiceKind, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow}, "")
	store := covStore(t, eks, eksUbuKf, kindKf)

	tests := []struct {
		name     string
		criteria *Criteria // the query (uncovered dim included, stated)
		dim      string
		want     string
		expect   []map[string]string
	}{
		{
			name: "single completion, single field (os)",
			criteria: &Criteria{Service: CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining,
				Platform: CriteriaPlatformKubeflow},
			dim: "platform", want: string(CriteriaPlatformKubeflow),
			expect: []map[string]string{{"os": string(CriteriaOSUbuntu)}},
		},
		{
			name: "cross-dimension singleton alternatives both returned",
			criteria: &Criteria{Accelerator: CriteriaAcceleratorH100,
				Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow},
			dim: "platform", want: string(CriteriaPlatformKubeflow),
			expect: []map[string]string{
				{"service": string(CriteriaServiceKind)},
				{"service": string(CriteriaServiceEKS), "os": string(CriteriaOSUbuntu)},
			},
		},
		{
			name: "conflicting stated dimension excludes overlay",
			criteria: &Criteria{Service: CriteriaServiceGKE,
				Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining,
				Platform: CriteriaPlatformKubeflow},
			dim: "platform", want: string(CriteriaPlatformKubeflow),
			expect: []map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.completionTuplesFor(tt.criteria, tt.dim, tt.want)
			if !reflect.DeepEqual(got, tt.expect) {
				t.Errorf("completionTuplesFor() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestMinimalTuples(t *testing.T) {
	tests := []struct {
		name   string
		in     []map[string]string
		expect []map[string]string
	}{
		{
			name: "superset dropped",
			in: []map[string]string{
				{"os": "ubuntu", "service": "eks"},
				{"os": "ubuntu"},
			},
			expect: []map[string]string{{"os": "ubuntu"}},
		},
		{
			name: "duplicates collapsed",
			in: []map[string]string{
				{"os": "ubuntu"}, {"os": "ubuntu"},
			},
			expect: []map[string]string{{"os": "ubuntu"}},
		},
		{
			name:   "empty tuple dropped (coverage existed pre-exclusion)",
			in:     []map[string]string{{}, {"os": "ubuntu"}},
			expect: []map[string]string{{"os": "ubuntu"}},
		},
		{
			name: "deterministic order: by size then key=value string",
			in: []map[string]string{
				{"service": "kind", "os": "ol"},
				{"os": "ubuntu"},
				{"os": "cos"},
			},
			expect: []map[string]string{
				{"os": "cos"}, {"os": "ubuntu"}, {"os": "ol", "service": "kind"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := minimalTuples(tt.in)
			if !reflect.DeepEqual(got, tt.expect) {
				t.Errorf("minimalTuples() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestVerifyCriteriaCoverage(t *testing.T) {
	eks := covOverlay("eks", &Criteria{Service: CriteriaServiceEKS}, "")
	eksUbuKf := covOverlay("h100-eks-ubuntu-training-kubeflow", &Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining, OS: CriteriaOSUbuntu,
		Platform: CriteriaPlatformKubeflow}, "eks")
	store := covStore(t, eks, eksUbuKf)

	t.Run("covered returns nil", func(t *testing.T) {
		err := store.verifyCriteriaCoverage(
			&Criteria{Service: CriteriaServiceEKS},
			[]string{baseRecipeName, "eks"}, nil, nil)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("uncovered returns InvalidRequest with single-field wording", func(t *testing.T) {
		err := store.verifyCriteriaCoverage(
			&Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
				Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow},
			[]string{baseRecipeName, "eks"}, nil, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
		}
		msg := err.Error()
		for _, want := range []string{"platform 'kubeflow'", "requires os", "ubuntu"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message %q missing %q", msg, want)
			}
		}
	})

	t.Run("unsupported combination wording", func(t *testing.T) {
		err := store.verifyCriteriaCoverage(
			&Criteria{Service: CriteriaServiceEKS, Platform: CriteriaPlatformDynamo},
			[]string{baseRecipeName, "eks"}, nil, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "no recipe provides platform 'dynamo'") {
			t.Errorf("unexpected message: %v", err)
		}
	})

	t.Run("context carries uncovered array and exclusions", func(t *testing.T) {
		err := store.verifyCriteriaCoverage(
			&Criteria{Service: CriteriaServiceEKS, Platform: CriteriaPlatformKubeflow},
			[]string{baseRecipeName, "eks"},
			[]ExcludedOverlay{{Name: "h100-eks-ubuntu-training-kubeflow",
				Reason: ExcludedOverlayReasonConstraintFailed}},
			[]ConstraintWarning{{Overlay: "h100-eks-ubuntu-training-kubeflow",
				Constraint: "OS.kernel.version", Expected: ">= 6.8", Actual: "6.5"}})
		var se *aicrerrors.StructuredError
		if !stderrors.As(err, &se) {
			t.Fatalf("expected StructuredError, got %v", err)
		}
		if se.Context["uncovered"] == nil {
			t.Error("context missing 'uncovered'")
		}
		if se.Context["excludedOverlays"] == nil {
			t.Error("context missing 'excludedOverlays'")
		}
	})

	t.Run("tuple-form wording for multi-field completions", func(t *testing.T) {
		// Query platform=kubeflow alone; eksUbuKf provides it but requires
		// multiple additional dimensions (service, accelerator, intent, os).
		// This triggers the multi-field tuple branch.
		err := store.verifyCriteriaCoverage(
			&Criteria{Platform: CriteriaPlatformKubeflow},
			[]string{baseRecipeName}, nil, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		msg := err.Error()
		// The exact tuple is: service=eks, accelerator=h100, intent=training, os=ubuntu
		// (in coverageDimensions order). Assert the full clause with correct join/parens.
		expectedClause := "platform 'kubeflow' requires additional criteria; supported combinations: (service=eks, accelerator=h100, intent=training, os=ubuntu)"
		if !strings.Contains(msg, expectedClause) {
			t.Errorf("message missing expected clause:\n  got:  %q\n  want to contain: %q", msg, expectedClause)
		}
	})

	t.Run("excluded-only provider wording", func(t *testing.T) {
		// The full eksUbuKf tuple is stated, so its completion tuple is
		// empty and dropped — but the overlay DOES provide the platform;
		// it was constraint-excluded. The clause must not claim "no recipe
		// provides" while excludedOverlays says otherwise.
		err := store.verifyCriteriaCoverage(
			&Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
				Intent: CriteriaIntentTraining, OS: CriteriaOSUbuntu,
				Platform: CriteriaPlatformKubeflow},
			[]string{baseRecipeName, "eks"},
			[]ExcludedOverlay{{Name: "h100-eks-ubuntu-training-kubeflow",
				Reason: ExcludedOverlayReasonConstraintFailed}}, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "platform 'kubeflow'") ||
			!strings.Contains(msg, "is provided only by overlays excluded by failing constraints (see excludedOverlays)") {

			t.Errorf("expected excluded-provider wording, got %q", msg)
		}
		if strings.Contains(msg, "no recipe provides platform") {
			t.Errorf("message must not claim no recipe provides an excluded-but-existing platform: %q", msg)
		}
	})

	t.Run("multi-tuple wording joins all supported combinations", func(t *testing.T) {
		eksKf := covOverlay("eks-ubuntu-kubeflow", &Criteria{
			Service: CriteriaServiceEKS, OS: CriteriaOSUbuntu,
			Platform: CriteriaPlatformKubeflow}, "")
		okeKf := covOverlay("oke-ubuntu-kubeflow", &Criteria{
			Service: CriteriaServiceOKE, OS: CriteriaOSUbuntu,
			Platform: CriteriaPlatformKubeflow}, "")
		multi := covStore(t, eksKf, okeKf)

		err := multi.verifyCriteriaCoverage(
			&Criteria{Platform: CriteriaPlatformKubeflow},
			[]string{baseRecipeName}, nil, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		expectedClause := "platform 'kubeflow' requires additional criteria; supported combinations: (service=eks, os=ubuntu), (service=oke, os=ubuntu)"
		if !strings.Contains(err.Error(), expectedClause) {
			t.Errorf("message missing multi-tuple clause:\n  got:  %q\n  want to contain: %q", err.Error(), expectedClause)
		}
	})

	t.Run("same-dimension singletons list all valid values sorted", func(t *testing.T) {
		eks := covOverlay("eks", &Criteria{Service: CriteriaServiceEKS}, "")
		ubuntuKf := covOverlay("eks-ubuntu-kubeflow", &Criteria{
			Service: CriteriaServiceEKS, OS: CriteriaOSUbuntu,
			Platform: CriteriaPlatformKubeflow}, "eks")
		rhelKf := covOverlay("eks-rhel-kubeflow", &Criteria{
			Service: CriteriaServiceEKS, OS: CriteriaOSRHEL,
			Platform: CriteriaPlatformKubeflow}, "eks")
		single := covStore(t, eks, ubuntuKf, rhelKf)

		err := single.verifyCriteriaCoverage(
			&Criteria{Service: CriteriaServiceEKS, Platform: CriteriaPlatformKubeflow},
			[]string{baseRecipeName, "eks"}, nil, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "requires os (valid: rhel, ubuntu)") {
			t.Errorf("message missing sorted multi-value list:\n  got: %q\n  want to contain: %q",
				err.Error(), "requires os (valid: rhel, ubuntu)")
		}
	})
}
