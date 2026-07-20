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
	"os"
	"sort"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

const goldenPath = "testdata/coverage_golden.yaml"

// coverageClassification is the golden outcome for one projected query.
type coverageClassification struct {
	Outcome    string   `yaml:"outcome"`              // "success" | "error"
	Uncovered  []string `yaml:"uncovered,omitempty"`  // coverage errors
	RequiresOS bool     `yaml:"requiresOS,omitempty"` // requireOSIfNeeded guard errors
	// ValidCompletions pins the completion-suggestion content per uncovered
	// dimension (canonical tupleKey strings, in minimalTuples order) so a
	// regression in the suggestion machinery on the real catalog flips a
	// golden entry instead of shipping wrong or empty guidance.
	ValidCompletions map[string][]string `yaml:"validCompletions,omitempty"`
}

// TestCoverageGoldenMatrix pins the resolution outcome of every projection
// (subset of stated dimensions) of every overlay's criteria against the
// embedded data. A data change that flips ANY projection — a valid query
// starting to error, or an erroring one silently passing — fails this test
// and forces a deliberate golden update (issue #1542, design section 8).
// Regenerate: AICR_UPDATE_GOLDEN=1 go test ./pkg/recipe/ -run TestCoverageGoldenMatrix
func TestCoverageGoldenMatrix(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	overlayNames := make([]string, 0, len(store.Overlays))
	for name := range store.Overlays {
		overlayNames = append(overlayNames, name)
	}
	sort.Strings(overlayNames)

	got := map[string]coverageClassification{}
	for _, name := range overlayNames {
		overlay := store.Overlays[name]
		oc := overlay.Spec.Criteria
		if oc == nil {
			continue
		}
		stated := []coverageDimension{}
		for _, dim := range coverageDimensions {
			if isSpecifiedCriteriaValue(dim.value(oc)) {
				stated = append(stated, dim)
			}
		}
		// Every non-empty subset of the stated dimensions.
		for mask := 1; mask < (1 << len(stated)); mask++ {
			q := &Criteria{}
			for i, dim := range stated {
				if mask&(1<<i) == 0 {
					continue
				}
				setCriteriaDimension(q, dim.name, dim.value(oc))
			}
			key := q.String()
			if _, done := got[key]; done {
				continue // identical projection from another overlay
			}
			got[key] = classify(ctx, t, store, q)
		}
	}

	if os.Getenv("AICR_UPDATE_GOLDEN") == "1" {
		writeGolden(t, got)
		t.Logf("golden updated: %d projections", len(got))
		return
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with AICR_UPDATE_GOLDEN=1 to create): %v", err)
	}
	want := map[string]coverageClassification{}
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	keys := sortedCoverageKeys(got)
	for _, k := range keys {
		w, ok := want[k]
		if !ok {
			t.Errorf("projection %q missing from golden (new data?) — regenerate deliberately", k)
			continue
		}
		if w.Outcome != got[k].Outcome || !equalStrings(w.Uncovered, got[k].Uncovered) ||
			w.RequiresOS != got[k].RequiresOS || !equalCompletions(w.ValidCompletions, got[k].ValidCompletions) {

			t.Errorf("projection %q flipped: golden %+v, now %+v", k, w, got[k])
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("golden projection %q no longer produced (overlay removed?) — regenerate deliberately", k)
		}
	}
}

// TestEveryLeafCriteriaResolves is the full-leaf invariant: each overlay's
// complete criteria must resolve successfully — a leaf must never become
// unreachable through its own stated dimensions.
func TestEveryLeafCriteriaResolves(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			if _, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria); err != nil {
				t.Fatalf("full criteria of %s failed to resolve: %v", name, err)
			}
		})
	}
}

func classify(ctx context.Context, t *testing.T, store *MetadataStore, q *Criteria) coverageClassification {
	t.Helper()
	_, err := store.BuildRecipeResult(ctx, q)
	if err == nil {
		return coverageClassification{Outcome: "success"}
	}
	msg := err.Error()
	if strings.Contains(msg, "specify an OS") {
		return coverageClassification{Outcome: "error", RequiresOS: true}
	}
	uncovered, completions := coverageDetailsFromError(err)
	if len(uncovered) == 0 {
		t.Fatalf("projection %q failed with unexpected error: %v", q.String(), err)
	}
	return coverageClassification{Outcome: "error", Uncovered: uncovered, ValidCompletions: completions}
}

func setCriteriaDimension(c *Criteria, name, value string) {
	switch name {
	case "service":
		c.Service = CriteriaServiceType(value)
	case "accelerator":
		c.Accelerator = CriteriaAcceleratorType(value)
	case "intent":
		c.Intent = CriteriaIntentType(value)
	case "os":
		c.OS = CriteriaOSType(value)
	case "platform":
		c.Platform = CriteriaPlatformType(value)
	}
}

// coverageDetailsFromError extracts the uncovered dimension names and their
// completion suggestions (as canonical tupleKey strings, preserving
// minimalTuples order) from a StructuredError's "uncovered" context entries,
// or nils when err does not carry one (e.g., a non-coverage error).
func coverageDetailsFromError(err error) ([]string, map[string][]string) {
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) {
		return nil, nil
	}
	entries, ok := se.Context["uncovered"].([]map[string]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(entries))
	var completions map[string][]string
	for _, e := range entries {
		name, ok := e["dimension"].(string)
		if !ok {
			continue
		}
		names = append(names, name)
		tuples, _ := e["validCompletions"].([]map[string]string)
		if len(tuples) == 0 {
			continue
		}
		keys := make([]string, 0, len(tuples))
		for _, tuple := range tuples {
			keys = append(keys, tupleKey(tuple))
		}
		if completions == nil {
			completions = map[string][]string{}
		}
		completions[name] = keys
	}
	return names, completions
}

// writeGolden marshals got to goldenPath. yaml.v3 sorts map keys on encode,
// so the output is stable for diffing without an explicit sort here.
func writeGolden(t *testing.T, got map[string]coverageClassification) {
	t.Helper()
	data, err := yaml.Marshal(got)
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	if err := os.WriteFile(goldenPath, data, 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

// sortedCoverageKeys returns m's keys in ascending order for deterministic iteration.
func sortedCoverageKeys(m map[string]coverageClassification) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// equalStrings reports whether a and b hold the same elements in order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalCompletions reports whether two per-dimension completion maps hold
// the same tuple-key lists (order-sensitive, matching minimalTuples order).
func equalCompletions(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !equalStrings(av, bv) {
			return false
		}
	}
	return true
}
