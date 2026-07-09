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

package validations

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// overrideValueFalse is the string form of a disabled `--set <key>:enabled=false`
// override — value overrides are collected as strings, so booleans arrive as
// their string literal.
const overrideValueFalse = "false"

// init auto-registers validation functions in this package.
// This allows the registry to discover validation functions automatically.
func init() {
	// Register all validation functions in this package
	// This is called automatically when the package is imported
	registerCheck("CheckWorkloadSelectorMissing", CheckWorkloadSelectorMissing)
	registerCheck("CheckAcceleratedSelectorMissing", CheckAcceleratedSelectorMissing)
	registerCheck("CheckHostMofedWithoutNetworkOperator", CheckHostMofedWithoutNetworkOperator)
	registerCheck("CheckWildcardAcceleratedToleration", CheckWildcardAcceleratedToleration)
}

// registerCheck is a helper to register validation functions from checks.go.
// It's called from init() to auto-register functions.
func registerCheck(name string, fn ValidationFunc) {
	// Use Register which will initialize the registry if needed
	Register(name, fn)
}

// CheckWorkloadSelectorMissing checks if workload-selector is missing when conditions are met.
// This is a generic check that can be used by any component.
func CheckWorkloadSelectorMissing(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if bundlerConfig == nil {
		return nil, nil
	}

	// Check if component exists in recipe
	hasComponent := false
	for _, ref := range recipeResult.ComponentRefs {
		if ref.Name == componentName {
			hasComponent = true
			break
		}
	}

	if !hasComponent {
		return nil, nil
	}

	// Check conditions (e.g., intent: training)
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}

	// Check if workload-selector is not set
	selector := bundlerConfig.WorkloadSelector()
	if len(selector) == 0 {
		baseMsg := fmt.Sprintf("%s is enabled but --workload-selector is not set", componentName)
		slog.Warn(baseMsg,
			"component", componentName,
			"conditions", conditions,
		)
		return []string{baseMsg}, nil
	}

	return nil, nil
}

// CheckAcceleratedSelectorMissing checks if accelerated-node-selector is missing when conditions are met.
// This is a generic check that can be used by any component.
func CheckAcceleratedSelectorMissing(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if bundlerConfig == nil {
		return nil, nil
	}

	// Check if component exists in recipe
	hasComponent := false
	for _, ref := range recipeResult.ComponentRefs {
		if ref.Name == componentName {
			hasComponent = true
			break
		}
	}

	if !hasComponent {
		return nil, nil
	}

	// Check conditions (e.g., intent: [training, inference])
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}

	// Check if accelerated-node-selector is not set
	selector := bundlerConfig.AcceleratedNodeSelector()
	if len(selector) == 0 {
		baseMsg := fmt.Sprintf("%s is enabled but --accelerated-node-selector is not set", componentName)
		slog.Warn(baseMsg,
			"component", componentName,
			"conditions", conditions,
		)
		return []string{baseMsg}, nil
	}

	return nil, nil
}

