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

package aicrjob

import (
	"encoding/json"
	"strings"
)

// RecipeResultKind is the kind of AICR's resolved recipe artifact.
const RecipeResultKind = "RecipeResult"

// SupportedRecipeAPIVersions are the RecipeResult apiVersions observed from
// AICR v1.0.0. A recipe that carries a configuration block (for example the
// GKE TCPXO interfaces) is emitted as aicr.run/v1beta2.
var SupportedRecipeAPIVersions = map[string]bool{
	"aicr.run/v1":      true,
	"aicr.run/v1beta2": true,
}

// recipeRequired are RecipeResult's required fields in AICR's published
// schema (api/aicr/v1/schemas/RecipeResult.schema.json at v1.0.0).
var recipeRequired = withHeader("criteria", "componentRefs", "deploymentOrder")

// RecipeResult is the subset of AICR's RecipeResult the operator reads:
// identity, criteria, components and their order. Fields AICR adds later are
// ignored, matching AICR's additive rules.
type RecipeResult struct {
	APIVersion      string         `json:"apiVersion"`
	Kind            string         `json:"kind"`
	Metadata        RecipeMetadata `json:"metadata"`
	Criteria        *Criteria      `json:"criteria"`
	ComponentRefs   []ComponentRef `json:"componentRefs"`
	DeploymentOrder []string       `json:"deploymentOrder"`
}

// RecipeMetadata identifies the AICR release that produced the recipe.
type RecipeMetadata struct {
	Version         string           `json:"version"`
	AppliedOverlays []string         `json:"appliedOverlays,omitempty"`
	SelectedProfile *SelectedProfile `json:"selectedProfile,omitempty"`
}

// SelectedProfile is the configuration profile value AICR selected for a
// profiled recipe (AICR ADR-015), such as gpuStack=gke-default.
type SelectedProfile struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EvidenceName is the recipe name AICR records in its evidence
// (pkg/evidence/attestation RecipeNameFor): the accelerator, service, OS,
// intent and platform that are set and not "any", joined by hyphens, or
// "recipe" when none is, with "-<profile>-<value>" in lower case appended for
// a profiled recipe.
func (r *RecipeResult) EvidenceName() string {
	var parts []string
	if c := r.Criteria; c != nil {
		for _, v := range []string{c.Accelerator, c.Service, c.OS, c.Intent, c.Platform} {
			if v != "" && v != "any" {
				parts = append(parts, v)
			}
		}
	}
	name := "recipe"
	if len(parts) > 0 {
		name = strings.Join(parts, "-")
	}
	if p := r.Metadata.SelectedProfile; p != nil {
		name += "-" + strings.ToLower(p.Name) + "-" + strings.ToLower(p.Value)
	}
	return name
}

// Criteria are the criteria a recipe was resolved for.
type Criteria struct {
	Service     string `json:"service,omitempty"`
	Accelerator string `json:"accelerator,omitempty"`
	Intent      string `json:"intent,omitempty"`
	OS          string `json:"os,omitempty"`
	Platform    string `json:"platform,omitempty"`
	Nodes       int    `json:"nodes,omitempty"`
}

// ComponentRef is one component of a recipe.
type ComponentRef struct {
	Name           string   `json:"name"`
	Namespace      string   `json:"namespace,omitempty"`
	Type           string   `json:"type"`
	Source         string   `json:"source"`
	Chart          string   `json:"chart,omitempty"`
	Version        string   `json:"version,omitempty"`
	Path           string   `json:"path,omitempty"`
	DependencyRefs []string `json:"dependencyRefs,omitempty"`
}

// ParseRecipeResult decodes a RecipeResult and applies the contract checks:
// a supported apiVersion and kind, every required field present, the version
// gate against the declared AICR version, and internal consistency of the
// component graph. Unknown fields are ignored.
func ParseRecipeResult(data []byte, declaredVersion string) (*RecipeResult, error) {
	if err := requireFields(data, RecipeResultKind, recipeRequired); err != nil {
		return nil, err
	}
	r := &RecipeResult{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, unsupported("decode RecipeResult: %v", err)
	}
	if r.Kind != RecipeResultKind || !SupportedRecipeAPIVersions[r.APIVersion] {
		return nil, unsupported("unsupported artifact %s %s", r.APIVersion, r.Kind)
	}
	if err := CheckArtifactVersion(declaredVersion, r.Metadata.Version); err != nil {
		return nil, err
	}
	return r, r.checkGraph()
}

func (r *RecipeResult) checkGraph() error {
	names := make(map[string]bool, len(r.ComponentRefs))
	for _, c := range r.ComponentRefs {
		if c.Name == "" || names[c.Name] {
			return unsupported("component name %q is empty or repeated", c.Name)
		}
		names[c.Name] = true
	}
	for _, c := range r.ComponentRefs {
		for _, d := range c.DependencyRefs {
			if !names[d] {
				return unsupported("component %q depends on unknown component %q", c.Name, d)
			}
		}
	}
	if len(r.DeploymentOrder) != len(r.ComponentRefs) {
		return unsupported("deploymentOrder lists %d components, componentRefs %d", len(r.DeploymentOrder), len(r.ComponentRefs))
	}
	seen := make(map[string]bool, len(r.DeploymentOrder))
	for _, n := range r.DeploymentOrder {
		if !names[n] || seen[n] {
			return unsupported("deploymentOrder entry %q is unknown or repeated", n)
		}
		seen[n] = true
	}
	return nil
}
