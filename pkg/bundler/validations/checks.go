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
	stderrors "errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// overrideValueFalse is the string form of a disabled `--set <key>:enabled=false`
// override — value overrides are collected as strings, so booleans arrive as
// their string literal.
const overrideValueFalse = "false"

// logKeyComponent is the shared structured-logging / error-context key
// naming the component a validation message or error is about.
const logKeyComponent = "component"

// init auto-registers validation functions in this package.
// This allows the registry to discover validation functions automatically.
func init() {
	// Register all validation functions in this package
	// This is called automatically when the package is imported
	registerCheck("CheckWorkloadSelectorMissing", CheckWorkloadSelectorMissing)
	registerCheck("CheckAcceleratedSelectorMissing", CheckAcceleratedSelectorMissing)
	registerCheck("CheckHostMofedWithoutNetworkOperator", CheckHostMofedWithoutNetworkOperator)
	registerCheck("CheckWildcardAcceleratedToleration", CheckWildcardAcceleratedToleration)
	registerCheck("CheckDriverOwnershipCoherence", CheckDriverOwnershipCoherence)
	registerCheck("CheckMariaDBOperatorOwnershipCoherence", CheckMariaDBOperatorOwnershipCoherence)
	registerCheck("CheckNVSentinelDriverLabelDetectable", CheckNVSentinelDriverLabelDetectable)
	registerCheck("CheckNVSentinelRuntimeClassCoherence", CheckNVSentinelRuntimeClassCoherence)
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
			logKeyComponent, componentName,
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
			logKeyComponent, componentName,
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
		logKeyComponent, componentName,
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
	slog.Warn(msg, logKeyComponent, componentName)

	return []string{msg}, nil
}

// draDriverComponentName is the CANONICAL ComponentRef.Name used by the
// recipe registry for the NVIDIA DRA GPU driver. Kept in sync with
// recipes/registry.yaml. Used only in log/error message text below —
// for actual component lookups, use resolveDRAComponentRef, which also
// matches the OCP variant (see draDriverComponentNames).
const draDriverComponentName = "nvidia-dra-driver-gpu"

// draDriverComponentNames are every registry-level component name that
// represents the NVIDIA DRA GPU driver, canonical plus OCP variant. This
// package cannot import pkg/bundler (dependency cycle — see
// componentOverrideKeys godoc for the same constraint elsewhere in this
// file), so this list is a local duplicate of pkg/bundler's
// draComponentNames; keep both in sync.
var draDriverComponentNames = []string{draDriverComponentName, "nvidia-dra-driver-gpu-ocp"}

// resolveDRAComponentRef looks up the DRA driver's ComponentRef by
// trying every known name variant in turn. recipeResult.GetComponentRef
// only matches an EXACT name, so a bare call with the canonical
// "nvidia-dra-driver-gpu" silently returns nil (skipping Rule 2
// entirely, with no error) on any recipe using the OCP variant instead.
func resolveDRAComponentRef(recipeResult *recipe.RecipeResult) *recipe.ComponentRef {
	for _, name := range draDriverComponentNames {
		if ref := recipeResult.GetComponentRef(name); ref != nil {
			return ref
		}
	}
	return nil
}

// operatorContainerDriverRoot is the host path the GPU Operator's driver
// container populates when the operator manages the driver
// (driver.enabled=true). It is also the gpu-operator chart default for
// hostPaths.driverInstallDir, so CheckDriverOwnershipCoherence uses it
// both as the fallback install dir when a recipe leaves the field unset
// and as the "legacy pre-flip recipe" signature: with
// driver.enabled=false nothing populates this path, so a DRA
// nvidiaDriverRoot still pointing at it is incoherent.
const operatorContainerDriverRoot = "/run/nvidia/driver"

// gpuOperatorManagedOverrideSet is the documented bundle-time override
// tuple that flips a preinstalled-driver recipe to GPU-Operator-managed
// mode. A private sibling of this constant (and of driverAbsentRemedy
// below) lives in pkg/client/v1's gpu_driver_state.go: importing across
// would create a dependency cycle through pkg/bundler, and a shared
// package for two small helpers was rejected — the duplication is the
// agreed disposition. Keep both copies in sync.
const gpuOperatorManagedOverrideSet = "--set gpuoperator:driver.enabled=true " +
	"--set gpuoperator:toolkit.enabled=true " +
	"--set gpuoperator:operator.runtimeClass=nvidia " +
	"--set dradriver:nvidiaDriverRoot=/run/nvidia/driver"

// gkeGPUOperatorManagedOverrideSet extends the override tuple for GKE
// remedies. GKE preinstalled-driver profiles (Google driver installer,
// documented for both COS and Ubuntu node images) pin
// hostPaths.driverInstallDir to /home/kubernetes/bin/nvidia, so a flip
// to operator-managed mode must move BOTH driver roots to the operator
// container root — otherwise the DRA lockstep rule blocks the exact
// bundle this remedy recommends.
const gkeGPUOperatorManagedOverrideSet = gpuOperatorManagedOverrideSet +
	" --set gpuoperator:hostPaths.driverInstallDir=/run/nvidia/driver"

// gkeManagedDriverRootPath is the Google driver-installer root that GKE
// preinstalled-driver profiles pin both driver roots to.
const gkeManagedDriverRootPath = "/home/kubernetes/bin/nvidia"

// legacyRecipeAlternativeRemedy returns the manual-override clause of the
// Rule 2 legacy-recipe message (the alternative to regenerating the
// recipe), scoped by service and OS exactly like driverAbsentRemedy: the
// GPU Operator cannot install the driver on GKE COS node images, so
// recommending the operator-managed override tuple there would produce a
// bundle that clears this gate coherently and then fails at deploy —
// COS gets a DRA-root retarget at the GKE-managed driver install path
// instead, and an unknown GKE OS gets both paths without asserting the
// recipe's OS supports either.
func legacyRecipeAlternativeRemedy(service recipe.CriteriaServiceType, os recipe.CriteriaOSType) string {
	const gkeCOSAlternative = "On GKE COS node images the GPU Operator cannot install the " +
		"driver, so the GPU-Operator-managed override set is not available there; if the " +
		"GPU nodes use the GKE-managed driver install, retarget the DRA driver root " +
		"instead: --set dradriver:nvidiaDriverRoot=" + gkeManagedDriverRootPath + "."
	if service != recipe.CriteriaServiceGKE {
		return "Or supply the full GPU-Operator-managed override set: " +
			gpuOperatorManagedOverrideSet + "."
	}
	switch os { //nolint:exhaustive // COS and Ubuntu are the only GKE node images with specific wording; everything else (unknown, any, or an OS GKE does not offer) gets both supported GKE paths
	case recipe.CriteriaOSCOS:
		return gkeCOSAlternative
	case recipe.CriteriaOSUbuntu:
		return "Or supply the full GPU-Operator-managed override set: " +
			gkeGPUOperatorManagedOverrideSet + "."
	default:
		return gkeCOSAlternative + " On GKE Ubuntu node images the GPU Operator can manage " +
			"the driver, so those may instead supply the full GPU-Operator-managed " +
			"override set: " + gkeGPUOperatorManagedOverrideSet + "."
	}
}

// driverAbsentRemedy returns the provider-appropriate remedy wording for
// the "preinstalled-driver recipe on a driverless cluster" mismatch,
// derived from the recipe's criteria service and OS. AKS pools created
// with `--gpu-driver none` are fixed by recreating them without the flag
// (or the override set); GKE+COS gets the COS-only wording (the GPU
// Operator cannot install the driver on COS), GKE+Ubuntu gets the
// operator-managed remedy (the pinned operator supports GKE driver
// management only on Ubuntu node images), and any other GKE OS —
// unknown, any, or one GKE does not offer — keeps the combined wording;
// anything else gets the generic reprovision wording plus the override
// set. Mirrors pkg/client/v1's resolution-time helper of the same name
// (see gpuOperatorManagedOverrideSet above for why the duplication
// exists).
func driverAbsentRemedy(service recipe.CriteriaServiceType, os recipe.CriteriaOSType, profiled bool) string {
	switch service { //nolint:exhaustive // only AKS and GKE have provider-specific wording; every other service takes the generic default
	case recipe.CriteriaServiceAKS:
		if !profiled {
			// Legacy pre-profile artifact: the ownership lock does not
			// apply (no metadata.selectedProfile), so the four-flag
			// bundle-time tuple remains this artifact's supported flip.
			return "Either recreate the GPU node pools without --gpu-driver " +
				"none (AKS installs the NVIDIA driver by default), or bundle " +
				"in GPU-Operator-managed mode: " + gpuOperatorManagedOverrideSet + "."
		}
		return "Either repair the AKS-managed driver install (recreate the " +
			"GPU node pools without --gpu-driver none; AKS installs the " +
			"NVIDIA driver by default) and recapture the snapshot, or switch " +
			"to operator-managed mode end to end: recreate the pools WITH " +
			"--gpu-driver none, recapture the snapshot, and regenerate with " +
			"--profile gpuStack=operator-managed. The operator-managed value's " +
			"constraint " +
			"requires the pools to read gpu-driver=None, so regenerating " +
			"against the current snapshot alone fails closed; and the " +
			"gpuStack profile owns the driver-ownership paths, so flipping " +
			"them via per-path --set overrides is rejected."
	case recipe.CriteriaServiceGKE:
		switch os { //nolint:exhaustive // COS and Ubuntu are the only GKE node images with specific wording; everything else (unknown, any, or an OS GKE does not offer) gets both supported GKE paths
		case recipe.CriteriaOSCOS:
			return "On GKE COS node images the GPU Operator cannot install the " +
				"driver. With the default gke-default gpuStack profile " +
				"(opt-out label absent) provision the GPU node pools with " +
				"the GKE-managed driver install (node pool " +
				"gpu-driver-version=default). With --profile " +
				"gpuStack=bundle-installer (pools labeled " +
				"gke-no-default-nvidia-gpu-device-plugin=true and created " +
				"with gpu-driver-version=disabled) the bundle's " +
				"gcp-driver-installer component supplies the driver with a " +
				"recipe-pinned version — do not deploy a standalone " +
				"DaemonSet alongside it; see " +
				"docs/integrator/gke-gpu-setup.md."
		case recipe.CriteriaOSUbuntu:
			// The pinned GPU Operator supports driver management
			// on GKE only on Ubuntu node images with containerd.
			return "On GKE Ubuntu node images the GPU Operator can manage " +
				"the driver: bundle in GPU-Operator-managed mode: " +
				gkeGPUOperatorManagedOverrideSet + "."
		default:
			// Unknown/any OS, or an OS GKE does not offer as a node image —
			// present both supported GKE paths without asserting the
			// recipe's OS supports either.
			return "On GKE COS node images the GPU Operator cannot install the " +
				"driver: provision the GPU node pools with the GKE-managed " +
				"driver install (node pool gpu-driver-version) instead. On " +
				"GKE Ubuntu node images the GPU Operator can manage the " +
				"driver, so those may bundle in GPU-Operator-managed mode: " +
				gkeGPUOperatorManagedOverrideSet + "."
		}
	default:
		return "Either reprovision the GPU nodes with a platform-installed " +
			"NVIDIA driver, or bundle in GPU-Operator-managed mode: " +
			gpuOperatorManagedOverrideSet + "."
	}
}

