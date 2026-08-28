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
	stderrors "errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestParseAccountingMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    AccountingMode
		wantErr bool
	}{
		{name: "disabled", input: "disabled", want: AccountingModeDisabled},
		{name: "customer managed", input: "customer-managed", want: AccountingModeCustomerManaged},
		{name: "AICR provided", input: "aicr-provided", want: AccountingModeAICRProvided},
		{name: "normalizes whitespace and case", input: " AICR-PROVIDED ", want: AccountingModeAICRProvided},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown", input: "managed", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseAccountingMode(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseAccountingMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseAccountingMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyBuildConfigAccountingModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode              AccountingMode
		accountingEnabled bool
		mariaDBInstall    bool
	}{
		{mode: AccountingModeDisabled, accountingEnabled: false, mariaDBInstall: false},
		{mode: AccountingModeCustomerManaged, accountingEnabled: true, mariaDBInstall: false},
		{mode: AccountingModeAICRProvided, accountingEnabled: true, mariaDBInstall: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			t.Parallel()
			result := accountingTestResult()
			cfg := &buildConfig{accountingMode: &tt.mode}
			if err := applyBuildConfig(result, cfg); err != nil {
				t.Fatalf("applyBuildConfig() error = %v", err)
			}
			if err := result.validateAccountingConfiguration(); err != nil {
				t.Fatalf("validateAccountingConfiguration() error = %v", err)
			}
			gotMode, present := result.AccountingMode()
			if !present || gotMode != tt.mode {
				t.Fatalf("AccountingMode() = %q, %v; want %q, true", gotMode, present, tt.mode)
			}
			if err := requireBoolOverride(result, slinkySlurmComponentName,
				[]string{"accounting", "enabled"}, tt.accountingEnabled); err != nil {
				t.Error(err)
			}
			healthCheck := result.GetComponentRef(slinkySlurmComponentName).HealthCheckAsserts
			hasAccountingHealth := strings.Contains(healthCheck, "name: validate-accounting-ready") &&
				strings.Contains(healthCheck, "name: slinky-slurm-accounting") &&
				strings.Contains(healthCheck, "(availableReplicas > `0`): true")
			if hasAccountingHealth != tt.accountingEnabled {
				t.Errorf("accounting health check present = %v, want %v",
					hasAccountingHealth, tt.accountingEnabled)
			}
			for _, constraint := range result.Constraints {
				switch constraint.Name {
				case "K8s.slinky-slurm.accounting-count",
					"K8s.slinky-slurm[id=controller/slurm/slinky-slurm].accounting-ref-present":
					t.Errorf("unexpected generated accounting constraint %q", constraint.Name)
				}
			}
			for _, name := range []string{
				mariaDBOperatorCRDsComponentName,
				mariaDBOperatorComponentName,
				slurmAccountingMariaDBComponentName,
			} {
				if err := requireBoolOverride(result, name, []string{"install"}, tt.mariaDBInstall); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestApplyBuildConfigAccountingUsesResolvedNamespace(t *testing.T) {
	t.Parallel()

	result := accountingTestResult()
	result.GetComponentRef(slinkySlurmComponentName).Namespace = "custom-slurm"
	mode := AccountingModeCustomerManaged
	if err := applyBuildConfig(result, &buildConfig{accountingMode: &mode}); err != nil {
		t.Fatalf("applyBuildConfig() error = %v", err)
	}
	healthCheck := result.GetComponentRef(slinkySlurmComponentName).HealthCheckAsserts
	if !strings.Contains(healthCheck, "namespace: custom-slurm") {
		t.Errorf("health check does not use resolved namespace:\n%s", healthCheck)
	}
}

func TestApplyBuildConfigRejectsProfileAccountingOwnershipOverlap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mode       AccountingMode
		ownedPaths map[string][]string
		wantErr    bool
		wantDetail string
	}{
		{
			name: "GPU ownership profile is disjoint",
			mode: AccountingModeAICRProvided,
			ownedPaths: map[string][]string{
				"gpu-operator": {"driver.enabled", profileComponentEnabledPath},
			},
		},
		{
			name: "exact accounting gate overlap",
			mode: AccountingModeCustomerManaged,
			ownedPaths: map[string][]string{
				slinkySlurmComponentName: {"accounting.enabled", profileComponentEnabledPath},
			},
			wantErr:    true,
			wantDetail: "slinky-slurm.accounting.enabled",
		},
		{
			name: "ancestor accounting overlap",
			mode: AccountingModeCustomerManaged,
			ownedPaths: map[string][]string{
				slinkySlurmComponentName: {"accounting", profileComponentEnabledPath},
			},
			wantErr:    true,
			wantDetail: "slinky-slurm.accounting",
		},
		{
			name: "MariaDB component presence overlap",
			mode: AccountingModeAICRProvided,
			ownedPaths: map[string][]string{
				mariaDBOperatorComponentName: {profileComponentEnabledPath},
			},
			wantErr:    true,
			wantDetail: "mariadb-operator.enabled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := accountingTestResult()
			result.Metadata.SelectedProfile = &SelectedProfile{
				Name:       "gpuStack",
				Value:      "custom",
				OwnedPaths: tt.ownedPaths,
			}
			err := applyBuildConfig(result, &buildConfig{accountingMode: &tt.mode})
			if (err != nil) != tt.wantErr {
				t.Fatalf("applyBuildConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), tt.wantDetail) {
					t.Fatalf("applyBuildConfig() error = %v, want containing %q", err, tt.wantDetail)
				}
				if result.Configuration != nil {
					t.Fatal("applyBuildConfig() mutated configuration before rejecting ownership overlap")
				}
				if result.APIVersion != RecipeResultAPIVersion {
					t.Fatalf("apiVersion = %q after rejection, want %q", result.APIVersion, RecipeResultAPIVersion)
				}
			}
		})
	}
}

