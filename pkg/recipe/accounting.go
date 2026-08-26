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
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"gopkg.in/yaml.v3"
)

const (
	slinkySlurmComponentName            = "slinky-slurm"
	mariaDBOperatorCRDsComponentName    = "mariadb-operator-crds"
	mariaDBOperatorComponentName        = "mariadb-operator"
	slurmAccountingMariaDBComponentName = "slurm-accounting-mariadb"
	accountingEnabledOverrideKey        = "enabled"
	componentEnabledOverrideKey         = "enabled"
	componentInstallOverrideKey         = "install"
)

// AccountingMode is the ownership model for a Slurm accounting database.
// It is intentionally not a recipe criterion: changing it changes resolved
// configuration, not catalog selection.
type AccountingMode string

const (
	AccountingModeDisabled        AccountingMode = "disabled"
	AccountingModeCustomerManaged AccountingMode = "customer-managed"
	AccountingModeAICRProvided    AccountingMode = "aicr-provided"
)

// AccountingModes returns the accepted wire values in stable display order.
func AccountingModes() []string {
	return []string{
		string(AccountingModeDisabled),
		string(AccountingModeCustomerManaged),
		string(AccountingModeAICRProvided),
	}
}

// ParseAccountingMode parses a customer-facing accounting mode.
func ParseAccountingMode(value string) (AccountingMode, error) {
	mode := AccountingMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case AccountingModeDisabled, AccountingModeCustomerManaged, AccountingModeAICRProvided:
		return mode, nil
	default:
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Slurm accounting mode %q: must be one of %s",
				value, strings.Join(AccountingModes(), ", ")))
	}
}

// RecipeConfiguration records typed desired-state choices that affect the
// resolved recipe without participating in catalog matching.
type RecipeConfiguration struct {
	Slurm *SlurmConfiguration `json:"slurm,omitempty" yaml:"slurm,omitempty"`

	// RuntimeInventory records the generation-time selection for the runtime
	// AI inventory component. See runtimeinventory.go.
	//
	// This is the second entry here, and the pattern is one bespoke selection
	// per optional component. That is deliberate for two (ADR-019 asks for
	// this component specifically, and a generic per-component disable needs
	// a policy for which components may be declined at all). If a third
	// arrives, revisit rather than extending by reflex.
	RuntimeInventory *RuntimeInventoryConfiguration `json:"runtimeInventory,omitempty" yaml:"runtimeInventory,omitempty"`
}

// SlurmConfiguration records Slurm-specific desired state.
type SlurmConfiguration struct {
	Accounting *SlurmAccountingConfiguration `json:"accounting,omitempty" yaml:"accounting,omitempty"`
}

// SlurmAccountingConfiguration records the selected accounting ownership mode.
type SlurmAccountingConfiguration struct {
	Mode AccountingMode `json:"mode" yaml:"mode"`
}

// BuildOption configures one recipe build without mutating the shared Builder.
type BuildOption func(*buildConfig)

type buildConfig struct {
	accountingMode       *AccountingMode
	runtimeInventoryMode *RuntimeInventoryMode
}

// WithAccountingMode selects the Slurm accounting ownership mode for one
// build. The mode is validated again at the build boundary.
func WithAccountingMode(mode AccountingMode) BuildOption {
	return func(cfg *buildConfig) {
		cfg.accountingMode = &mode
	}
}

func resolveBuildConfig(criteria *Criteria, opts ...BuildOption) (*buildConfig, error) {
	cfg := &buildConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	isSlurm := criteria != nil && criteria.Platform == CriteriaPlatformSlurm
	if cfg.accountingMode != nil && !isSlurm {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"Slurm accounting mode can only be set when recipe platform is slurm")
	}
	if !isSlurm {
		return cfg, nil
	}
	if cfg.accountingMode == nil {
		mode := AccountingModeDisabled
		cfg.accountingMode = &mode
	}
	mode, err := ParseAccountingMode(string(*cfg.accountingMode))
	if err != nil {
		return nil, err
	}
	cfg.accountingMode = &mode
	return cfg, nil
}

