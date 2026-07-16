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

package cli

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// mkCoverageErr builds an error shaped exactly like
// pkg/recipe/coverage.go's verifyCriteriaCoverage output: ErrCodeInvalidRequest
// with a Context["uncovered"] entry per dimension name.
func mkCoverageErr(dims ...string) error {
	entries := make([]map[string]any, 0, len(dims))
	for _, d := range dims {
		entries = append(entries, map[string]any{
			"dimension":        d,
			"requestedValue":   "whatever",
			"validCompletions": []map[string]string{},
		})
	}
	return errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
		"uncovered": entries,
	})
}

func TestRelaxSnapshotDerivedCoverage(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		criteria    *recipe.Criteria
		touched     map[string]bool
		wantOK      bool
		wantCleared []string // dims expected to be reset to CriteriaAnyValue on success
	}{
		{
			name: "derived-only uncovered dimension relaxes",
			err:  mkCoverageErr(coverageDimOS),
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceType("kind"),
				Accelerator: recipe.CriteriaAcceleratorType("h100"),
				Intent:      recipe.CriteriaIntentType("inference"),
				OS:          recipe.CriteriaOSType("ubuntu"),
				Platform:    recipe.CriteriaPlatformType("dynamo"),
			},
			touched:     map[string]bool{}, // nothing user-stated
			wantOK:      true,
			wantCleared: []string{coverageDimOS},
		},
		{
			name: "user-stated uncovered dimension propagates error",
			err:  mkCoverageErr(coverageDimIntent),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				Intent:  recipe.CriteriaIntentType("training"),
			},
			touched: map[string]bool{coverageDimIntent: true},
			wantOK:  false,
		},
		{
			name: "mixed derived and stated uncovered propagates error",
			err:  mkCoverageErr(coverageDimOS, coverageDimAccelerator),
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceType("kind"),
				Accelerator: recipe.CriteriaAcceleratorType("h100"),
				OS:          recipe.CriteriaOSType("ubuntu"),
			},
			// accelerator was user-stated (--accelerator h100); os was derived.
			touched: map[string]bool{coverageDimAccelerator: true},
			wantOK:  false,
		},
		{
			name: "wrapped coverage error still relaxes",
			// A future wrap between the builder and the CLI (e.g. an
			// ErrCodeInternal decoration at the facade) must not silently
			// disable relaxation — extraction walks the wrap chain.
			err: errors.Wrap(errors.ErrCodeInternal, "recipe resolution failed",
				mkCoverageErr(coverageDimOS)),
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceType("kind"),
				OS:      recipe.CriteriaOSType("ubuntu"),
			},
			touched:     map[string]bool{},
			wantOK:      true,
			wantCleared: []string{coverageDimOS},
		},
		{
			name:     "non-coverage InvalidRequest error does not retry",
			err:      errors.New(errors.ErrCodeInvalidRequest, "some other invalid request"),
			criteria: recipe.NewCriteria(),
			touched:  map[string]bool{},
			wantOK:   false,
		},
		{
			name:     "nil error does not retry",
			err:      nil,
			criteria: recipe.NewCriteria(),
			touched:  map[string]bool{},
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			original := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(original)

			relaxed, ok := relaxSnapshotDerivedCoverage(tt.err, tt.criteria, tt.touched)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if relaxed != nil {
					t.Errorf("relaxed = %+v, want nil when ok=false", relaxed)
				}
				if strings.Contains(buf.String(), "relaxing snapshot-detected") {
					t.Errorf("unexpected relax warning logged: %q", buf.String())
				}
				return
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
				if !strings.Contains(out, dim) {
					t.Errorf("expected relax warning to name dimension %q, got: %q", dim, out)
				}
			}
		})
	}
}

