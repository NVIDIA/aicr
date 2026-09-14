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
	"context"
	stderrors "errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
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
	result, err := snapshots(context.Background(), baseline, target)
	if err != nil {
		// context.Background cannot be canceled. Keep the legacy no-error API
		// total if that invariant ever changes.
		return &Result{Changes: make([]Change, 0)}
	}
	return result
}

// SnapshotsWithContext compares two snapshots while honoring cancellation
// throughout the in-memory traversal. It returns no partial result when the
// context is canceled or its deadline expires.
func SnapshotsWithContext(ctx context.Context, baseline, target *snapshotter.Snapshot) (*Result, error) {
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "snapshot diff context is required (got nil)")
	}

	result, err := snapshots(ctx, baseline, target)
	if err != nil {
		return nil, snapshotContextError(err)
	}
	return result, nil
}

func snapshots(ctx context.Context, baseline, target *snapshotter.Snapshot) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if baseline == nil || target == nil {
		return &Result{Changes: make([]Change, 0)}, nil
	}

	result := &Result{
		Changes: make([]Change, 0),
	}

	baseByType, err := indexMeasurements(ctx, baseline.Measurements)
	if err != nil {
		return nil, err
	}
	targetByType, err := indexMeasurements(ctx, target.Measurements)
	if err != nil {
		return nil, err
	}

	allTypes, err := mergeKeys(ctx, baseByType, targetByType)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Strings(allTypes)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, typeName := range allTypes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		baseMeasurement, baseExists := baseByType[typeName]
		targetMeasurement, targetExists := targetByType[typeName]

		if !baseExists {
			changes, err := addedMeasurement(ctx, targetMeasurement)
			if err != nil {
				return nil, err
			}
			result.Changes = append(result.Changes, changes...)
			continue
		}
		if !targetExists {
			changes, err := removedMeasurement(ctx, baseMeasurement)
			if err != nil {
				return nil, err
			}
			result.Changes = append(result.Changes, changes...)
			continue
		}

		changes, err := compareMeasurements(ctx, baseMeasurement, targetMeasurement)
		if err != nil {
			return nil, err
		}
		result.Changes = append(result.Changes, changes...)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result.Changes, func(i, j int) bool {
		return result.Changes[i].Path < result.Changes[j].Path
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, c := range result.Changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return result, nil
}

// --- helpers ---

// safeReadingString returns the string representation of a Reading,
// or "<nil>" if the Reading is nil so that nil values are
// distinguishable from legitimate empty strings.
func safeReadingString(r measurement.Reading) string {
	if r == nil {
		return "<nil>"
	}

	value := reflect.ValueOf(r)
	kind := value.Kind()
	nilCapable := kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface ||
		kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice
	if nilCapable && value.IsNil() {
		return "<nil>"
	}

	return r.String()
}

func indexMeasurements(ctx context.Context, measurements []*measurement.Measurement) (map[string]*measurement.Measurement, error) {
	idx := make(map[string]*measurement.Measurement, len(measurements))
	for _, m := range measurements {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m == nil {
			continue
		}
		idx[string(m.Type)] = m
	}
	return idx, nil
}

func compareMeasurements(ctx context.Context, base, target *measurement.Measurement) ([]Change, error) {
	var changes []Change

	var err error
	base, target, err = alignTopologyEncoding(ctx, base, target)
	if err != nil {
		return nil, err
	}

	baseByName, err := indexSubtypes(ctx, base.Subtypes)
	if err != nil {
		return nil, err
	}
	targetByName, err := indexSubtypes(ctx, target.Subtypes)
	if err != nil {
		return nil, err
	}

	allNames, err := mergeKeys(ctx, baseByName, targetByName)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Strings(allNames)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, name := range allNames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		baseSt, baseExists := baseByName[name]
		targetSt, targetExists := targetByName[name]

		prefix := string(base.Type) + "." + name

		if !baseExists {
			added, err := addedSubtype(ctx, prefix, targetSt)
			if err != nil {
				return nil, err
			}
			changes = append(changes, added...)
			continue
		}
		if !targetExists {
			removed, err := removedSubtype(ctx, prefix, baseSt)
			if err != nil {
				return nil, err
			}
			changes = append(changes, removed...)
			continue
		}

		readingChanges, err := compareReadings(ctx, prefix, baseSt.Data, targetSt.Data)
		if err != nil {
			return nil, err
		}
		changes = append(changes, readingChanges...)
		stringChanges, err := compareStrings(ctx, prefix+".context", baseSt.Context, targetSt.Context)
		if err != nil {
			return nil, err
		}
		changes = append(changes, stringChanges...)
		itemChanges, err := compareItems(ctx, prefix, baseSt.Items, targetSt.Items)
		if err != nil {
			return nil, err
		}
		changes = append(changes, itemChanges...)
	}

	return changes, nil
}

