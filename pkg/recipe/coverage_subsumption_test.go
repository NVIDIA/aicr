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
	stderrors "errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// retiredOSGuard is the deleted requireOSIfNeeded, preserved verbatim as a
// TEST-ONLY oracle.
//
// Issue #1782 asks for a rule that "genuinely subsumes the guard, then delete
// it". Subsumption is a property between two predicates, so asserting it
// against a restatement of the guard would prove nothing — the real one is
// kept here and the property is checked against it directly. Do not call this
// from production code; it exists to be outlived.
func retiredOSGuard(s *MetadataStore, criteria *Criteria, overlays []*RecipeMetadata) bool {
	if criteria.Service == CriteriaServiceAny || criteria.Service == "" {
		return false
	}
	if criteria.OS != CriteriaOSAny && criteria.OS != "" {
		return false
	}
	accel := criteria.Accelerator
	if accel == CriteriaAcceleratorAny {
		accel = ""
	}
	for _, o := range overlays {
		if o.Spec.Criteria == nil {
			continue
		}
		c := o.Spec.Criteria
		if c.Service != criteria.Service {
			continue
		}
		overlayAccel := c.Accelerator
		if overlayAccel == CriteriaAcceleratorAny {
			overlayAccel = ""
		}
		if overlayAccel != accel {
			continue
		}
		return false
	}
	return len(retiredAvailableOS(s, criteria)) > 0
}