// TestUncoveredCoverageDimensions pins the extraction contract directly:
// the coverage error must be found anywhere in the wrap chain (not just as
// the outermost StructuredError), and the uncovered payload must be accepted
// in both its in-process shape ([]map[string]any) and its decoded-JSON shape
// ([]any of map[string]any).
func TestUncoveredCoverageDimensions(t *testing.T) {
	jsonShaped := errors.NewWithContext(errors.ErrCodeInvalidRequest, "coverage failed", map[string]any{
		"uncovered": []any{
			map[string]any{"dimension": coverageDimOS},
			map[string]any{"dimension": coverageDimIntent},
		},
	})

	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "unwrapped coverage error",
			err:  mkCoverageErr(coverageDimOS),
			want: []string{coverageDimOS},
		},
		{
			name: "coverage error wrapped with a different code",
			err: errors.Wrap(errors.ErrCodeInternal, "resolve failed",
				mkCoverageErr(coverageDimOS, coverageDimService)),
			want: []string{coverageDimOS, coverageDimService},
		},
		{
			name: "coverage error wrapped with the same code",
			err: errors.Wrap(errors.ErrCodeInvalidRequest, "resolve failed",
				mkCoverageErr(coverageDimAccelerator)),
			want: []string{coverageDimAccelerator},
		},
		{
			name: "decoded-JSON payload shape",
			err:  jsonShaped,
			want: []string{coverageDimOS, coverageDimIntent},
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
			name: "nil error",
			err:  nil,
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uncoveredCoverageDimensions(tt.err)
			if !slices.Equal(got, tt.want) {
				t.Errorf("uncoveredCoverageDimensions() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRelaxSnapshotDerivedCoverage_RealResolutionError drives the snapshot
// relax flow of buildRecipeFromCmdWithConfig against a REAL builder error
// rather than the synthetic mkCoverageErr shape: a snapshot fingerprinting
// to service=kind + os=ubuntu fails ResolveRecipeFromSnapshot's coverage
// post-condition on the embedded catalog (no kind overlay states os), the
// snapshot-derived os is relaxed, and the retry resolves. This pins the
// error-shape contract uncoveredCoverageDimensions depends on — if the
// facade or builder ever re-wraps the coverage error in a way extraction
// cannot see, this test fails instead of relaxation silently disabling
// (issue #1542 follow-up from the PR #1784 review).
func TestRelaxSnapshotDerivedCoverage_RealResolutionError(t *testing.T) {
	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion("v-test"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if loadErr := client.LoadCatalog(t.Context()); loadErr != nil {
		t.Fatalf("LoadCatalog: %v", loadErr)
	}

	// Minimal snapshot: provider=kind (service), a server version satisfying
	// the kind overlay's K8s.server.version constraint, and an OS release
	// that fingerprints to ubuntu — the dimension no kind overlay covers.
	snap := &snapshotter.Snapshot{
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

	criteria := fingerprint.FromMeasurements(snap.Measurements).ToCriteria(client.CriteriaRegistry())
	if criteria.Service != recipe.CriteriaServiceKind || criteria.OS != recipe.CriteriaOSUbuntu {
		t.Fatalf("fingerprint = service=%q os=%q, want service=kind os=ubuntu", criteria.Service, criteria.OS)
	}

	_, err = client.ResolveRecipeFromSnapshot(t.Context(), aicr.WrapCriteria(criteria), aicr.WrapSnapshot(snap))
	if err == nil {
		t.Fatal("expected coverage failure for snapshot-derived os on the kind overlay tree, got success — did a kind overlay gain os coverage?")
	}

	uncovered := uncoveredCoverageDimensions(err)
	if !slices.Equal(uncovered, []string{coverageDimOS}) {
		t.Fatalf("uncoveredCoverageDimensions(real builder error) = %v, want [%s]; error shape may have drifted from pkg/recipe/coverage.go: %v",
			uncovered, coverageDimOS, err)
	}

	relaxed, ok := relaxSnapshotDerivedCoverage(err, criteria, map[string]bool{})
	if !ok {
		t.Fatalf("relaxSnapshotDerivedCoverage(real builder error) ok = false, want true: %v", err)
	}
	if got := string(relaxed.OS); got != recipe.CriteriaAnyValue {
		t.Fatalf("relaxed.OS = %q, want unstated (%q)", got, recipe.CriteriaAnyValue)
	}

	result, err := client.ResolveRecipeFromSnapshot(t.Context(), aicr.WrapCriteria(relaxed), aicr.WrapSnapshot(snap))
	if err != nil {
		t.Fatalf("retry after relaxing os failed: %v", err)
	}
	if result == nil || len(result.Components) == 0 {
		t.Fatal("retry after relax resolved an empty recipe")
	}
}
