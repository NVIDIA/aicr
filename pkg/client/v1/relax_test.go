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

package aicr

import (
	"bytes"
	stderrors "errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// dimNames projects extracted entries to their names for order-sensitive
// comparison.
func dimNames(dims []uncoveredDimension) []CriteriaDimension {
	if dims == nil {
		return nil
	}
	out := make([]CriteriaDimension, 0, len(dims))
	for _, d := range dims {
		out = append(out, d.name)
	}
	return out
}

// statedSet builds a statedDimensionSet from dimension names, panicking on an
// unknown one so a typo in a test fixture surfaces as a failure rather than a
// silently-empty set (which would make a never-relax-stated case pass for the
// wrong reason).
func statedSet(dims ...CriteriaDimension) statedDimensionSet {
	var s statedDimensionSet
	for _, d := range dims {
		next, ok := s.with(d)
		if !ok {
			panic("statedSet: unknown dimension " + string(d))
		}
		s = next
	}
	return s
}

// mkCoverageErr builds an error shaped exactly like pkg/recipe/coverage.go's
// verifyCriteriaCoverage output: ErrCodeInvalidRequest with a
// Context["uncovered"] entry per dimension name.
func mkCoverageErr(dims ...CriteriaDimension) error {
	entries := make([]map[string]any, 0, len(dims))
	for _, d := range dims {
		entries = append(entries, map[string]any{
			"dimension":          string(d),
			"requestedValue":     "whatever",
			"validCompletions":   []map[string]string{},
			"constraintExcluded": false,
		})
	}
	return errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
		"uncovered": entries,
	})
}

// mkConstraintExcludedErr shapes a coverage failure where the dimension's only
// provider was removed by constraint evaluation — the fail-open direction.
func mkConstraintExcludedErr(dim CriteriaDimension) error {
	return errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
		"uncovered": []map[string]any{{
			"dimension":          string(dim),
			"requestedValue":     "whatever",
			"validCompletions":   []map[string]string{},
			"constraintExcluded": true,
		}},
		"excludedOverlays": []map[string]any{{"name": "some-overlay", "reason": "constraint-failed"}},
	})
}

// TestCriteriaDimensionNamesMatchRecipe pins the facade's dimension vocabulary
// against pkg/recipe's canonical list. These names are hand-declared here (they
// are part of this package's public API) but must match the strings that appear
// as details.uncovered[].dimension, because relaxation switches on them.
//
// A mismatch would be silent and unsafe in one specific direction: a stated
// dimension whose facade constant no longer matches the reported name never
// gets marked as stated, so WithSnapshotCriteriaRelaxation would clear a value
// the caller explicitly asked for.
func TestCriteriaDimensionNamesMatchRecipe(t *testing.T) {
	want := recipe.CoverageDimensionNames()

	got := make([]string, 0, len(AllCriteriaDimensions()))
	for _, dim := range AllCriteriaDimensions() {
		got = append(got, string(dim))
	}

	if !slices.Equal(got, want) {
		t.Fatalf("AllCriteriaDimensions() = %v, want %v (pkg/recipe.CoverageDimensionNames); "+
			"the facade's dimension constants have drifted from the coverage error's vocabulary",
			got, want)
	}
}