func TestValidateAccountingConfigurationRejectsDecodedProfileOverlap(t *testing.T) {
	t.Parallel()

	result := accountingTestResult()
	mode := AccountingModeCustomerManaged
	if err := applyBuildConfig(result, &buildConfig{accountingMode: &mode}); err != nil {
		t.Fatalf("applyBuildConfig() error = %v", err)
	}
	result.Metadata.SelectedProfile = &SelectedProfile{
		Name:  "slurmBehavior",
		Value: "accounting-off",
		OwnedPaths: map[string][]string{
			slinkySlurmComponentName: {"accounting.enabled", profileComponentEnabledPath},
		},
	}
	err := result.ValidateCoherence()
	if err == nil || !strings.Contains(err.Error(), "configuration ownership conflict") {
		t.Fatalf("ValidateCoherence() error = %v, want ownership conflict", err)
	}
}

func TestValidateAccountingConfigurationRequiresEnabledComponents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mode      AccountingMode
		component string
		wantErr   bool
	}{
		{
			name:      "AICR-provided requires Slinky Slurm",
			mode:      AccountingModeAICRProvided,
			component: slinkySlurmComponentName,
			wantErr:   true,
		},
		{
			name:      "AICR-provided requires MariaDB Operator CRDs",
			mode:      AccountingModeAICRProvided,
			component: mariaDBOperatorCRDsComponentName,
			wantErr:   true,
		},
		{
			name:      "AICR-provided requires MariaDB Operator",
			mode:      AccountingModeAICRProvided,
			component: mariaDBOperatorComponentName,
			wantErr:   true,
		},
		{
			name:      "AICR-provided requires accounting database",
			mode:      AccountingModeAICRProvided,
			component: slurmAccountingMariaDBComponentName,
			wantErr:   true,
		},
		{
			name:      "customer-managed requires Slinky Slurm",
			mode:      AccountingModeCustomerManaged,
			component: slinkySlurmComponentName,
			wantErr:   true,
		},
		{
			name:      "customer-managed permits disabled AICR database",
			mode:      AccountingModeCustomerManaged,
			component: slurmAccountingMariaDBComponentName,
		},
		{
			name:      "disabled mode permits disabled Slinky Slurm",
			mode:      AccountingModeDisabled,
			component: slinkySlurmComponentName,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := accountingTestResult()
			if err := applyBuildConfig(result, &buildConfig{accountingMode: &tt.mode}); err != nil {
				t.Fatalf("applyBuildConfig() error = %v", err)
			}
			result.GetComponentRef(tt.component).Overrides[componentEnabledOverrideKey] = false

			err := result.validateAccountingConfiguration()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAccountingConfiguration() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("validateAccountingConfiguration() error = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.component) ||
				!strings.Contains(err.Error(), "must be enabled") {

				t.Fatalf("validateAccountingConfiguration() error = %v, want disabled component detail", err)
			}
		})
	}
}

