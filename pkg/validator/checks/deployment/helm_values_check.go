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

package deployment

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "helm-values",
		Description: "Verify deployed Helm values match recipe configuration",
		Phase:       "deployment",
		Func:        validateHelmValues,
		TestName:    "TestCheckHelmValues",
	})
}

// validateHelmValues compares intended recipe values against deployed Helm release
// values captured in the snapshot. Only user-supplied values (from the recipe's
// ValuesFile + Overrides) are compared against user-supplied values from the
// Helm release (.Config). Chart defaults are excluded from both sides.
func validateHelmValues(ctx *checks.ValidationContext) error {
	if ctx.Snapshot == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "snapshot is not available")
	}
	if ctx.Recipe == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "recipe is not available")
	}

	helmData := getHelmSubtypeData(ctx)
	if helmData == nil {
		slog.Debug("no helm data in snapshot, skipping helm-values check")
		return nil
	}

	var failures []string

	for _, ref := range ctx.Recipe.ComponentRefs {
		if ref.Type != recipe.ComponentTypeHelm {
			continue
		}

		intended, err := ctx.Recipe.GetValuesForComponent(ref.Name)
		if err != nil {
			slog.Debug("could not get values for component",
				slog.String("component", ref.Name),
				slog.String("error", err.Error()))
			continue
		}
		if len(intended) == 0 {
			continue
		}

		// Check if this component exists in the snapshot's helm data
		if _, ok := helmData[ref.Name+".chart"]; !ok {
			slog.Debug("component not found in snapshot helm data, skipping",
				slog.String("component", ref.Name))
			continue
		}

		flat := flattenValues(intended)
		for key, expectedVal := range flat {
			snapshotKey := ref.Name + ".values." + key
			deployed, ok := helmData[snapshotKey]
			if !ok {
				// Key not in snapshot — the user set this value but it wasn't
				// captured. This isn't necessarily a mismatch (snapshot may
				// not capture every value), so skip.
				continue
			}

			deployedStr := deployed.String()
			if !valuesEqual(expectedVal, deployedStr) {
				failures = append(failures, fmt.Sprintf(
					"%s: key %q expected %q, got %q",
					ref.Name, key, expectedVal, deployedStr))
			}
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("helm values mismatch:\n  %s", strings.Join(failures, "\n  ")))
	}
	return nil
}

// getHelmSubtypeData finds the K8s measurement's "helm" subtype and returns its data.
func getHelmSubtypeData(ctx *checks.ValidationContext) map[string]measurement.Reading {
	for _, m := range ctx.Snapshot.Measurements {
		if m.Type == measurement.TypeK8s {
			st := m.GetSubtype("helm")
			if st != nil {
				return st.Data
			}
		}
	}
	return nil
}

// flattenValues recursively flattens a nested map into dot-notation keys with
// string values suitable for comparison against snapshot readings.
func flattenValues(data map[string]any) map[string]string {
	result := make(map[string]string)
	flattenValuesRecursive(data, "", result)
	return result
}

func flattenValuesRecursive(data map[string]any, prefix string, result map[string]string) {
	for key, value := range data {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}

		switch v := value.(type) {
		case map[string]any:
			flattenValuesRecursive(v, fullKey, result)
		case []any:
			if len(v) > 0 {
				jsonBytes, err := json.Marshal(v)
				if err == nil {
					result[fullKey] = string(jsonBytes)
				}
			}
		case string:
			result[fullKey] = v
		case bool:
			result[fullKey] = fmt.Sprintf("%t", v)
		case float64:
			result[fullKey] = fmt.Sprintf("%v", v)
		case int:
			result[fullKey] = fmt.Sprintf("%d", v)
		case int64:
			result[fullKey] = fmt.Sprintf("%d", v)
		default:
			result[fullKey] = fmt.Sprintf("%v", v)
		}
	}
}

// valuesEqual compares two string representations of values, normalizing
// common type differences between YAML parsing and Helm's JSON storage.
// For example, YAML parses 1 as int while JSON parses it as float64;
// both flatten to "1" but edge cases like "1.0" vs "1" need handling.
func valuesEqual(expected, actual string) bool {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)

	if expected == actual {
		return true
	}

	// Numeric normalization: parse both as float64 to handle
	// int vs float differences (YAML int 1 vs JSON float64 1.0).
	if ef, ee := strconv.ParseFloat(expected, 64); ee == nil {
		if af, ae := strconv.ParseFloat(actual, 64); ae == nil {
			return ef == af
		}
	}

	// Boolean normalization: case-insensitive comparison
	// handles "True"/"true", "FALSE"/"false".
	if eb, ee := strconv.ParseBool(expected); ee == nil {
		if ab, ae := strconv.ParseBool(actual); ae == nil {
			return eb == ab
		}
	}

	return false
}