func compareReadings(ctx context.Context, prefix string, base, target map[string]measurement.Reading) ([]Change, error) {
	var changes []Change

	allKeys, err := mergeKeys(ctx, base, target)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Strings(allKeys)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, key := range allKeys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := dataPath(prefix, key)
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if baseVal != targetVal {
			changes = append(changes, Change{Kind: Modified, Severity: SeverityInfo, Path: path, Baseline: strPtr(baseVal), Target: strPtr(targetVal)})
		}
	}

	return changes, nil
}

func dataPath(prefix, key string) string {
	if key == "context" || strings.HasPrefix(key, "context.") ||
		key == "items" || strings.HasPrefix(key, "items.") || strings.HasPrefix(key, "items[") {

		return prefix + "[" + strconv.Quote(key) + "]"
	}
	return prefix + "." + key
}

func addedReadings(ctx context.Context, prefix string, values map[string]measurement.Reading) ([]Change, error) {
	return compareReadings(ctx, prefix, nil, values)
}

func removedReadings(ctx context.Context, prefix string, values map[string]measurement.Reading) ([]Change, error) {
	return compareReadings(ctx, prefix, values, nil)
}

func compareStrings(ctx context.Context, prefix string, base, target map[string]string) ([]Change, error) {
	changes := make([]Change, 0)
	keys, err := mergeKeys(ctx, base, target)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Strings(keys)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := prefix + "." + key
		baseValue, baseExists := base[key]
		targetValue, targetExists := target[key]

		switch {
		case !baseExists:
			changes = append(changes, Change{Kind: Added, Severity: SeverityInfo, Path: path, Target: strPtr(targetValue)})
		case !targetExists:
			changes = append(changes, Change{Kind: Removed, Severity: SeverityInfo, Path: path, Baseline: strPtr(baseValue)})
		case baseValue != targetValue:
			changes = append(changes, Change{Kind: Modified, Severity: SeverityInfo, Path: path, Baseline: strPtr(baseValue), Target: strPtr(targetValue)})
		}
	}

	return changes, nil
}

func addedStrings(ctx context.Context, prefix string, values map[string]string) ([]Change, error) {
	return compareStrings(ctx, prefix, nil, values)
}

func removedStrings(ctx context.Context, prefix string, values map[string]string) ([]Change, error) {
	return compareStrings(ctx, prefix, values, nil)
}

func itemPrefix(prefix string, index int) string {
	return fmt.Sprintf("%s.items[%d]", prefix, index)
}

func lengthChange(prefix string, kind ChangeKind, baseline, target *string) Change {
	return Change{
		Kind:     kind,
		Severity: SeverityInfo,
		Path:     prefix + ".items.length",
		Baseline: baseline,
		Target:   target,
	}
}

func compareItems(ctx context.Context, prefix string, base, target []measurement.ItemEntry) ([]Change, error) {
	changes := make([]Change, 0)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(base) != len(target) {
		changes = append(changes, lengthChange(
			prefix,
			Modified,
			strPtr(strconv.Itoa(len(base))),
			strPtr(strconv.Itoa(len(target))),
		))
	}

	shared := min(len(base), len(target))
	for i := range shared {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := itemPrefix(prefix, i)
		stringChanges, err := compareStrings(ctx, path+".context", base[i].Context, target[i].Context)
		if err != nil {
			return nil, err
		}
		changes = append(changes, stringChanges...)
		readingChanges, err := compareReadings(ctx, path+".data", base[i].Data, target[i].Data)
		if err != nil {
			return nil, err
		}
		changes = append(changes, readingChanges...)
	}
	for i := shared; i < len(target); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		added, err := addedItem(ctx, itemPrefix(prefix, i), &target[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, added...)
	}
	for i := shared; i < len(base); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		removed, err := removedItem(ctx, itemPrefix(prefix, i), &base[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, removed...)
	}

	return changes, nil
}