// componentOverrideKeys returns the candidate --set override-map keys for
// componentName, in priority order: the exact name first, then either the
// registry's ValueOverrideKeys aliases or (when the registry is
// unavailable) the non-hyphenated form. Reimplements
// DefaultBundler.componentOverrideKeys (pkg/bundler/bundler.go) locally —
// this package cannot import pkg/bundler (cycle) — so overrides supplied
// under canonical names and aliases are resolved exactly as the bundler
// resolves them when it renders the same values moments later.
func componentOverrideKeys(componentName string, provider recipe.DataProvider) []string {
	keys := []string{componentName}

	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		if nonHyphenated := strings.ReplaceAll(componentName, "-", ""); nonHyphenated != componentName {
			keys = append(keys, nonHyphenated)
		}
		return keys
	}

	if comp := registry.Get(componentName); comp != nil {
		keys = append(keys, comp.ValueOverrideKeys...)
	}

	return keys
}

// mergeOverridesAcrossKeys merges the per-path override maps stored under
// every candidate key into a single map, higher-priority key (earlier in
// keys) winning on a path collision. Local twin of the pkg/bundler helper
// of the same name — see componentOverrideKeys for why it is duplicated.
func mergeOverridesAcrossKeys[V any](allOverrides map[string]map[string]V, keys []string) map[string]V {
	var merged map[string]V
	for _, key := range slices.Backward(keys) {
		overrides, ok := allOverrides[key]
		if !ok {
			continue
		}
		if merged == nil {
			merged = make(map[string]V, len(overrides))
		}
		maps.Copy(merged, overrides)
	}
	return merged
}

// componentDisabled reports whether the component ref is disabled — by
// the recipe (overrides.enabled=false) or by a bundle-time --set enabled
// toggle. The toggle is resolved exactly as the bundler's
// filterEnabledComponents resolves it (getSetEnabledOverride,
// pkg/bundler/bundler.go): the enabled overrides supplied under the
// canonical component name and its registry aliases are merged with the
// canonical name winning on collision, then parsed with
// strconv.ParseBool so spellings like "0" and "False" count. A value
// that does not parse is treated as not-disabled here — the bundler
// rejects it with ErrCodeInvalidRequest in filterEnabledComponents,
// which runs before component validations, so the check never sees it
// in the bundle flow. A disabled component renders nothing, so it
// cannot participate in a driver mismatch.
func componentDisabled(ref *recipe.ComponentRef, bundlerConfig *config.Config, keys []string) bool {
	if !ref.IsEnabled() {
		return true
	}
	if bundlerConfig == nil {
		return false
	}
	merged := mergeOverridesAcrossKeys(bundlerConfig.ValueOverrides(), keys)
	raw, ok := merged[config.ComponentEnabledKey]
	if !ok {
		return false
	}
	enabled, parseErr := strconv.ParseBool(raw)
	if parseErr != nil {
		return false
	}
	return !enabled
}

// effectiveComponentValues resolves the FINAL effective Helm values for a
// component on behalf of the verification named by purpose (e.g.
// "driver-ownership coherence"), which the error messages quote so each
// gate reports its own subject.
//
// It resolves the values for a
// component: the recipe merge (base values → valuesFile → inline
// overrides) plus the user's bundle-time overrides applied in the
// bundler's own order — scalar --set first, then typed
// --set-json/--set-file — with the "enabled" toggle stripped (it controls
// component inclusion, not chart values). Overrides are resolved under
// the canonical component name and its registry aliases.
//
// A recipe-side resolution failure (GetValuesForComponentWithContext)
// returns a blocking error so the gate fails closed rather than skip a
// recipe whose driver ownership cannot be verified (e.g. a legacy recipe
// with a missing valuesFile path). The bundler's extractComponentValues
// independently fails closed on the same failure (pkg/bundler/bundler.go),
// so a bundle whose values cannot be resolved is never emitted by either
// path.
//
// Override-apply failures (--set / --set-json / --set-file) are blocking too.
// Bundle generation normally rejects them during value extraction before
// validations run, but this helper is also usable directly. Skipping an
// override it cannot reconstruct would disarm the gate for that candidate —
// with gpuDriverState=absent that could let a driverless configuration pass —
// so the gate independently fails closed.
func effectiveComponentValues(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, componentName string, keys []string, purpose string) (map[string]any, error) {
	values, err := recipeResult.GetValuesForComponentWithContext(ctx, componentName)
	if err != nil {
		// Preserve an already-coded classification (overlay read failures
		// are ErrCodeInternal, provider reads can be ErrCodeTimeout, ...):
		// reclassifying everything as ErrCodeInvalidRequest would turn
		// retryable infrastructure failures into deterministic 4xx caller
		// mistakes for SDK and server consumers, which map HTTP status
		// from the outermost code. Non-coded errors default to
		// invalid-request — the recipe content is what failed to resolve.
		code := aicrerrors.ErrCodeInvalidRequest
		if structured, ok := stderrors.AsType[*aicrerrors.StructuredError](err); ok {
			code = structured.Code
		}
		return nil, aicrerrors.WrapWithContext(code,
			fmt.Sprintf("cannot verify %s for component %q: "+
				"failed to resolve its effective values", purpose, componentName),
			err, map[string]any{logKeyComponent: componentName})
	}
	if bundlerConfig == nil {
		return values, nil
	}

	if setOverrides := mergeOverridesAcrossKeys(bundlerConfig.ValueOverrides(), keys); len(setOverrides) > 0 {
		delete(setOverrides, config.ComponentEnabledKey)
		if applyErr := component.ApplyMapOverrides(values, setOverrides); applyErr != nil {
			return nil, aicrerrors.WrapWithContext(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("cannot verify %s for component %q: "+
					"failed to apply --set overrides to its effective values", purpose, componentName),
				applyErr, map[string]any{logKeyComponent: componentName})
		}
	}

	if typedOverrides := mergeOverridesAcrossKeys(bundlerConfig.ValueOverridesTyped(), keys); len(typedOverrides) > 0 {
		delete(typedOverrides, config.ComponentEnabledKey)
		if applyErr := component.ApplyTypedOverrides(values, typedOverrides); applyErr != nil {
			return nil, aicrerrors.WrapWithContext(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("cannot verify %s for component %q: "+
					"failed to apply --set-json/--set-file overrides to its effective values", purpose, componentName),
				applyErr, map[string]any{logKeyComponent: componentName})
		}
	}

	return values, nil
}

// resolveInstallDir resolves the effective gpu-operator
// hostPaths.driverInstallDir: absent → the chart default
// (operatorContainerDriverRoot), declared non-empty string →
// path.Clean'd (trailing-slash spellings compare equal, mirroring
// pkg/recipe/driver_root_lockstep_test.go), declared empty string →
// the default (the operator's own transformForDriverInstallDir treats
// "" identically to the default, gpu-operator v26.7.0). An explicitly
// null or non-map hostPaths section is rejected with a blocking
// message: Helm null-coalescing deletes a null key together with its
// chart defaults, so the chart's unconditional .Values.hostPaths.rootFS
// access (clusterpolicy.yaml, v26.7.0) fails at install. A declared
// value that cleans to a relative path is rejected too — host-path
// mounts require absolute paths.
func resolveInstallDir(values map[string]any, componentName string) (string, bool, []string) {
	installDir := operatorContainerDriverRoot
	rawHostPaths, hostPathsPresent := values["hostPaths"]
	if !hostPathsPresent {
		return installDir, false, nil
	}
	if rawHostPaths == nil {
		return installDir, false, []string{fmt.Sprintf(
			"%s: hostPaths is explicitly null — Helm deletes a null key together with "+
				"its chart defaults, so the chart's unconditional hostPaths.* field "+
				"accesses fail at install. Remove the null override or set "+
				"hostPaths.driverInstallDir to a real path.", componentName)}
	}
	hostPaths, isMap := rawHostPaths.(map[string]any)
	if !isMap {
		return installDir, false, []string{fmt.Sprintf(
			"%s: hostPaths=%v (%T) is not a map, so hostPaths.driverInstallDir cannot "+
				"be read and driver-root coherence cannot be verified.",
			componentName, rawHostPaths, rawHostPaths)}
	}
	rawDir, leafPresent := hostPaths["driverInstallDir"]
	if !leafPresent {
		return installDir, false, nil
	}
	dir, isStr := rawDir.(string)
	if !isStr {
		// A present non-string leaf (null, boolean, number, object) is
		// rejected rather than silently defaulted: the emitted values
		// would carry it verbatim, and the pinned ClusterPolicy CRD
		// types hostPaths.driverInstallDir as a string (gpu-operator
		// v26.7.0 nvidia.com_clusterpolicies.yaml), so the install
		// fails while a defaulted check would have validated against
		// /run/nvidia/driver instead.
		return installDir, false, []string{fmt.Sprintf(
			"%s: hostPaths.driverInstallDir=%v (%T) is not a string — the ClusterPolicy "+
				"CRD requires a string path, so this value fails at install while the "+
				"coherence check would otherwise validate against the substituted default. "+
				"Set a string path (e.g. %s) or remove the override.",
			componentName, rawDir, rawDir, operatorContainerDriverRoot)}
	}
	if dir == "" {
		// Intentionally default-equivalent: the operator's own
		// transformForDriverInstallDir early-returns on "" exactly like
		// the default (gpu-operator v26.7.0, controllers/object_controls.go).
		return installDir, false, nil
	}
	cleaned := path.Clean(dir)
	if !path.IsAbs(cleaned) {
		// A relative install dir can never be a valid host path — the
		// operator renders it into host-path mounts, which require
		// absolute paths — and a relative spelling would also compare
		// unequal to every legitimate absolute driver root, so silently
		// accepting it either blocks with a misleading mismatch remedy
		// or (paired with a matching relative DRA root) passes the gate
		// while the install is broken.
		return installDir, false, []string{fmt.Sprintf(
			"%s: hostPaths.driverInstallDir=%q is not an absolute path — the operator "+
				"renders it into host-path mounts, which require absolute paths, so no "+
				"node can mount it. Set an absolute path such as %s.",
			componentName, dir, operatorContainerDriverRoot)}
	}
	return cleaned, true, nil
}

