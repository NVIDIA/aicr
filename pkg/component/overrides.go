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

package component

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ApplyMapOverrides applies overrides to a map[string]any using dot-notation paths.
// Handles nested maps by traversing the path segments and creating nested maps as needed.
// Useful for applying --set flag overrides to values.yaml content.
func ApplyMapOverrides(target map[string]any, overrides map[string]string) error {
	if target == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "target map cannot be nil")
	}

	if len(overrides) == 0 {
		return nil
	}

	var errs []string
	for path, value := range overrides {
		if err := setMapValueByPath(target, path, value); err != nil {
			errs = append(errs, fmt.Sprintf("%s=%s: %v", path, value, err))
		}
	}

	if len(errs) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to apply map overrides: %s", strings.Join(errs, "; ")))
	}

	return nil
}

// getOrCreateNestedMap traverses a dot-separated path in a nested map,
// creating intermediate maps as needed, and returns the parent map
// and the final key. When strict is true, returns an error if an
// intermediate path segment exists but is not a map. When strict is
// false, non-map values are silently replaced with new maps.
func getOrCreateNestedMap(m map[string]any, path string, strict bool) (map[string]any, string, error) {
	parts := strings.Split(path, ".")
	current := m

	for _, part := range parts[:len(parts)-1] {
		if next, ok := current[part]; ok {
			if nextMap, ok := next.(map[string]any); ok {
				current = nextMap
			} else if strict {
				return nil, "", errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("path segment %q exists but is not a map (type: %T)", part, next))
			} else {
				newMap := make(map[string]any)
				current[part] = newMap
				current = newMap
			}
		} else {
			newMap := make(map[string]any)
			current[part] = newMap
			current = newMap
		}
	}

	return current, parts[len(parts)-1], nil
}

// setMapValueByPath sets a value in a nested map using dot-notation path.
// Creates nested maps as needed. Converts string values to bools when appropriate.
func setMapValueByPath(target map[string]any, path, value string) error {
	parent, key, err := getOrCreateNestedMap(target, path, true)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to resolve override path", err)
	}

	parent[key] = convertMapValue(value)

	return nil
}

// convertMapValue converts a string value to an appropriate Go type.
// Handles bools ("true"/"false") and numbers.
func convertMapValue(value string) any {
	// Try bool conversion
	if value == StrTrue {
		return true
	}
	if value == StrFalse {
		return false
	}

	// Try integer conversion
	if i, err := strconv.ParseInt(value, 10, 64); err == nil {
		return i
	}

	// Try float conversion
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}

	// Return as string
	return value
}

// ApplyNodeSelectorOverrides applies node selector overrides to a values map.
// If nodeSelector is non-empty, it sets or merges with the existing nodeSelector field.
// The function applies to the specified paths in the values map (e.g., "nodeSelector", "webhook.nodeSelector").
func ApplyNodeSelectorOverrides(values map[string]any, nodeSelector map[string]string, paths ...string) {
	if len(nodeSelector) == 0 || values == nil {
		return
	}

	// Default to top-level "nodeSelector" if no paths specified
	if len(paths) == 0 {
		paths = []string{"nodeSelector"}
	}

	for _, path := range paths {
		setNodeSelectorAtPath(values, nodeSelector, path)
	}
}

// setNodeSelectorAtPath sets the node selector at the specified dot-notation path.
func setNodeSelectorAtPath(values map[string]any, nodeSelector map[string]string, path string) {
	parent, key, _ := getOrCreateNestedMap(values, path, false)

	// Set the node selector - convert map[string]string to map[string]any
	nsMap := make(map[string]any, len(nodeSelector))
	for k, v := range nodeSelector {
		nsMap[k] = v
	}
	parent[key] = nsMap
}

