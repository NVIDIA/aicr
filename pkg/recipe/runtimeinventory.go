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
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// runtimeInventoryComponentName is the component this selection controls.
// Named as a constant rather than inlined so the coupling between the recipe
// configuration and the registry entry is greppable from both ends.
const runtimeInventoryComponentName = "k8s-aibom"

// runtimeInventoryValuesFile is the values file h100-gke-cos-inference names.
// A granted ref points at the same file so a recipe that received the
// component by opt-in and one that declares it hydrate identically.
const runtimeInventoryValuesFile = "components/k8s-aibom/values.yaml"

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
	// RuntimeInventoryEnabled keeps or grants the component depending on what
	// the recipe already resolves: it confirms an existing selection when the
	// recipe declares the component, grants it (GKE only, #2962) when the
	// recipe neither declares nor declines it, and is rejected -- not
	// re-enabled -- when the recipe explicitly declines it, so this cannot be
	// used to opt a recipe into a combination it deliberately excludes.
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

// grantRuntimeInventoryComponent adds the component to a recipe that does not
// declare it, mirroring what h100-gke-cos-inference declares by hand: name,
// type, and the shared values file, with everything else from the registry.
//
// Only reached for GKE recipes that neither declare nor decline the component
// (#2962). The registry is the single source for chart, repository and
// version, so a granted recipe and a declaring one render the same artifact.
func grantRuntimeInventoryComponent(result *RecipeResult) error {
	registry, err := GetComponentRegistryFor(result.provider)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load the component registry to grant the runtime inventory component")
	}
	if registry.Get(runtimeInventoryComponentName) == nil {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("component %q is not in the registry; cannot grant it",
				runtimeInventoryComponentName))
	}

	result.ComponentRefs = append(result.ComponentRefs, ComponentRef{
		Name:       runtimeInventoryComponentName,
		Type:       ComponentTypeHelm,
		ValuesFile: runtimeInventoryValuesFile,
	})

	// The package-level pass rather than ref.ApplyRegistryDefaults: only this
	// one hydrates healthCheck.assertFile, and finalizeRecipeResult's
	// recipe-wide hydration has already run by the time the grant appends. A
	// ref defaulted the per-ref way carries no asserts, and a component with
	// no asserts is skipped by the deployment validator rather than failing —
	// so the gap would surface as a passing check, not a missing one.
	if err := applyRegistryDefaults(result.provider, result.ComponentRefs[len(result.ComponentRefs)-1:]); err != nil {
		return err
	}
	return nil
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

	// A GKE recipe that simply does not mention the component can receive it
	// (#2962). Absence is not a decision -- unlike the decline below, which is.
	//
	// Scoped to GKE because that is the footprint qualified for the widened
	// adoption; every other service keeps the original rejection, so a typo'd
	// criterion that lands on EKS still fails loudly rather than shipping a
	// component nothing qualified there.
	if result.GetComponentRef(runtimeInventoryComponentName) == nil {
		gke := result.Criteria != nil && result.Criteria.Service == CriteriaServiceGKE
		if parsed != RuntimeInventoryEnabled || !gke {
			msg := fmt.Sprintf("runtime inventory mode %q requires the recipe to declare component %q; "+
				"this recipe does not resolve it",
				parsed, runtimeInventoryComponentName)
			// Naming the service is what separates "I typo'd --service" from
			// "this recipe genuinely lacks the component"; without it the
			// message sends a GKE-expecting user hunting for a declaration.
			if parsed == RuntimeInventoryEnabled {
				service := "unset"
				if result.Criteria != nil && result.Criteria.Service != "" {
					service = string(result.Criteria.Service)
				}
				msg += fmt.Sprintf(", and %q adds it only to a %q recipe (this one is %q)",
					RuntimeInventoryEnabled, CriteriaServiceGKE, service)
			}
			return errors.New(errors.ErrCodeInvalidRequest, msg)
		}
		if err := grantRuntimeInventoryComponent(result); err != nil {
			return err
		}
		slog.Info("granted runtime inventory component to a GKE recipe",
			"component", runtimeInventoryComponentName,
			"service", string(result.Criteria.Service),
			"accelerator", string(result.Criteria.Accelerator),
			"intent", string(result.Criteria.Intent))
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
