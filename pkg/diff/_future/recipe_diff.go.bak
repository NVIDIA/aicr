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

// Package diff compares AICR snapshots and evaluates them against recipe
// constraints to detect configuration drift. It supports two modes:
//
//   - Snapshot-vs-snapshot: raw field-level comparison between two states.
//   - Recipe-vs-snapshot: evaluate recipe constraints and component versions
//     against a snapshot to detect drift from the recipe-defined desired state.
package diff

import (
	"sort"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
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
	// SeverityInfo indicates an informational change that does not violate constraints.
	SeverityInfo Severity = "info"
	// SeverityWarning indicates the change violates a warning-level constraint.
	SeverityWarning Severity = "warning"
	// SeverityError indicates the change violates an error-level constraint.
	SeverityError Severity = "error"
)

// Change represents a single field-level difference between two snapshots.
type Change struct {
	// Kind is the type of change (added, removed, modified).
	Kind ChangeKind `json:"kind" yaml:"kind"`
	// Severity classifies the impact (info, warning, error).
	Severity Severity `json:"severity" yaml:"severity"`
	// Path is the dot-separated location (e.g., "K8s.server.version").
	Path string `json:"path" yaml:"path"`
	// Baseline is the value in the baseline snapshot (empty for Added).
	Baseline string `json:"baseline,omitempty" yaml:"baseline,omitempty"`
	// Target is the value in the target snapshot (empty for Removed).
	Target string `json:"target,omitempty" yaml:"target,omitempty"`
}

// ConstraintResult represents the evaluation of a single recipe constraint against a snapshot.
type ConstraintResult struct {
	// Name is the constraint path (e.g., "K8s.server.version").
	Name string `json:"name" yaml:"name"`
	// Expected is the constraint expression (e.g., ">= 1.30").
	Expected string `json:"expected" yaml:"expected"`
	// Actual is the value found in the snapshot.
	Actual string `json:"actual" yaml:"actual"`
	// Passed indicates if the constraint is satisfied.
	Passed bool `json:"passed" yaml:"passed"`
	// Severity is the constraint severity from the recipe.
	Severity Severity `json:"severity" yaml:"severity"`
	// Remediation is guidance from the recipe for fixing violations.
	Remediation string `json:"remediation,omitempty" yaml:"remediation,omitempty"`
	// Error is the evaluation error if any.
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
}

// Component drift status constants.
const (
	ComponentStatusOK          = "ok"
	ComponentStatusMismatch    = "version-mismatch"
	ComponentStatusNotObserved = "not-observed"
)

// ComponentDrift represents drift in a recipe component's version or presence.
type ComponentDrift struct {
	// Name is the component name (e.g., "gpu-operator").
	Name string `json:"name" yaml:"name"`
	// ExpectedVersion is the version from the recipe.
	ExpectedVersion string `json:"expectedVersion,omitempty" yaml:"expectedVersion,omitempty"`
	// ActualVersion is the version found in the snapshot (empty if not found).
	ActualVersion string `json:"actualVersion,omitempty" yaml:"actualVersion,omitempty"`
	// Namespace is the expected deployment namespace.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	// Status describes the drift (ComponentStatusOK, ComponentStatusMismatch, ComponentStatusNotObserved).
	Status string `json:"status" yaml:"status"`
}

// ValidationPhaseSummary summarizes drift in a validation phase's configuration.
type ValidationPhaseSummary struct {
	// Phase name (deployment, performance, conformance).
	Phase string `json:"phase" yaml:"phase"`
	// Checks listed in the recipe for this phase.
	Checks []string `json:"checks" yaml:"checks"`
	// Constraints for this phase, if any.
	ConstraintResults []ConstraintResult `json:"constraintResults,omitempty" yaml:"constraintResults,omitempty"`
}