func TestRelaxDerivedCoverage(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		criteria    *recipe.Criteria
		stated      statedDimensionSet
		wantOK      bool
		wantCleared []CriteriaDimension
	}{
		{
			name: "derived-only uncovered dimension relaxes",
			err:  mkCoverageErr(DimensionOS),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
			},
			stated:      statedSet(),
			wantOK:      true,
			wantCleared: []CriteriaDimension{DimensionOS},
		},
		{
			name:     "stated uncovered dimension refuses to relax",
			err:      mkCoverageErr(DimensionOS),
			criteria: &recipe.Criteria{OS: recipe.CriteriaOSType("ubuntu")},
			stated:   statedSet(DimensionOS),
			wantOK:   false,
		},
		{
			name:     "mixed stated and derived refuses to relax",
			err:      mkCoverageErr(DimensionOS, DimensionAccelerator),
			criteria: recipe.NewCriteria(),
			stated:   statedSet(DimensionAccelerator),
			wantOK:   false,
		},
		{
			// service is retained (not uncovered), so a coverage dimension
			// survives the clear — without it this case would collapse and be
			// refused by the generic-fallback guard.
			name: "multiple derived dimensions all relax",
			err:  mkCoverageErr(DimensionOS, DimensionIntent),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
				Intent:  recipe.CriteriaIntentType("training"),
			},
			stated:      statedSet(),
			wantOK:      true,
			wantCleared: []CriteriaDimension{DimensionOS, DimensionIntent},
		},
		{
			name: "coverage error wrapped with a different code still relaxes",
			err: errors.Wrap(errors.ErrCodeInternal, "resolve failed",
				mkCoverageErr(DimensionOS)),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
			},
			stated:      statedSet(),
			wantOK:      true,
			wantCleared: []CriteriaDimension{DimensionOS},
		},
		{
			name:     "non-coverage InvalidRequest error does not retry",
			err:      errors.New(errors.ErrCodeInvalidRequest, "some other invalid request"),
			criteria: recipe.NewCriteria(),
			stated:   statedSet(),
			wantOK:   false,
		},
		{
			name:     "nil error does not retry",
			err:      nil,
			criteria: recipe.NewCriteria(),
			stated:   statedSet(),
			wantOK:   false,
		},
		{
			// The fail-open this guard exists for: the dimension IS carried by
			// an overlay, which the observed cluster's constraints excluded.
			// Relaxing turns "your cluster fails this overlay" into a pass.
			name: "constraint-excluded dimension refuses to relax",
			err:  mkConstraintExcludedErr(DimensionService),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
			},
			stated: statedSet(),
			wantOK: false,
		},
		{
			// Ambiguity fails closed: no per-dimension attribution, but the
			// resolve did exclude overlays, so the cause cannot be ruled out.
			name: "missing attribution with exclusions present refuses to relax",
			err: errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
				"uncovered":        []map[string]any{{"dimension": string(DimensionOS)}},
				"excludedOverlays": []map[string]any{{"name": "x", "reason": "constraint-failed"}},
			}),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
			},
			stated: statedSet(),
			wantOK: false,
		},
		{
			// Same missing attribution, but nothing was excluded, so the
			// dimension is genuinely catalog-agnostic and safe to relax.
			name: "missing attribution with no exclusions still relaxes",
			err: errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
				"uncovered": []map[string]any{{"dimension": string(DimensionOS)}},
			}),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
			},
			stated:      statedSet(),
			wantOK:      true,
			wantCleared: []CriteriaDimension{DimensionOS},
		},
		{
			// Relaxing the only stated dimension leaves nothing that selects an
			// overlay, so the retry resolves the generic fallback at exit 0
			// (#1888).
			name:     "relaxation collapsing to no coverage dimension is refused",
			err:      mkCoverageErr(DimensionOS),
			criteria: &recipe.Criteria{OS: recipe.CriteriaOSType("ubuntu")},
			stated:   statedSet(),
			wantOK:   false,
		},
		{
			// Same collapse, but with the nodes value fingerprint.ToCriteria
			// sets from node-count topology. nodes scores a Specificity point
			// while gating no overlay (#1781), so a Specificity()-based guard
			// is skipped here and the retry resolves criteria(nodes=8) into
			// the generic fallback. The guard must count coverage dimensions.
			name: "collapse guard is not fooled by a surviving nodes value",
			err:  mkCoverageErr(DimensionOS),
			criteria: &recipe.Criteria{
				OS:    recipe.CriteriaOSType("ubuntu"),
				Nodes: 8,
			},
			stated: statedSet(),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			original := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(original)

			// Guard against the copy-not-mutate contract: the caller's criteria
			// must be untouched so a refused relaxation leaves nothing behind.
			before := *tt.criteria

			relaxed, cleared, ok := relaxDerivedCoverage(tt.err, tt.criteria, tt.stated)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if *tt.criteria != before {
				t.Errorf("input criteria mutated: got %+v, want %+v", *tt.criteria, before)
			}
			if !tt.wantOK {
				if relaxed != nil {
					t.Errorf("relaxed = %+v, want nil when ok=false", relaxed)
				}
				if cleared != nil {
					t.Errorf("cleared = %v, want nil when ok=false", cleared)
				}
				if strings.Contains(buf.String(), "relaxing snapshot-detected") {
					t.Errorf("unexpected relax warning logged: %q", buf.String())
				}
				return
			}

			if !slices.Equal(cleared, tt.wantCleared) {
				t.Errorf("cleared = %v, want %v", cleared, tt.wantCleared)
			}
			for _, dim := range tt.wantCleared {
				if got := criteriaDimensionValue(relaxed, dim); got != recipe.CriteriaAnyValue {
					t.Errorf("dimension %q = %q, want unstated (%q)", dim, got, recipe.CriteriaAnyValue)
				}
			}
			out := buf.String()
			if !strings.Contains(out, "level=WARN") {
				t.Errorf("expected a WARN log for relaxation, got: %q", out)
			}
			for _, dim := range tt.wantCleared {
				if !strings.Contains(out, string(dim)) {
					t.Errorf("expected relax warning to name dimension %q, got: %q", dim, out)
				}
			}
		})
	}
}

