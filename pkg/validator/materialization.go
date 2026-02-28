// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package validator

import (
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// compareComponentsAgainstSnapshot compares recipe component references against
// actual deployment data found in a snapshot. Returns a ComponentResult per component.
func compareComponentsAgainstSnapshot(recipeResult *recipe.RecipeResult, snap *snapshotter.Snapshot) []ComponentResult {
	if snap == nil || recipeResult == nil || len(recipeResult.ComponentRefs) == 0 {
		return nil
	}

	helmData := findSubtypeData(snap, "helm")
	argocdData := findSubtypeData(snap, "argocd")

	results := make([]ComponentResult, 0, len(recipeResult.ComponentRefs))
	for _, ref := range recipeResult.ComponentRefs {
		switch ref.Type {
		case recipe.ComponentTypeHelm:
			results = append(results, compareHelmComponent(ref, helmData))
		case recipe.ComponentTypeKustomize:
			results = append(results, compareArgocdComponent(ref, argocdData))
		default:
			slog.Debug("unknown component type, skipping materialization check",
				"component", ref.Name, "type", ref.Type)
		}
	}

	return results
}

// findSubtypeData locates a named subtype within K8s measurements and returns its data.
func findSubtypeData(snap *snapshotter.Snapshot, subtypeName string) map[string]measurement.Reading {
	for _, m := range snap.Measurements {
		if m.Type != measurement.TypeK8s {
			continue
		}
		if st := m.GetSubtype(subtypeName); st != nil {
			return st.Data
		}
	}
	return nil
}

// compareHelmComponent checks a Helm component reference against snapshot helm data.
func compareHelmComponent(ref recipe.ComponentRef, helmData map[string]measurement.Reading) ComponentResult {
	result := ComponentResult{
		Name: ref.Name,
		Type: string(recipe.ComponentTypeHelm),
		Expected: ComponentExpected{
			Chart:     ref.Chart,
			Version:   ref.Version,
			Namespace: ref.Namespace,
		},
	}

	if len(helmData) == 0 {
		result.Status = ValidationStatusSkipped
		result.Message = "no helm data in snapshot"
		return result
	}

	chartKey := ref.Name + ".chart"
	chartReading, found := helmData[chartKey]
	if !found {
		result.Status = ValidationStatusFail
		result.Message = "helm release not found in snapshot"
		return result
	}

	// Populate actual values from snapshot
	result.Actual.Chart = readingStr(chartReading)
	if r, ok := helmData[ref.Name+".version"]; ok {
		result.Actual.Version = readingStr(r)
	}
	if r, ok := helmData[ref.Name+".namespace"]; ok {
		result.Actual.Namespace = readingStr(r)
	}

	// Compare expected vs actual
	var mismatches []string
	if ref.Chart != "" && result.Actual.Chart != ref.Chart {
		mismatches = append(mismatches, "chart mismatch: expected "+ref.Chart+", got "+result.Actual.Chart)
	}
	if ref.Version != "" && result.Actual.Version != ref.Version {
		mismatches = append(mismatches, "version mismatch: expected "+ref.Version+", got "+result.Actual.Version)
	}
	if ref.Namespace != "" && result.Actual.Namespace != ref.Namespace {
		mismatches = append(mismatches, "namespace mismatch: expected "+ref.Namespace+", got "+result.Actual.Namespace)
	}

	if len(mismatches) > 0 {
		result.Status = ValidationStatusFail
		result.Message = strings.Join(mismatches, "; ")
	} else {
		result.Status = ValidationStatusPass
	}

	return result
}

// compareArgocdComponent checks a Kustomize component reference against ArgoCD snapshot data.
func compareArgocdComponent(ref recipe.ComponentRef, argocdData map[string]measurement.Reading) ComponentResult {
	expectedVersion := ref.Version
	if expectedVersion == "" {
		expectedVersion = ref.Tag
	}

	result := ComponentResult{
		Name: ref.Name,
		Type: string(recipe.ComponentTypeKustomize),
		Expected: ComponentExpected{
			Source:  ref.Source,
			Version: expectedVersion,
		},
	}

	if argocdData == nil {
		result.Status = ValidationStatusSkipped
		result.Message = "no argocd data in snapshot"
		return result
	}

	// Try single-source pattern first, then multi-source
	repoKey := ref.Name + ".source.repoURL"
	revisionKey := ref.Name + ".source.targetRevision"
	repoReading, found := argocdData[repoKey]
	if !found {
		repoKey = ref.Name + ".sources.0.repoURL"
		revisionKey = ref.Name + ".sources.0.targetRevision"
		repoReading, found = argocdData[repoKey]
	}

	if !found {
		result.Status = ValidationStatusFail
		result.Message = "argocd application not found in snapshot"
		return result
	}

	// Populate actual values
	result.Actual.Source = readingStr(repoReading)
	if r, ok := argocdData[revisionKey]; ok {
		result.Actual.Version = readingStr(r)
	}
	if r, ok := argocdData[ref.Name+".namespace"]; ok {
		result.Actual.Namespace = readingStr(r)
	}

	// Compare expected vs actual
	var mismatches []string
	if ref.Source != "" && result.Actual.Source != ref.Source {
		mismatches = append(mismatches, "source mismatch: expected "+ref.Source+", got "+result.Actual.Source)
	}
	if expectedVersion != "" && result.Actual.Version != expectedVersion {
		mismatches = append(mismatches, "version mismatch: expected "+expectedVersion+", got "+result.Actual.Version)
	}

	if len(mismatches) > 0 {
		result.Status = ValidationStatusFail
		result.Message = strings.Join(mismatches, "; ")
	} else {
		result.Status = ValidationStatusPass
	}

	return result
}

// readingStr extracts a string from a measurement.Reading, returning empty string for nil.
func readingStr(r measurement.Reading) string {
	if r == nil {
		return ""
	}
	if s, ok := r.Any().(string); ok {
		return s
	}
	return r.String()
}