// resolveDRARoot resolves the effective nvidia-dra-driver-gpu
// nvidiaDriverRoot for Rule 2. Only a genuinely ABSENT key falls back to
// the chart-default assumption ("/", DRA chart v0.5.0 values.yaml). A
// present null, empty-string, or non-string value is rejected: unlike the
// gpu-operator's driverInstallDir (where "" is default-equivalent, see
// resolveInstallDir), the DRA chart pipes the raw value through
// trimSuffix/dir in its kubeletplugin template, so null fails Helm
// rendering outright and "" renders empty/relative host paths — neither
// matches the absent-key default, and Rule 2 would otherwise silently
// skip every branch (rootDeclared=false with driver.enabled=false fires
// nothing). A declared value that cleans to a relative path is rejected
// for the same reason: it renders unmountable relative host paths AND
// (being unequal to /run/nvidia/driver) slips past the legacy-signature
// branch when the driver is disabled.
func resolveDRARoot(draValues map[string]any, componentName string) (string, bool, []string) {
	rawRoot, rootPresent := draValues["nvidiaDriverRoot"]
	if !rootPresent {
		return "", false, nil
	}
	root, isStr := rawRoot.(string)
	if !isStr {
		return "", false, []string{fmt.Sprintf(
			"%s: %s nvidiaDriverRoot=%v (%T) is not a string — the DRA chart pipes this "+
				"value through path template functions, so a non-string fails Helm rendering. "+
				"Set a string path (the preinstalled-driver profile uses \"/\"; the "+
				"operator-managed profile uses the driver install dir) or remove the override "+
				"to use the chart default.",
			componentName, draDriverComponentName, rawRoot, rawRoot)}
	}
	if root == "" {
		return "", false, []string{fmt.Sprintf(
			"%s: %s nvidiaDriverRoot is declared as an empty string — the DRA chart renders "+
				"it verbatim into host paths (it is NOT treated as the absent-key default), "+
				"producing empty/relative mounts. Set a real path or remove the override.",
			componentName, draDriverComponentName)}
	}
	// path.Clean so /run/nvidia/driver/ and /run/nvidia/driver compare
	// equal in both the managed-mode equality and the legacy-signature
	// check.
	cleaned := path.Clean(root)
	if !path.IsAbs(cleaned) {
		// Same hazard as the empty string above, plus a lockstep bypass:
		// a relative spelling of the operator container root (e.g. the
		// missing-leading-slash typo run/nvidia/driver) compares unequal
		// to /run/nvidia/driver, so with driver.enabled=false no Rule 2
		// branch would fire and the broken relative mount would bundle.
		return "", false, []string{fmt.Sprintf(
			"%s: %s nvidiaDriverRoot=%q is not an absolute path — the DRA chart renders "+
				"it into kubelet-plugin host-path mounts, which require absolute paths. "+
				"Set an absolute path (the preinstalled-driver profile uses \"/\"; the "+
				"operator-managed profile uses the driver install dir).",
			componentName, draDriverComponentName, root)}
	}
	return cleaned, true, nil
}

// dynamicOwnershipViolations returns one blocking message per --dynamic
// declaration (resolved under the component's canonical name and registry
// aliases, mirroring dynamicPathSetFor's override-key matching) that
// targets a guarded ownership path, the parent of one, or a child of one.
// A dynamic ownership path defers the value to the operator-editable
// cluster-values.yaml, where no bundle-time gate runs — editing it there
// recreates the driver/DRA incoherence this validation blocks.
func dynamicOwnershipViolations(bundlerConfig *config.Config, componentName string, keys []string, guarded []string) []string {
	hits := dynamicPathIntersections(bundlerConfig, keys, guarded)
	msgs := make([]string, 0, len(hits))
	for _, hit := range hits {
		msgs = append(msgs, fmt.Sprintf(
			"%s: --dynamic %s:%s targets the driver-ownership path %s — dynamic "+
				"paths move to the operator-editable cluster-values.yaml at install "+
				"time, where no ownership-coherence gate runs, so editing it there "+
				"can recreate the driver/DRA incoherence this check blocks. Bake "+
				"ownership values statically (--set/--set-json) instead.",
			componentName, hit.key, hit.path, hit.guard))
	}
	return msgs
}

// dynamicPathIntersection records one --dynamic declaration (key:path)
// that intersects a guarded values path.
type dynamicPathIntersection struct {
	key   string
	path  string
	guard string
}

// dynamicPathIntersections returns every --dynamic declaration under any
// of the candidate component keys whose path intersects a guarded path
// (equal, ancestor, or descendant). Shared by the driver-ownership guard
// above and the NVSentinel gates below: dynamic paths are exported to
// the operator-editable cluster-values.yaml, which the deployer loads
// AFTER the statically validated values, so an install-time edit there
// silently undoes whatever a bundle-time gate verified.
func dynamicPathIntersections(bundlerConfig *config.Config, keys []string, guarded []string) []dynamicPathIntersection {
	if bundlerConfig == nil || !bundlerConfig.HasDynamicValues() {
		return nil
	}
	dynamic := bundlerConfig.DynamicValues()
	var hits []dynamicPathIntersection
	for _, key := range keys {
		for _, p := range dynamic[key] {
			for _, g := range guarded {
				if p == g || strings.HasPrefix(g, p+".") || strings.HasPrefix(p, g+".") {
					hits = append(hits, dynamicPathIntersection{key: key, path: p, guard: g})
					break
				}
			}
		}
	}
	return hits
}

// nvsentinelDynamicGuardViolations rejects --dynamic declarations that
// intersect the values paths the NVSentinel gates verify. Both gates
// validate the STATIC resolved values; a dynamic declaration on the same
// path moves it into cluster-values.yaml, loaded after them at install
// time, so an operator edit there undoes the validated remedy with no
// gate left to notice — including the degenerate immediate case, where
// a path the recipe never set is exported as an empty stub ready to be
// filled with a breaking value. gateReason describes, in the message's
// "which <gateReason>" slot, what the guarded path means to the gate and
// what an install-time edit would break.
func nvsentinelDynamicGuardViolations(bundlerConfig *config.Config, componentName string, keys []string, guarded []string, gateReason string) []string {
	hits := dynamicPathIntersections(bundlerConfig, keys, guarded)
	msgs := make([]string, 0, len(hits))
	for _, hit := range hits {
		msgs = append(msgs, fmt.Sprintf(
			"%s: --dynamic %s:%s targets %s, which %s. Dynamic paths move to the "+
				"operator-editable cluster-values.yaml at install time, loaded after "+
				"the values this gate verified, so editing it there silently undoes "+
				"the validated configuration. Bake the value statically "+
				"(--set/--set-json) instead.",
			componentName, hit.key, hit.path, hit.guard, gateReason))
	}
	return msgs
}

// ownershipToggle resolves the boolean toggle at values[section].enabled
// for the driver-ownership rules, distinguishing absent from
// present-but-invalid. Returns:
//
//   - (nil, ""): the toggle is genuinely absent — the section is missing,
//     or the section map carries no "enabled" key. The caller falls back
//     to the chart default.
//   - (&b, ""): the toggle is a real boolean.
//   - (nil, problem): the section or the toggle is present with an
//     unusable value (an explicitly null section, a non-map section, or a
//     non-boolean toggle). The caller must reject rather than default.
//     An explicitly null section is NOT equivalent to absent: Helm's
//     null-coalescing deletes the key together with its chart defaults,
//     so .Values.<section> is nil at render time and the gpu-operator
//     templates fail on unconditional field access (e.g.
//     .Values.driver.manager.repository in _helpers.tpl, v26.7.0) —
//     ownership cannot be verified and the install would fail anyway.
//     A non-boolean toggle is rejected because the chart renders the
//     value unquoted, so YAML re-typing at install time can flip it to a
//     boolean this check never saw.
func ownershipToggle(values map[string]any, section string) (*bool, string) {
	raw, sectionPresent := values[section]
	if !sectionPresent {
		return nil, ""
	}
	if raw == nil {
		return nil, fmt.Sprintf("%s is explicitly null — Helm deletes a null key together "+
			"with its chart defaults, so the chart's unconditional %s.* field accesses fail "+
			"at install and driver ownership cannot be verified", section, section)
	}
	m, isMap := raw.(map[string]any)
	if !isMap {
		return nil, fmt.Sprintf("%s=%v (%T) is not a map, so %s.enabled cannot be read",
			section, raw, raw, section)
	}
	v, keyPresent := m["enabled"]
	if !keyPresent {
		return nil, ""
	}
	b, isBool := v.(bool)
	if !isBool {
		if v == nil {
			return nil, fmt.Sprintf("%s.enabled is null, not a boolean", section)
		}
		return nil, fmt.Sprintf("%s.enabled=%v (%T) is not a boolean", section, v, v)
	}
	return &b, ""
}

