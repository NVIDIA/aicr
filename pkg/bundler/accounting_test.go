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

package bundler

import (
	stderrors "errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
)

func TestEnforceAccountingOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    recipe.AccountingMode
		option  config.Option
		wantErr bool
	}{
		{
			name: "reject scalar accounting gate",
			mode: recipe.AccountingModeCustomerManaged,
			option: config.WithValueOverrides(map[string]map[string]string{
				"slinkyslurm": {"accounting.enabled": "true"},
			}),
			wantErr: true,
		},
		{
			name: "reject MariaDB install gate",
			mode: recipe.AccountingModeAICRProvided,
			option: config.WithValueOverrides(map[string]map[string]string{
				"mariadboperator": {"install": "true"},
			}),
			wantErr: true,
		},
		{
			name: "reject dynamic gate",
			mode: recipe.AccountingModeCustomerManaged,
			option: config.WithDynamicValues(map[string][]string{
				"slinky-slurm": {"accounting.enabled"},
			}),
			wantErr: true,
		},
		{
			name: "allow customer connection metadata",
			mode: recipe.AccountingModeCustomerManaged,
			option: config.WithValueOverrides(map[string]map[string]string{
				"slinkyslurm": {"accounting.storageConfig.host": "db.example.com"},
			}),
		},
		{
			name: "reject AICR-provided legacy users collection",
			mode: recipe.AccountingModeAICRProvided,
			option: config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{
					Component: "slurmaccountingmariadb",
					Path:      "users",
					Value:     []any{},
				},
			}),
			wantErr: true,
		},
		{
			name: "allow AICR-provided storage sizing",
			mode: recipe.AccountingModeAICRProvided,
			option: config.WithValueOverrides(map[string]map[string]string{
				"slurmaccountingmariadb": {"mariadb.storage.size": "100Gi"},
			}),
		},
		{
			name: "reject AICR-provided cleanup policy override",
			mode: recipe.AccountingModeAICRProvided,
			option: config.WithValueOverrides(map[string]map[string]string{
				"slurmaccountingmariadb": {"mariadb.cleanupPolicy": "Delete"},
			}),
			wantErr: true,
		},
		{
			name:    "reject filter omitting Slinky",
			mode:    recipe.AccountingModeCustomerManaged,
			option:  config.WithBundlers([]string{"slinky-slurm-operator"}),
			wantErr: true,
		},
		{
			name: "reject AICR-provided filter omitting database",
			mode: recipe.AccountingModeAICRProvided,
			option: config.WithBundlers([]string{
				"slinky-slurm",
				"mariadb-operator-crds",
				"mariadb-operator",
			}),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := New(WithConfig(config.NewConfig(tt.option)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = b.enforceAccountingOwnership(accountingBundlerTestResult(tt.mode))
			if (err != nil) != tt.wantErr {
				t.Fatalf("enforceAccountingOwnership() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnforceAccountingOwnershipWarnsForLegacyOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		option config.Option
	}{
		{
			name: "scalar override",
			option: config.WithValueOverrides(map[string]map[string]string{
				"slinkyslurm": {"accounting.enabled": "true"},
			}),
		},
		{
			name: "typed override",
			option: config.WithValueOverridesTypedPaths([]config.TypedComponentPath{{
				Component: "slinkyslurm",
				Path:      "accounting.enabled",
				Value:     true,
			}}),
		},
		{
			name: "dynamic override",
			option: config.WithDynamicValues(map[string][]string{
				"slinkyslurm": {"accounting.enabled"},
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := New(WithConfig(config.NewConfig(tt.option)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			result := accountingBundlerTestResult(recipe.AccountingModeDisabled)
			result.APIVersion = recipe.RecipeResultAPIVersion
			result.Configuration = nil

			if err := b.enforceAccountingOwnership(result); err != nil {
				t.Fatalf("enforceAccountingOwnership() error = %v", err)
			}
			if len(b.warnings) != 1 ||
				!strings.Contains(b.warnings[0], "--slurm-accounting-mode customer-managed") {

				t.Fatalf("warnings = %v, want legacy accounting deprecation", b.warnings)
			}
		})
	}
}

func TestAccountingOwnershipCoversAICRProvidedContract(t *testing.T) {
	t.Parallel()

	ownership := recipe.AccountingOwnership(recipe.AccountingModeAICRProvided)
	customerOwnership := recipe.AccountingOwnership(recipe.AccountingModeCustomerManaged)
	contracts := map[string]map[string]any{
		"slinky-slurm":             slurmAICRProvidedContract(),
		"slurm-accounting-mariadb": mariaDBAICRProvidedContract(),
	}
	for componentName, contract := range contracts {
		for valuePath := range contract {
			if !containsExactPath(ownership.Paths[componentName], valuePath) {
				t.Errorf("AICR-provided ownership does not cover contract path %s.%s",
					componentName, valuePath)
			}
			if containsExactPath(customerOwnership.Paths[componentName], valuePath) {
				t.Errorf("customer-managed ownership unexpectedly covers AICR-provided contract path %s.%s",
					componentName, valuePath)
			}
		}
	}
}

func containsExactPath(paths []string, want string) bool {
	return slices.Contains(paths, want)
}

func TestValidateAccountingValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mode    recipe.AccountingMode
		values  map[string]map[string]any
		mutate  func(map[string]map[string]any)
		wantErr bool
	}{
		{
			name:   "disabled",
			mode:   recipe.AccountingModeDisabled,
			values: accountingComponentValues(false, false),
		},
		{
			name:   "customer-managed complete connection",
			mode:   recipe.AccountingModeCustomerManaged,
			values: accountingComponentValues(true, false),
		},
		{
			name:   "customer-managed accepts whole-number float64 port",
			mode:   recipe.AccountingModeCustomerManaged,
			values: accountingComponentValues(true, false),
			mutate: func(values map[string]map[string]any) {
				storage := values["slinky-slurm"]["accounting"].(map[string]any)["storageConfig"].(map[string]any)
				storage["port"] = float64(3306)
			},
		},
		{
			name:   "customer-managed missing host",
			mode:   recipe.AccountingModeCustomerManaged,
			values: accountingComponentValues(true, false),
			mutate: func(values map[string]map[string]any) {
				delete(values["slinky-slurm"]["accounting"].(map[string]any)["storageConfig"].(map[string]any), "host")
			},
			wantErr: true,
		},
		{
			name:   "customer-managed rejects non-integer port",
			mode:   recipe.AccountingModeCustomerManaged,
			values: accountingComponentValues(true, false),
			mutate: func(values map[string]map[string]any) {
				values["slinky-slurm"]["accounting"].(map[string]any)["storageConfig"].(map[string]any)["port"] = 3306.5
			},
			wantErr: true,
		},
		{
			name:   "AICR-provided complete contract",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
		},
		{
			name:   "AICR-provided changed connection secret",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
			mutate: func(values map[string]map[string]any) {
				ref := values["slinky-slurm"]["accounting"].(map[string]any)["storageConfig"].(map[string]any)["passwordKeyRef"].(map[string]any)
				ref["name"] = "other-secret"
			},
			wantErr: true,
		},
		{
			name:   "AICR-provided changed initial user",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
			mutate: func(values map[string]map[string]any) {
				mariaDB := values["slurm-accounting-mariadb"]["mariadb"].(map[string]any)
				mariaDB["username"] = "other-user"
			},
			wantErr: true,
		},
		{
			name:   "AICR-provided rejects separate User CR values",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
			mutate: func(values map[string]map[string]any) {
				values["slurm-accounting-mariadb"]["users"] = []any{
					map[string]any{"name": "slurm"},
				}
			},
			wantErr: true,
		},
		{
			name:   "AICR-provided changed generated password contract",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
			mutate: func(values map[string]map[string]any) {
				ref := values["slurm-accounting-mariadb"]["mariadb"].(map[string]any)["passwordSecretKeyRef"].(map[string]any)
				ref["generate"] = false
			},
			wantErr: true,
		},
		{
			name:   "AICR-provided rejects string boolean",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
			mutate: func(values map[string]map[string]any) {
				ref := values["slurm-accounting-mariadb"]["mariadb"].(map[string]any)["passwordSecretKeyRef"].(map[string]any)
				ref["generate"] = "true"
			},
			wantErr: true,
		},
		{
			name:   "AICR-provided rejects string port",
			mode:   recipe.AccountingModeAICRProvided,
			values: accountingComponentValues(true, true),
			mutate: func(values map[string]map[string]any) {
				storage := values["slinky-slurm"]["accounting"].(map[string]any)["storageConfig"].(map[string]any)
				storage["port"] = "3306"
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.mutate != nil {
				tt.mutate(tt.values)
			}
			err := ValidateAccountingValues(accountingBundlerTestResult(tt.mode), tt.values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAccountingValues() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEmbeddedAICRProvidedAccountingContract(t *testing.T) {
	t.Parallel()

	result, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		Intent:      recipe.CriteriaIntentTraining,
		OS:          recipe.CriteriaOSUbuntu,
		Platform:    recipe.CriteriaPlatformSlurm,
	}, recipe.WithAccountingMode(recipe.AccountingModeAICRProvided))
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}

	b, err := New(WithConfig(config.NewConfig(
		config.WithSystemNodeSelector(map[string]string{"role": "system"}),
		config.WithSystemNodeTolerations([]corev1.Toleration{{
			Key:      "dedicated",
			Operator: corev1.TolerationOpEqual,
			Value:    "system",
			Effect:   corev1.TaintEffectNoSchedule,
		}}),
	)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	values, err := b.extractComponentValues(t.Context(), result)
	if err != nil {
		t.Fatalf("extractComponentValues() error = %v", err)
	}
	if err := ValidateAccountingValues(result, values); err != nil {
		t.Fatalf("embedded AICR-provided values violate accounting contract: %v", err)
	}

	ref := result.GetComponentRef("slurm-accounting-mariadb")
	if ref == nil {
		t.Fatal("slurm-accounting-mariadb component missing")
	}
	if len(ref.ManifestFiles) != 0 {
		t.Errorf("ManifestFiles = %v, want none; initial database and user belong in the MariaDB CR",
			ref.ManifestFiles)
	}
	mariaDBValues := values["slurm-accounting-mariadb"]
	for path, want := range map[string]any{
		"mariadb.resources.requests.memory": "6Gi",
		"mariadb.resources.limits.memory":   "8Gi",
	} {
		if got, found := component.GetValueByPath(mariaDBValues, path); !found || got != want {
			t.Errorf("slurm-accounting-mariadb %s = %v, %t; want %v, true",
				path, got, found, want)
		}
	}
	myCnf, found := component.GetValueByPath(mariaDBValues, "mariadb.myCnf")
	if !found {
		t.Fatal("slurm-accounting-mariadb mariadb.myCnf not found")
	}
	myCnfString, ok := myCnf.(string)
	if !ok {
		t.Fatalf("slurm-accounting-mariadb mariadb.myCnf = %T, want string", myCnf)
	}
	for _, setting := range []string{
		"innodb_buffer_pool_size=4096M",
		"innodb_log_file_size=1024M",
		"innodb_lock_wait_timeout=900",
		"max_allowed_packet=16M",
	} {
		if !strings.Contains(myCnfString, setting) {
			t.Errorf("slurm-accounting-mariadb mariadb.myCnf missing %q", setting)
		}
	}
	for componentName, selectorPath := range map[string]string{
		"slurm-accounting-mariadb": "mariadb.nodeSelector.role",
		"slinky-slurm":             "accounting.podSpec.nodeSelector.role",
	} {
		if got, found := component.GetValueByPath(values[componentName], selectorPath); !found || got != "system" {
			t.Errorf("%s %s = %v, %t; want system, true",
				componentName, selectorPath, got, found)
		}
	}
	for componentName, tolerationPath := range map[string]string{
		"slurm-accounting-mariadb": "mariadb.tolerations",
		"slinky-slurm":             "accounting.podSpec.tolerations",
	} {
		got, found := component.GetValueByPath(values[componentName], tolerationPath)
		tolerations, ok := got.([]any)
		if !found || !ok || len(tolerations) != 1 {
			t.Errorf("%s %s = %v, %t; want one injected system toleration",
				componentName, tolerationPath, got, found)
		}
	}
}

func TestMakeRejectsDisabledAccountingComponentBeforeWriting(t *testing.T) {
	t.Parallel()

	result, err := recipe.NewBuilder().BuildFromCriteria(t.Context(), &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		Intent:      recipe.CriteriaIntentTraining,
		OS:          recipe.CriteriaOSUbuntu,
		Platform:    recipe.CriteriaPlatformSlurm,
	}, recipe.WithAccountingMode(recipe.AccountingModeAICRProvided))
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	result.GetComponentRef("mariadb-operator").Overrides["enabled"] = false

	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	outputDir := t.TempDir()
	_, err = b.Make(t.Context(), result, outputDir)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("Make() error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), `component "mariadb-operator" must be enabled`) {
		t.Fatalf("Make() error = %v, want disabled MariaDB Operator detail", err)
	}
	entries, readErr := os.ReadDir(outputDir)
	if readErr != nil {
		t.Fatalf("ReadDir() error = %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("Make() wrote %d entries before rejecting invalid accounting configuration", len(entries))
	}
}

func accountingComponentValues(accountingEnabled, includeMariaDB bool) map[string]map[string]any {
	values := map[string]map[string]any{
		"slinky-slurm": {
			"accounting": map[string]any{
				"enabled": accountingEnabled,
				"storageConfig": map[string]any{
					"host":     "mariadb",
					"port":     3306,
					"database": "slurm_acct_db",
					"username": "slurm",
					"passwordKeyRef": map[string]any{
						"name": "mariadb-password",
						"key":  "password",
					},
				},
			},
		},
	}
	if includeMariaDB {
		values["slurm-accounting-mariadb"] = map[string]any{
			"fullnameOverride":  "mariadb",
			"namespaceOverride": "slurm",
			"mariadb": map[string]any{
				"rootPasswordSecretKeyRef": map[string]any{
					"name":     "mariadb-root-password",
					"key":      "root",
					"generate": true,
				},
				"username": "slurm",
				"database": "slurm_acct_db",
				"passwordSecretKeyRef": map[string]any{
					"name":     "mariadb-password",
					"key":      "password",
					"generate": true,
				},
				"cleanupPolicy": "Skip",
			},
			"users":     []any{},
			"databases": []any{},
			"grants":    []any{},
		}
	}
	return values
}

func accountingBundlerTestResult(mode recipe.AccountingMode) *recipe.RecipeResult {
	accountingEnabled := mode != recipe.AccountingModeDisabled
	install := mode == recipe.AccountingModeAICRProvided
	return &recipe.RecipeResult{
		Kind:       recipe.RecipeResultKind,
		APIVersion: recipe.RecipeResultAPIVersion,
		Criteria:   &recipe.Criteria{Platform: recipe.CriteriaPlatformSlurm},
		Configuration: &recipe.RecipeConfiguration{
			Slurm: &recipe.SlurmConfiguration{
				Accounting: &recipe.SlurmAccountingConfiguration{Mode: mode},
			},
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name: "slinky-slurm",
				Overrides: map[string]any{
					"accounting": map[string]any{"enabled": accountingEnabled},
				},
			},
			{Name: "mariadb-operator-crds", Overrides: map[string]any{"install": install}},
			{Name: "mariadb-operator", Overrides: map[string]any{"install": install}},
			{Name: "slurm-accounting-mariadb", Overrides: map[string]any{"install": install}},
		},
	}
}
