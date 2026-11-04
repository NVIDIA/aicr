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
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Validation phase names as they appear in Summary.Validation, in the order
// the phases run.
const (
	SummaryPhaseReadiness   = "readiness"
	SummaryPhaseDeployment  = "deployment"
	SummaryPhasePerformance = "performance"
	SummaryPhaseConformance = "conformance"
)

// Summary is the in-memory projection of an already loaded or resolved
// RecipeResult. Its construction performs no I/O; upstream profile-coherence
// validation remains separate. Every slice keeps a deterministic order:
// components follow deployment order, constraints and overlays keep recipe
// order, and validation phases run readiness, deployment, performance,
// conformance.
type Summary struct {
	// APIVersion and Kind identify the document the summary was built from.
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`

	// Version is the recipe version (the CLI version that generated it).
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// GPUDriver is the snapshot-observed driver state recorded in the recipe
	// ("preinstalled" or "absent"), or "unknown" when the recipe records none.
	GPUDriver string `json:"gpuDriver" yaml:"gpuDriver"`

	// Profile is the selected configuration profile in name=value form.
	// Empty when the recipe was generated without one.
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`

	// Criteria is the criteria the recipe was generated for.
	Criteria Criteria `json:"criteria" yaml:"criteria"`

	// AppliedOverlays lists the overlays merged into the recipe, in order of
	// application.
	AppliedOverlays []string `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`

	// Constraints are the recipe's merged constraints.
	Constraints []SummaryConstraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// Components are the recipe's components in deployment order.
	Components []SummaryComponent `json:"components" yaml:"components"`

	// Validation lists the validation phases that declare checks.
	Validation []SummaryPhase `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// SummaryConstraint is one recipe constraint, with the name exactly as the
// recipe holds it.
type SummaryConstraint struct {
	Name  string `json:"name" yaml:"name"`
	Value string `json:"value" yaml:"value"`
}

// SummaryComponent is one recipe component. Version holds the chart version
// of a Helm component and the tag of a Kustomize component.
type SummaryComponent struct {
	Name      string   `json:"name" yaml:"name"`
	Type      string   `json:"type,omitempty" yaml:"type,omitempty"`
	Version   string   `json:"version,omitempty" yaml:"version,omitempty"`
	Namespace string   `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty" yaml:"dependsOn,omitempty"`
}

// SummaryPhase is the checks one validation phase runs. Name is one of the
// SummaryPhase* constants.
type SummaryPhase struct {
	Name   string   `json:"name" yaml:"name"`
	Checks []string `json:"checks" yaml:"checks"`
}

// BuildSummary projects r into a Summary. It performs no I/O and does not
// modify r. Returns ErrCodeInvalidRequest when r is nil.
func BuildSummary(r *RecipeResult) (*Summary, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe is required to build a summary")
	}

	s := &Summary{
		APIVersion:      r.APIVersion,
		Kind:            r.Kind,
		Version:         r.Metadata.Version,
		GPUDriver:       r.Metadata.GPUDriverState,
		AppliedOverlays: append([]string(nil), r.Metadata.AppliedOverlays...),
		Components:      summaryComponents(r),
		Validation:      summaryPhases(r.Validation),
	}
	if s.GPUDriver == "" {
		s.GPUDriver = "unknown"
	}
	if p := r.Metadata.SelectedProfile; p != nil {
		s.Profile = p.Name + "=" + p.Value
	}
	if r.Criteria != nil {
		s.Criteria = *r.Criteria
	}
	for _, c := range r.Constraints {
		s.Constraints = append(s.Constraints, SummaryConstraint{Name: c.Name, Value: c.Value})
	}
	return s, nil
}

// summaryComponents orders r's components by DeploymentOrder. Names in the
// order that match no component are skipped; components the order does not
// mention follow in ComponentRefs order.
func summaryComponents(r *RecipeResult) []SummaryComponent {
	byName := make(map[string]int, len(r.ComponentRefs))
	for i, ref := range r.ComponentRefs {
		if _, dup := byName[ref.Name]; !dup {
			byName[ref.Name] = i
		}
	}

	placed := make([]bool, len(r.ComponentRefs))
	out := make([]SummaryComponent, 0, len(r.ComponentRefs))
	add := func(i int) {
		placed[i] = true
		out = append(out, summaryComponent(r.ComponentRefs[i]))
	}
	for _, name := range r.DeploymentOrder {
		if i, ok := byName[name]; ok && !placed[i] {
			add(i)
		}
	}
	for i := range r.ComponentRefs {
		if !placed[i] {
			add(i)
		}
	}
	return out
}

func summaryComponent(ref ComponentRef) SummaryComponent {
	version := ref.Version
	if ref.Type == ComponentTypeKustomize {
		version = ref.Tag
	}
	return SummaryComponent{
		Name:      ref.Name,
		Type:      string(ref.Type),
		Version:   version,
		Namespace: ref.Namespace,
		DependsOn: append([]string(nil), ref.DependencyRefs...),
	}
}

func summaryPhases(v *ValidationConfig) []SummaryPhase {
	if v == nil {
		return nil
	}
	var out []SummaryPhase
	for _, p := range []struct {
		name  string
		phase *ValidationPhase
	}{
		{SummaryPhaseReadiness, v.Readiness},
		{SummaryPhaseDeployment, v.Deployment},
		{SummaryPhasePerformance, v.Performance},
		{SummaryPhaseConformance, v.Conformance},
	} {
		if p.phase == nil || len(p.phase.Checks) == 0 {
			continue
		}
		out = append(out, SummaryPhase{Name: p.name, Checks: append([]string(nil), p.phase.Checks...)})
	}
	return out
}