// retiredAvailableOS is the deleted availableOSForCriteria, likewise test-only.
func retiredAvailableOS(s *MetadataStore, criteria *Criteria) []string {
	seen := map[string]struct{}{}
	for _, overlay := range s.Overlays {
		c := overlay.Spec.Criteria
		if c == nil || c.OS == "" || c.OS == CriteriaOSAny {
			continue
		}
		queryCopy := *criteria
		queryCopy.OS = c.OS
		if c.Matches(&queryCopy) {
			seen[string(c.OS)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// TestJointSufficiencySubsumesRetiredGuard is the gate on issue #1782's actual
// requirement: every query the retired guard would have rejected must still be
// rejected. It runs over generated catalogs rather than only the embedded one,
// because the embedded catalog cannot produce the shape where the guard and
// per-dimension coverage disagree — that is the whole reason #1782 exists.
//
// The reverse containment is deliberately NOT asserted. The new rule is
// strictly stronger: it reads the subset from the query instead of hardcoding
// service+accelerator, so it rejects cases the guard let through.
func TestJointSufficiencySubsumesRetiredGuard(t *testing.T) {
	ctx := context.Background()

	values := map[string][]string{
		"service":     {"", "eks", "gke"},
		"accelerator": {"", "h100", "a100"},
		"intent":      {"", "training"},
		"os":          {"", "ubuntu", "cos"},
	}
	// Every overlay shape over the dimension values above, minus the
	// match-everything overlay (criteria nil is rejected upstream).
	shapes := []*Criteria{}
	for _, svc := range values["service"] {
		for _, acc := range values["accelerator"] {
			for _, intent := range values["intent"] {
				for _, os := range values["os"] {
					c := &Criteria{}
					setCriteriaDimension(c, "service", svc)
					setCriteriaDimension(c, "accelerator", acc)
					setCriteriaDimension(c, "intent", intent)
					setCriteriaDimension(c, "os", os)
					if c.Specificity() == 0 {
						continue
					}
					shapes = append(shapes, c)
				}
			}
		}
	}

	base := &RecipeMetadata{}
	base.Metadata.Name = testRecipeBase

	guardFired := 0
	// Catalogs of three overlays drawn from the shape space. Three is enough
	// to express the split-coverage catalog (service alone, accelerator alone,
	// both plus an os) that the guard was written for.
	for i := 0; i < len(shapes); i++ {
		for j := i + 1; j < len(shapes); j++ {
			for k := j + 1; k < len(shapes); k++ {
				overlays := map[string]*RecipeMetadata{}
				for n, c := range []*Criteria{shapes[i], shapes[j], shapes[k]} {
					o := &RecipeMetadata{}
					o.Metadata.Name = fmt.Sprintf("o%d", n)
					o.Spec.Criteria = c
					overlays[o.Metadata.Name] = o
				}
				store := &MetadataStore{Base: base, Overlays: overlays}

				for _, q := range shapes {
					query := *q
					matched := store.FindMatchingOverlays(&query)
					if !retiredOSGuard(store, &query, matched) {
						continue
					}
					guardFired++
					if _, err := store.BuildRecipeResult(ctx, &query); err == nil {
						t.Fatalf("SUBSUMPTION VIOLATED: retired guard rejected %s but resolution succeeded\n"+
							"  catalog: %s | %s | %s",
							query.String(), shapes[i].String(), shapes[j].String(), shapes[k].String())
					}
				}
			}
		}
	}
	if guardFired == 0 {
		t.Fatal("generated no catalog where the retired guard fires; the test proves nothing")
	}
	t.Logf("subsumption held: retired guard fired on %d (catalog, query) pairs, all still rejected", guardFired)
}

// TestStrictGapErrorRendering pins the strict-gap message and context shape
// directly, independent of catalog shape.
//
// Both are otherwise under-exercised. The embedded catalog produces zero
// joint-sufficiency failures — completeness always fires first — so
// testdata/coverage_golden.yaml has no strictDimensions entries and the
// golden helper's extraction branch never runs against real data. And the
// multi-value join executes only when one dimension has several reaching
// values, which the single-strict-dimension catalog cannot produce.
func TestStrictGapErrorRendering(t *testing.T) {
	criteria := &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100}

	tests := []struct {
		name         string
		gaps         []strictGap
		excluded     []ExcludedOverlay
		warnings     []ConstraintWarning
		wantContains []string
	}{
		{
			name:         "single value",
			gaps:         []strictGap{{dimension: string(FieldOS), validValues: []string{"ubuntu"}}},
			wantContains: []string{"specify os (valid: ubuntu)"},
		},
		{
			name:         "multiple values join with a comma",
			gaps:         []strictGap{{dimension: string(FieldOS), validValues: []string{"cos", "ubuntu"}}},
			wantContains: []string{"specify os (valid: cos, ubuntu)"},
		},
		{
			name: "multiple gaps join with a comma",
			gaps: []strictGap{
				{dimension: string(FieldOS), validValues: []string{"ubuntu"}},
				{dimension: string(FieldIntent), validValues: []string{"training"}},
			},
			wantContains: []string{"os (valid: ubuntu)", "intent (valid: training)"},
		},
		{
			// On the evaluator path a covering overlay may have been removed by
			// a failing constraint. The demand still stands, but the caller
			// needs the exclusion context or they will state the os and only
			// then meet the real failure.
			name: "constraint context is attached",
			gaps: []strictGap{{dimension: string(FieldOS), validValues: []string{"ubuntu"}}},
			excluded: []ExcludedOverlay{{
				Name:   "h100-eks-ubuntu-training",
				Reason: ExcludedOverlayReasonConstraintFailed,
			}},
			warnings: []ConstraintWarning{{
				Overlay:    "h100-eks-ubuntu-training",
				Constraint: "K8s.server.version",
				Expected:   ">= 1.34",
				Actual:     "1.30",
				Reason:     "constraint not satisfied",
			}},
			wantContains: []string{"specify os (valid: ubuntu)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := strictGapError(criteria, tt.gaps, tt.excluded, tt.warnings)
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message = %q, want it to contain %q", err.Error(), want)
				}
			}

			var se *aicrerrors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %v", err)
			}
			if se.Context["uncovered"] != nil {
				t.Error("strict-gap failure must not populate `uncovered`; relaxation would clear it")
			}

			// Assert the whole strictDimensions payload, not just the keys: a
			// caller reconstructing a retry query reads validValues, so a
			// regression that dropped or reordered them would otherwise pass.
			entries, ok := se.Context["strictDimensions"].([]map[string]any)
			if !ok {
				t.Fatalf("strictDimensions = %#v, want []map[string]any", se.Context["strictDimensions"])
			}
			if len(entries) != len(tt.gaps) {
				t.Fatalf("strictDimensions has %d entries, want %d", len(entries), len(tt.gaps))
			}
			for i, gap := range tt.gaps {
				if entries[i]["dimension"] != gap.dimension {
					t.Errorf("entry %d dimension = %v, want %v", i, entries[i]["dimension"], gap.dimension)
				}
				values, ok := entries[i]["validValues"].([]string)
				if !ok {
					t.Errorf("entry %d validValues = %#v, want []string", i, entries[i]["validValues"])
					continue
				}
				if !equalStrings(values, gap.validValues) {
					t.Errorf("entry %d validValues = %v, want %v", i, values, gap.validValues)
				}
			}

			// excludedOverlays / constraintWarnings are attached verbatim, and
			// absent rather than empty when there is nothing to report.
			if tt.excluded == nil {
				// Two-value lookup: a key present but holding nil is a
				// different wire shape from an absent key, and `!= nil`
				// cannot tell them apart.
				if v, present := se.Context["excludedOverlays"]; present {
					t.Errorf("excludedOverlays = %#v, want the key absent", v)
				}
			} else if got, ok := se.Context["excludedOverlays"].([]ExcludedOverlay); !ok {
				t.Errorf("excludedOverlays = %#v, want []ExcludedOverlay", se.Context["excludedOverlays"])
			} else if !reflect.DeepEqual(got, tt.excluded) {
				t.Errorf("excludedOverlays = %+v, want %+v", got, tt.excluded)
			}

			if tt.warnings == nil {
				if v, present := se.Context["constraintWarnings"]; present {
					t.Errorf("constraintWarnings = %#v, want the key absent", v)
				}
			} else if got, ok := se.Context["constraintWarnings"].([]ConstraintWarning); !ok {
				t.Errorf("constraintWarnings = %#v, want []ConstraintWarning", se.Context["constraintWarnings"])
			} else if !reflect.DeepEqual(got, tt.warnings) {
				t.Errorf("constraintWarnings = %+v, want %+v", got, tt.warnings)
			}

			// The golden matrix classifies via this extraction, so exercise it
			// here rather than relying on catalog shape to reach it.
			got := strictDimensionsFromError(err)
			want := make([]string, 0, len(tt.gaps))
			for _, g := range tt.gaps {
				want = append(want, g.dimension)
			}
			if !equalStrings(got, want) {
				t.Errorf("strictDimensionsFromError = %v, want %v", got, want)
			}
		})
	}
}