// checkConditions verifies that the recipe result meets the specified conditions.
// Conditions are arrays of strings for OR matching (single element arrays are equivalent to single values).
// Reuses matching logic from recipe/criteria.go.
func checkConditions(recipeResult *recipe.RecipeResult, conditions map[string][]string) bool {
	if len(conditions) == 0 {
		return true
	}

	if recipeResult.Criteria == nil {
		return false
	}

	for key, expectedValues := range conditions {
		var actualValue string

		// Get actual value from criteria
		switch key {
		case "intent":
			actualValue = string(recipeResult.Criteria.Intent)
		case "service":
			actualValue = string(recipeResult.Criteria.Service)
		case "accelerator":
			actualValue = string(recipeResult.Criteria.Accelerator)
		case "os":
			actualValue = string(recipeResult.Criteria.OS)
		case "platform":
			actualValue = string(recipeResult.Criteria.Platform)
		default:
			// Unknown condition key, skip
			continue
		}

		// Check if actualValue matches any of the expected values (OR matching)
		found := false
		for _, expectedStr := range expectedValues {
			// Use recipe.MatchesCriteriaField for consistent matching logic
			if recipe.MatchesCriteriaField(actualValue, expectedStr) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

// nodewrightCustomizationsOverrideAliases are the registry valueOverrideKeys for
// the nodewright-customizations component — the aliases (beyond the exact
// component name) a user passes to --set to disable it, e.g.
// --set nodewrightcustomizations:enabled=false. The bundler resolves --set
// overrides under the exact component name AND these aliases
// (DefaultBundler.componentOverrideKeys), so the disable check below mirrors
// that set to avoid a false positive when a user disables via one form and the
// check reads another.
var nodewrightCustomizationsOverrideAliases = []string{"nodewrightcustomizations", "skyhookcustomizations"}

// CheckWildcardAcceleratedToleration reports when the effective accelerated-node
// tolerations for a component include a wildcard (keyless operator: Exists)
// toleration. Scope it via registry conditions to services where the wildcard
// is harmful — on AKS, admission collapses a pod's toleration list to just the
// wildcard when one is present, which defeats the nodewright operator's drain
// exemption for its own package pods and deadlocks packages that declare
// interrupts (NVIDIA/nodewright#296). That deadlock requires manual node
// cordon/reboot to recover, so the registry wires this at severity: error to
// block the bundle until a keyed toleration is supplied.
//
// The default bundle path always hits this: with no
// --accelerated-node-toleration flag the CLI falls back to
// snapshotter.DefaultTolerations() (a single bare operator: Exists). An empty
// toleration list is flagged too, because the tuning manifest template renders
// its own wildcard fallback when none are injected.
//
// A component disabled via --set (e.g. the documented RDMA opt-out
// --set nodewrightcustomizations:enabled=false) renders no package pods and
// cannot deadlock, so it is skipped regardless of the toleration shape.
func CheckWildcardAcceleratedToleration(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if bundlerConfig == nil {
		return nil, nil
	}

	// Check if component exists in recipe
	hasComponent := false
	for _, ref := range recipeResult.ComponentRefs {
		if ref.Name == componentName {
			hasComponent = true
			break
		}
	}

	if !hasComponent {
		return nil, nil
	}

	// Check conditions (e.g., service: aks)
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}

	// A disabled component renders nothing, so it cannot deadlock — skip it.
	// Check the exact component name and its registry aliases, mirroring how
	// the bundler resolves --set overrides so any disable form is honored.
	overrides := bundlerConfig.ValueOverrides()
	for _, key := range append([]string{componentName}, nodewrightCustomizationsOverrideAliases...) {
		if overrides[key]["enabled"] == overrideValueFalse {
			return nil, nil
		}
	}

	tolerations := bundlerConfig.AcceleratedNodeTolerations()
	wildcard := len(tolerations) == 0 // template falls back to its own wildcard
	for _, tol := range tolerations {
		if tol.Key == "" {
			wildcard = true
			break
		}
	}

	if !wildcard {
		return nil, nil
	}

	baseMsg := fmt.Sprintf("%s renders a wildcard (keyless) accelerated-node toleration", componentName)
	slog.Warn(baseMsg,
		"component", componentName,
		"conditions", conditions,
	)
	return []string{baseMsg}, nil
}

// CheckHostMofedWithoutNetworkOperator warns when network-operator is disabled
// via --set but gpu-operator still has driver.rdma.useHostMofed=true (the
// AKS default). Without network-operator, no host MOFED is present and
// useHostMofed should be set to false.
func CheckHostMofedWithoutNetworkOperator(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if bundlerConfig == nil {
		return nil, nil
	}

	// Check conditions (e.g., service: aks)
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}

	// Check if network-operator is disabled via --set
	overrides := bundlerConfig.ValueOverrides()
	netOpOverrides := overrides["networkoperator"]
	if netOpOverrides == nil {
		return nil, nil
	}

	enabledVal, hasEnabled := netOpOverrides["enabled"]
	if !hasEnabled || enabledVal != overrideValueFalse {
		return nil, nil
	}

	// network-operator is disabled — check if useHostMofed is overridden to false
	gpuOpOverrides := overrides["gpuoperator"]
	if gpuOpOverrides != nil {
		if mofedVal, ok := gpuOpOverrides["driver.rdma.useHostMofed"]; ok && mofedVal == overrideValueFalse {
			return nil, nil
		}
	}

	msg := fmt.Sprintf(
		"%s: network-operator is disabled but driver.rdma.useHostMofed is not set to false"+
			" — add --set gpuoperator:driver.rdma.useHostMofed=false to avoid MOFED-related errors",
		componentName,
	)
	slog.Warn(msg, "component", componentName)

	return []string{msg}, nil
}