// draLockstepViolations evaluates Rule 2 (DRA driver-root lockstep) on
// the effective nvidia-dra-driver-gpu values. installDirTrusted is false
// when resolveInstallDir rejected the declared driverInstallDir — the
// lockstep switch is suppressed then, because it would compare against
// the guessed chart default and emit a second, misleading remedy (the
// same suppression applies to invalid declared roots and dynamic,
// bundle-time-deferred root declarations).
func draLockstepViolations(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config,
	provider recipe.DataProvider, componentName string, service recipe.CriteriaServiceType,
	osCriteria recipe.CriteriaOSType, driverEnabled bool, installDir string, installDirTrusted bool,
) ([]string, []error) {

	draRef := resolveDRAComponentRef(recipeResult)
	if draRef == nil {
		return nil, nil
	}
	draKeys := componentOverrideKeys(draRef.Name, provider)
	if componentDisabled(draRef, bundlerConfig, draKeys) {
		return nil, nil
	}
	var msgs []string
	// Same install-time-deferral hazard as the gpu-operator ownership
	// paths in the caller: a dynamic DRA driver root escapes Rule 2 at
	// bundle time.
	draDynMsgs := dynamicOwnershipViolations(bundlerConfig, draRef.Name, draKeys,
		[]string{"nvidiaDriverRoot"})
	msgs = append(msgs, draDynMsgs...)
	draValues, draErr := effectiveComponentValues(ctx, recipeResult, bundlerConfig, draRef.Name, draKeys, "driver-ownership coherence")
	if draErr != nil {
		return msgs, []error{draErr}
	}
	root, rootDeclared, rootMsgs := resolveDRARoot(draValues, componentName)
	msgs = append(msgs, rootMsgs...)
	if len(rootMsgs) > 0 {
		// An invalid declared root already blocks the bundle; running the
		// lockstep switch against a guessed value would only add a
		// misleading second remedy.
		return msgs, nil
	}
	if !installDirTrusted || len(draDynMsgs) > 0 {
		// Same suppression, other inputs: a rejected driverInstallDir left
		// only a guessed default to compare against, and a dynamic
		// nvidiaDriverRoot defers the real value past bundle time — either
		// way the switch below would emit a second remedy computed from a
		// value the bundle will not actually use.
		return msgs, nil
	}

	switch {
	case driverEnabled && rootDeclared && root != installDir:
		msgs = append(msgs, fmt.Sprintf(
			"%s: the operator-managed driver installs to hostPaths.driverInstallDir=%s "+
				"but the DRA kubelet plugin reads nvidiaDriverRoot=%s; CDI spec generation "+
				"will fail and DRA-allocated pods will stall in ContainerCreating. Fix with "+
				"--set %s:nvidiaDriverRoot=%s (or, on a cluster with a "+
				"platform-preinstalled driver, the full preinstalled-driver profile).",
			componentName, installDir, root, draKeys[len(draKeys)-1], installDir))
	case driverEnabled && !rootDeclared:
		msgs = append(msgs, fmt.Sprintf(
			"%s: nvidiaDriverRoot is not declared for %s, so it falls to the DRA chart "+
				"default (/), which cannot match the operator-managed driver install dir. "+
				"Set --set %s:nvidiaDriverRoot=%s.",
			componentName, draRef.Name, draKeys[len(draKeys)-1], installDir))
	case !driverEnabled && root == operatorContainerDriverRoot:
		// The alternative clause is service- AND OS-aware (GKE profiles pin
		// hostPaths.driverInstallDir to the Google installer path, and COS
		// cannot take the operator-managed tuple at all) — see
		// legacyRecipeAlternativeRemedy.
		msgs = append(msgs, fmt.Sprintf(
			"%s: driver.enabled=false but %s still reads nvidiaDriverRoot=%s — nothing "+
				"populates that path when the operator does not manage the driver. This is "+
				"commonly the signature of a recipe generated before the preinstalled-driver "+
				"default flip: regenerate the recipe (aicr recipe ...) for this AICR version. %s",
			componentName, draDriverComponentName, operatorContainerDriverRoot,
			legacyRecipeAlternativeRemedy(service, osCriteria)))
	}
	return msgs, nil
}

// CheckDriverOwnershipCoherence fails a bundle whose FINAL effective
// values (recipe merge plus all --set/--set-json/--set-file overrides)
// render an incoherent GPU driver-ownership profile. Two rules:
//
// Rule 1 (driverless cluster, gated on recorded snapshot state): when the
// snapshot that produced the recipe observed no NVIDIA kernel driver on
// the sampled GPU node (metadata.gpuDriverState=absent — recorded by
// pkg/client/v1's snapshot-driven resolution, the `--gpu-driver none`
// signature), the effective config must have the operator install the
// full stack: driver.enabled=true and, when declared, toolkit.enabled
// not false. Deploying the preinstalled-driver assumption onto that
// cluster leaves GPU nodes driverless — nothing on the node provides a
// driver and the recipe does not install one. Recipes without a recorded
// state (criteria-only resolves, older recipes, snapshots without a
// usable driver-loaded reading) are not gated by this rule. A recorded
// state outside the two documented constants is rejected outright — the
// empty-string disarm is deliberate, an unrecognized nonempty spelling
// in a loaded or hand-edited recipe is not.
//
// Rule 2 (DRA driver-root lockstep, metadata-independent): when
// nvidia-dra-driver-gpu is bundled alongside gpu-operator, its
// nvidiaDriverRoot must track the driver owner — see
// pkg/recipe/driver_root_lockstep_test.go for the full invariant
// rationale (issue #1087). With driver.enabled=true the DRA kubelet
// plugin must read the operator install dir
// (hostPaths.driverInstallDir), or CDI spec generation fails and
// DRA-allocated pods stall in ContainerCreating; with
// driver.enabled=false the root must not be the operator container root
// /run/nvidia/driver, which nothing populates in that mode — the
// signature of a legacy pre-flip recipe whose valuesFile now resolves
// the preinstalled-driver defaults while its baked DRA override still
// points at the operator path. Because this rule evaluates effective
// values only, it catches those legacy recipes with no recorded
// gpuDriverState.
//
// Independent of both rules, an explicitly declared gpu-operator
// hostPaths.driverInstallDir that cleans to "/" is always rejected:
// it is the host path the operator-validator bind-mounts as the
// driver-validation container's rootfs target, and runc rejects a
// mount whose destination is "/" — the issue #1106 regression (see
// pkg/recipe/driver_root_lockstep_test.go, invariant 1). The DRA
// nvidiaDriverRoot of "/" is NOT flagged — it is the legitimate
// preinstalled-driver value.
//
// The check runs at bundle generation — not at snapshot-driven recipe
// resolution, which only warns — because this is the first point where
// the user's --set ownership overrides are known: `aicr recipe` has no
// --set, so a resolution-time hard failure would leave supported LEGACY
// GPU-Operator-managed clusters unable to reach the documented override.
// On ADR-015-profiled recipes (AKS gpuStack) the --set escape does not
// apply — ownership paths are profile-owned and per-path flips are
// rejected — so the profiled remedy is out-of-band (fix/recreate pools,
// recapture, regenerate with --profile); see driverAbsentRemedy.
// Registered with severity error on gpu-operator (recipes/registry.yaml),
// which converts the returned messages into a blocking
// ErrCodeInvalidRequest in RunValidations. The check returns hard
// errors only when a component's effective values cannot be resolved
// or the user's overrides cannot be reapplied to them (see
// effectiveComponentValues): either way the values this gate must
// verify cannot be reconstructed, so coherence fails closed.
func CheckDriverOwnershipCoherence(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil {
		return nil, nil
	}
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	gpuOpRef := recipeResult.GetComponentRef(componentName)
	if gpuOpRef == nil {
		return nil, nil
	}
	provider := recipeResult.DataProvider()
	gpuOpKeys := componentOverrideKeys(componentName, provider)
	if componentDisabled(gpuOpRef, bundlerConfig, gpuOpKeys) {
		return nil, nil
	}

	// --dynamic declarations targeting an ownership path defer that value
	// to install-time editing: the deployer moves the path (seeding it
	// even when absent) into the operator-editable cluster-values.yaml
	// (splitDynamicPaths, pkg/bundler/deployer/localformat), where no
	// bundle-time gate runs. Flipping ownership there recreates exactly
	// the driver/DRA incoherence this check exists to block, so such
	// declarations are rejected up front — before values resolution, so
	// the rejection fires even when resolution itself fails.
	if dynMsgs := dynamicOwnershipViolations(bundlerConfig, componentName, gpuOpKeys,
		[]string{"driver.enabled", "toolkit.enabled", "hostPaths.driverInstallDir"}); len(dynMsgs) > 0 {
		for _, msg := range dynMsgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return dynMsgs, nil
	}

	// Nothing validates metadata.gpuDriverState on the load/adopt
	// boundaries (PrepareAndValidate), so a loaded or hand-edited recipe
	// can carry any string. Rule 1 keys on exact equality with "absent",
	// so an unrecognized nonempty spelling ("Absent", "ABSENT", a typo)
	// would silently degrade to the deliberate empty=unknown disarm state
	// and let a driverless bundle through. Fail closed: reject nonempty
	// values outside the two documented constants. Checked before values
	// resolution so the rejection fires even when resolution fails.
	var msgs []string
	recordedState := recipeResult.Metadata.GPUDriverState
	if recordedState != "" && recordedState != recipe.GPUDriverStatePreinstalled && recordedState != recipe.GPUDriverStateAbsent {
		msgs = append(msgs, fmt.Sprintf(
			"%s: metadata.gpuDriverState=%q is not a recognized value — expected %q, "+
				"%q, or empty (unknown). The driverless-cluster rule keys on this field, "+
				"so an unrecognized spelling would silently disarm it. Fix the field or "+
				"regenerate the recipe (aicr recipe ...) with this AICR version.",
			componentName, recordedState, recipe.GPUDriverStatePreinstalled, recipe.GPUDriverStateAbsent))
	}

	values, resolveErr := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, gpuOpKeys, "driver-ownership coherence")
	if resolveErr != nil {
		for _, msg := range msgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return msgs, []error{resolveErr}
	}

	var service recipe.CriteriaServiceType
	var osCriteria recipe.CriteriaOSType
	if recipeResult.Criteria != nil {
		service = recipeResult.Criteria.Service
		osCriteria = recipeResult.Criteria.OS
	}

	var errs []error

	// driver.enabled defaults to true in the gpu-operator chart and
	// toolkit.enabled matters only when explicitly declared false — but
	// those defaults apply ONLY when the key is genuinely absent. A key
	// that is present with a non-boolean value is rejected instead of
	// defaulted: the pinned chart interpolates both toggles unquoted into
	// the ClusterPolicy (`enabled: {{ .Values.driver.enabled }}`,
	// deployments/gpu-operator/templates/clusterpolicy.yaml), so YAML
	// re-types the rendered scalar at install time — the string "false"
	// (e.g. --set-json gpuoperator:driver.enabled='"false"', or a --set
	// spelling like "False" that ConvertMapValue does not coerce) deploys
	// as boolean false while a defaulted check would still assume the
	// operator manages the driver, silently bypassing both rules.
	driverToggle, driverProblem := ownershipToggle(values, "driver")
	toolkitToggle, toolkitProblem := ownershipToggle(values, "toolkit")
	toggleRejected := false
	for _, problem := range []string{driverProblem, toolkitProblem} {
		if problem == "" {
			continue
		}
		toggleRejected = true
		msgs = append(msgs, fmt.Sprintf(
			"%s: %s — the chart interpolates this value unquoted into the ClusterPolicy, "+
				"so YAML re-types it at install time (a string \"false\" deploys as boolean "+
				"false) and driver ownership cannot be verified. Set a bare boolean instead, "+
				"e.g. --set gpuoperator:driver.enabled=false or --set-json "+
				"gpuoperator:driver.enabled=false (no quotes around the value).",
			componentName, problem))
	}
	if toggleRejected {
		// The ownership rules below would evaluate against guessed
		// defaults; with an unverifiable toggle the guess can invert the
		// remedy (e.g. telling the user to retarget the DRA root at the
		// operator install dir when their intent was to disable the
		// driver). The rejection above already blocks the bundle, so
		// return it alone.
		for _, msg := range msgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return msgs, errs
	}
	driverEnabled := true
	if driverToggle != nil {
		driverEnabled = *driverToggle
	}
	toolkitDisabled := toolkitToggle != nil && !*toolkitToggle

	// Rule 1: recorded driverless cluster vs preinstalled-driver profile.
	// An effectively enabled gcp-driver-installer disarms it: the bundle
	// itself provisions the driver, so the driverless snapshot is the
	// expected pre-deployment state (a correctly provisioned
	// bundle-installer pool must be able to generate its own bundle). The
	// supply check runs lazily inside the guard so its hard-fail surface
	// exists only when Rule 1 would actually fire; there, a resolution
	// failure for the installer's values fails closed as a hard error
	// rather than degrading to the misleading driverless remediation.
	if recipeResult.Metadata.GPUDriverState == recipe.GPUDriverStateAbsent && (!driverEnabled || toolkitDisabled) {
		bundleSuppliesDriver, supplyErr := BundleSuppliesGKEDriver(ctx, recipeResult, bundlerConfig)
		if supplyErr != nil {
			for _, msg := range msgs {
				slog.Warn(msg, logKeyComponent, componentName)
			}
			return msgs, []error{supplyErr}
		}
		if !bundleSuppliesDriver {
			msgs = append(msgs, fmt.Sprintf(
				"%s: the effective values assume a platform-preinstalled NVIDIA driver "+
					"and container toolkit (driver.enabled=false and/or toolkit.enabled=false), "+
					"but the snapshot that produced this recipe observed no NVIDIA kernel "+
					"driver on the sampled GPU node. Deploying this bundle would leave GPU "+
					"nodes driverless. %s",
				componentName, driverAbsentRemedy(service, osCriteria, recipeResult.Metadata.SelectedProfile != nil)))
		}
	}

	installDir, installDirDeclared, hostPathMsgs := resolveInstallDir(values, componentName)
	msgs = append(msgs, hostPathMsgs...)
	// A rejected hostPaths/driverInstallDir declaration already blocks the
	// bundle; installDir now holds the guessed chart default, so the
	// lockstep switch below must not compare against it (mirrors the
	// toggle-problem early return above and the rootMsgs break below).
	installDirTrusted := len(hostPathMsgs) == 0

	// driverInstallDir "/" is illegal regardless of DRA presence, driver
	// ownership, or root equality: it is the host path the
	// operator-validator bind-mounts as the driver-validation container's
	// rootfs target, and runc rejects a mount whose destination is "/"
	// ("mountpoint is on the top of rootfs") — the issue #1106 regression
	// guarded by pkg/recipe/driver_root_lockstep_test.go invariant 1. The
	// DRA nvidiaDriverRoot of "/" is NOT flagged: that is the legitimate
	// preinstalled-driver value.
	if installDirDeclared && installDir == "/" {
		msgs = append(msgs, fmt.Sprintf(
			"%s: hostPaths.driverInstallDir=/ is invalid — it is a mount destination "+
				"the operator-validator bind-mounts as a container rootfs target, and runc "+
				"rejects a mount whose destination is / (\"mountpoint is on the top of "+
				"rootfs\", the issue #1106 regression). Point it at a dedicated host "+
				"directory such as %s.",
			componentName, operatorContainerDriverRoot))
	}

	// Rule 2: DRA driver-root lockstep on effective values.
	draMsgs, draErrs := draLockstepViolations(ctx, recipeResult, bundlerConfig, provider,
		componentName, service, osCriteria, driverEnabled, installDir, installDirTrusted)
	msgs = append(msgs, draMsgs...)
	// errs is still empty here — every earlier error path returns
	// directly — so adopt the helper's slice instead of appending.
	errs = draErrs

	for _, msg := range msgs {
		slog.Warn(msg, logKeyComponent, componentName)
	}
	return msgs, errs
}

