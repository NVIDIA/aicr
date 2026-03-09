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

package main

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
	"github.com/NVIDIA/aicr/validators"
)

// checkHelmValues compares intended recipe values against deployed Helm release
// values captured in the snapshot.
func checkHelmValues(ctx *validators.Context) error {
	if ctx.Snapshot == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "snapshot is not available")
	}
	if ctx.Recipe == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "recipe is not available")
	}

	helmData := getHelmSubtypeData(ctx)
	if helmData == nil {
		return validators.Skip("no helm data in snapshot")
	}

	var failures []string

	for _, ref := range ctx.Recipe.ComponentRefs {
		if ref.Type != recipe.ComponentTypeHelm {
			continue
		}

		intended, err := ctx.Recipe.GetValuesForComponent(ref.Name)
		if err != nil {
			slog.Warn("could not get values for component", "component", ref.Name, "error", err)
			continue
		}
		if len(intended) == 0 {
			continue
		}

		if _, ok := helmData[ref.Name+".chart"]; !ok {
			slog.Info("component not found in snapshot helm data, skipping", "component", ref.Name)
			continue
		}

		flat := flattenValues(intended)
		for key, expectedVal := range flat {
			snapshotKey := ref.Name + ".values." + key
			deployed, ok := helmData[snapshotKey]
			if !ok {
				continue
			}

			deployedStr := deployed.String()
			if !helmValuesEqual(expectedVal, deployedStr) {
				failures = append(failures, fmt.Sprintf(
					"%s: key %q expected %q, got %q",
					ref.Name, key, expectedVal, deployedStr))
			}
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		// Evidence to stdout
		fmt.Println("Helm values mismatches:")
		for _, f := range failures {
			fmt.Printf("  %s\n", f)
		}
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("helm values mismatch: %d key(s) differ", len(failures)))
	}

	fmt.Println("All Helm values match recipe configuration")
	return nil
}

func getHelmSubtypeData(ctx *validators.Context) map[string]measurement.Reading {
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

func helmValuesEqual(expected, actual string) bool {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)

	if expected == actual {
		return true
	}

	if ef, ee := strconv.ParseFloat(expected, 64); ee == nil {
		if af, ae := strconv.ParseFloat(actual, 64); ae == nil {
			return ef == af
		}
	}

	if eb, ee := strconv.ParseBool(expected); ee == nil {
		if ab, ae := strconv.ParseBool(actual); ae == nil {
			return eb == ab
		}
	}

	return false
}
