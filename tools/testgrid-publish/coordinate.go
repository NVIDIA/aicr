// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

import (
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// RecipeCriteria holds the resolved dimensions extracted from recipe.yaml.
// The coordinate dimensions (Service–Platform) feed pkg/recipe.CoordinateFor;
// the K8s fields are testgrid-specific metadata that do not exist in recipe.Criteria.
type RecipeCriteria struct {
	Service       string // e.g. "eks"
	Accelerator   string // e.g. "h100"
	OS            string // e.g. "ubuntu"
	Intent        string // e.g. "training"
	Platform      string // optional, e.g. "kubeflow"
	K8sVersion    string // full semver with leading "v" stripped, e.g. "1.33.4"
	K8sConstraint string // declared constraint, e.g. ">=1.28"

	// ProfileSegment is the shared lowercase "<name>-<value>" segment
	// (attestation.ProfileSegment) for a profile-bearing recipe, or "".
	// Deliberately NOT joined into the TestGrid coordinate: per ADR-015,
	// the publisher's digest-bound build ID already partitions per value
	// (selection changes the bundle digest), so profiled runs stay on the
	// family's canonical tab. Recipe Health / presence links are NOT yet
	// profile-aware — the AKS presence entries are withheld
	// (pkg/testgrid/presence.yaml) until profile-aware Health links land.
	// The corroboration projection (pkg/evidence/project) is where the
	// segment is path-forming.
	ProfileSegment string
}

// CoordinateFor delegates to pkg/recipe.CoordinateFor after converting the
// plain-string RecipeCriteria to a typed *recipe.Criteria. The profile
// segment is intentionally not appended (see RecipeCriteria.ProfileSegment).
func CoordinateFor(c RecipeCriteria) (recipe.Coordinate, error) {
	coord, err := recipe.CoordinateFor(toRecipeCriteria(c))
	if err != nil {
		return recipe.Coordinate{}, err
	}
	return coord, nil
}

// toRecipeCriteria converts our local plain-string struct to the typed
// *recipe.Criteria required by pkg/recipe.CoordinateFor. Normalization
// (lowercase, trimSpace) has already been applied by parseCriteria.
// Validation (empty, "any", "/") is enforced by recipe.CoordinateFor itself.
func toRecipeCriteria(c RecipeCriteria) *recipe.Criteria {
	return &recipe.Criteria{
		Service:     recipe.CriteriaServiceType(c.Service),
		Accelerator: recipe.CriteriaAcceleratorType(c.Accelerator),
		OS:          recipe.CriteriaOSType(c.OS),
		Intent:      recipe.CriteriaIntentType(c.Intent),
		Platform:    recipe.CriteriaPlatformType(c.Platform),
	}
}
