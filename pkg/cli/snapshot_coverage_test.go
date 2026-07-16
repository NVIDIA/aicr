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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
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