// ApplyTolerationsOverrides applies toleration overrides to a values map.
// If tolerations is non-empty, it sets or replaces the existing tolerations field.
// The function applies to the specified paths in the values map (e.g., "tolerations", "webhook.tolerations").
func ApplyTolerationsOverrides(values map[string]any, tolerations []corev1.Toleration, paths ...string) {
	if len(tolerations) == 0 || values == nil {
		return
	}

	// Default to top-level "tolerations" if no paths specified
	if len(paths) == 0 {
		paths = []string{"tolerations"}
	}

	// Convert tolerations to YAML-friendly format
	tolList := TolerationsToPodSpec(tolerations)

	for _, path := range paths {
		setTolerationsAtPath(values, tolList, path)
	}
}

// setTolerationsAtPath sets the tolerations at the specified dot-notation path.
func setTolerationsAtPath(values map[string]any, tolerations []map[string]any, path string) {
	parent, key, _ := getOrCreateNestedMap(values, path, false)

	// Convert to []any for proper YAML serialization
	tolInterface := make([]any, len(tolerations))
	for i, t := range tolerations {
		tolInterface[i] = t
	}
	parent[key] = tolInterface
}

// TolerationsToPodSpec converts a slice of corev1.Toleration to a YAML-friendly format.
// This format matches what Kubernetes expects in pod specs and Helm values.
func TolerationsToPodSpec(tolerations []corev1.Toleration) []map[string]any {
	result := make([]map[string]any, 0, len(tolerations))

	for _, t := range tolerations {
		tolMap := make(map[string]any)

		// Only include non-empty fields to keep YAML clean
		if t.Key != "" {
			tolMap["key"] = t.Key
		}
		if t.Operator != "" {
			tolMap["operator"] = string(t.Operator)
		}
		if t.Value != "" {
			tolMap["value"] = t.Value
		}
		if t.Effect != "" {
			tolMap["effect"] = string(t.Effect)
		}
		if t.TolerationSeconds != nil {
			tolMap["tolerationSeconds"] = *t.TolerationSeconds
		}

		result = append(result, tolMap)
	}

	return result
}

// navigateParent walks target along path's parent segments without mutating
// the map. Returns (parent, finalKey, true) when every intermediate segment
// resolves to a nested map, or (nil, "", false) if any intermediate is
// missing or exists but is not a map. The final segment is not looked up —
// callers read, write, or delete it themselves.
func navigateParent(target map[string]any, path string) (map[string]any, string, bool) {
	parts := strings.Split(path, ".")
	current := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part]
		if !ok {
			return nil, "", false
		}
		nextMap, ok := next.(map[string]any)
		if !ok {
			return nil, "", false
		}
		current = nextMap
	}
	return current, parts[len(parts)-1], true
}

// GetValueByPath retrieves a value from a nested map using dot-notation path.
// Returns the value and true if found, or nil and false if any path segment is missing.
func GetValueByPath(target map[string]any, path string) (any, bool) {
	parent, key, ok := navigateParent(target, path)
	if !ok {
		return nil, false
	}
	val, ok := parent[key]
	return val, ok
}

// RemoveValueByPath removes a value from a nested map at the given dot-notation path.
// Returns true if the value existed and was removed, false otherwise.
func RemoveValueByPath(target map[string]any, path string) bool {
	parent, key, ok := navigateParent(target, path)
	if !ok {
		return false
	}
	if _, exists := parent[key]; !exists {
		return false
	}
	delete(parent, key)
	return true
}

// SetValueByPath sets a value in a nested map at the given dot-notation path,
// creating intermediate maps as needed. Non-map intermediate segments are
// replaced with new maps (permissive mode).
func SetValueByPath(target map[string]any, path string, value any) {
	parent, key, _ := getOrCreateNestedMap(target, path, false)
	parent[key] = value
}

// nodeSelectorToMatchExpressions converts a map of node selectors to matchExpressions format.
// This format is used by some CRDs like Skyhook that use label selector syntax.
// Each key=value pair becomes a matchExpression with operator "In" and single value.
func nodeSelectorToMatchExpressions(nodeSelector map[string]string) []map[string]any {
	if len(nodeSelector) == 0 {
		return nil
	}

	result := make([]map[string]any, 0, len(nodeSelector))
	for key, value := range nodeSelector {
		expr := map[string]any{
			"key":      key,
			"operator": "In",
			"values":   []string{value},
		}
		result = append(result, expr)
	}

	return result
}
