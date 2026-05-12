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

package diff

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func makeSnapshot(measurements ...*measurement.Measurement) *snapshotter.Snapshot {
	snap := snapshotter.NewSnapshot()
	snap.Header = header.Header{
		Kind:       header.KindSnapshot,
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Metadata:   map[string]string{},
	}
	snap.Measurements = measurements
	return snap
}

func makeMeasurement(t measurement.Type, subtypes ...measurement.Subtype) *measurement.Measurement {
	return &measurement.Measurement{
		Type:     t,
		Subtypes: subtypes,
	}
}

func makeSubtype(name string, data map[string]measurement.Reading) measurement.Subtype {
	return measurement.Subtype{
		Name: name,
		Data: data,
	}
}

func TestSnapshots_IdenticalSnapshots(t *testing.T) {
	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version":  measurement.Str("1.32.4"),
				"platform": measurement.Str("eks"),
			}),
		),
	)

	result := Snapshots(snap, snap)

	if result.HasDrift() {
		t.Errorf("expected no drift for identical snapshots, got %d changes", result.Summary.Total)
	}
}

func TestSnapshots_ModifiedReading(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.31.0"),
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.32.4"),
			}),
		),
	)

	result := Snapshots(baseline, target)

	if result.Summary.Modified != 1 {
		t.Fatalf("expected 1 modified, got %d", result.Summary.Modified)
	}

	c := result.Changes[0]
	if c.Kind != Modified || c.Severity != SeverityInfo {
		t.Errorf("expected Modified/info, got %s/%s", c.Kind, c.Severity)
	}
	if c.Path != "K8s.server.version" {
		t.Errorf("expected path K8s.server.version, got %s", c.Path)
	}
	if c.Baseline == nil || c.Target == nil {
		t.Fatalf("expected non-nil Baseline and Target, got %v / %v", c.Baseline, c.Target)
	}
	if *c.Baseline != "1.31.0" || *c.Target != "1.32.4" {
		t.Errorf("expected 1.31.0 → 1.32.4, got %s → %s", *c.Baseline, *c.Target)
	}
}