func addedItem(ctx context.Context, prefix string, item *measurement.ItemEntry) ([]Change, error) {
	changes, err := addedStrings(ctx, prefix+".context", item.Context)
	if err != nil {
		return nil, err
	}
	readings, err := addedReadings(ctx, prefix+".data", item.Data)
	if err != nil {
		return nil, err
	}
	return append(changes, readings...), nil
}

func removedItem(ctx context.Context, prefix string, item *measurement.ItemEntry) ([]Change, error) {
	changes, err := removedStrings(ctx, prefix+".context", item.Context)
	if err != nil {
		return nil, err
	}
	readings, err := removedReadings(ctx, prefix+".data", item.Data)
	if err != nil {
		return nil, err
	}
	return append(changes, readings...), nil
}

func addedItems(ctx context.Context, prefix string, items []measurement.ItemEntry) ([]Change, error) {
	if len(items) == 0 {
		return nil, nil
	}

	changes := []Change{
		lengthChange(prefix, Added, nil, strPtr(strconv.Itoa(len(items)))),
	}
	for i := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		added, err := addedItem(ctx, itemPrefix(prefix, i), &items[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, added...)
	}
	return changes, nil
}

func removedItems(ctx context.Context, prefix string, items []measurement.ItemEntry) ([]Change, error) {
	if len(items) == 0 {
		return nil, nil
	}

	changes := []Change{
		lengthChange(prefix, Removed, strPtr(strconv.Itoa(len(items))), nil),
	}
	for i := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		removed, err := removedItem(ctx, itemPrefix(prefix, i), &items[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, removed...)
	}
	return changes, nil
}

func addedMeasurement(ctx context.Context, m *measurement.Measurement) ([]Change, error) {
	changes := make([]Change, 0, len(m.Subtypes))
	for i := range m.Subtypes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		added, err := addedSubtype(ctx, string(m.Type)+"."+m.Subtypes[i].Name, &m.Subtypes[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, added...)
	}
	return changes, nil
}

func removedMeasurement(ctx context.Context, m *measurement.Measurement) ([]Change, error) {
	changes := make([]Change, 0, len(m.Subtypes))
	for i := range m.Subtypes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		removed, err := removedSubtype(ctx, string(m.Type)+"."+m.Subtypes[i].Name, &m.Subtypes[i])
		if err != nil {
			return nil, err
		}
		changes = append(changes, removed...)
	}
	return changes, nil
}

func addedSubtype(ctx context.Context, prefix string, st *measurement.Subtype) ([]Change, error) {
	changes, err := addedReadings(ctx, prefix, st.Data)
	if err != nil {
		return nil, err
	}
	stringChanges, err := addedStrings(ctx, prefix+".context", st.Context)
	if err != nil {
		return nil, err
	}
	changes = append(changes, stringChanges...)
	items, err := addedItems(ctx, prefix, st.Items)
	if err != nil {
		return nil, err
	}
	return append(changes, items...), nil
}

func removedSubtype(ctx context.Context, prefix string, st *measurement.Subtype) ([]Change, error) {
	changes, err := removedReadings(ctx, prefix, st.Data)
	if err != nil {
		return nil, err
	}
	stringChanges, err := removedStrings(ctx, prefix+".context", st.Context)
	if err != nil {
		return nil, err
	}
	changes = append(changes, stringChanges...)
	items, err := removedItems(ctx, prefix, st.Items)
	if err != nil {
		return nil, err
	}
	return append(changes, items...), nil
}

func indexSubtypes(ctx context.Context, subtypes []measurement.Subtype) (map[string]*measurement.Subtype, error) {
	idx := make(map[string]*measurement.Subtype, len(subtypes))
	for i := range subtypes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		idx[subtypes[i].Name] = &subtypes[i]
	}
	return idx, nil
}

func mergeKeys[V any](ctx context.Context, a, b map[string]V) ([]string, error) {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seen[k] = struct{}{}
	}
	for k := range b {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func snapshotContextError(cause error) error {
	if stderrors.Is(cause, context.Canceled) {
		return errors.Wrap(errors.ErrCodeCanceled, "snapshot diff canceled", cause)
	}
	return errors.Wrap(errors.ErrCodeTimeout, "snapshot diff deadline exceeded", cause)
}
