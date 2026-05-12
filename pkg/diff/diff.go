// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package diff compares AICR snapshots to detect configuration drift.
// It performs field-level comparison between two snapshots, reporting
// added, removed, and modified readings across all measurement types.
package diff

import (
	"sort"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// ChangeKind describes the type of difference detected.
type ChangeKind string

const (
	// Added indicates a value exists in the target but not the baseline.
	Added ChangeKind = "added"
	// Removed indicates a value exists in the baseline but not the target.
	Removed ChangeKind = "removed"
	// Modified indicates a value changed between baseline and target.
	Modified ChangeKind = "modified"
)

// Severity classifies the impact of a detected change.
type Severity string

const (
	// SeverityInfo indicates an informational change.
	SeverityInfo Severity = "info"
)

// Change represents a single field-level difference between two snapshots.
//
// Baseline and Target are pointers so the JSON/YAML schema can distinguish a
// genuinely-absent side (nil → field omitted via omitempty) from a present
// reading whose value happens to be the empty string (`&""` → field present
// as `""`). Conflating these on the wire would make Modified-to-empty
// indistinguishable from Removed for downstream consumers.
type Change struct {
	// Kind is the type of change (added, removed, modified).
	Kind ChangeKind `json:"kind" yaml:"kind"`
	// Severity classifies the impact.
	Severity Severity `json:"severity" yaml:"severity"`
	// Path is the dot-separated location (e.g., "K8s.server.version").
	Path string `json:"path" yaml:"path"`
	// Baseline is the value in the baseline snapshot. Nil for Added changes.
	Baseline *string `json:"baseline,omitempty" yaml:"baseline,omitempty"`
	// Target is the value in the target snapshot. Nil for Removed changes.
	Target *string `json:"target,omitempty" yaml:"target,omitempty"`
}

// strPtr returns a pointer to s. Used at Change construction sites to make
// the present-but-empty case (`&""`) visually distinct from the absent case
// (nil).
func strPtr(s string) *string {
	return &s
}

// Result contains the complete diff output.
type Result struct {
	// BaselineSource identifies the baseline (file path, ConfigMap URI, etc.).
	BaselineSource string `json:"baselineSource,omitempty" yaml:"baselineSource,omitempty"`
	// TargetSource identifies the target snapshot.
	TargetSource string `json:"targetSource,omitempty" yaml:"targetSource,omitempty"`
	// Changes is the list of field-level differences.
	Changes []Change `json:"changes" yaml:"changes"`
	// Summary contains aggregate counts.
	Summary Summary `json:"summary" yaml:"summary"`
}

// Summary provides aggregate counts.
type Summary struct {
	Added    int `json:"added" yaml:"added"`
	Removed  int `json:"removed" yaml:"removed"`
	Modified int `json:"modified" yaml:"modified"`
	Total    int `json:"total" yaml:"total"`
}

// HasDrift returns true if any field-level changes were detected.
// Derives the answer from len(Changes) directly so a caller-constructed
// Result (where Summary may not have been populated) reports correctly,
// and a nil receiver safely returns false instead of panicking.
func (r *Result) HasDrift() bool {
	if r == nil {
		return false
	}
	return len(r.Changes) > 0
}

// Snapshots compares two snapshots and returns a structured diff result.
// The baseline is the reference state; the target is the current state.
// If either baseline or target is nil, returns an empty Result (no drift).
func Snapshots(baseline, target *snapshotter.Snapshot) *Result {
	if baseline == nil || target == nil {
		return &Result{Changes: make([]Change, 0)}
	}

	result := &Result{
		Changes: make([]Change, 0),
	}

	baseByType := indexMeasurements(baseline.Measurements)
	targetByType := indexMeasurements(target.Measurements)

	allTypes := mergeKeys(baseByType, targetByType)
	sort.Strings(allTypes)

	for _, typeName := range allTypes {
		baseMeasurement, baseExists := baseByType[typeName]
		targetMeasurement, targetExists := targetByType[typeName]

		if !baseExists {
			result.Changes = append(result.Changes, addedMeasurement(targetMeasurement)...)
			continue
		}
		if !targetExists {
			result.Changes = append(result.Changes, removedMeasurement(baseMeasurement)...)
			continue
		}

		result.Changes = append(result.Changes, compareMeasurements(baseMeasurement, targetMeasurement)...)
	}

	sort.Slice(result.Changes, func(i, j int) bool {
		return result.Changes[i].Path < result.Changes[j].Path
	})

	for _, c := range result.Changes {
		switch c.Kind {
		case Added:
			result.Summary.Added++
		case Removed:
			result.Summary.Removed++
		case Modified:
			result.Summary.Modified++
		}
	}
	result.Summary.Total = len(result.Changes)

	return result
}

// --- helpers ---

// safeReadingString returns the string representation of a Reading,
// or "<nil>" if the Reading is nil so that nil values are
// distinguishable from legitimate empty strings.
func safeReadingString(r measurement.Reading) string {
	if r == nil {
		return "<nil>"
	}
	return r.String()
}

func indexMeasurements(measurements []*measurement.Measurement) map[string]*measurement.Measurement {
	idx := make(map[string]*measurement.Measurement, len(measurements))
	for _, m := range measurements {
		if m == nil {
			continue
		}
		idx[string(m.Type)] = m
	}
	return idx
}

func compareMeasurements(base, target *measurement.Measurement) []Change {
	var changes []Change

	baseByName := indexSubtypes(base.Subtypes)
	targetByName := indexSubtypes(target.Subtypes)

	allNames := mergeKeys(baseByName, targetByName)
	sort.Strings(allNames)

	for _, name := range allNames {
		baseSt, baseExists := baseByName[name]
		targetSt, targetExists := targetByName[name]

		prefix := string(base.Type) + "." + name

		if !baseExists {
			changes = append(changes, addedSubtype(prefix, targetSt)...)
			continue
		}
		if !targetExists {
			changes = append(changes, removedSubtype(prefix, baseSt)...)
			continue
		}

		changes = append(changes, compareReadings(prefix, baseSt.Data, targetSt.Data)...)
	}

	return changes
}

func compareReadings(prefix string, base, target map[string]measurement.Reading) []Change {
	var changes []Change

	allKeys := mergeKeys(base, target)
	sort.Strings(allKeys)

	for _, key := range allKeys {
		path := prefix + "." + key
		baseReading, baseExists := base[key]
		targetReading, targetExists := target[key]

		if !baseExists {
			changes = append(changes, Change{Kind: Added, Severity: SeverityInfo, Path: path, Target: strPtr(safeReadingString(targetReading))})
			continue
		}
		if !targetExists {
			changes = append(changes, Change{Kind: Removed, Severity: SeverityInfo, Path: path, Baseline: strPtr(safeReadingString(baseReading))})
			continue
		}

		baseVal := safeReadingString(baseReading)
		targetVal := safeReadingString(targetReading)
		if baseVal != targetVal {
			changes = append(changes, Change{Kind: Modified, Severity: SeverityInfo, Path: path, Baseline: strPtr(baseVal), Target: strPtr(targetVal)})
		}
	}

	return changes
}

func addedMeasurement(m *measurement.Measurement) []Change {
	changes := make([]Change, 0, len(m.Subtypes))
	for i := range m.Subtypes {
		changes = append(changes, addedSubtype(string(m.Type)+"."+m.Subtypes[i].Name, &m.Subtypes[i])...)
	}
	return changes
}

func removedMeasurement(m *measurement.Measurement) []Change {
	changes := make([]Change, 0, len(m.Subtypes))
	for i := range m.Subtypes {
		changes = append(changes, removedSubtype(string(m.Type)+"."+m.Subtypes[i].Name, &m.Subtypes[i])...)
	}
	return changes
}

func addedSubtype(prefix string, st *measurement.Subtype) []Change {
	changes := make([]Change, 0, len(st.Data))
	for key, reading := range st.Data {
		changes = append(changes, Change{Kind: Added, Severity: SeverityInfo, Path: prefix + "." + key, Target: strPtr(safeReadingString(reading))})
	}
	return changes
}

func removedSubtype(prefix string, st *measurement.Subtype) []Change {
	changes := make([]Change, 0, len(st.Data))
	for key, reading := range st.Data {
		changes = append(changes, Change{Kind: Removed, Severity: SeverityInfo, Path: prefix + "." + key, Baseline: strPtr(safeReadingString(reading))})
	}
	return changes
}

func indexSubtypes(subtypes []measurement.Subtype) map[string]*measurement.Subtype {
	idx := make(map[string]*measurement.Subtype, len(subtypes))
	for i := range subtypes {
		idx[subtypes[i].Name] = &subtypes[i]
	}
	return idx
}

func mergeKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	return keys
}