// Result contains the complete diff output.
type Result struct {
	// Mode describes the comparison mode ("snapshot-vs-snapshot" or "recipe-vs-snapshot").
	Mode string `json:"mode" yaml:"mode"`
	// BaselineSource identifies the baseline (file path, recipe name, etc.).
	BaselineSource string `json:"baselineSource,omitempty" yaml:"baselineSource,omitempty"`
	// TargetSource identifies the target snapshot.
	TargetSource string `json:"targetSource,omitempty" yaml:"targetSource,omitempty"`
	// Changes is the list of field-level differences (snapshot-vs-snapshot mode).
	Changes []Change `json:"changes,omitempty" yaml:"changes,omitempty"`
	// ConstraintResults is the list of top-level constraint evaluations (recipe-vs-snapshot mode).
	ConstraintResults []ConstraintResult `json:"constraintResults,omitempty" yaml:"constraintResults,omitempty"`
	// ComponentDrifts reports per-component drift (recipe-vs-snapshot mode).
	ComponentDrifts []ComponentDrift `json:"componentDrifts,omitempty" yaml:"componentDrifts,omitempty"`
	// ValidationPhases reports per-phase constraint and check status (recipe-vs-snapshot mode).
	ValidationPhases []ValidationPhaseSummary `json:"validationPhases,omitempty" yaml:"validationPhases,omitempty"`
	// Summary contains aggregate counts.
	Summary Summary `json:"summary" yaml:"summary"`
}

// Summary provides aggregate counts.
type Summary struct {
	Added    int `json:"added" yaml:"added"`
	Removed  int `json:"removed" yaml:"removed"`
	Modified int `json:"modified" yaml:"modified"`
	Total    int `json:"total" yaml:"total"`
	// Constraint-specific counts (recipe-vs-snapshot mode).
	ConstraintsPassed int `json:"constraintsPassed,omitempty" yaml:"constraintsPassed,omitempty"`
	ConstraintsFailed int `json:"constraintsFailed,omitempty" yaml:"constraintsFailed,omitempty"`
	ConstraintsError  int `json:"constraintsError,omitempty" yaml:"constraintsError,omitempty"`
	// Component-specific counts (recipe-vs-snapshot mode).
	ComponentsOK      int `json:"componentsOk,omitempty" yaml:"componentsOk,omitempty"`
	ComponentsDrifted int `json:"componentsDrifted,omitempty" yaml:"componentsDrifted,omitempty"`
}

// HasDrift returns true if any field-level changes or constraint violations were detected.
func (r *Result) HasDrift() bool {
	if r.Mode == "recipe-vs-snapshot" {
		return r.Summary.ConstraintsFailed > 0 || r.Summary.ConstraintsError > 0 || r.Summary.ComponentsDrifted > 0
	}
	return r.Summary.Total > 0
}