// TestUncoveredCoverageDimensions pins the extraction contract directly: the
// coverage error must be found anywhere in the wrap chain (not just as the
// outermost StructuredError), and the uncovered payload must be accepted in
// both its in-process shape ([]map[string]any) and its decoded-JSON shape
// ([]any of map[string]any).
func TestUncoveredCoverageDimensions(t *testing.T) {
	jsonShaped := errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
		"uncovered": []any{
			map[string]any{"dimension": string(DimensionOS)},
			map[string]any{"dimension": string(DimensionIntent)},
		},
	})

	tests := []struct {
		name string
		err  error
		want []CriteriaDimension
	}{
		{
			name: "unwrapped coverage error",
			err:  mkCoverageErr(DimensionOS),
			want: []CriteriaDimension{DimensionOS},
		},
		{
			name: "coverage error wrapped with a different code",
			err: errors.Wrap(errors.ErrCodeInternal, "resolve failed",
				mkCoverageErr(DimensionOS, DimensionService)),
			want: []CriteriaDimension{DimensionOS, DimensionService},
		},
		{
			name: "coverage error wrapped with the same code",
			err: errors.Wrap(errors.ErrCodeInvalidRequest, "resolve failed",
				mkCoverageErr(DimensionAccelerator)),
			want: []CriteriaDimension{DimensionAccelerator},
		},
		{
			name: "decoded-JSON payload shape",
			err:  jsonShaped,
			want: []CriteriaDimension{DimensionOS, DimensionIntent},
		},
		{
			name: "InvalidRequest without uncovered context",
			err:  errors.New(errors.ErrCodeInvalidRequest, "bad flag"),
			want: nil,
		},
		{
			name: "uncovered entries missing dimension keys",
			err: errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
				"uncovered": []map[string]any{{"requestedValue": "ubuntu"}},
			}),
			want: nil,
		},
		{
			// An unknown name cannot be cleared, so reporting it would claim a
			// relaxation that did not happen.
			name: "unrecognized dimension name is skipped",
			err: errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
				"uncovered": []map[string]any{{"dimension": "nodes"}},
			}),
			want: nil,
		},
		{
			name: "nil error",
			err:  nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dimNames(uncoveredCoverageDimensions(tt.err))
			if !slices.Equal(got, tt.want) {
				t.Errorf("uncoveredCoverageDimensions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithSnapshotCriteriaRelaxation_OptionConfig(t *testing.T) {
	tests := []struct {
		name       string
		stated     []CriteriaDimension
		wantErr    bool
		wantStated statedDimensionSet
	}{
		{
			// The all-derived case: enabling the policy must not depend on a
			// non-empty argument list, or the most common query silently gets
			// strict resolution.
			name:       "no dimensions enables relaxation with an empty stated set",
			stated:     nil,
			wantStated: statedSet(),
		},
		{
			name:       "stated dimensions are recorded",
			stated:     []CriteriaDimension{DimensionOS, DimensionService},
			wantStated: statedSet(DimensionOS, DimensionService),
		},
		{
			name:    "unknown dimension is rejected",
			stated:  []CriteriaDimension{"OS"},
			wantErr: true,
		},
		{
			name:    "nodes is not a coverage dimension",
			stated:  []CriteriaDimension{"nodes"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := resolveRecipeConfig(WithSnapshotCriteriaRelaxation(tt.stated...))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unknown dimension, got nil")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRecipeConfig: %v", err)
			}
			if !cfg.relaxDerived {
				t.Error("relaxDerived = false, want true whenever the option is passed")
			}
			if cfg.stated != tt.wantStated {
				t.Fatalf("stated = %08b, want %08b", cfg.stated, tt.wantStated)
			}
		})
	}
}

