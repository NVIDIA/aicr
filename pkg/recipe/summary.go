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
	"github.com/NVIDIA/aicr/pkg/header"
)

// RecipeSummaryKind is the kind of a Summary document. It differs from
// RecipeResultKind so that a saved summary fails the recipe loader's kind
// gate instead of loading as a recipe.
const RecipeSummaryKind = "RecipeSummary"

// RecipeSummaryAPIVersion is the apiVersion of a Summary document: the
// ADR-022 §7 default for a new kind. The kind is output-only; AICR never
// reads it back, so it has no read gate.
const RecipeSummaryAPIVersion = header.GroupVersionV1Beta1

// Validation phase names as they appear in Summary.Validation, in run order:
// the readiness pre-flight, then validator.PhaseOrder (deployment,
// conformance, performance). pkg/recipe cannot import pkg/validator, so this
// order is kept by hand; a pkg/cli test fails if it drifts.
const (
	SummaryPhaseReadiness   = "readiness"
	SummaryPhaseDeployment  = "deployment"
	SummaryPhaseConformance = "conformance"
	SummaryPhasePerformance = "performance"
)

// Summary is the in-memory projection of an already loaded or resolved
// RecipeResult. Its construction performs no I/O; upstream profile-coherence
// validation remains separate. Every slice keeps a deterministic order:
// enabled components follow deployment order, constraints and overlays keep
// recipe order, and validation phases run readiness, deployment, conformance,
// performance.
type Summary struct {
	// APIVersion and Kind identify the summary document itself:
	// RecipeSummaryAPIVersion and RecipeSummaryKind.
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`

	// Source is the header of the recipe the summary was built from.
	Source SummarySource `json:"source" yaml:"source"`

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

	// Components are the recipe's enabled components in deployment order.
	// Components disabled by enabled: false or install: false are omitted.
	Components []SummaryComponent `json:"components" yaml:"components"`

	// Validation lists the validation phases that declare checks.
	Validation []SummaryPhase `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// SummarySource is the apiVersion and kind of the recipe a Summary was built
// from.
type SummarySource struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
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
		APIVersion:      RecipeSummaryAPIVersion,
		Kind:            RecipeSummaryKind,
		Source:          SummarySource{APIVersion: r.APIVersion, Kind: r.Kind},
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

// summaryComponents orders r's enabled components by DeploymentOrder. Names
// in the order that match no enabled component are skipped; enabled
// components the order does not mention follow in ComponentRefs order.
// Disabled components are omitted, as the bundler and the deployment order
// omit them.
func summaryComponents(r *RecipeResult) []SummaryComponent {
	declared, enabled := componentSets(r.ComponentRefs)
	byName := make(map[string]int, len(r.ComponentRefs))
	// done marks refs already listed, and disabled refs, which are never listed.
	done := make([]bool, len(r.ComponentRefs))
	for i, ref := range r.ComponentRefs {
		if !ref.IsEnabled() {
			done[i] = true
			continue
		}
		if _, dup := byName[ref.Name]; !dup {
			byName[ref.Name] = i
		}
	}

	out := make([]SummaryComponent, 0, len(byName))
	add := func(i int) {
		done[i] = true
		out = append(out, summaryComponent(r.ComponentRefs[i], declared, enabled))
	}
	for _, name := range r.DeploymentOrder {
		if i, ok := byName[name]; ok && !done[i] {
			add(i)
		}
	}
	for i := range r.ComponentRefs {
		if !done[i] {
			add(i)
		}
	}
	return out
}

// summaryComponent projects ref. Like the bundler, it drops dependency edges
// to declared-but-disabled components (satisfied externally) and keeps edges
// to undeclared names verbatim.
func summaryComponent(ref ComponentRef, declared, enabled map[string]struct{}) SummaryComponent {
	version := ref.Version
	if ref.Type == ComponentTypeKustomize {
		version = ref.Tag
	}
	var dependsOn []string
	for _, dep := range ref.DependencyRefs {
		_, isDeclared := declared[dep]
		_, isEnabled := enabled[dep]
		if isDeclared && !isEnabled {
			continue
		}
		dependsOn = append(dependsOn, dep)
	}
	return SummaryComponent{
		Name:      ref.Name,
		Type:      string(ref.Type),
		Version:   version,
		Namespace: ref.Namespace,
		DependsOn: dependsOn,
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
		{SummaryPhaseConformance, v.Conformance},
		{SummaryPhasePerformance, v.Performance},
	} {
		if p.phase == nil || len(p.phase.Checks) == 0 {
			continue
		}
		out = append(out, SummaryPhase{Name: p.name, Checks: append([]string(nil), p.phase.Checks...)})
	}
	return out
}