// gpuOperatorComponentNames are the registry-level component names that
// carry GPU Operator driver-ownership values (the canonical chart and its
// OCP variant). This package cannot import pkg/bundler (dependency cycle
// — see draDriverComponentNames above for the identical constraint and
// the agreed keep-in-sync disposition), so this list is a local
// duplicate of pkg/bundler's gpuOperatorComponentNames; it also mirrors
// the pair that registers CheckDriverOwnershipCoherence in
// recipes/registry.yaml. Keep all three in sync.
var gpuOperatorComponentNames = []string{"gpu-operator", "gpu-operator-ocp"}

// nvsentinelAssumeDriverInstalledOverrideSet is the documented
// bundle-time override that makes the NVSentinel labeler apply
// nvsentinel.dgxc.nvidia.com/driver.installed without watching for a
// driver pod. It renders the labeler's --assume-driver-installed flag,
// the chart-level automation of the Manual Labeling Procedure in
// NVSentinel design 018. Phrased as a remedy alongside
// gpuOperatorManagedOverrideSet above.
const nvsentinelAssumeDriverInstalledOverrideSet = "--set nv-sentinel:labeler.assumeDriverInstalled=true"

// nvsentinelDriverLabelPath is the nvsentinel values path that carries
// the flag. "labeler" is the subchart key (the chart declares no alias),
// so the override path is subchart-scoped rather than top-level.
const nvsentinelDriverLabelPath = "labeler.assumeDriverInstalled"

// gkeBundleInstallerProfileValue is the GKE gpuStack value under which the
// bundle's gcp-driver-installer component (issue #1716) carries the
// cos-gpu-installer DaemonSet. Its pods ARE a driver pod the NVSentinel
// labeler detects, so the label gate below must not reject this value.
const gkeBundleInstallerProfileValue = "bundle-installer"

// gpuStackProfileName is the ADR-015 configuration-profile name that
// selects who installs the GPU driver on AKS and GKE.
const gpuStackProfileName = "gpuStack"

// gcpDriverInstallerComponentName is the values-gated GKE COS driver
// component (issue #1716): present unconditionally in the GKE COS
// composition, it renders the cos-gpu-installer DaemonSet only when its
// nested installer.enabled gate is on.
const gcpDriverInstallerComponentName = "gcp-driver-installer"

// BundleSuppliesGKEDriver reports whether the composed bundle carries an
// effectively enabled gcp-driver-installer — i.e. the bundle itself
// provisions the NVIDIA kernel driver, so metadata.gpuDriverState=absent
// is the expected pre-deployment state of a correctly provisioned pool
// (gpu-driver-version=disabled) rather than a misconfiguration. It keys
// off the EFFECTIVE installer gate (recipe values plus any --set
// overrides in bundlerConfig), not the selected profile name: a --set
// that flips the gate must flip this answer with it. The gate mirrors
// the manifest template exactly (toString(installer.enabled) == "true"),
// so only a value that actually renders the DaemonSet counts as a
// producer; anything else — absent, false, or an unrecognized type —
// leaves the driverless Rule 1 gate armed (fail closed). The lookup runs
// against the declared-union view so a subset bundle
// (--bundlers gpu-operator) still observes the installer its sibling
// bundle carries. bundlerConfig may be nil (the resolution-time caller
// in pkg/client/v1 has no override channel).
func BundleSuppliesGKEDriver(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config) (bool, error) {
	if recipeResult == nil {
		return false, nil
	}
	unionView := declaredUnionView(recipeResult)
	ref := unionView.GetComponentRef(gcpDriverInstallerComponentName)
	if ref == nil {
		return false, nil
	}
	keys := componentOverrideKeys(gcpDriverInstallerComponentName, unionView.DataProvider())
	if componentDisabled(ref, bundlerConfig, keys) {
		return false, nil
	}
	values, err := effectiveComponentValues(ctx, unionView, bundlerConfig,
		gcpDriverInstallerComponentName, keys, "bundle-supplied driver detection")
	if err != nil {
		return false, err
	}
	installer, ok := values["installer"].(map[string]any)
	if !ok {
		return false, nil
	}
	return fmt.Sprint(installer["enabled"]) == "true", nil
}

// resolveGPUOperatorRef looks up the GPU Operator's ComponentRef by
// trying every known name variant in turn, mirroring
// resolveDRAComponentRef: GetComponentRef matches an EXACT name, so a
// bare canonical-name call silently returns nil on an OCP recipe.
//
// The lookup runs against a DECLARED-union view of the recipe
// (declaredUnionView): the bundler filters ComponentRefs before
// validations run, so on a subset bundle (bundlers=nvsentinel) the
// gpu-operator ref is absent from the filtered refs even though its
// declaration — driver.enabled, operator.runtimeClass — is exactly the
// platform evidence the NVSentinel gates key on. The returned view must
// also be what the caller resolves the GPU Operator's values against,
// so GetValuesForComponentWithContext finds the ref.
func resolveGPUOperatorRef(recipeResult *recipe.RecipeResult) (string, *recipe.ComponentRef, *recipe.RecipeResult) {
	union := declaredUnionView(recipeResult)
	for _, name := range gpuOperatorComponentNames {
		if ref := union.GetComponentRef(name); ref != nil {
			return name, ref, union
		}
	}
	return "", nil, union
}

// declaredUnionView returns recipeResult when no pre-filter union is
// attached, and otherwise a shallow copy whose ComponentRefs is the
// declared union — so ref lookups and values resolution see every
// component the recipe names, not just the ones this bundle renders.
// Attachment is the explicit HasDeclaredComponents bit, not a slice-
// length inference.
func declaredUnionView(recipeResult *recipe.RecipeResult) *recipe.RecipeResult {
	if !recipeResult.HasDeclaredComponents() {
		return recipeResult
	}
	view := *recipeResult
	view.ComponentRefs = recipeResult.DeclaredComponentRefs()
	return &view
}

// helmTruthy reports whether value would satisfy a Helm `{{ if }}` guard,
// which is how the labeler chart consumes assumeDriverInstalled
// (`{{- if .Values.assumeDriverInstalled }}` → `--assume-driver-installed`).
// Helm inherits Go's text/template truth rule, so delegate to
// template.IsTrue rather than re-deriving it: empty maps, slices, arrays,
// and strings are FALSE (an empty-map value — e.g. --set-json
// nv-sentinel:labeler.assumeDriverInstalled={} — renders WITHOUT the
// flag, recreating the exact silent 0-desired state the gate exists to
// block), non-empty strings are TRUE (so a --set spelling the chart
// would honor is never rejected), numbers by non-zero, booleans by
// value. The !ok case (no meaningful truth value) is fail-closed to
// not-the-remedy.
func helmTruthy(value any) bool {
	truth, ok := template.IsTrue(value)
	return ok && truth
}

