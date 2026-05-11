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
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// HydrateResult builds a fully hydrated map from a RecipeResult.
// Component values are merged via GetValuesForComponent so the output
// contains the final resolved configuration, not file references.
func HydrateResult(result *RecipeResult) (map[string]any, error) {
	if result == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe result is nil")
	}

	hydrated := map[string]any{
		"kind":       result.Kind,
		"apiVersion": result.APIVersion,
		"metadata": map[string]any{
			"version":            result.Metadata.Version,
			"appliedOverlays":    result.Metadata.AppliedOverlays,
			"excludedOverlays":   result.Metadata.ExcludedOverlays,
			"constraintWarnings": result.Metadata.ConstraintWarnings,
		},
		"deploymentOrder": result.DeploymentOrder,
	}

	if result.Criteria != nil {
		hydrated["criteria"] = map[string]any{
			"service":     string(result.Criteria.Service),
			"accelerator": string(result.Criteria.Accelerator),
			"intent":      string(result.Criteria.Intent),
			"os":          string(result.Criteria.OS),
			"platform":    string(result.Criteria.Platform),
			"nodes":       result.Criteria.Nodes,
		}
	}

	if len(result.Constraints) > 0 {
		constraintList := make([]map[string]any, 0, len(result.Constraints))
		for _, c := range result.Constraints {
			entry := map[string]any{
				"name":   c.Name,
				keyValue: c.Value,
			}
			if c.Severity != "" {
				entry["severity"] = c.Severity
			}
			if c.Remediation != "" {
				entry["remediation"] = c.Remediation
			}
			if c.Unit != "" {
				entry["unit"] = c.Unit
			}
			constraintList = append(constraintList, entry)
		}
		hydrated["constraints"] = constraintList
	}

	components := make(map[string]any, len(result.ComponentRefs))
	for _, ref := range result.ComponentRefs {
		comp := map[string]any{
			"name":   ref.Name,
			"type":   string(ref.Type),
			"source": ref.Source,
		}

		if ref.Namespace != "" {
			comp["namespace"] = ref.Namespace
		}
		if ref.Chart != "" {
			comp["chart"] = ref.Chart
		}
		if ref.Version != "" {
			comp["version"] = ref.Version
		}
		if ref.Tag != "" {
			comp["tag"] = ref.Tag
		}
		if ref.Path != "" {
			comp["path"] = ref.Path
		}
		if len(ref.DependencyRefs) > 0 {
			comp["dependencyRefs"] = ref.DependencyRefs
		}

		values, err := result.GetValuesForComponent(ref.Name)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to hydrate values for component %q", ref.Name), err)
		}
		if len(values) > 0 {
			comp["values"] = values
		}

		components[ref.Name] = comp
	}
	hydrated["components"] = components

	return hydrated, nil
}

// Select walks a dot-path selector against a hydrated map and returns
// the value at that path. Returns ErrCodeNotFound for invalid paths.
// An empty selector returns the entire map.
func Select(hydrated map[string]any, selector string) (any, error) {
	selector = strings.TrimPrefix(selector, ".")
	if selector == "" {
		return hydrated, nil
	}

	parts := strings.Split(selector, ".")
	var current any = hydrated

	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("selector %q: cannot descend into non-map value at %q", selector, part))
		}

		val, exists := m[part]
		if !exists {
			available := sortedKeys(m)
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("selector %q: key %q not found, available keys: %s", selector, part, strings.Join(available, ", ")))
		}
		current = val
	}

	return current, nil
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