// AccountingOwnership returns the canonical component paths controlled by the
// selected Slurm accounting mode. Every configured mode owns the derived gates
// and component-presence decisions. AICR-provided mode additionally owns the
// fixed cross-component MariaDB connection contract.
func AccountingOwnership(mode AccountingMode) OwnershipDomain {
	paths := map[string][]string{
		slinkySlurmComponentName: {
			"accounting.enabled",
			profileComponentEnabledPath,
		},
		mariaDBOperatorCRDsComponentName: {
			profileComponentEnabledPath,
			componentInstallOverrideKey,
		},
		mariaDBOperatorComponentName: {
			profileComponentEnabledPath,
			componentInstallOverrideKey,
		},
		slurmAccountingMariaDBComponentName: {
			profileComponentEnabledPath,
			componentInstallOverrideKey,
		},
	}
	if mode == AccountingModeAICRProvided {
		paths[slinkySlurmComponentName] = append(paths[slinkySlurmComponentName],
			"accounting.storageConfig.database",
			"accounting.storageConfig.host",
			"accounting.storageConfig.passwordKeyRef.key",
			"accounting.storageConfig.passwordKeyRef.name",
			"accounting.storageConfig.port",
			"accounting.storageConfig.username",
		)
		paths[slurmAccountingMariaDBComponentName] = append(
			paths[slurmAccountingMariaDBComponentName],
			"databases",
			"fullnameOverride",
			"grants",
			"mariadb.cleanupPolicy",
			"mariadb.database",
			"mariadb.passwordSecretKeyRef.generate",
			"mariadb.passwordSecretKeyRef.key",
			"mariadb.passwordSecretKeyRef.name",
			"mariadb.rootPasswordSecretKeyRef.generate",
			"mariadb.rootPasswordSecretKeyRef.key",
			"mariadb.rootPasswordSecretKeyRef.name",
			"mariadb.username",
			"namespaceOverride",
			"users",
		)
	}
	return OwnershipDomain{
		Name:  fmt.Sprintf("configuration.slurm.accounting.mode=%s", mode),
		Paths: paths,
	}
}

func validateAccountingProfileOwnership(result *RecipeResult, mode AccountingMode) error {
	if result == nil || result.Metadata.SelectedProfile == nil {
		return nil
	}
	selected := result.Metadata.SelectedProfile
	return ValidateOwnershipDisjoint(
		OwnershipDomain{
			Name:  fmt.Sprintf("profile %s=%s", selected.Name, selected.Value),
			Paths: selected.OwnedPaths,
		},
		AccountingOwnership(mode),
	)
}

func applyBuildConfig(result *RecipeResult, cfg *buildConfig) error {
	if result == nil || cfg == nil {
		return nil
	}
	selected := false
	if cfg.runtimeInventoryMode != nil {
		if err := applyRuntimeInventoryMode(result, *cfg.runtimeInventoryMode); err != nil {
			return err
		}
		selected = true
	}
	if cfg.accountingMode == nil {
		// Deployment order still has to be refreshed: a selection that ran
		// above may have disabled a component, and the accounting path's own
		// recompute is not reached on this branch.
		if selected {
			return recomputeDeploymentOrder(result)
		}
		return nil
	}

	mode := *cfg.accountingMode
	if err := validateAccountingProfileOwnership(result, mode); err != nil {
		return err
	}
	// Update in place rather than assigning a fresh RecipeConfiguration:
	// another selection may already have recorded its own section, and
	// replacing the struct would silently discard it while leaving that
	// selection's component overrides applied. A recipe that acts on a
	// decision it no longer records is worse than one that never made it.
	if result.Configuration == nil {
		result.Configuration = &RecipeConfiguration{}
	}
	result.Configuration.Slurm = &SlurmConfiguration{
		Accounting: &SlurmAccountingConfiguration{Mode: mode},
	}
	result.APIVersion = ConfiguredRecipeResultAPIVersion

	accountingEnabled := mode != AccountingModeDisabled
	mariaDBInstall := mode == AccountingModeAICRProvided

	if err := setComponentOverride(result, slinkySlurmComponentName,
		map[string]any{"accounting": map[string]any{accountingEnabledOverrideKey: accountingEnabled}}); err != nil {
		return err
	}
	if accountingEnabled {
		if err := appendSlurmAccountingHealthCheck(result); err != nil {
			return err
		}
	}
	for _, name := range []string{
		mariaDBOperatorCRDsComponentName,
		mariaDBOperatorComponentName,
		slurmAccountingMariaDBComponentName,
	} {
		if err := setComponentOverride(result, name, map[string]any{componentInstallOverrideKey: mariaDBInstall}); err != nil {
			return err
		}
	}
	return recomputeDeploymentOrder(result)
}

// recomputeDeploymentOrder refreshes DeploymentOrder after a selection has
// changed which components are enabled.
//
// TopologicalSort emits enabled components only, so a selection that disables
// one without recomputing leaves the disabled component listed in the emitted
// recipe's deploymentOrder. The bundler re-filters by IsEnabled, so nothing
// mis-deploys, but the artifact contradicts itself and `aicr query --selector
// deploymentOrder` reports a component the same document marks disabled.
func recomputeDeploymentOrder(result *RecipeResult) error {
	spec := &RecipeMetadataSpec{ComponentRefs: result.ComponentRefs}
	deploymentOrder, err := spec.TopologicalSort()
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to recompute deployment order after applying a build selection")
	}
	result.DeploymentOrder = deploymentOrder
	return nil
}

const slurmAccountingHealthStepTemplate = `
name: validate-accounting-ready
try:
  - assert:
      resource:
        apiVersion: apps/v1
        kind: StatefulSet
        metadata:
          name: %q
          namespace: %q
        status:
          (availableReplicas > ` + "`0`" + `): true
`