// nvsentinelAssumesDriverInstalled reports whether the effective
// nvsentinel values enable the labeler's assume-driver-installed mode.
func nvsentinelAssumesDriverInstalled(values map[string]any) bool {
	labeler, ok := values["labeler"].(map[string]any)
	if !ok {
		return false
	}
	raw, present := labeler["assumeDriverInstalled"]
	if !present {
		return false
	}
	return helmTruthy(raw)
}

// labelerObservesDriverPod reports whether some driver pod the NVSentinel
// labeler recognizes exists on this recipe's platform even though the GPU
// Operator installs no driver.
//
// One shipping configuration qualifies: GKE COS with
// --profile gpuStack=bundle-installer, whose bundle-carried installer is
// Google's standalone nvidia-driver-installer DaemonSet on pools created
// with gpu-driver-version=disabled (recipes/overlays/gke-cos.yaml), the
// bundle's gcp-driver-installer component carries the cos-gpu-installer
// DaemonSet. The labeler's driver-pod detection sees those pods, so the
// driver.installed label IS applied and the gate below must stay silent.
// The sibling value gke-default bakes the driver into the node at pool
// creation, so no driver pod exists there — that value is affected.
//
// The exemption is scoped to GKE COS recipes, not to the profile
// identifier alone: profile names are not reserved, and an external
// --data overlay on any service can declare a gpuStack profile whose
// value happens to be named bundle-installer — with no installer
// DaemonSet ever deploying. Fail closed on anything but the one shape
// the embedded catalog documents (recipes/overlays/gke-cos.yaml); a
// recipe without criteria stays blocked for the same reason.
func labelerObservesDriverPod(recipeResult *recipe.RecipeResult) bool {
	if recipeResult.Criteria == nil ||
		recipeResult.Criteria.Service != recipe.CriteriaServiceGKE ||
		recipeResult.Criteria.OS != recipe.CriteriaOSCOS {

		return false
	}
	selected := recipeResult.Metadata.SelectedProfile
	if selected == nil {
		return false
	}
	return selected.Name == gpuStackProfileName && selected.Value == gkeBundleInstallerProfileValue
}

// NVSENTINEL GATE POLICY — what an nvsentinel gate means when parts of
// nvsentinel are disabled or filtered away (three review rounds probed
// this from different angles; keep the rules in one place):
//
//  1. Evidence is the DECLARED union; enforcement follows the OUTPUT.
//     The gates read other components (gpu-operator) from the
//     pre-filter declaration via resolveGPUOperatorRef — a component
//     excluded from a subset bundle still describes the platform — but
//     a gate only runs at all when nvsentinel itself is rendered
//     (RunComponentValidations iterates the filtered refs). This is
//     the ADR-018 union rule.
//  2. An explicitly-disabled consumer subchart renders nothing, so it
//     neither triggers a gate nor may deployment validation require
//     it: the RuntimeClass gate skips when metadata-collector is
//     disabled, the driver-label gate skips when BOTH label consumers
//     (metadata-collector, syslog monitors) are disabled, and the
//     health check's DaemonSet assertions use the negative form that
//     tolerates a true 404. A PARTIALLY disabled consumer set does not
//     skip — the remaining consumer still needs the remedy.
//  3. When rendered, a consumer must be fully rolled out — 0 desired
//     (the #2175 signature) and partial rollout both fail the health
//     check; the gates exist so that state is rejected at bundle time
//     instead.
//
// CheckNVSentinelDriverLabelDetectable blocks a bundle whose NVSentinel
// deployment would silently come up half-rolled-out.
//
// The NVSentinel labeler decides
// nvsentinel.dgxc.nvidia.com/driver.installed by watching for a GPU
// driver pod. Where the driver ships in the node image and no driver pod
// exists — AKS gpuStack=azure-managed, GKE COS gpuStack=gke-default, OKE
// — the label is never applied, so metadata-collector and both
// syslog-health-monitor DaemonSets report 0 desired pods. Nothing
// reports an error: a DaemonSet whose node selector matches no node is
// not unhealthy, it emits no event, and gpu-health-monitor keeps running
// because it selects on the DCGM label instead. The stack looks healthy
// while half of it was never scheduled (issue #2175).
//
// The chart automates the remedy: labeler.assumeDriverInstalled renders
// --assume-driver-installed, which is the Manual Labeling Procedure of
// NVSentinel design 018 expressed as configuration. Manually labeling
// nodes is NOT an equivalent workaround — the labeler computes an empty
// desired value when no driver pod exists and removes the label on its
// next reconcile.
//
// The gate fires only when every one of the following holds, so the flag
// is never demanded where the GPU Operator owns the driver (setting it
// there would skip detection and mask an unloaded or unhealthy driver):
//
//   - nvsentinel is present and enabled,
//   - the GPU Operator is present, enabled, and has driver.enabled=false
//     in its FINAL effective values (recipe merge plus the user's
//     --set/--set-json overrides), so the documented
//     GPU-Operator-managed override set clears the gate,
//   - no other driver pod source the labeler recognizes exists
//     (labelerObservesDriverPod — GKE gpuStack=bundle-installer), and
//   - labeler.assumeDriverInstalled is not truthy in nvsentinel's
//     effective values.
//
// It stays silent when the GPU Operator is absent or disabled: no AICR
// recipe ships nvsentinel without it, and with no driver-ownership
// signal to key on the gate would be guessing rather than detecting.
//
// When the driver toggle cannot be read as a boolean the gate defers to
// CheckDriverOwnershipCoherence, which rejects that on the same values,
// rather than adding a second message derived from a guessed default.
// That deferral holds only while the sibling actually runs: it is
// registered on the GPU Operator (recipes/registry.yaml) and
// RunComponentValidations iterates the FILTERED ComponentRefs, so a
// subset bundle (bundlers=nvsentinel) renders nvsentinel without it and
// nothing would report the malformed toggle. The declared union
// supplies this gate's evidence but cannot make the sibling execute, so
// the gate fails closed when the operator it read the toggle from is
// not itself rendered.
//
// Registered with severity error on nvsentinel (recipes/registry.yaml),
// which converts the returned message into a blocking
// ErrCodeInvalidRequest in RunValidations. Hard errors are returned only
// when a component's effective values cannot be resolved or the user's
// overrides cannot be reapplied to them (see effectiveComponentValues):
// the state this gate must verify is then unknown, so it fails closed.
func CheckNVSentinelDriverLabelDetectable(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	// A nil bundler config is the values-only Client.BundleComponents SDK
	// path, which exposes no way to pass --set. That used to no-op this
	// gate: the remedy was a bundle-time flag only, so firing here would
	// have made every affected recipe permanently unbundleable through
	// that API with no expressible fix. Since #2181 the recipes
	// themselves carry labeler.assumeDriverInstalled for every supported
	// configuration that needs it, so the gate is satisfiable from
	// resolved values alone and runs on this path too. The helpers below
	// read recipe values and skip the override merge when the config is
	// nil.
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	provider := recipeResult.DataProvider()
	sentinelKeys := componentOverrideKeys(componentName, provider)
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}

	gpuOpName, gpuOpRef, gpuOpView := resolveGPUOperatorRef(recipeResult)
	if gpuOpRef == nil {
		return nil, nil
	}
	gpuOpKeys := componentOverrideKeys(gpuOpName, provider)
	if componentDisabled(gpuOpRef, bundlerConfig, gpuOpKeys) {
		return nil, nil
	}

	gpuOpValues, err := effectiveComponentValues(ctx, gpuOpView, bundlerConfig, gpuOpName, gpuOpKeys, "NVSentinel driver-label detectability")
	if err != nil {
		return nil, []error{err}
	}
	driverToggle, driverProblem := ownershipToggle(gpuOpValues, "driver")
	// unreadableOwnership records that the operator's driver toggle could
	// not be read AND no sibling will report it. The gate deliberately
	// does not block here: every exit below (an observable driver pod,
	// both label consumers disabled, the remedy already set) is an
	// independent reason #2175 cannot occur, and short-circuiting past
	// them would reject bundles that are fine — a bundle carrying the
	// remedy most of all, since the flag makes the label unconditional
	// whatever the operator's toggle says. Ownership is instead read
	// conservatively as host-installed, so the gate blocks exactly where
	// a readable driver.enabled=false would have.
	unreadableOwnership := false
	if driverProblem != "" {
		if recipeResult.GetComponentRef(gpuOpName) != nil {
			// The operator renders in this bundle, so
			// CheckDriverOwnershipCoherence reports the malformed toggle
			// against the component that declares it; a second message
			// derived from a guessed default would only mislead.
			return nil, nil
		}
		// Subset bundle (bundlers=nvsentinel): the operator supplied the
		// evidence through the declared union but its own checks never
		// run, so this gate is the only place the malformed toggle can
		// surface. A falsy non-boolean (--set-json
		// gpuoperator:driver.enabled=0, an explicitly null section)
		// renders no driver pod — exactly the state that leaves the
		// labeler at 0 desired.
		unreadableOwnership = true
	}
	if !unreadableOwnership && (driverToggle == nil || *driverToggle) {
		// Absent key → the chart default (true): the operator installs
		// the driver, so its driver pod is exactly what the labeler
		// watches for. No dynamic guard fires on this exit: #2175 is
		// unreachable here regardless of any install-time edit to the
		// remedy or consumer paths (the driver pod exists either way),
		// and the flag is likewise permitted statically on these
		// platforms — a guard would reject valid unaffected bundles
		// (e.g. EKS with a --dynamic on a consumer-enable path).
		return nil, nil
	}

	if labelerObservesDriverPod(recipeResult) {
		// Same reasoning as the driver-enabled exit: Google's installer
		// DaemonSet supplies an observable driver pod, so no install-time
		// edit to the guarded paths can create the 0-desired state.
		return nil, nil
	}

	// Affected platform (host-installed driver, no observable pod) — the
	// exits below are decisions an install-time edit could undo, so the
	// dynamic guards are scoped from here on, each firing only when the
	// gate's outcome actually depends on the guarded path. Truth table
	// (pinned by the test rows):
	//
	//   (a)  both consumers statically disabled, no consumer-enable
	//        dynamic → skip; a remedy-path dynamic is allowed (nothing
	//        reads the label, so no install-time edit to the remedy can
	//        recreate #2175);
	//   (b)  both statically disabled, NO usable static remedy, and a
	//        consumer-enable path is dynamic → blocked (the skip's basis
	//        is install-time editable and a re-enabled consumer would
	//        find no remedy);
	//   (b1) both statically disabled, static remedy applied and the
	//        remedy path itself static → a consumer-enable dynamic is
	//        allowed (an install-time re-enable finds the remedy in
	//        place, so #2175 cannot recur);
	//   (b2) both statically disabled, static remedy applied but the
	//        remedy path ALSO dynamic → blocked (both bases are
	//        install-time editable: strip the remedy, re-enable a
	//        consumer);
	//   (c)  consumers active → the gate depends on the remedy, so a
	//        remedy-path dynamic is blocked whether the static remedy is
	//        set (an edit strips it) or not (the export defers it).
	sentinelValues, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys, "NVSentinel driver-label detectability")
	if err != nil {
		return nil, []error{err}
	}
	remedyStatic := nvsentinelAssumesDriverInstalled(sentinelValues)
	remedyDynamic := len(dynamicPathIntersections(bundlerConfig, sentinelKeys,
		[]string{nvsentinelDriverLabelPath})) > 0
	// Consumer-aware skip: metadata-collector and the syslog monitors are
	// the components that select on the driver.installed label. When the
	// resolved values explicitly disable BOTH subcharts, nothing reads
	// the label and demanding the remedy would serve no consumer. A
	// single disabled consumer does not skip — the other still needs the
	// label. (See the policy comment above CheckNVSentinelDriverLabelDetectable.)
	// Evaluated BEFORE the remedy-path guard: with both consumers gone
	// and their enable paths static, the remedy is irrelevant and a
	// dynamic on it is harmless (row (a)).
	if nvsentinelDriverLabelConsumerDisabled(sentinelValues, "metadataCollector") &&
		nvsentinelDriverLabelConsumerDisabled(sentinelValues, "syslogHealthMonitor") {
		// Row (b1): a static remedy on a static remedy path makes an
		// install-time consumer re-enable safe — the label is applied
		// regardless — so the consumer-enable guard must not fire.
		if remedyStatic && !remedyDynamic {
			return nil, nil
		}
		// Rows (b)/(b2): the consumer-enable paths are guarded exactly
		// when this skip is what clears the gate AND no static remedy
		// would cover a re-enabled consumer — either none is set, or the
		// remedy path is itself dynamic and an install-time edit can
		// strip it.
		if dynMsgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys,
			[]string{"global.metadataCollector.enabled", "global.syslogHealthMonitor.enabled"},
			"cleared the driver-label gate (both label consumers are disabled, so "+
				"nothing reads the label — an install-time edit re-enabling a consumer "+
				"would recreate the silent 0-desired DaemonSet state of issue #2175, "+
				"because the static remedy is absent or itself declared dynamic)"); len(dynMsgs) > 0 {
			for _, msg := range dynMsgs {
				slog.Warn(msg, logKeyComponent, componentName)
			}
			return dynMsgs, nil
		}

		return nil, nil
	}

	// Row (c): consumers render, so the gate stands or falls on the
	// remedy — whether it passes via the static remedy or blocks
	// demanding it, a dynamic on the path defers the remedy to the
	// operator-editable cluster-values.yaml, where stripping it
	// recreates #2175.
	if dynMsgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys,
		[]string{nvsentinelDriverLabelPath},
		"the driver-label gate verifies on this platform (an install-time edit "+
			"can strip the remedy and recreate the silent 0-desired DaemonSet "+
			"state of issue #2175)"); len(dynMsgs) > 0 {
		for _, msg := range dynMsgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return dynMsgs, nil
	}

	if remedyStatic {
		return nil, nil
	}

	// Name only the label consumers this configuration actually renders:
	// with one subchart explicitly disabled its DaemonSets do not exist,
	// and listing them would misstate the blast radius.
	var affected []string
	if !nvsentinelDriverLabelConsumerDisabled(sentinelValues, "metadataCollector") {
		affected = append(affected, "metadata-collector")
	}
	if !nvsentinelDriverLabelConsumerDisabled(sentinelValues, "syslogHealthMonitor") {
		affected = append(affected, "syslog-health-monitor-regular", "syslog-health-monitor-kata")
	}

	// The ownership clause differs when the toggle could not be read: the
	// gate is acting on the conservative reading, and saying
	// driver.enabled=false outright would misreport what the values say.
	ownershipClause := fmt.Sprintf(
		"the effective values leave the NVIDIA driver to the node image "+
			"(%s driver.enabled=false)", gpuOpName)
	if unreadableOwnership {
		ownershipClause = fmt.Sprintf(
			"the GPU Operator's driver ownership is unreadable (%s) and this bundle "+
				"does not render the GPU Operator (a bundlers= subset), so the check "+
				"that normally reports a malformed toggle does not run on it — read "+
				"conservatively, the driver is left to the node image", driverProblem)
	}
	msg := fmt.Sprintf(
		"%s: %s and no driver pod the NVSentinel labeler can "+
			"observe is deployed, but %s is not set. The labeler therefore never "+
			"applies the nvsentinel.dgxc.nvidia.com/driver.installed node label, and "+
			"%s come up with 0 desired pods — silently: a "+
			"DaemonSet that matches no node reports no error and emits no event, and "+
			"gpu-health-monitor stays healthy because it selects on the DCGM label "+
			"instead, so the stack looks fully rolled out. Set the documented upstream "+
			"flag at bundle time: %s. Note that labeling the nodes by hand does not "+
			"persist — the labeler removes the label on its next reconcile. Do not set "+
			"the flag where the GPU Operator installs the driver: it skips driver-pod "+
			"detection entirely and would mask an unloaded driver.",
		componentName, ownershipClause, nvsentinelDriverLabelPath,
		strings.Join(affected, ", "), nvsentinelAssumeDriverInstalledOverrideSet)
	slog.Warn(msg, logKeyComponent, componentName)
	return []string{msg}, nil
}