// kindUbuntuSnapshot is a minimal snapshot that fingerprints to
// service=kind + os=ubuntu. The embedded catalog's kind overlay tree states no
// os, so a strict snapshot resolve fails the coverage post-condition on os —
// the exact "derived but undistinguished" case relaxation exists for.
func kindUbuntuSnapshot() *snapshotter.Snapshot {
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: "K8s",
				Subtypes: []measurement.Subtype{
					{
						Name: "node",
						Data: map[string]measurement.Reading{
							"provider": measurement.Str("kind"),
						},
					},
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("1.33.0"),
						},
					},
				},
			},
			{
				Type: "OS",
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID": measurement.Str("ubuntu"),
						},
					},
				},
			},
		},
	}
}

// constraintFailingKindSnapshot fingerprints to service=kind on a Kubernetes
// version below the kind overlay's `K8s.server.version >= 1.32` constraint, so
// constraint evaluation excludes the only overlay that covers service=kind.
func constraintFailingKindSnapshot() *snapshotter.Snapshot {
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: "K8s",
				Subtypes: []measurement.Subtype{
					{
						Name: "node",
						Data: map[string]measurement.Reading{
							"provider": measurement.Str("kind"),
						},
					},
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("1.20.0"),
						},
					},
				},
			},
		},
	}
}

