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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestRejectOverridesForAbsentComponents covers the fail-closed rule for
// value overrides that cannot take effect: a --set / --set-json naming a
// component that will not appear in the generated bundle. Before this
// gate the override was silently discarded and the command exited 0.
func TestRejectOverridesForAbsentComponents(t *testing.T) {
	t.Parallel()

	enabledRef := func(name string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: name, Type: "Helm"}
	}
	disabledRef := func(name string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: name, Type: "Helm", Overrides: map[string]any{"enabled": false}}
	}

	tests := []struct {
		name         string
		refs         []recipe.ComponentRef
		overrides    map[string]map[string]string
		typed        []config.TypedComponentPath
		dynamic      map[string][]string
		bundlers     []string
		wantErr      bool
		wantContains []string
	}{
		{
			name:      "present component, arbitrary path → accepted",
			refs:      []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"nv-sentinel": {"labeler.assumeDriverInstalled": "true"}},
		},
		{
			name:      "present component under a registry alias → accepted",
			refs:      []recipe.ComponentRef{enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"gpuoperator": {"driver.enabled": "true"}},
		},
		{
			name:      "enabled=false on a present component → accepted (this is how you remove it)",
			refs:      []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"nv-sentinel": {"enabled": "false"}},
		},
		{
			name: "enabled=false plus another path on the same component → rejected",
			refs: []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"nv-sentinel": {
				"enabled":                       "false",
				"labeler.assumeDriverInstalled": "true",
			}},
			wantErr: true,
			wantContains: []string{
				"--set nv-sentinel:labeler.assumeDriverInstalled cannot take effect",
				"not in the generated bundle",
				"enabled=false value override removed it",
			},
		},
		{
			name: "--set-json on a component removed by --set enabled=false → rejected",
			refs: []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"nv-sentinel": {
				"enabled": "false",
			}},
			typed: []config.TypedComponentPath{
				{Component: "nv-sentinel", Path: "labeler.tolerations", Value: []any{}},
			},
			wantErr: true,
			wantContains: []string{
				"--set-json/--set-file nv-sentinel:labeler.tolerations cannot take effect",
			},
		},
		{
			name:      "path on a component the RECIPE disabled → rejected",
			refs:      []recipe.ComponentRef{disabledRef("cert-manager"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"certmanager": {"installCRDs": "true"}},
			wantErr:   true,
			wantContains: []string{
				"component \"cert-manager\" is not in the generated bundle because the recipe disables it",
			},
		},
		{
			name:      "enabled=false on a component the recipe already disabled → accepted (agrees with reality)",
			refs:      []recipe.ComponentRef{disabledRef("cert-manager"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"certmanager": {"enabled": "false"}},
		},
		{
			// A redundant enabled=false must not mask the more fundamental
			// recipe-disabled exclusion reason in the rejection message.
			name: "redundant enabled=false + another path on a recipe-disabled component → rejected with the recipe reason",
			refs: []recipe.ComponentRef{disabledRef("cert-manager"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"certmanager": {
				"enabled":     "false",
				"installCRDs": "true",
			}},
			wantErr: true,
			wantContains: []string{
				"because the recipe disables it",
			},
		},
		{
			name:      "path on a component excluded by the bundlers filter → rejected",
			refs:      []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			bundlers:  []string{"gpu-operator"},
			overrides: map[string]map[string]string{"nv-sentinel": {"labeler.assumeDriverInstalled": "true"}},
			wantErr:   true,
			wantContains: []string{
				"component \"nvsentinel\" is not in the generated bundle because the bundlers filter excludes it",
			},
		},
		{
			name:      "enabled=true on a component the bundlers filter removed → rejected (used to vanish)",
			refs:      []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			bundlers:  []string{"gpu-operator"},
			overrides: map[string]map[string]string{"nv-sentinel": {"enabled": "true"}},
			wantErr:   true,
			wantContains: []string{
				"--set nv-sentinel:enabled cannot take effect",
				"the bundlers filter excludes it",
			},
		},
		{
			name:      "enabled=false on a component the bundlers filter removed → accepted (agrees with reality)",
			refs:      []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			bundlers:  []string{"gpu-operator"},
			overrides: map[string]map[string]string{"nv-sentinel": {"enabled": "false"}},
		},
		{
			name:     "typed enabled=true on a filtered-out component → rejected",
			refs:     []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			bundlers: []string{"gpu-operator"},
			typed: []config.TypedComponentPath{
				{Component: "nv-sentinel", Path: "enabled", Value: true},
			},
			wantErr: true,
			wantContains: []string{
				"--set-json/--set-file nv-sentinel:enabled cannot take effect",
			},
		},
		{
			// No typed-enabled exemption in either direction: "enabled" is
			// valid only on scalar --set (ComponentEnabledKey's contract;
			// extractComponentValues rejects it on every PRESENT
			// component), so on an absent component it is rejected here
			// rather than exempted into silence.
			name:     "typed enabled=false on a filtered-out component → rejected too",
			refs:     []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			bundlers: []string{"gpu-operator"},
			typed: []config.TypedComponentPath{
				{Component: "nv-sentinel", Path: "enabled", Value: false},
			},
			wantErr: true,
			wantContains: []string{
				"--set-json/--set-file nv-sentinel:enabled cannot take effect",
			},
		},
		{
			name:      "path on a name the recipe never declares → rejected, listing declared names",
			refs:      []recipe.ComponentRef{enabledRef("nvsentinel"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"nosuchcomponent": {"foo.bar": "1"}},
			wantErr:   true,
			wantContains: []string{
				"unknown component \"nosuchcomponent\"",
				"recipe declares: nvsentinel, gpu-operator",
			},
		},
		{
			name:      "enabled key on an undeclared name → rejected too (nothing can act on it)",
			refs:      []recipe.ComponentRef{enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"nvsentinal": {"enabled": "false"}},
			wantErr:   true,
			wantContains: []string{
				"unknown component \"nvsentinal\"",
			},
		},
		{
			name:    "--dynamic on a present component → accepted",
			refs:    []recipe.ComponentRef{enabledRef("aws-ebs-csi-driver"), enabledRef("gpu-operator")},
			dynamic: map[string][]string{"awsebscsidriver": {"controller.replicaCount"}},
		},
		{
			name:    "--dynamic on a declared component the recipe disables → rejected",
			refs:    []recipe.ComponentRef{disabledRef("aws-ebs-csi-driver"), enabledRef("gpu-operator")},
			dynamic: map[string][]string{"awsebscsidriver": {"controller.replicaCount"}},
			wantErr: true,
			wantContains: []string{
				"--dynamic awsebscsidriver:controller.replicaCount cannot take effect",
				"the recipe disables it",
			},
		},
		{
			name:      "--dynamic on a component removed by --set enabled=false → rejected",
			refs:      []recipe.ComponentRef{enabledRef("aws-ebs-csi-driver"), enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{"awsebscsidriver": {"enabled": "false"}},
			dynamic:   map[string][]string{"awsebscsidriver": {"controller.replicaCount"}},
			wantErr:   true,
			wantContains: []string{
				"--dynamic awsebscsidriver:controller.replicaCount cannot take effect",
			},
		},
		{
			name:    "--dynamic enabled path on an absent component → rejected (never a removal idiom)",
			refs:    []recipe.ComponentRef{disabledRef("aws-ebs-csi-driver"), enabledRef("gpu-operator")},
			dynamic: map[string][]string{"awsebscsidriver": {"enabled"}},
			wantErr: true,
			wantContains: []string{
				"--dynamic awsebscsidriver:enabled cannot take effect",
			},
		},
		{
			name:      "reserved deployer key is not a component → accepted",
			refs:      []recipe.ComponentRef{enabledRef("gpu-operator")},
			overrides: map[string]map[string]string{config.DeployerOverrideKey: {"namePrefix": "tenant-a-"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := []config.Option{}
			if tt.overrides != nil {
				opts = append(opts, config.WithValueOverrides(tt.overrides))
			}
			if tt.typed != nil {
				opts = append(opts, config.WithValueOverridesTypedPaths(tt.typed))
			}
			if tt.dynamic != nil {
				opts = append(opts, config.WithDynamicValues(tt.dynamic))
			}
			if tt.bundlers != nil {
				opts = append(opts, config.WithBundlers(tt.bundlers))
			}
			b := &DefaultBundler{Config: config.NewConfig(opts...)}
			rr := &recipe.RecipeResult{ComponentRefs: tt.refs}

			enabledRefs, _, excluded, filterErr := b.filterEnabledComponents(rr)
			if filterErr != nil {
				t.Fatalf("filterEnabledComponents() error = %v", filterErr)
			}
			err := b.rejectOverridesForAbsentComponents(rr, enabledRefs, excluded)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%s", want, err.Error())
				}
			}
		})
	}
}

// TestRejectOverridesForAbsentComponents_RecipeDisabledReEnableUnchanged
// pins that the pre-existing "cannot be re-enabled" rejection still owns
// the --set <c>:enabled=true case on a recipe-disabled component, so the
// new gate cannot double-report or contradict it.
func TestRejectOverridesForAbsentComponents_RecipeDisabledReEnableUnchanged(t *testing.T) {
	t.Parallel()

	b := &DefaultBundler{Config: config.NewConfig(config.WithValueOverrides(
		map[string]map[string]string{"certmanager": {"enabled": "true", "installCRDs": "true"}}))}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
		{Name: "cert-manager", Type: "Helm", Overrides: map[string]any{"enabled": false}},
		{Name: "gpu-operator", Type: "Helm"},
	}}

	enabledRefs, _, _, err := b.filterEnabledComponents(rr)
	if err == nil {
		t.Fatalf("filterEnabledComponents() enabledRefs = %v, want the re-enable rejection", enabledRefs)
	}
	if !strings.Contains(err.Error(), "cannot be re-enabled") {
		t.Errorf("error = %v, want the pre-existing re-enable rejection", err)
	}
}