// defaultRuntimeClassName is the shared chart default: the gpu-operator
// chart ships operator.runtimeClass: nvidia (v26.7.0, verified against
// the pinned chart values), and nvsentinel's metadata-collector subchart
// ships runtimeClassName: "nvidia" (v1.9.0, charts/metadata-collector/
// values.yaml:31). Either side left unset therefore resolves to this
// name.
const defaultRuntimeClassName = "nvidia"

// nvsentinelMetadataCollectorRuntimeClassPath is the nvsentinel values
// path carrying the metadata-collector pod's runtimeClassName.
// "metadata-collector" is the subchart key (no alias in the parent
// Chart.yaml), so the override path is subchart-scoped.
const nvsentinelMetadataCollectorRuntimeClassPath = "metadata-collector.runtimeClassName"

// resolvedStringValue walks a dot-separated path through values and
// returns the string found there. ok is false when the path is absent
// or any intermediate step is not a map — the caller then applies the
// chart default. A present-but-non-string leaf returns ok=false with
// valid=false so callers can distinguish "absent → default" from
// "present but unreadable → cannot verify".
func resolvedStringValue(values map[string]any, path string) (value string, ok bool, valid bool) {
	cur := values
	parts := strings.Split(path, ".")
	for i, part := range parts {
		raw, present := cur[part]
		if !present {
			return "", false, true
		}
		if i == len(parts)-1 {
			s, isString := raw.(string)
			if !isString {
				return "", false, false
			}
			return s, true, true
		}
		next, isMap := raw.(map[string]any)
		if !isMap {
			return "", false, true
		}
		cur = next
	}
	return "", false, true
}

// nvsentinelDriverLabelConsumerDisabled reports whether the resolved
// nvsentinel values switch OFF the named subchart via its chart
// condition (global.<key>.enabled=false). Absent or malformed keys
// count as enabled — the chart defaults both consumers to true, and the
// gate must fail closed when it cannot prove nobody reads the label.
func nvsentinelDriverLabelConsumerDisabled(values map[string]any, key string) bool {
	global, ok := values["global"].(map[string]any)
	if !ok {
		return false
	}
	section, ok := global[key].(map[string]any)
	if !ok {
		return false
	}
	enabled, isBool := section["enabled"].(bool)
	return isBool && !enabled
}

// nvsentinelMetadataCollectorDisabled reports whether the resolved
// nvsentinel values switch the metadata-collector subchart off
// (global.metadataCollector.enabled=false, the chart's subchart
// condition). With the subchart disabled no DaemonSet renders, so a
// runtime-class mismatch has nothing to reject.
func nvsentinelMetadataCollectorDisabled(values map[string]any) bool {
	return nvsentinelDriverLabelConsumerDisabled(values, "metadataCollector")
}

