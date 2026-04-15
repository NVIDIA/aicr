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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
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

// --- Snapshot-vs-Snapshot Tests ---

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
	if result.Mode != "snapshot-vs-snapshot" {
		t.Errorf("expected mode snapshot-vs-snapshot, got %s", result.Mode)
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
	if c.Baseline != "1.31.0" || c.Target != "1.32.4" {
		t.Errorf("expected 1.31.0 → 1.32.4, got %s → %s", c.Baseline, c.Target)
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

// --- Recipe-vs-Snapshot Tests ---

func TestRecipeVsSnapshot_AllConstraintsPassed(t *testing.T) {
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 1.30", Severity: "error"},
			{Name: "OS.release.ID", Value: "ubuntu", Severity: "error"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
		makeMeasurement(measurement.TypeOS,
			makeSubtype("release", map[string]measurement.Reading{"ID": measurement.Str("ubuntu")}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.Mode != "recipe-vs-snapshot" {
		t.Errorf("expected mode recipe-vs-snapshot, got %s", result.Mode)
	}
	if result.Summary.ConstraintsPassed != 2 {
		t.Errorf("expected 2 passed, got %d", result.Summary.ConstraintsPassed)
	}
	if result.HasDrift() {
		t.Errorf("expected no drift when all constraints pass")
	}
}

func TestRecipeVsSnapshot_ConstraintFailed(t *testing.T) {
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 1.32", Severity: "error", Remediation: "Upgrade K8s to 1.32+"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.31.0")}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.Summary.ConstraintsFailed != 1 {
		t.Fatalf("expected 1 failed, got %d", result.Summary.ConstraintsFailed)
	}
	if !result.HasDrift() {
		t.Errorf("expected drift when constraint fails")
	}

	cr := result.ConstraintResults[0]
	if cr.Passed || cr.Severity != SeverityError || cr.Actual != "1.31.0" {
		t.Errorf("unexpected constraint result: passed=%v severity=%s actual=%s", cr.Passed, cr.Severity, cr.Actual)
	}
	if cr.Remediation != "Upgrade K8s to 1.32+" {
		t.Errorf("expected remediation guidance, got %s", cr.Remediation)
	}
}

func TestRecipeVsSnapshot_WarningSeverity(t *testing.T) {
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "GPU.device.driver", Value: ">= 550.0", Severity: "warning"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{"driver": measurement.Str("535.129.03")}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.ConstraintResults[0].Severity != SeverityWarning {
		t.Errorf("expected severity warning, got %s", result.ConstraintResults[0].Severity)
	}
}

func TestRecipeVsSnapshot_MissingMeasurement(t *testing.T) {
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "GPU.device.driver", Value: ">= 535.0", Severity: "error"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("server", map[string]measurement.Reading{"version": measurement.Str("1.32.4")}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.Summary.ConstraintsError != 1 {
		t.Fatalf("expected 1 error, got %d", result.Summary.ConstraintsError)
	}
	if result.ConstraintResults[0].Error == "" {
		t.Errorf("expected error for missing measurement")
	}
}

func TestRecipeVsSnapshot_EmptyConstraints(t *testing.T) {
	rec := &recipe.RecipeResult{Constraints: []recipe.Constraint{}}
	result := RecipeVsSnapshot(rec, makeSnapshot())
	if result.HasDrift() {
		t.Errorf("expected no drift with no constraints")
	}
}

// --- Component Drift Tests ---

func TestRecipeVsSnapshot_ComponentPresent(t *testing.T) {
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "24.9.0", Namespace: "gpu-operator"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("image", map[string]measurement.Reading{
				"gpu-operator": measurement.Str("24.9.0"),
			}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.Summary.ComponentsOK != 1 {
		t.Errorf("expected 1 component ok, got %d", result.Summary.ComponentsOK)
	}
	if result.ComponentDrifts[0].Status != "ok" {
		t.Errorf("expected status ok, got %s", result.ComponentDrifts[0].Status)
	}
}

func TestRecipeVsSnapshot_ComponentMissing(t *testing.T) {
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "24.9.0", Namespace: "gpu-operator"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("image", map[string]measurement.Reading{
				"cert-manager": measurement.Str("1.14.0"),
			}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.Summary.ComponentsDrifted != 1 {
		t.Errorf("expected 1 component drifted, got %d", result.Summary.ComponentsDrifted)
	}
	if result.ComponentDrifts[0].Status != ComponentStatusNotObserved {
		t.Errorf("expected status missing, got %s", result.ComponentDrifts[0].Status)
	}
	if !result.HasDrift() {
		t.Errorf("expected drift when component is missing")
	}
}

func TestRecipeVsSnapshot_ComponentVersionMismatch(t *testing.T) {
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "24.9.0", Namespace: "gpu-operator"},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeK8s,
			makeSubtype("image", map[string]measurement.Reading{
				"gpu-operator": measurement.Str("24.6.0"),
			}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if result.ComponentDrifts[0].Status != ComponentStatusMismatch {
		t.Errorf("expected status version-mismatch, got %s", result.ComponentDrifts[0].Status)
	}
	if result.ComponentDrifts[0].ExpectedVersion != "24.9.0" || result.ComponentDrifts[0].ActualVersion != "24.6.0" {
		t.Errorf("expected 24.9.0 vs 24.6.0, got %s vs %s",
			result.ComponentDrifts[0].ExpectedVersion, result.ComponentDrifts[0].ActualVersion)
	}
}

func TestRecipeVsSnapshot_DisabledComponentSkipped(t *testing.T) {
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "24.9.0", Overrides: map[string]any{"enabled": false}},
		},
	}

	snap := makeSnapshot()
	result := RecipeVsSnapshot(rec, snap)

	if len(result.ComponentDrifts) != 0 {
		t.Errorf("expected disabled component to be skipped, got %d drifts", len(result.ComponentDrifts))
	}
}

// --- Validation Phase Tests ---

func TestRecipeVsSnapshot_ValidationPhases(t *testing.T) {
	rec := &recipe.RecipeResult{
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Checks: []string{"operator-health", "expected-resources"},
			},
			Performance: &recipe.ValidationPhase{
				Checks: []string{"nccl-all-reduce-bw"},
				Constraints: []recipe.Constraint{
					{Name: "GPU.device.driver", Value: ">= 535.0", Severity: "error"},
				},
			},
		},
	}

	snap := makeSnapshot(
		makeMeasurement(measurement.TypeGPU,
			makeSubtype("device", map[string]measurement.Reading{
				"driver": measurement.Str("535.129.03"),
			}),
		),
	)

	result := RecipeVsSnapshot(rec, snap)

	if len(result.ValidationPhases) != 2 {
		t.Fatalf("expected 2 validation phases, got %d", len(result.ValidationPhases))
	}

	// Deployment phase
	dp := result.ValidationPhases[0]
	if dp.Phase != "deployment" {
		t.Errorf("expected deployment phase, got %s", dp.Phase)
	}
	if len(dp.Checks) != 2 {
		t.Errorf("expected 2 checks, got %d", len(dp.Checks))
	}

	// Performance phase with constraints
	pp := result.ValidationPhases[1]
	if pp.Phase != "performance" {
		t.Errorf("expected performance phase, got %s", pp.Phase)
	}
	if len(pp.ConstraintResults) != 1 {
		t.Fatalf("expected 1 phase constraint, got %d", len(pp.ConstraintResults))
	}
	if !pp.ConstraintResults[0].Passed {
		t.Errorf("expected phase constraint to pass")
	}
}

// --- Table Output Tests ---

func TestWriteTable_SnapshotMode(t *testing.T) {
	result := &Result{
		Mode: "snapshot-vs-snapshot",
		Changes: []Change{
			{Kind: Modified, Severity: SeverityInfo, Path: "K8s.server.version", Baseline: "1.31.0", Target: "1.32.4"},
			{Kind: Added, Severity: SeverityInfo, Path: "GPU.device.memory", Target: "81559 MiB"},
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

func TestWriteTable_RecipeMode(t *testing.T) {
	result := &Result{
		Mode: "recipe-vs-snapshot",
		ConstraintResults: []ConstraintResult{
			{Name: "K8s.server.version", Expected: ">= 1.32", Actual: "1.31.0", Passed: false, Severity: SeverityError, Remediation: "Upgrade K8s"},
			{Name: "OS.release.ID", Expected: "ubuntu", Actual: "ubuntu", Passed: true, Severity: SeverityError},
		},
		ComponentDrifts: []ComponentDrift{
			{Name: "gpu-operator", ExpectedVersion: "24.9.0", ActualVersion: "24.6.0", Status: ComponentStatusMismatch},
		},
		Summary: Summary{ConstraintsPassed: 1, ConstraintsFailed: 1, ComponentsDrifted: 1},
	}

	var buf bytes.Buffer
	if err := WriteTable(&buf, result); err != nil {
		t.Fatalf("WriteTable failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "CONSTRAINTS") {
		t.Errorf("expected CONSTRAINTS header")
	}
	if !strings.Contains(output, "FAIL") {
		t.Errorf("expected FAIL status")
	}
	if !strings.Contains(output, "COMPONENTS") {
		t.Errorf("expected COMPONENTS header")
	}
	if !strings.Contains(output, "VERSION-MISMATCH") {
		t.Errorf("expected VERSION-MISMATCH status")
	}
	if !strings.Contains(output, "DRIFT DETECTED") {
		t.Errorf("expected DRIFT DETECTED")
	}
}

func TestWriteTable_NoChanges(t *testing.T) {
	result := &Result{
		Mode:    "snapshot-vs-snapshot",
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
