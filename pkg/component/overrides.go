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

package component

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// titleCaser is cached to avoid per-call allocation.
var titleCaser = cases.Title(language.English)

// String constants for override values.
const (
	strVersion = "version"
	strDriver  = "driver"
	strEnabled = "enabled"
)

// ApplyValueOverrides applies overrides to a struct using reflection.
// Supports dot-notation paths (e.g., "gds.enabled", "driver.version").
// Automatically handles type conversion for strings, bools, ints, and nested structs.
// Returns an error containing all failed overrides instead of stopping at the first failure.
func ApplyValueOverrides(target any, overrides map[string]string) error {
	if len(overrides) == 0 {
		return nil
	}

	targetValue := reflect.ValueOf(target)
	if targetValue.Kind() != reflect.Ptr {
		return errors.New(errors.ErrCodeInvalidRequest, "target must be a pointer to a struct")
	}

	targetValue = targetValue.Elem()
	if targetValue.Kind() != reflect.Struct {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("target must be a pointer to a struct, got %s", targetValue.Kind()))
	}

	// Collect all errors instead of failing on first error
	var errs []string
	for path, value := range overrides {
		if err := setFieldByPath(targetValue, path, value); err != nil {
			errs = append(errs, fmt.Sprintf("%s=%s: %v", path, value, err))
		}
	}

	if len(errs) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("failed to apply overrides: %s", strings.Join(errs, "; ")))
	}

	return nil
}

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

// setFieldByPath sets a field value using dot-notation path.
// Supports both flat fields (EnableGDS for "gds.enabled") and nested structs (Driver.Version for "driver.version").
func setFieldByPath(structValue reflect.Value, path, value string) error {
	parts := strings.Split(path, ".")

	// Special case: Handle multi-segment paths (e.g., "manager.resources.cpu.limit" -> "ManagerCPULimit")
	if len(parts) == 4 {
		flatFieldName := deriveMultiSegmentFieldName(parts)
		if flatFieldName != "" {
			if flatField, found := findField(structValue, flatFieldName, path); found {
				return setFieldValue(flatField, value)
			}
		}
	}

	// Special case: Try to find a flat field that matches the full path pattern first
	// This handles cases like "gds.enabled" -> "EnableGDS", "mig.strategy" -> "MIGStrategy"
	if len(parts) == 2 {
		flatFieldName := deriveFlatFieldName(parts[0], parts[1])
		if flatField, found := findField(structValue, flatFieldName, path); found {
			return setFieldValue(flatField, value)
		}
	}

	// Standard traversal for nested structs
	currentValue := structValue

	// Traverse to the target field
	for i, part := range parts {
		isLast := i == len(parts)-1

		// Convert path segment to field name (e.g., "gds" -> "GDS", "enabled" -> "Enabled")
		fieldName := pathToFieldName(part)

		// Find field (case-insensitive search)
		field, found := findField(currentValue, fieldName, part)
		if !found {
			return errors.New(errors.ErrCodeNotFound, fmt.Sprintf("field not found: %s (searching for %s in %s)", path, part, currentValue.Type()))
		}

		if !field.IsValid() {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid field: %s", path))
		}

		if !field.CanSet() {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("cannot set field: %s (field is not settable)", path))
		}

		if isLast {
			// Set the final field value
			return setFieldValue(field, value)
		}

		// Navigate to nested struct
		switch field.Kind() {
		case reflect.Struct:
			currentValue = field
		case reflect.Ptr:
			// Handle pointer to struct
			if field.IsNil() {
				// Create new struct instance
				field.Set(reflect.New(field.Type().Elem()))
			}
			currentValue = field.Elem()
		case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
			reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128,
			reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
			reflect.Slice, reflect.String, reflect.UnsafePointer:
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("cannot traverse non-struct field: %s (type: %s)", part, field.Type()))
		}
	}

	return nil
}

// deriveMultiSegmentFieldName handles paths with 4 segments.
// Examples:
//   - ["manager", "resources", "cpu", "limit"] -> "ManagerCPULimit"
//   - ["manager", "resources", "memory", "limit"] -> "ManagerMemoryLimit"
//   - ["manager", "resources", "cpu", "request"] -> "ManagerCPURequest"
//   - ["manager", "resources", "memory", "request"] -> "ManagerMemoryRequest"
func deriveMultiSegmentFieldName(parts []string) string {
	if len(parts) != 4 {
		return ""
	}

	// Handle Skyhook manager resource paths: manager.resources.{cpu|memory}.{limit|request}
	if strings.ToLower(parts[0]) == "manager" && strings.ToLower(parts[1]) == "resources" {
		resourceType := strings.ToUpper(parts[2]) // "cpu" -> "CPU", "memory" -> "MEMORY"
		if strings.ToLower(parts[2]) == "memory" {
			resourceType = "Memory"
		}
		actionType := pathToFieldName(parts[3]) // "limit" -> "Limit", "request" -> "Request"
		return "Manager" + resourceType + actionType
	}

	return ""
}