func TestSnapshots_AddedReading(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{
				"driver": measurement.Str("535.129.03"),
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{
				"driver": measurement.Str("535.129.03"),
				"model":  measurement.Str("H100"),
			}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Added != 1 {
		t.Fatalf("expected 1 added, got %d", result.Summary.Added)
	}
}

func TestSnapshots_RemovedReading(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeOS,
			makeSubtype("release", map[string]measurement.Reading{
				"ID":      measurement.Str("ubuntu"),
				"VERSION": measurement.Str("24.04"),
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeOS,
			makeSubtype("release", map[string]measurement.Reading{
				"ID": measurement.Str("ubuntu"),
			}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Removed != 1 {
		t.Fatalf("expected 1 removed, got %d", result.Summary.Removed)
	}
}

func TestSnapshots_MixedChanges(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version":  measurement.Str("1.31.0"),
				"platform": measurement.Str("eks"),
			}),
		),
		makeMeasurement(measurement.TypeSystemD,
			makeSubtype("kubelet", map[string]measurement.Reading{
				"active": measurement.Str("active"),
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.32.4"),
			}),
		),
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{
				"driver": measurement.Str("535.129.03"),
			}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Modified != 1 {
		t.Errorf("expected 1 modified, got %d", result.Summary.Modified)
	}
	if result.Summary.Removed != 2 {
		t.Errorf("expected 2 removed, got %d", result.Summary.Removed)
	}
	if result.Summary.Added != 1 {
		t.Errorf("expected 1 added, got %d", result.Summary.Added)
	}
}

func TestSnapshots_EmptySnapshots(t *testing.T) {
	result := Snapshots(makeSnapshot(), makeSnapshot())
	if result.HasDrift() {
		t.Errorf("expected no drift for empty snapshots")
	}
}

// TestHasDrift_DerivedFromChanges verifies HasDrift derives from len(Changes)
// rather than Summary.Total, so a caller-constructed Result whose Summary
// hasn't been populated still reports drift correctly. Also verifies a nil
// receiver returns false instead of panicking.
func TestHasDrift_DerivedFromChanges(t *testing.T) {
	tests := []struct {
		name   string
		result *Result
		want   bool
	}{
		{"nil receiver", nil, false},
		{"empty changes, zero summary", &Result{Changes: []Change{}}, false},
		{
			name: "changes present but summary zero (caller-constructed)",
			result: &Result{
				Changes: []Change{
					{Kind: Modified, Severity: SeverityInfo, Path: "K8s.server.version", Baseline: strPtr("a"), Target: strPtr("b")},
				},
				// Summary.Total intentionally left at 0
			},
			want: true,
		},
		{
			name: "summary populated, no changes (mismatched but Changes wins)",
			result: &Result{
				Changes: []Change{},
				Summary: Summary{Total: 99},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.HasDrift(); got != tt.want {
				t.Errorf("HasDrift() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSnapshots_NilInputs(t *testing.T) {
	result := Snapshots(nil, nil)
	if result.HasDrift() {
		t.Errorf("expected no drift for nil snapshots")
	}
	if result.Changes == nil {
		t.Errorf("expected non-nil changes slice")
	}
}

func TestSnapshots_NilMeasurementEntries(t *testing.T) {
	// Snapshots loaded from malformed YAML may contain nil entries.
	// Should skip them without panicking.
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
		nil, // malformed entry
	)
	target := makeSnapshot(
		nil,
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
	)

	result := Snapshots(baseline, target)
	if result.HasDrift() {
		t.Errorf("expected no drift — nil entries should be skipped, got %d changes", result.Summary.Total)
	}
}

func TestSnapshots_NilReadingValues(t *testing.T) {
	// A malformed snapshot can have nil Reading values in the Data map.
	// safeReadingString must handle them without panicking.
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.32.4"),
				"nilkey":  nil,
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.33.0"),
				"nilkey":  nil,
			}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Modified != 1 {
		t.Errorf("expected 1 modified change, got %d", result.Summary.Modified)
	}
}

func TestSnapshots_NilVsNonNilReading(t *testing.T) {
	// When one side has nil and the other has a concrete Reading,
	// the diff must surface a change (not silently skip it).
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.32.4"),
				"missing": nil,
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.32.4"),
				"missing": measurement.Str("now-present"),
			}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Modified != 1 {
		t.Errorf("expected 1 modified change (nil→value), got %d", result.Summary.Modified)
	}
	for _, c := range result.Changes {
		if c.Path == "K8s.server.missing" {
			if c.Baseline == nil || *c.Baseline != "<nil>" {
				t.Errorf("baseline for nil reading = %v, want pointer to %q", c.Baseline, "<nil>")
			}
			if c.Target == nil || *c.Target != "now-present" {
				t.Errorf("target = %v, want pointer to %q", c.Target, "now-present")
			}
		}
	}
}

// TestSnapshots_ExplicitEmptyStringPreserved verifies the *string schema fix:
// a Modified change from "X" to Str("") must serialize/preserve the empty
// string distinctly from a Removed change. omitempty on a plain string would
// drop the "" target, making the two indistinguishable downstream.
func TestSnapshots_ExplicitEmptyStringPreserved(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str("1.32.4"),
			}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{
				"version": measurement.Str(""),
			}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Modified != 1 {
		t.Fatalf("expected 1 modified change, got %d", result.Summary.Modified)
	}

	c := result.Changes[0]
	if c.Kind != Modified {
		t.Errorf("expected Modified, got %s", c.Kind)
	}
	if c.Target == nil {
		t.Fatal("expected non-nil Target pointer for explicit empty-string reading")
	}
	if *c.Target != "" {
		t.Errorf("expected Target to be explicit empty string, got %q", *c.Target)
	}
	if c.Baseline == nil || *c.Baseline != "1.32.4" {
		t.Errorf("expected Baseline pointer to %q, got %v", "1.32.4", c.Baseline)
	}
}

func TestSnapshots_DeterministicOrder(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeOS,
			makeSubtype("release", map[string]measurement.Reading{"ID": measurement.Str("ubuntu")}),
		),
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.31.0")}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeOS,
			makeSubtype("release", map[string]measurement.Reading{"ID": measurement.Str("rhel")}),
		),
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
	)

	for i := 0; i < 10; i++ {
		result := Snapshots(baseline, target)
		if len(result.Changes) != 2 {
			t.Fatalf("run %d: expected 2 changes, got %d", i, len(result.Changes))
		}
		if result.Changes[0].Path != "K8s.server.version" {
			t.Errorf("run %d: expected K8s.server.version first, got %s", i, result.Changes[0].Path)
		}
	}
}

func TestSnapshots_AddedMeasurementType(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{"driver": measurement.Str("535.129.03")}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Added != 1 {
		t.Errorf("expected 1 added (new measurement type), got %d", result.Summary.Added)
	}
}

func TestSnapshots_RemovedMeasurementType(t *testing.T) {
	baseline := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{"driver": measurement.Str("535.129.03")}),
		),
	)
	target := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
	)

	result := Snapshots(baseline, target)
	if result.Summary.Removed != 1 {
		t.Errorf("expected 1 removed (dropped measurement type), got %d", result.Summary.Removed)
	}
}