// CheckNVSentinelRuntimeClassCoherence blocks a bundle whose NVSentinel
// metadata-collector pods would be rejected at admission.
//
// The metadata-collector DaemonSet sets runtimeClassName (chart default
// "nvidia"), and the GPU Operator's ClusterPolicy controller creates the
// primary RuntimeClass named after operator.runtimeClass (also default
// "nvidia"). When a recipe retargets operator.runtimeClass — the AKS
// azure-managed profile sets nvidia-container-runtime because that is
// the handler preconfigured on the AKS node image — no RuntimeClass
// named "nvidia" exists on the cluster, and the API server rejects every
// metadata-collector pod at admission: `pod rejected: RuntimeClass
// "nvidia" not found` (issue #2176).
//
// The failure mode is easy to misread: the pods are rejected before a
// pod object is created, so there is nothing to kubectl describe — the
// DaemonSet shows N desired / 0 created and the only signal is a
// FailedCreate event on it. Distinct from and additive to the
// driver-label gap (CheckNVSentinelDriverLabelDetectable, #2175): the
// label gets metadata-collector scheduled, the runtime class gets its
// pods admitted.
//
// This is a value comparison, not a platform matrix: the gate fires
// exactly when the two resolved names differ, treating either side
// unset as the shared chart default "nvidia" (both defaults verified
// against the pinned charts — see defaultRuntimeClassName). It
// therefore passes wherever the recipes leave operator.runtimeClass at
// its default (EKS, GKE, OKE, AKS operator-managed) and fails AKS
// azure-managed until the override is passed. An explicitly EMPTY
// metadata-collector.runtimeClassName also passes: the subchart omits
// the field entirely then, and a pod without runtimeClassName is always
// admitted.
//
// The gate stays silent when the GPU Operator is absent or disabled
// (nothing manages RuntimeClasses, so there is no authoritative name to
// compare against), when the metadata-collector subchart is disabled
// (global.metadataCollector.enabled=false — no DaemonSet renders), when
// either value is present but not a string (the install fails on its
// own terms; guessing a default here could invert the verdict).
//
// It also runs on a nil bundler config — the values-only
// Client.BundleComponents path. That path used to be exempt because the
// remedy was a --set it cannot express; since #2181 the AKS profile
// owns both operator.runtimeClass and
// metadata-collector.runtimeClassName, so the coherent state is
// reachable from resolved values alone.
//
// Registered with severity error on nvsentinel (recipes/registry.yaml).
// Hard errors are returned only when effective values cannot be
// resolved (effectiveComponentValues) — the state this gate must verify
// is then unknown, so it fails closed.
func CheckNVSentinelRuntimeClassCoherence(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	provider := recipeResult.DataProvider()
	sentinelKeys := componentOverrideKeys(componentName, provider)
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}

	gpuOpName, gpuOpRef, gpuOpView := resolveGPUOperatorRef(recipeResult)
	if gpuOpRef == nil {
		return nil, nil
	}
	gpuOpKeys := componentOverrideKeys(gpuOpName, provider)
	if componentDisabled(gpuOpRef, bundlerConfig, gpuOpKeys) {
		return nil, nil
	}

	gpuOpValues, err := effectiveComponentValues(ctx, gpuOpView, bundlerConfig, gpuOpName, gpuOpKeys, "NVSentinel RuntimeClass coherence")
	if err != nil {
		return nil, []error{err}
	}
	operatorClass, present, valid := resolvedStringValue(gpuOpValues, "operator.runtimeClass")
	if !valid {
		return nil, nil
	}
	if !present || operatorClass == "" {
		// Absent and explicitly empty both fall back to the chart
		// default: the gpu-operator chart templates the RuntimeClass
		// name via a default-applying helper.
		operatorClass = defaultRuntimeClassName
	}

	sentinelValues, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys, "NVSentinel RuntimeClass coherence")
	if err != nil {
		return nil, []error{err}
	}
	if nvsentinelMetadataCollectorDisabled(sentinelValues) {
		// The enable condition is guarded exactly when this skip is what
		// clears the gate AND an install-time re-enable would deploy a
		// collector whose runtime class is NOT verifiably coherent. When
		// the statically resolved collector class is already coherent
		// with the operator's (matching, or explicitly empty and
		// therefore omitted) and neither class path is itself dynamic, a
		// re-enabled collector is admitted — the alignment was verified
		// even though nothing renders now — so the guard must not fire.
		// It fires when the class is unset-default-
		// mismatched, misaligned, unreadable, or either class path is
		// install-time editable.
		disabledClass, disabledPresent, disabledValid := resolvedStringValue(sentinelValues, nvsentinelMetadataCollectorRuntimeClassPath)
		if !disabledPresent {
			disabledClass = defaultRuntimeClassName
		}
		coherent := disabledValid && (disabledClass == "" || disabledClass == operatorClass)
		// Which class paths make the coherent state install-time editable
		// mirrors the enabled-collector logic below: an explicitly EMPTY
		// collector class omits runtimeClassName from the pod spec, so a
		// re-enabled collector is admitted under ANY operator class and
		// only the collector's own path (an edit could fill it with a
		// nonexistent class) endangers the verified state —
		// operator.runtimeClass does not. A MATCHING non-empty class
		// depends on both sides staying put, so both paths count.
		classPathsDynamic := len(dynamicPathIntersections(bundlerConfig, sentinelKeys,
			[]string{nvsentinelMetadataCollectorRuntimeClassPath})) > 0
		if !disabledValid || disabledClass != "" {
			classPathsDynamic = classPathsDynamic ||
				len(dynamicPathIntersections(bundlerConfig, gpuOpKeys,
					[]string{"operator.runtimeClass"})) > 0
		}
		if coherent && !classPathsDynamic {
			return nil, nil
		}
		if dynMsgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys,
			[]string{"global.metadataCollector.enabled"},
			"cleared the RuntimeClass-coherence gate (the metadata-collector "+
				"subchart is disabled and its runtime class is not verifiably "+
				"coherent — misaligned, unreadable, or itself declared dynamic — so "+
				"an install-time edit re-enabling it can deploy a collector whose "+
				"pods are rejected at admission, issue #2176)"); len(dynMsgs) > 0 {
			for _, msg := range dynMsgs {
				slog.Warn(msg, logKeyComponent, componentName)
			}
			return dynMsgs, nil
		}

		return nil, nil
	}

	collectorClass, present, valid := resolvedStringValue(sentinelValues, nvsentinelMetadataCollectorRuntimeClassPath)
	if !valid {
		return nil, nil
	}
	if !present {
		collectorClass = defaultRuntimeClassName
	}
	guardReason := "the RuntimeClass-coherence gate verifies (an install-time edit can " +
		"desynchronize the collector from the operator's RuntimeClass and every " +
		"metadata-collector pod is then rejected at admission — issue #2176)"
	if collectorClass == "" {
		// Explicitly empty: the subchart omits runtimeClassName from the
		// pod spec entirely, and a pod without one is always admitted —
		// so retargeting the OPERATOR's class at install time cannot
		// cause #2176 and operator.runtimeClass is not guarded here. The
		// COLLECTOR path stays guarded: an install-time edit could
		// replace the empty value with a class name no RuntimeClass
		// matches, recreating exactly the admission rejection this exit
		// stands behind not happening.
		if dynMsgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys,
			[]string{nvsentinelMetadataCollectorRuntimeClassPath}, guardReason); len(dynMsgs) > 0 {
			for _, msg := range dynMsgs {
				slog.Warn(msg, logKeyComponent, componentName)
			}
			return dynMsgs, nil
		}

		return nil, nil
	}

	// The collector renders with a real runtime class and this gate is
	// about to stand behind the coherence of the two resolved names. A
	// --dynamic on either class path defers it to the operator-editable
	// cluster-values.yaml, where a retarget desynchronizes exactly what
	// was verified — so both paths are guarded whenever the comparison is
	// reached, on every platform (a matching pair is one edit away from a
	// mismatched one). Not guarded on the blocking path below: a
	// mismatched bundle is rejected outright either way.
	dynMsgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys,
		[]string{nvsentinelMetadataCollectorRuntimeClassPath}, guardReason)
	dynMsgs = append(dynMsgs, nvsentinelDynamicGuardViolations(bundlerConfig, componentName, gpuOpKeys,
		[]string{"operator.runtimeClass"}, guardReason)...)
	if len(dynMsgs) > 0 {
		for _, msg := range dynMsgs {
			slog.Warn(msg, logKeyComponent, componentName)
		}
		return dynMsgs, nil
	}

	if collectorClass == operatorClass {
		return nil, nil
	}

	msg := fmt.Sprintf(
		"%s: metadata-collector sets runtimeClassName=%q but the GPU Operator creates "+
			"its primary RuntimeClass as %q (%s operator.runtimeClass), so no RuntimeClass "+
			"with that name will exist and the API server rejects every metadata-collector "+
			"pod at admission (`pod rejected: RuntimeClass %q not found`). The DaemonSet "+
			"then shows desired pods but zero created — no pod object ever exists, so there "+
			"is nothing to kubectl describe; the only signal is a FailedCreate event on the "+
			"DaemonSet. Align the collector with the operator's runtime class at bundle "+
			"time: --set nv-sentinel:%s=%s.",
		componentName, collectorClass, operatorClass, gpuOpName, collectorClass,
		nvsentinelMetadataCollectorRuntimeClassPath, operatorClass)
	slog.Warn(msg, logKeyComponent, componentName)
	return []string{msg}, nil
}

// CheckMariaDBOperatorOwnershipCoherence enforces the snapshot-driven
// installation-safety policy for AICR-provided Slurm accounting. Existing
// MariaDB CRs and inconclusive discovery block bundling; an API with no
// detected CRs produces a warning; conclusive absence is silent. An empty
// state means no snapshot evidence was recorded and produces a non-blocking
// warning so criteria-only and older-snapshot workflows remain compatible.
func CheckMariaDBOperatorOwnershipCoherence(_ context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	ref := recipeResult.GetComponentRef(componentName)
	if ref == nil {
		return nil, nil
	}
	keys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(ref, bundlerConfig, keys) {
		return nil, nil
	}
	mode, configured := recipeResult.AccountingMode()
	if !configured || mode != recipe.AccountingModeAICRProvided {
		return nil, nil
	}

	state := recipeResult.Metadata.MariaDBOperatorState
	switch state {
	case "":
		return []string{fmt.Sprintf(
			"%s: no metadata.mariaDBOperatorState snapshot evidence was recorded. "+
				"Bundling AICR-provided accounting is allowed, but MariaDB Operator conflicts "+
				"were not evaluated. Regenerate the recipe from a current snapshot to verify "+
				"the target cluster before deployment",
			componentName)}, nil
	case recipe.MariaDBOperatorStateAbsent:
		return nil, nil
	case recipe.MariaDBOperatorStateAPIDetected:
		return []string{fmt.Sprintf(
			"%s: the snapshot detected the official MariaDB Operator API but no MariaDB resources. "+
				"Bundling AICR-provided accounting is allowed, but verify that installing another "+
				"operator instance will not conflict; otherwise regenerate with "+
				"--slurm-accounting-mode customer-managed",
			componentName)}, nil
	case recipe.MariaDBOperatorStateCRsDetected:
		return nil, []error{aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: the snapshot detected existing official MariaDB resources. "+
				"AICR-provided accounting would install a competing database stack. "+
				"Regenerate the recipe with --slurm-accounting-mode customer-managed "+
				"to use the existing database",
			componentName))}
	case recipe.MariaDBOperatorStateUnknown:
		return nil, []error{aicrerrors.New(aicrerrors.ErrCodeConflict, fmt.Sprintf(
			"%s: MariaDB Operator conflict evidence is inconclusive, so AICR-provided "+
				"accounting cannot be installed safely. Capture a fresh snapshot with sufficient "+
				"Kubernetes discovery permissions, or regenerate the recipe with "+
				"--slurm-accounting-mode customer-managed",
			componentName))}
	default:
		return nil, []error{aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s: metadata.mariaDBOperatorState=%q is not recognized. Regenerate the recipe "+
				"with this AICR version before bundling AICR-provided accounting",
			componentName, state))}
	}
}