func TestBuilderDefaultDisabledEqualsExplicitDisabled(t *testing.T) {
	t.Parallel()

	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
		Platform:    CriteriaPlatformSlurm,
	}
	builder := NewBuilder()
	defaulted, err := builder.BuildFromCriteria(t.Context(), criteria)
	if err != nil {
		t.Fatalf("default BuildFromCriteria() error = %v", err)
	}
	explicit, err := builder.BuildFromCriteria(
		t.Context(), criteria, WithAccountingMode(AccountingModeDisabled))
	if err != nil {
		t.Fatalf("explicit BuildFromCriteria() error = %v", err)
	}
	if !reflect.DeepEqual(defaulted, explicit) {
		t.Error("default and explicit disabled recipe results differ")
	}
}

func TestResolveBuildConfig(t *testing.T) {
	t.Parallel()

	t.Run("Slurm defaults to disabled", func(t *testing.T) {
		t.Parallel()
		cfg, err := resolveBuildConfig(&Criteria{Platform: CriteriaPlatformSlurm})
		if err != nil {
			t.Fatalf("resolveBuildConfig() error = %v", err)
		}
		if cfg.accountingMode == nil || *cfg.accountingMode != AccountingModeDisabled {
			t.Fatalf("accounting mode = %v, want disabled", cfg.accountingMode)
		}
	})

	t.Run("non-Slurm rejects explicit mode", func(t *testing.T) {
		t.Parallel()
		_, err := resolveBuildConfig(&Criteria{Platform: CriteriaPlatformKubeflow},
			WithAccountingMode(AccountingModeDisabled))
		if err == nil {
			t.Fatal("resolveBuildConfig() error = nil, want error")
		}
	})

	t.Run("normalizes explicit mode", func(t *testing.T) {
		t.Parallel()
		cfg, err := resolveBuildConfig(&Criteria{Platform: CriteriaPlatformSlurm},
			WithAccountingMode(AccountingMode(" AICR-PROVIDED ")))
		if err != nil {
			t.Fatalf("resolveBuildConfig() error = %v", err)
		}
		if cfg.accountingMode == nil || *cfg.accountingMode != AccountingModeAICRProvided {
			t.Fatalf("accounting mode = %v, want aicr-provided", cfg.accountingMode)
		}
	})
}

func TestBuilderAccountingBuildsAreIsolated(t *testing.T) {
	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
		Platform:    CriteriaPlatformSlurm,
	}
	builder := NewBuilder()
	modes := []AccountingMode{
		AccountingModeAICRProvided,
		AccountingModeDisabled,
		AccountingModeCustomerManaged,
		AccountingModeAICRProvided,
		AccountingModeDisabled,
	}
	results := make([]*RecipeResult, len(modes))
	errs := make([]error, len(modes))
	var wg sync.WaitGroup
	for i := range modes {
		wg.Go(func() {
			results[i], errs[i] = builder.BuildFromCriteria(
				t.Context(), criteria, WithAccountingMode(modes[i]))
		})
	}
	wg.Wait()

	for i, mode := range modes {
		if errs[i] != nil {
			t.Fatalf("BuildFromCriteria(%q) error = %v", mode, errs[i])
		}
		gotMode, present := results[i].AccountingMode()
		if !present || gotMode != mode {
			t.Errorf("result %d AccountingMode() = %q, %t; want %q, true",
				i, gotMode, present, mode)
		}
		wantInstall := mode == AccountingModeAICRProvided
		ref := results[i].GetComponentRef(slurmAccountingMariaDBComponentName)
		if ref == nil {
			t.Fatalf("result %d missing %s", i, slurmAccountingMariaDBComponentName)
		}
		gotInstall, ok := ref.Overrides[componentInstallOverrideKey].(bool)
		if !ok || gotInstall != wantInstall {
			t.Errorf("result %d install = %v; want %t", i,
				ref.Overrides[componentInstallOverrideKey], wantInstall)
		}
	}
}