func TestSnapshots_SourceSetByCaller(t *testing.T) {
	// Snapshots() does not populate BaselineSource/TargetSource from metadata.
	// The caller (CLI) sets these after the call to provide the file path or URI.
	result := Snapshots(makeSnapshot(), makeSnapshot())
	if result.BaselineSource != "" {
		t.Errorf("expected empty baseline source, got %q", result.BaselineSource)
	}
	if result.TargetSource != "" {
		t.Errorf("expected empty target source, got %q", result.TargetSource)
	}

	result.BaselineSource = "before.yaml"
	result.TargetSource = "after.yaml"
	if result.BaselineSource != "before.yaml" {
		t.Errorf("expected baseline source before.yaml, got %q", result.BaselineSource)
	}
}

// --- Table Output Tests ---

func TestWriteTable_WithChanges(t *testing.T) {
	result := &Result{
		Changes: []Change{
			{Kind: Modified, Severity: SeverityInfo, Path: "K8s.server.version", Baseline: strPtr("1.31.0"), Target: strPtr("1.32.4")},
			{Kind: Added, Severity: SeverityInfo, Path: "GPU.device.memory", Target: strPtr("81559 MiB")},
		},
		Summary: Summary{Added: 1, Modified: 1, Total: 2},
	}

	var buf bytes.Buffer
	if err := WriteTable(&buf, result); err != nil {
		t.Fatalf("WriteTable failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "CHANGES") {
		t.Errorf("expected CHANGES header in table output")
	}
	if !strings.Contains(output, "K8s.server.version") {
		t.Errorf("expected path in table output")
	}
	if !strings.Contains(output, "MODIFIED") {
		t.Errorf("expected MODIFIED kind in table output")
	}
}

// TestWriteTable_NilResult verifies that WriteTable refuses to render a nil
// Result instead of panicking on the len(result.Changes) deref.
func TestWriteTable_NilResult(t *testing.T) {
	var buf bytes.Buffer
	err := WriteTable(&buf, nil)
	if err == nil {
		t.Fatal("expected error for nil Result, got nil")
	}
	if !strings.Contains(err.Error(), "non-nil Result") {
		t.Errorf("expected error mentioning non-nil Result, got: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for nil Result, got %d bytes", buf.Len())
	}
}

func TestWriteTable_NoChanges(t *testing.T) {
	result := &Result{
		Changes: []Change{},
		Summary: Summary{},
	}

	var buf bytes.Buffer
	if err := WriteTable(&buf, result); err != nil {
		t.Fatalf("WriteTable failed: %v", err)
	}

	if !strings.Contains(buf.String(), "NO CHANGES") {
		t.Errorf("expected NO CHANGES for empty diff")
	}
}

func TestWriteTable_DriftDetected(t *testing.T) {
	result := &Result{
		Changes: []Change{
			{Kind: Modified, Severity: SeverityInfo, Path: "K8s.server.version", Baseline: strPtr("1.31.0"), Target: strPtr("1.32.4")},
		},
		Summary: Summary{Modified: 1, Total: 1},
	}

	var buf bytes.Buffer
	if err := WriteTable(&buf, result); err != nil {
		t.Fatalf("WriteTable failed: %v", err)
	}

	if !strings.Contains(buf.String(), "DRIFT DETECTED") {
		t.Errorf("expected DRIFT DETECTED footer")
	}
}

// failingWriter returns an error after N successful writes. Used to simulate
// broken pipes and full disks during table output.
type failingWriter struct {
	successes int
	calls     int
}

func (fw *failingWriter) Write(p []byte) (int, error) {
	fw.calls++
	if fw.calls > fw.successes {
		return 0, fmt.Errorf("simulated write failure")
	}
	return len(p), nil
}

func TestWriteTable_PropagatesWriteErrors(t *testing.T) {
	tests := []struct {
		name      string
		result    *Result
		successes int
	}{
		{
			name: "no changes fails on first write",
			result: &Result{
				Changes: []Change{},
				Summary: Summary{},
			},
			successes: 0,
		},
		{
			name: "with changes fails on first write",
			result: &Result{
				Changes: []Change{
					{Kind: Modified, Severity: SeverityInfo, Path: "K8s.server.version", Baseline: strPtr("1.31.0"), Target: strPtr("1.32.4")},
				},
				Summary: Summary{Modified: 1, Total: 1},
			},
			successes: 0,
		},
		{
			name: "with changes fails mid-stream after header succeeds",
			result: &Result{
				Changes: []Change{
					{Kind: Modified, Severity: SeverityInfo, Path: "K8s.server.version", Baseline: strPtr("1.31.0"), Target: strPtr("1.32.4")},
				},
				Summary: Summary{Modified: 1, Total: 1},
			},
			successes: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fw := &failingWriter{successes: tt.successes}
			err := WriteTable(fw, tt.result)
			if err == nil {
				t.Fatal("expected error from failing writer, got nil")
			}
			if !strings.Contains(err.Error(), "failed to write diff table output") {
				t.Errorf("expected wrapped error, got: %v", err)
			}
		})
	}
}