// deriveFlatFieldName creates a flat field name from a dotted path.
// Examples:
//   - ("gds", "enabled") -> "EnableGDS"
//   - ("mig", "strategy") -> "MIGStrategy"
//   - ("driver", "version") -> "DriverVersion"
//   - ("operator", "version") -> "GPUOperatorVersion"
//   - ("toolkit", "version") -> "NvidiaContainerToolkitVersion"
//   - ("driver", "repository") -> "DriverRegistry"
//   - ("sandboxWorkloads", "enabled") -> "EnableSecureBoot"
//   - ("driver", "useOpenKernelModules") -> "UseOpenKernelModule"
func deriveFlatFieldName(prefix, suffix string) string {
	prefixTitle := pathToFieldName(prefix)
	suffixTitle := pathToFieldName(suffix)

	// Handle special mappings for common patterns
	prefixLower := strings.ToLower(prefix)
	suffixLower := strings.ToLower(suffix)

	// Special case mappings based on prefix and suffix combinations
	switch {
	// operator.version -> GPUOperatorVersion
	case prefixLower == "operator" && suffixLower == strVersion:
		return "GPUOperatorVersion"
	// toolkit.version -> NvidiaContainerToolkitVersion
	case prefixLower == "toolkit" && suffixLower == strVersion:
		return "NvidiaContainerToolkitVersion"
	// driver.repository -> DriverRegistry
	case prefixLower == strDriver && suffixLower == "repository":
		return "DriverRegistry"
	// driver.registry -> DriverRegistry (Network Operator)
	case prefixLower == strDriver && suffixLower == "registry":
		return "DriverRegistry"
	// ofed.version -> OFEDVersion
	case prefixLower == "ofed" && suffixLower == strVersion:
		return "OFEDVersion"
	// ofed.deploy -> DeployOFED
	case prefixLower == "ofed" && suffixLower == "deploy":
		return "DeployOFED"
	// nic.type -> NicType
	case prefixLower == "nic" && suffixLower == "type":
		return "NicType"
	// containerRuntime.socket -> ContainerRuntimeSocket
	case prefixLower == "containerruntime" && suffixLower == "socket":
		return "ContainerRuntimeSocket"
	// hostDevice.enabled -> EnableHostDevice
	case prefixLower == "hostdevice" && suffixLower == strEnabled:
		return "EnableHostDevice"
	// operator.registry -> OperatorRegistry (Skyhook)
	case prefixLower == "operator" && suffixLower == "registry":
		return "OperatorRegistry"
	// kubeRbacProxy.version -> KubeRbacProxyVersion
	case prefixLower == "kuberbacproxy" && suffixLower == strVersion:
		return "KubeRbacProxyVersion"
	// agent.image -> SkyhookAgentImage
	case prefixLower == "agent" && suffixLower == "image":
		return "SkyhookAgentImage"
	}

	// Special case: tolerations.key -> TolerationKey
	if prefixLower == "tolerations" && suffixLower == "key" {
		return "TolerationKey"
	}

	// Special case: tolerations.value -> TolerationValue
	if prefixLower == "tolerations" && suffixLower == "value" {
		return "TolerationValue"
	}

	// Special case: sandboxWorkloads.enabled -> EnableSecureBoot
	if prefixLower == "sandboxworkloads" && suffixLower == strEnabled {
		return "EnableSecureBoot"
	}

	// Special case: driver.useOpenKernelModules -> UseOpenKernelModule (singular)
	if prefixLower == strDriver && (suffixLower == "useopenkernelmodules" || suffixLower == "useopenkernelmodule") {
		return "UseOpenKernelModule"
	}

	// Handle "enabled" suffix specially - often becomes "Enable<Prefix>"
	if suffixLower == strEnabled {
		return "Enable" + prefixTitle
	}

	// Otherwise concatenate: Driver + Version = DriverVersion
	return prefixTitle + suffixTitle
}

// findField searches for a field by name (case-insensitive) or by matching the original path segment.
func findField(structValue reflect.Value, fieldName, pathSegment string) (reflect.Value, bool) {
	structType := structValue.Type()

	// Try exact match first
	field := structValue.FieldByName(fieldName)
	if field.IsValid() {
		return field, true
	}

	// Try case-insensitive search
	for i := 0; i < structValue.NumField(); i++ {
		f := structType.Field(i)
		if strings.EqualFold(f.Name, fieldName) || strings.EqualFold(f.Name, pathSegment) {
			return structValue.Field(i), true
		}

		// Also try matching common patterns (e.g., "EnableGDS" for "gds.enabled")
		if matchesPattern(f.Name, pathSegment) {
			return structValue.Field(i), true
		}
	}

	return reflect.Value{}, false
}

