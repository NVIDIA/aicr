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
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// runtimeInventoryComponentName is the component this selection controls.
// Named as a constant rather than inlined so the coupling between the recipe
// configuration and the registry entry is greppable from both ends.
const runtimeInventoryComponentName = "k8s-aibom"

// RuntimeInventoryMode is the generation-time selection for the runtime AI
// inventory component.
//
// ADR-019 requires stock adoption to carry "generation-time, recipe-recorded
// selection and opt-out semantics", and explicitly rejects a bundle-time
// `--set k8s-aibom:enabled=false` because that changes neither the recipe nor
// its health checks. This mode is recorded in the emitted recipe and takes the
// component (and therefore its health check) out of the resolved set, which is
// the contract that objection asks for.
type RuntimeInventoryMode string

const (
	// RuntimeInventoryEnabled keeps the component the recipe declares. It
	// confirms an existing selection rather than granting one: a recipe that
	// declines the component is rejected instead of re-enabled, so this cannot
	// be used to opt a recipe into a combination it deliberately excludes.
	RuntimeInventoryEnabled RuntimeInventoryMode = "enabled"
	// RuntimeInventoryDisabled removes it from the resolved recipe.
	RuntimeInventoryDisabled RuntimeInventoryMode = "disabled"
)

// RuntimeInventoryModes returns the accepted values, for CLI help and
// validation messages.
func RuntimeInventoryModes() []string {
	return []string{
		string(RuntimeInventoryEnabled),
		string(RuntimeInventoryDisabled),
	}
}

// ParseRuntimeInventoryMode validates a mode string. Matching is exact:
// accepting case variants would let a config file and a flag disagree about
// what "Disabled" means.
func ParseRuntimeInventoryMode(value string) (RuntimeInventoryMode, error) {
	switch RuntimeInventoryMode(value) {
	case RuntimeInventoryEnabled, RuntimeInventoryDisabled:
		return RuntimeInventoryMode(value), nil
	default:
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid runtime inventory mode %q: must be one of %s",
				value, strings.Join(RuntimeInventoryModes(), ", ")))
	}
}

// RuntimeInventoryConfiguration records the selected mode in the recipe.
type RuntimeInventoryConfiguration struct {
	Mode RuntimeInventoryMode `json:"mode" yaml:"mode"`
}

// WithRuntimeInventoryMode selects the runtime inventory mode for one build.
// The value is validated again at the build boundary.
func WithRuntimeInventoryMode(mode RuntimeInventoryMode) BuildOption {
	return func(cfg *buildConfig) {
		cfg.runtimeInventoryMode = &mode
	}
}

// RuntimeInventoryMode reports the recorded mode, and whether the recipe
// records one at all. A recipe built without the selection records nothing,
// so a stock recipe is byte-identical to one generated before this existed.
func (r *RecipeResult) RuntimeInventoryMode() (RuntimeInventoryMode, bool) {
	if r == nil || r.Configuration == nil || r.Configuration.RuntimeInventory == nil {
		return "", false
	}
	return r.Configuration.RuntimeInventory.Mode, true
}

// applyRuntimeInventoryMode records the selection and takes the component out
// of the resolved set when disabled.
//
// Unlike the Slurm accounting selection, which can be validated from criteria
// alone (platform=slurm), this one depends on whether the resolved recipe
// actually declares the component, so the guard lives here rather than in
// resolveBuildConfig.
func applyRuntimeInventoryMode(result *RecipeResult, mode RuntimeInventoryMode) error {
	parsed, err := ParseRuntimeInventoryMode(string(mode))
	if err != nil {
		return err
	}

	// Fail closed on a recipe that never declares the component. Selecting a
	// mode here is a mistake — wrong criteria, a typo, a recipe that simply
	// does not carry it — and silently succeeding would record a decision the
	// recipe cannot honor. Checked before Configuration is written so a
	// rejected build leaves no partial record.
	if result.GetComponentRef(runtimeInventoryComponentName) == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("runtime inventory mode %q requires the recipe to declare component %q; "+
				"this recipe does not resolve it",
				parsed, runtimeInventoryComponentName))
	}

	// Fail closed when the resolved recipe already declined the component and
	// the caller asks to enable it. This must be read BEFORE the override is
	// written: setComponentOverride writes the same `install` key an overlay
	// uses to decline, so the write clobbers the overlay's decision and the
	// post-write check below would only read back what it just wrote.
	//
	// h100-gke-cos-inference-dynamo is the case that matters. It declines the
	// inherited component deliberately, because k8s-aibom alongside grove and
	// dynamo-platform is a combination nothing has qualified. Re-enabling it
	// from the command line must not silently produce that stack.
	//
	// Mirrors the bundle-time guard in filterEnabledComponents, which already
	// rejects the equivalent `--set k8s-aibom:enabled=true`. The two paths
	// should not disagree about whether a recipe-level decline is overridable.
	declinedByRecipe := !result.GetComponentRef(runtimeInventoryComponentName).IsEnabled()
	if parsed == RuntimeInventoryEnabled && declinedByRecipe {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q is disabled by the recipe and cannot be re-enabled "+
				"with --runtime-inventory enabled; remove the override in the recipe or "+
				"select a recipe that enables it",
				runtimeInventoryComponentName))
	}

	if result.Configuration == nil {
		result.Configuration = &RecipeConfiguration{}
	}
	result.Configuration.RuntimeInventory = &RuntimeInventoryConfiguration{Mode: parsed}
	result.APIVersion = ConfiguredRecipeResultAPIVersion

	// The component's health check lives on this same ref, so disabling the
	// component removes its check from deployment validation without any
	// separate bookkeeping. That is the half ADR-019 says a bundle-time
	// override cannot deliver.
	install := parsed == RuntimeInventoryEnabled
	if err := setComponentOverride(result, runtimeInventoryComponentName,
		map[string]any{componentInstallOverrideKey: install}); err != nil {
		return err
	}

	// Confirm the selection actually took effect rather than trusting the key
	// we just wrote. IsEnabled fails closed on either the `enabled` or the
	// `install` override, so an overlay that already set `enabled: false`
	// leaves the component disabled while this records mode: enabled — a
	// recipe stating a decision it does not implement. Compare the resolved
	// predicate, not the key.
	ref := result.GetComponentRef(runtimeInventoryComponentName)
	if ref.IsEnabled() != install {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("runtime inventory mode %q cannot be applied to component %q: "+
				"another override in the resolved recipe holds it %s; "+
				"remove that override or drop the mode",
				parsed, runtimeInventoryComponentName,
				map[bool]string{true: "enabled", false: "disabled"}[ref.IsEnabled()]))
	}
	return nil
}