// TestResolveRecipeFromSnapshot_ConstraintExcludedIsNotRelaxed pins the
// fail-open direction: a cluster whose Kubernetes version fails the matching
// overlay's constraint must NOT be handed a broader recipe that resolves
// cleanly.
//
// Before this guard, the derived service=kind was cleared and the retry
// resolved criteria(any) into an 11-component generic recipe at exit 0, while
// the identical query with service stated explicitly correctly failed. The
// operator's cluster is too old either way; only one of those answers says so.
func TestResolveRecipeFromSnapshot_ConstraintExcludedIsNotRelaxed(t *testing.T) {
	client := newEmbeddedTestClient(t)
	snap := constraintFailingKindSnapshot()

	criteria := fingerprint.FromMeasurements(snap.Measurements).ToCriteria(client.CriteriaRegistry())
	if criteria.Service != recipe.CriteriaServiceKind {
		t.Fatalf("fingerprint service = %q, want kind", criteria.Service)
	}

	strictErr := func(t *testing.T) error {
		t.Helper()
		_, err := client.ResolveRecipeFromSnapshot(t.Context(), WrapCriteria(criteria), WrapSnapshot(snap))
		if err == nil {
			t.Fatal("strict resolve succeeded on a constraint-failing cluster; " +
				"did the kind overlay's version constraint change?")
		}
		return err
	}

	t.Run("strict resolve reports the constraint exclusion", func(t *testing.T) {
		err := strictErr(t)
		var se *errors.StructuredError
		if !stderrors.As(err, &se) {
			t.Fatalf("error is not structured: %v", err)
		}
		if se.Context["excludedOverlays"] == nil {
			t.Fatalf("coverage error carries no excludedOverlays; attribution would be lost: %v", err)
		}
		dims := uncoveredCoverageDimensions(err)
		if len(dims) != 1 || dims[0].name != DimensionService {
			t.Fatalf("uncovered = %v, want a single service entry: %v", dims, err)
		}
		if !dims[0].constraintExcluded {
			t.Error("service is not marked constraintExcluded; pkg/recipe's attribution has drifted")
		}
	})

	t.Run("relaxation refuses and preserves the original error", func(t *testing.T) {
		result, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(criteria), WrapSnapshot(snap),
			WithSnapshotCriteriaRelaxation())
		if err == nil {
			t.Fatalf("relaxation produced a recipe (%q, %d components, relaxed=%v) for a cluster "+
				"that fails the overlay's constraints; the failure was silently discarded",
				result.Name, len(result.Components), result.RelaxedDimensions)
		}
		// Same error the strict path returns — relaxation must not reword or
		// reclassify the constraint failure.
		if err.Error() != strictErr(t).Error() {
			t.Errorf("relaxed error differs from strict error:\n got: %v\nwant: %v", err, strictErr(t))
		}
	})

	// Isolates the constraint-excluded guard from the generic-fallback guard.
	// Adding a detected h100 keeps accelerator stated, so clearing the
	// constraint-excluded service still leaves criteria(accelerator=h100) —
	// specificity 1, which sails past the collapse check. Only the
	// constraint-excluded guard refuses here; with it disabled this resolves
	// an 11-component recipe.
	t.Run("refuses even when specificity survives the clear", func(t *testing.T) {
		withGPU := constraintFailingKindSnapshot()
		withGPU.Measurements = append(withGPU.Measurements, &measurement.Measurement{
			Type: "GPU",
			Subtypes: []measurement.Subtype{
				{
					Name: "hardware",
					Data: map[string]measurement.Reading{"model": measurement.Str("h100")},
				},
			},
		})

		gpuCriteria := fingerprint.FromMeasurements(withGPU.Measurements).ToCriteria(client.CriteriaRegistry())
		if gpuCriteria.Specificity() < 2 {
			t.Fatalf("criteria %s has specificity %d, want >= 2 so the clear cannot collapse to any",
				gpuCriteria.String(), gpuCriteria.Specificity())
		}

		result, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(gpuCriteria), WrapSnapshot(withGPU),
			WithSnapshotCriteriaRelaxation())
		if err == nil {
			t.Fatalf("relaxation produced %q (%d components, relaxed=%v); a constraint-excluded "+
				"dimension was relaxed because specificity survived",
				result.Name, len(result.Components), result.RelaxedDimensions)
		}
	})

	// The asymmetry that made the bug visible: stating the same value
	// explicitly always failed. Both paths must now agree.
	t.Run("agrees with an explicitly stated dimension", func(t *testing.T) {
		stated := recipe.NewCriteria()
		stated.Service = recipe.CriteriaServiceKind
		_, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(stated), WrapSnapshot(snap),
			WithSnapshotCriteriaRelaxation(DimensionService))
		if err == nil {
			t.Fatal("explicitly stated service=kind resolved on a constraint-failing cluster")
		}
	})
}

func newEmbeddedTestClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(
		WithRecipeSource(EmbeddedSource()),
		WithVersion("v-test"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if loadErr := client.LoadCatalog(t.Context()); loadErr != nil {
		t.Fatalf("LoadCatalog: %v", loadErr)
	}
	return client
}

// TestResolveRecipeFromSnapshot_Relaxation drives the policy end to end against
// a REAL builder error rather than the synthetic mkCoverageErr shape. This pins
// the error-shape contract uncoveredCoverageDimensions depends on: if the
// builder ever re-wraps the coverage error in a way extraction cannot see, the
// relax case fails here instead of relaxation silently disabling (issue #1542
// follow-up from the PR #1784 review, moved to the facade in #2027).
func TestResolveRecipeFromSnapshot_Relaxation(t *testing.T) {
	client := newEmbeddedTestClient(t)
	snap := kindUbuntuSnapshot()

	criteria := fingerprint.FromMeasurements(snap.Measurements).ToCriteria(client.CriteriaRegistry())
	if criteria.Service != recipe.CriteriaServiceKind || criteria.OS != recipe.CriteriaOSUbuntu {
		t.Fatalf("fingerprint = service=%q os=%q, want service=kind os=ubuntu", criteria.Service, criteria.OS)
	}

	t.Run("strict without the option", func(t *testing.T) {
		_, err := client.ResolveRecipeFromSnapshot(t.Context(), WrapCriteria(criteria), WrapSnapshot(snap))
		if err == nil {
			t.Fatal("expected a coverage failure for snapshot-derived os on the kind overlay tree, " +
				"got success — did a kind overlay gain os coverage?")
		}
		if got := dimNames(uncoveredCoverageDimensions(err)); !slices.Equal(got, []CriteriaDimension{DimensionOS}) {
			t.Fatalf("uncovered = %v, want [%s]; the coverage error shape may have drifted "+
				"from pkg/recipe/coverage.go: %v", got, DimensionOS, err)
		}
	})

	t.Run("relaxes a derived dimension and reports it", func(t *testing.T) {
		result, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(criteria), WrapSnapshot(snap),
			WithSnapshotCriteriaRelaxation())
		if err != nil {
			t.Fatalf("resolve with relaxation: %v", err)
		}
		if len(result.Components) == 0 {
			t.Error("relaxed resolve produced an empty recipe")
		}
		if !slices.Equal(result.RelaxedDimensions, []CriteriaDimension{DimensionOS}) {
			t.Errorf("RelaxedDimensions = %v, want [%s]", result.RelaxedDimensions, DimensionOS)
		}
	})

	// The invariant: naming os as stated must prevent the very relaxation the
	// subtest above proves is otherwise available.
	t.Run("never relaxes a stated dimension", func(t *testing.T) {
		_, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(criteria), WrapSnapshot(snap),
			WithSnapshotCriteriaRelaxation(DimensionOS))
		if err == nil {
			t.Fatal("resolve succeeded with os stated; a caller-stated dimension was relaxed")
		}
		if got := dimNames(uncoveredCoverageDimensions(err)); !slices.Equal(got, []CriteriaDimension{DimensionOS}) {
			t.Fatalf("uncovered = %v, want the original coverage error naming [%s]: %v",
				got, DimensionOS, err)
		}
	})

	// Stating an unrelated dimension leaves os derived, so relaxation proceeds.
	t.Run("stating an unrelated dimension still relaxes os", func(t *testing.T) {
		result, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(criteria), WrapSnapshot(snap),
			WithSnapshotCriteriaRelaxation(DimensionService, DimensionIntent))
		if err != nil {
			t.Fatalf("resolve with relaxation: %v", err)
		}
		if !slices.Equal(result.RelaxedDimensions, []CriteriaDimension{DimensionOS}) {
			t.Errorf("RelaxedDimensions = %v, want [%s]", result.RelaxedDimensions, DimensionOS)
		}
	})

	// A successful first attempt must not claim a relaxation happened.
	t.Run("successful resolve reports no relaxed dimensions", func(t *testing.T) {
		plain := recipe.NewCriteria()
		plain.Service = recipe.CriteriaServiceKind
		result, err := client.ResolveRecipeFromSnapshotWithOptions(
			t.Context(), WrapCriteria(plain), WrapSnapshot(snap),
			WithSnapshotCriteriaRelaxation())
		if err != nil {
			t.Fatalf("resolve service=kind: %v", err)
		}
		if len(result.RelaxedDimensions) != 0 {
			t.Errorf("RelaxedDimensions = %v, want empty when the first attempt succeeds",
				result.RelaxedDimensions)
		}
	})
}

// TestWithSnapshotCriteriaRelaxation_RejectedOnCriteriaPath pins that the
// option fails loudly where it has no meaning. Ignoring it would leave a caller
// believing they had `--snapshot` semantics; worse, with an empty stated set
// there is nothing to stop it clearing dimensions the caller typed.
func TestWithSnapshotCriteriaRelaxation_RejectedOnCriteriaPath(t *testing.T) {
	client := newEmbeddedTestClient(t)

	criteria := recipe.NewCriteria()
	criteria.Service = recipe.CriteriaServiceKind

	_, err := client.ResolveRecipeFromCriteriaWithOptions(
		t.Context(), WrapCriteria(criteria), WithSnapshotCriteriaRelaxation())
	if err == nil {
		t.Fatal("expected WithSnapshotCriteriaRelaxation to be rejected on the criteria-only path")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "snapshot resolve path") {
		t.Errorf("error message should explain where the option is valid, got: %v", err)
	}

	// Without the option the same resolve succeeds, so the rejection above is
	// attributable to the option rather than to the criteria.
	if _, err := client.ResolveRecipeFromCriteria(t.Context(), WrapCriteria(criteria)); err != nil {
		t.Fatalf("criteria-only resolve without the option: %v", err)
	}
}