// matchesPattern checks if a field name matches a path segment using common patterns.
// Examples:
//   - "EnableGDS" matches "gds" (Enable + acronym pattern)
//   - "DriverVersion" matches both "driver" and "version"
//   - "MIGStrategy" matches both "mig" and "strategy"
//   - "GPUOperatorVersion" matches "operator" or "gpu-operator"
func matchesPattern(fieldName, pathSegment string) bool {
	fieldLower := strings.ToLower(fieldName)
	segmentLower := strings.ToLower(pathSegment)

	// Check if field name contains the segment
	if strings.Contains(fieldLower, segmentLower) {
		return true
	}

	// Check for "Enable" prefix pattern (EnableGDS matches "gds")
	if strings.HasPrefix(fieldLower, "enable") {
		withoutEnable := strings.TrimPrefix(fieldLower, "enable")
		if withoutEnable == segmentLower {
			return true
		}
	}

	// Check for compound words (DriverVersion matches "driver", GPUOperatorVersion matches "operator")
	// Look for the segment at the start of the field name
	if strings.HasPrefix(fieldLower, segmentLower) {
		return true
	}

	// Handle dash-separated segments (gpu-operator matches GPUOperator)
	if strings.Contains(segmentLower, "-") {
		dashless := strings.ReplaceAll(segmentLower, "-", "")
		if strings.Contains(fieldLower, dashless) {
			return true
		}
	}

	return false
}

// pathToFieldName converts a path segment to a potential field name.
// Examples:
//   - "gds" -> "GDS"
//   - "enabled" -> "Enabled"
//   - "mig_strategy" -> "MIGStrategy"
func pathToFieldName(segment string) string {
	// Handle common acronyms that should stay uppercase
	acronyms := map[string]string{
		"gds":   "GDS",
		"gpu":   "GPU",
		"mig":   "MIG",
		"dcgm":  "DCGM",
		"cpu":   "CPU",
		"api":   "API",
		"cdi":   "CDI",
		"gdr":   "GDR",
		"rdma":  "RDMA",
		"sriov": "SRIOV",
		"vfio":  "VFIO",
		"vgpu":  "VGPU",
		"ofed":  "OFED",
		"crds":  "CRDs",
		"rbac":  "RBAC",
		"tls":   "TLS",
		"nfd":   "NFD",
		"gfd":   "GFD",
	}

	segmentLower := strings.ToLower(segment)

	// Check if it's a known acronym
	if acronym, found := acronyms[segmentLower]; found {
		return acronym
	}

	// Handle underscore-separated words (e.g., "mig_strategy" -> "MIGStrategy")
	if strings.Contains(segment, "_") {
		parts := strings.Split(segment, "_")
		var result strings.Builder
		for _, part := range parts {
			if acronym, found := acronyms[strings.ToLower(part)]; found {
				result.WriteString(acronym)
			} else {
				result.WriteString(titleCaser.String(part))
			}
		}
		return result.String()
	}

	// Handle dash-separated words (e.g., "gpu-operator" -> "GPUOperator")
	if strings.Contains(segment, "-") {
		parts := strings.Split(segment, "-")
		var result strings.Builder
		for _, part := range parts {
			if acronym, found := acronyms[strings.ToLower(part)]; found {
				result.WriteString(acronym)
			} else {
				result.WriteString(titleCaser.String(part))
			}
		}
		return result.String()
	}

	// Simple title case
	return titleCaser.String(segment)
}

// setFieldValue sets a reflect.Value with automatic type conversion.
func setFieldValue(field reflect.Value, value string) error {
	fieldType := field.Type()

	switch fieldType.Kind() {
	case reflect.String:
		field.SetString(value)
		return nil

	case reflect.Bool:
		boolVal, err := parseBool(value)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid boolean value %q", value), err)
		}
		field.SetBool(boolVal)
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		intVal, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid integer value %q", value), err)
		}
		field.SetInt(intVal)
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		uintVal, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid unsigned integer value %q", value), err)
		}
		field.SetUint(uintVal)
		return nil

	case reflect.Float32, reflect.Float64:
		floatVal, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid float value %q", value), err)
		}
		field.SetFloat(floatVal)
		return nil

	case reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128,
		reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Ptr, reflect.Slice, reflect.Struct, reflect.UnsafePointer:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported field type: %s", fieldType))
	}

	return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported field type: %s", fieldType))
}

// parseBool parses boolean values with support for various formats.
func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case StrTrue, "yes", "1", "on", strEnabled:
		return true, nil
	case StrFalse, "no", "0", "off", "disabled":
		return false, nil
	default:
		return false, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("cannot parse %q as boolean", value))
	}
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

// NodeSelectorToMatchExpressions converts a map of node selectors to matchExpressions format.
// This format is used by some CRDs like Skyhook that use label selector syntax.
// Each key=value pair becomes a matchExpression with operator "In" and single value.
func NodeSelectorToMatchExpressions(nodeSelector map[string]string) []map[string]any {
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