// TestMake_SubsetBundleKeepsNVSentinelGateEvidence pins the union-
// evidence rule end to end through DefaultBundler.Make: a supported
// bundlers=nvsentinel subset bundle previously SKIPPED the NVSentinel
// driver-label gate, because filterEnabledComponents dropped the
// gpu-operator ref (carrying driver.enabled=false — the gate's platform
// evidence) before validations ran. The declared union now travels with
// the validations view, so the subset bundle is blocked exactly like
// the full one; with the documented remedy it passes.
func TestMake_SubsetBundleKeepsNVSentinelGateEvidence(t *testing.T) {
	t.Parallel()

	newRecipe := func() *recipe.RecipeResult {
		return &recipe.RecipeResult{
			APIVersion: "aicr.run/v1alpha2",
			Kind:       "RecipeResult",
			Criteria:   &recipe.Criteria{Service: recipe.CriteriaServiceOKE, Accelerator: "a100", Intent: "training"},
			ComponentRefs: []recipe.ComponentRef{
				{
					Name: "gpu-operator", Namespace: "gpu-operator", Version: "v26.3.3",
					Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator",
					Overrides: map[string]any{"driver": map[string]any{"enabled": false}},
				},
				{
					Name: "nvsentinel", Namespace: "nvsentinel", Version: "v1.9.0",
					Type: "helm", Source: "oci://ghcr.io/nvidia", Chart: "nvsentinel",
				},
			},
			DeploymentOrder: []string{"gpu-operator", "nvsentinel"},
		}
	}

	t.Run("subset bundle without the remedy → blocked", func(t *testing.T) {
		t.Parallel()
		b, err := New(WithConfig(config.NewConfig(config.WithBundlers([]string{"nvsentinel"}))))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		_, makeErr := b.Make(t.Context(), newRecipe(), t.TempDir())
		if makeErr == nil {
			t.Fatal("Make() = nil error, want the driver-label gate rejection on the subset bundle")
		}
		if !strings.Contains(makeErr.Error(), "labeler.assumeDriverInstalled") {
			t.Errorf("error = %v, want the driver-label gate message", makeErr)
		}
	})

	t.Run("subset bundle with the remedy → passes the gate", func(t *testing.T) {
		t.Parallel()
		b, err := New(WithConfig(config.NewConfig(
			config.WithBundlers([]string{"nvsentinel"}),
			config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"labeler.assumeDriverInstalled": "true"},
			}),
		)))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if _, makeErr := b.Make(t.Context(), newRecipe(), t.TempDir()); makeErr != nil {
			t.Fatalf("Make() error = %v, want subset bundle to pass with the remedy", makeErr)
		}
	})
}