// Snapshots compares two snapshots and returns a structured diff result.
// The baseline is the reference state; the target is the current state.
// Both baseline and target must be non-nil.
func Snapshots(baseline, target *snapshotter.Snapshot) *Result {
	if baseline == nil || target == nil {
		return &Result{Mode: "snapshot-vs-snapshot", Changes: make([]Change, 0)}
	}

	result := &Result{
		Mode:    "snapshot-vs-snapshot",
		Changes: make([]Change, 0),
	}

	if baseline.Metadata != nil {
		if src, ok := baseline.Metadata["source-node"]; ok {
			result.BaselineSource = src
		}
	}
	if target.Metadata != nil {
		if src, ok := target.Metadata["source-node"]; ok {
			result.TargetSource = src
		}
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

// RecipeVsSnapshot evaluates a recipe's constraints and components against a snapshot
// to detect drift from the recipe-defined desired state. This uses the same constraint
// evaluation path as `aicr validate --readiness` (pkg/constraints.Evaluate).
func RecipeVsSnapshot(rec *recipe.RecipeResult, snap *snapshotter.Snapshot) *Result {
	result := &Result{
		Mode:              "recipe-vs-snapshot",
		ConstraintResults: make([]ConstraintResult, 0, len(rec.Constraints)),
		ComponentDrifts:   make([]ComponentDrift, 0),
		ValidationPhases:  make([]ValidationPhaseSummary, 0),
	}

	// 1. Evaluate top-level constraints (same path as validator.checkReadiness)
	for _, constraint := range rec.Constraints {
		cr := evaluateConstraint(constraint, snap)
		result.ConstraintResults = append(result.ConstraintResults, cr)

		if cr.Error != "" {
			result.Summary.ConstraintsError++
		} else if cr.Passed {
			result.Summary.ConstraintsPassed++
		} else {
			result.Summary.ConstraintsFailed++
		}
	}

	sort.Slice(result.ConstraintResults, func(i, j int) bool {
		return result.ConstraintResults[i].Name < result.ConstraintResults[j].Name
	})

	// 2. Check component drift (version and presence from componentRefs)
	result.ComponentDrifts = checkComponentDrift(rec, snap)
	for _, cd := range result.ComponentDrifts {
		switch cd.Status {
		case ComponentStatusOK:
			result.Summary.ComponentsOK++
		default:
			result.Summary.ComponentsDrifted++
		}
	}

	// 3. Summarize validation phase configuration and count phase-level constraints
	result.ValidationPhases = checkValidationPhases(rec, snap)
	for _, vp := range result.ValidationPhases {
		for _, cr := range vp.ConstraintResults {
			if cr.Error != "" {
				result.Summary.ConstraintsError++
			} else if cr.Passed {
				result.Summary.ConstraintsPassed++
			} else {
				result.Summary.ConstraintsFailed++
			}
		}
	}

	result.Summary.Total = len(result.ConstraintResults)

	return result
}

// checkComponentDrift compares recipe componentRefs against snapshot container images.
// The K8s collector captures deployed container images in K8s.image (image name → tag).
// Component names are matched against image names using the chart name or component name.
func checkComponentDrift(rec *recipe.RecipeResult, snap *snapshotter.Snapshot) []ComponentDrift {
	drifts := make([]ComponentDrift, 0, len(rec.ComponentRefs))

	// Build index of deployed container images from snapshot (K8s.image subtype)
	deployedImages := extractContainerImages(snap)

	for _, ref := range rec.ComponentRefs {
		if !ref.IsEnabled() {
			continue
		}

		cd := ComponentDrift{
			Name:            ref.Name,
			ExpectedVersion: ref.Version,
			Namespace:       ref.Namespace,
		}

		// Try to find the component's container image in the snapshot.
		// Match by component name or chart name against image names.
		actualVersion, found := deployedImages[ref.Name]
		if !found && ref.Chart != "" {
			actualVersion, found = deployedImages[ref.Chart]
		}

		if !found {
			cd.Status = ComponentStatusNotObserved
			cd.ActualVersion = ""
		} else {
			cd.ActualVersion = actualVersion
			if ref.Version != "" && actualVersion != "" && actualVersion != ref.Version {
				cd.Status = ComponentStatusMismatch
			} else {
				cd.Status = ComponentStatusOK
			}
		}

		drifts = append(drifts, cd)
	}

	sort.Slice(drifts, func(i, j int) bool {
		return drifts[i].Name < drifts[j].Name
	})

	return drifts
}

// extractContainerImages builds a map of image-name → tag from snapshot's K8s.image subtype.
// The K8s collector strips registry prefixes and splits name:tag, so entries look like:
//
//	"gpu-operator": "v24.9.0"
//	"cert-manager-controller": "v1.14.0"
func extractContainerImages(snap *snapshotter.Snapshot) map[string]string {
	images := make(map[string]string)

	for _, m := range snap.Measurements {
		if m.Type != measurement.TypeK8s {
			continue
		}
		st := m.GetSubtype("image")
		if st == nil {
			continue
		}
		for key, reading := range st.Data {
			images[key] = reading.String()
		}
	}

	return images
}

// checkValidationPhases summarizes validation phase config and evaluates phase-level constraints.
func checkValidationPhases(rec *recipe.RecipeResult, snap *snapshotter.Snapshot) []ValidationPhaseSummary {
	if rec.Validation == nil {
		return nil
	}

	phases := make([]ValidationPhaseSummary, 0, 3)

	type phaseInfo struct {
		name  string
		phase *recipe.ValidationPhase
	}

	for _, p := range []phaseInfo{
		{"deployment", rec.Validation.Deployment},
		{"performance", rec.Validation.Performance},
		{"conformance", rec.Validation.Conformance},
	} {
		if p.phase == nil {
			continue
		}

		summary := ValidationPhaseSummary{
			Phase:  p.name,
			Checks: p.phase.Checks,
		}

		// Evaluate phase-level constraints if any
		if len(p.phase.Constraints) > 0 {
			summary.ConstraintResults = make([]ConstraintResult, 0, len(p.phase.Constraints))
			for _, c := range p.phase.Constraints {
				cr := evaluateConstraint(c, snap)
				summary.ConstraintResults = append(summary.ConstraintResults, cr)
			}
		}

		phases = append(phases, summary)
	}

	return phases
}

// evaluateConstraint evaluates a single recipe constraint against a snapshot.
// Uses the same constraints.Evaluate path as validator.checkReadiness.
func evaluateConstraint(c recipe.Constraint, snap *snapshotter.Snapshot) ConstraintResult {
	cr := ConstraintResult{
		Name:        c.Name,
		Expected:    c.Value,
		Remediation: c.Remediation,
	}

	switch c.Severity {
	case "warning":
		cr.Severity = SeverityWarning
	default:
		cr.Severity = SeverityError
	}

	eval := constraints.Evaluate(c, snap)
	if eval.Error != nil {
		cr.Error = eval.Error.Error()
		cr.Actual = eval.Actual
		return cr
	}

	cr.Actual = eval.Actual
	cr.Passed = eval.Passed
	return cr
}

// --- snapshot-vs-snapshot helpers ---

func indexMeasurements(measurements []*measurement.Measurement) map[string]*measurement.Measurement {
	idx := make(map[string]*measurement.Measurement, len(measurements))
	for _, m := range measurements {
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
			changes = append(changes, Change{Kind: Added, Severity: SeverityInfo, Path: path, Target: targetReading.String()})
			continue
		}
		if !targetExists {
			changes = append(changes, Change{Kind: Removed, Severity: SeverityInfo, Path: path, Baseline: baseReading.String()})
			continue
		}

		baseVal := baseReading.String()
		targetVal := targetReading.String()
		if baseVal != targetVal {
			changes = append(changes, Change{Kind: Modified, Severity: SeverityInfo, Path: path, Baseline: baseVal, Target: targetVal})
		}
	}

	return changes
}

func addedMeasurement(m *measurement.Measurement) []Change {
	var changes []Change
	for _, st := range m.Subtypes {
		changes = append(changes, addedSubtype(string(m.Type)+"."+st.Name, &st)...)
	}
	return changes
}

func removedMeasurement(m *measurement.Measurement) []Change {
	var changes []Change
	for _, st := range m.Subtypes {
		changes = append(changes, removedSubtype(string(m.Type)+"."+st.Name, &st)...)
	}
	return changes
}

func addedSubtype(prefix string, st *measurement.Subtype) []Change {
	changes := make([]Change, 0, len(st.Data))
	for key, reading := range st.Data {
		changes = append(changes, Change{Kind: Added, Severity: SeverityInfo, Path: prefix + "." + key, Target: reading.String()})
	}
	return changes
}

func removedSubtype(prefix string, st *measurement.Subtype) []Change {
	changes := make([]Change, 0, len(st.Data))
	for key, reading := range st.Data {
		changes = append(changes, Change{Kind: Removed, Severity: SeverityInfo, Path: prefix + "." + key, Baseline: reading.String()})
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