func TestValidateAccountingConfigurationRejectsNonCanonicalMode(t *testing.T) {
	result, err := NewBuilder().BuildFromCriteria(t.Context(), &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
		Platform:    CriteriaPlatformSlurm,
	}, WithAccountingMode(AccountingModeAICRProvided))
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	result.Configuration.Slurm.Accounting.Mode = AccountingMode("AICR-PROVIDED")
	if err := result.ValidateCoherence(); err == nil ||
		!strings.Contains(err.Error(), "must use canonical value") {

		t.Fatalf("ValidateCoherence() error = %v, want non-canonical mode rejection", err)
	}
}

func TestRecipeResultDeepCopyAccountingConfiguration(t *testing.T) {
	t.Parallel()
	result := accountingTestResult()
	mode := AccountingModeAICRProvided
	if err := applyBuildConfig(result, &buildConfig{accountingMode: &mode}); err != nil {
		t.Fatalf("applyBuildConfig() error = %v", err)
	}
	copied := result.DeepCopy()
	copied.Configuration.Slurm.Accounting.Mode = AccountingModeDisabled
	if got, _ := result.AccountingMode(); got != AccountingModeAICRProvided {
		t.Fatalf("source accounting mode mutated through copy: got %q", got)
	}
}

func TestBuilderMaterializesDisabledForSlurm(t *testing.T) {
	result, err := NewBuilder().BuildFromCriteria(t.Context(), &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
		Platform:    CriteriaPlatformSlurm,
	})
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	if result.APIVersion != ConfiguredRecipeResultAPIVersion {
		t.Errorf("apiVersion = %q, want %q", result.APIVersion, ConfiguredRecipeResultAPIVersion)
	}
	if mode, present := result.AccountingMode(); !present || mode != AccountingModeDisabled {
		t.Errorf("AccountingMode() = %q, %v; want disabled, true", mode, present)
	}
	for _, name := range []string{
		mariaDBOperatorCRDsComponentName,
		mariaDBOperatorComponentName,
		slurmAccountingMariaDBComponentName,
	} {
		ref := result.GetComponentRef(name)
		if ref == nil {
			t.Errorf("missing stable component ref %q", name)
			continue
		}
		if ref.IsEnabled() {
			t.Errorf("component %q enabled in disabled mode", name)
		}
	}
}

func TestBuilderDeploymentOrderMatchesEnabledComponents(t *testing.T) {
	t.Parallel()

	for _, mode := range []AccountingMode{
		AccountingModeDisabled,
		AccountingModeCustomerManaged,
		AccountingModeAICRProvided,
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteria(t.Context(), &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformSlurm,
			}, WithAccountingMode(mode))
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}

			inDeploymentOrder := make(map[string]bool, len(result.DeploymentOrder))
			for _, name := range result.DeploymentOrder {
				inDeploymentOrder[name] = true
			}
			for _, ref := range result.ComponentRefs {
				if got, want := inDeploymentOrder[ref.Name], ref.IsEnabled(); got != want {
					t.Errorf("component %q in deploymentOrder = %t, enabled = %t; order=%v",
						ref.Name, got, want, result.DeploymentOrder)
				}
			}
		})
	}
}

func accountingTestResult() *RecipeResult {
	return &RecipeResult{
		Kind:       RecipeResultKind,
		APIVersion: RecipeResultAPIVersion,
		Criteria:   &Criteria{Platform: CriteriaPlatformSlurm},
		ComponentRefs: []ComponentRef{
			{
				Name:      slinkySlurmComponentName,
				Namespace: "slurm",
				Overrides: map[string]any{},
				HealthCheckAsserts: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []
`,
			},
			{Name: mariaDBOperatorCRDsComponentName, Overrides: map[string]any{}},
			{Name: mariaDBOperatorComponentName, Overrides: map[string]any{}},
			{Name: slurmAccountingMariaDBComponentName, Overrides: map[string]any{}},
		},
	}
}