func appendSlurmAccountingHealthCheck(result *RecipeResult) error {
	ref := result.GetComponentRef(slinkySlurmComponentName)
	if ref == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Slurm recipe is missing required component %q", slinkySlurmComponentName))
	}
	if strings.TrimSpace(ref.HealthCheckAsserts) == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"enabled Slurm accounting requires a slinky-slurm health check")
	}
	if strings.TrimSpace(ref.Namespace) == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"enabled Slurm accounting requires a namespace for slinky-slurm")
	}

	var check map[string]any
	if err := yaml.Unmarshal([]byte(ref.HealthCheckAsserts), &check); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to parse slinky-slurm health check for accounting specialization", err)
	}
	spec, ok := check["spec"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest,
			"slinky-slurm health check has no object spec")
	}
	steps, ok := spec["steps"].([]any)
	if !ok {
		return errors.New(errors.ErrCodeInvalidRequest,
			"slinky-slurm health check has no steps list")
	}
	var accountingStep map[string]any
	stepData := fmt.Sprintf(slurmAccountingHealthStepTemplate,
		ref.Name+"-accounting", ref.Namespace)
	if err := yaml.Unmarshal([]byte(stepData), &accountingStep); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to parse built-in Slurm accounting health check", err)
	}
	spec["steps"] = append(steps, accountingStep)
	data, err := serializer.MarshalYAMLDeterministic(check)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to serialize Slurm accounting health check")
	}
	ref.HealthCheckAsserts = string(data)
	return nil
}

func setComponentOverride(result *RecipeResult, name string, override map[string]any) error {
	ref := result.GetComponentRef(name)
	if ref == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Slurm recipe is missing required accounting component %q", name))
	}
	if ref.Overrides == nil {
		ref.Overrides = make(map[string]any)
	}
	deepMergeMap(ref.Overrides, override)
	return nil
}

// AccountingMode returns the recipe-selected Slurm accounting mode. The bool
// is false for legacy recipes that do not carry typed accounting evidence.
func (r *RecipeResult) AccountingMode() (AccountingMode, bool) {
	if r == nil || r.Configuration == nil || r.Configuration.Slurm == nil ||
		r.Configuration.Slurm.Accounting == nil {

		return "", false
	}
	return r.Configuration.Slurm.Accounting.Mode, true
}

func (r *RecipeResult) validateAccountingConfiguration() error {
	mode, present := r.AccountingMode()
	if !present {
		return nil
	}
	if !header.IsSupportedProfileAPIVersion(r.APIVersion) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("configuration.slurm.accounting requires apiVersion %q or %q (got %q)",
				ConfiguredRecipeResultAPIVersion, header.GroupVersionV1Beta2, r.APIVersion))
	}
	if r.Criteria == nil || r.Criteria.Platform != CriteriaPlatformSlurm {
		return errors.New(errors.ErrCodeInvalidRequest,
			"configuration.slurm.accounting is only valid for a Slurm recipe")
	}
	normalizedMode, err := ParseAccountingMode(string(mode))
	if err != nil {
		return err
	}
	if normalizedMode != mode {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("configuration.slurm.accounting.mode must use canonical value %q (got %q)",
				normalizedMode, mode))
	}
	if err := validateAccountingProfileOwnership(r, mode); err != nil {
		return err
	}

	expectedAccounting := mode != AccountingModeDisabled
	expectedInstall := mode == AccountingModeAICRProvided
	if err := requireBoolOverride(r, slinkySlurmComponentName,
		[]string{"accounting", "enabled"}, expectedAccounting); err != nil {
		return err
	}
	if expectedAccounting {
		if err := requireAccountingComponentEnabled(r, slinkySlurmComponentName, mode); err != nil {
			return err
		}
	}
	for _, name := range []string{
		mariaDBOperatorCRDsComponentName,
		mariaDBOperatorComponentName,
		slurmAccountingMariaDBComponentName,
	} {
		if err := requireBoolOverride(r, name, []string{"install"}, expectedInstall); err != nil {
			return err
		}
		if expectedInstall {
			if err := requireAccountingComponentEnabled(r, name, mode); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireAccountingComponentEnabled(
	result *RecipeResult,
	component string,
	mode AccountingMode,
) error {

	ref := result.GetComponentRef(component)
	if ref == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("accounting mode requires component %q", component))
	}
	if !ref.IsEnabled() {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q must be enabled for Slurm accounting mode %q", component, mode))
	}
	return nil
}

func requireBoolOverride(result *RecipeResult, component string, path []string, expected bool) error {
	ref := result.GetComponentRef(component)
	if ref == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("accounting mode requires component %q", component))
	}
	var value any = ref.Overrides
	for _, key := range path {
		m, ok := value.(map[string]any)
		if !ok {
			return accountingOverrideError(component, path, expected, value)
		}
		value, ok = m[key]
		if !ok {
			return accountingOverrideError(component, path, expected, nil)
		}
	}
	actual, ok := value.(bool)
	if !ok || actual != expected {
		return accountingOverrideError(component, path, expected, value)
	}
	return nil
}

func accountingOverrideError(component string, path []string, expected bool, actual any) error {
	return errors.New(errors.ErrCodeInvalidRequest,
		fmt.Sprintf("component %q override %s must be %t for the selected Slurm accounting mode (got %v)",
			component, strings.Join(path, "."), expected, actual))
}
