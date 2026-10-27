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
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/drainscope"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// drainerOn is the global toggle a NVSentinel remediation step mixin sets.
func drainerOn() map[string]any {
	return map[string]any{"nodeDrainer": map[string]any{"enabled": true}}
}

func renderedSystemNamespaces(t *testing.T, values map[string]any) *regexp.Regexp {
	t.Helper()
	raw, ok := component.GetValueByPath(values, drainscope.SystemNamespacesPath)
	if !ok {
		t.Fatal("node-drainer.systemNamespaces not rendered")
	}
	pattern, isString := raw.(string)
	if !isString {
		t.Fatalf("node-drainer.systemNamespaces = %T, want a string", raw)
	}
	return regexp.MustCompile(pattern)
}

// TestNVSentinelDrainScope_ProtectsEveryLeafNamespace enables node-drainer on
// every shipped leaf that deploys NVSentinel and requires the generated
// pattern to protect every namespace that leaf deploys into. OCP leaves
// deploy cert-manager and NFD to their own namespaces, so generation must
// follow the resolved recipe.
func TestNVSentinelDrainScope_ProtectsEveryLeafNamespace(t *testing.T) {
	ctx := context.Background()
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Version: "v0.0.0-test"})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	seen := map[string]bool{}
	checked := 0
	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result.GetComponentRef(drainscope.ComponentName) == nil {
			continue
		}
		t.Run(leaf.Entry.Name, func(t *testing.T) {
			values, err := leaf.Result.GetValuesForComponentWithContext(ctx, drainscope.ComponentName)
			if err != nil {
				t.Fatalf("GetValuesForComponent: %v", err)
			}
			values["global"] = drainerOn()
			if applyErr := ApplyNVSentinelDrainScope(ctx, values, leaf.Result); applyErr != nil {
				t.Fatalf("ApplyNVSentinelDrainScope: %v", applyErr)
			}
			re := renderedSystemNamespaces(t, values)
			// Derived here rather than with drainscope.RecipeNamespaces, so a
			// namespace that function dropped would not drop out of both sides.
			for _, ref := range leaf.Result.ComponentRefs {
				ns := ref.Namespace
				if ns == "" {
					if cfg := registry.Get(ref.Name); cfg != nil {
						ns = cfg.Helm.DefaultNamespace
					}
				}
				if ns == "" {
					t.Errorf("component %q has no namespace to protect", ref.Name)
					continue
				}
				seen[ns] = true
				if !re.MatchString(ns) {
					t.Errorf("namespace %q is not protected by %s", ns, re)
				}
			}
			if re.MatchString("default") {
				t.Errorf("%s protects the default namespace; user pods there would never drain", re)
			}
		})
		checked++
	}
	if checked == 0 {
		t.Fatal("no leaf deploys nvsentinel; the test proves nothing")
	}
	// The two the chart default misses that hurt most (#2626), and the OCP
	// namespaces only generation from the resolved recipe can see.
	for _, ns := range []string{"cert-manager", "nvidia-network-operator", "cert-manager-operator", "openshift-nfd"} {
		if !seen[ns] {
			t.Errorf("no leaf deploys into %q; the coverage this test claims no longer holds", ns)
		}
	}
}

// TestNVSentinelDrainScope_ProtectsEveryRegistryNamespace declares every
// registry component without a namespace, so each one falls back to its
// registry default, and requires every default to be protected.
func TestNVSentinelDrainScope_ProtectsEveryRegistryNamespace(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	refs := make([]recipe.ComponentRef, 0, len(registry.Components))
	for _, c := range registry.Components {
		refs = append(refs, recipe.ComponentRef{Name: c.Name})
	}
	values := map[string]any{"global": drainerOn()}
	if err := ApplyNVSentinelDrainScope(t.Context(), values, &recipe.RecipeResult{ComponentRefs: refs}); err != nil {
		t.Fatalf("ApplyNVSentinelDrainScope: %v", err)
	}
	re := renderedSystemNamespaces(t, values)
	for _, c := range registry.Components {
		if c.Helm.DefaultNamespace == "" {
			t.Errorf("%s has no defaultNamespace, so the drain scope cannot protect it", c.Name)
			continue
		}
		if !re.MatchString(c.Helm.DefaultNamespace) {
			t.Errorf("%s's namespace %q is not protected by %s", c.Name, c.Helm.DefaultNamespace, re)
		}
	}
	for _, ns := range []string{"cert-manager", "nvidia-network-operator"} {
		if !re.MatchString(ns) {
			t.Errorf("%q is not protected by %s", ns, re)
		}
	}
}

// TestExtractComponentValues_NVSentinelDrainScope runs the step through the
// bundler's value pipeline: it sees the recipe's namespaces, including the
// Talos privileged- ones os-talos assigns, keeps an operator's --set pattern
// as an alternative, and never touches a bundle with node-drainer off or a
// custom drain that owns every node.
func TestExtractComponentValues_NVSentinelDrainScope(t *testing.T) {
	sentinel := func(namespace string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: drainscope.ComponentName, Namespace: namespace,
			Type: recipe.ComponentTypeHelm, Overrides: overrides}
	}
	talos := []recipe.ComponentRef{
		{Name: "gpu-operator", Namespace: "privileged-gpu-operator", Type: recipe.ComponentTypeHelm},
		{Name: "cert-manager", Type: recipe.ComponentTypeHelm},
	}
	defaultScope := []any{map[string]any{"name": "*", "mode": drainscope.DefaultMode}}

	tests := []struct {
		name      string
		sentinel  recipe.ComponentRef
		opts      []config.Option
		protected []string
		drained   []string
		wantUser  any
		wantNoKey bool
	}{
		{
			name:      "drainer on, Talos namespaces",
			sentinel:  sentinel("privileged-nvsentinel", map[string]any{"global": drainerOn()}),
			protected: []string{"privileged-nvsentinel", "privileged-gpu-operator", "cert-manager", "kube-system"},
			drained:   []string{"default", "gpu-operator-x"},
			wantUser:  defaultScope,
		},
		{
			name:     "operator --set pattern kept",
			sentinel: sentinel("nvsentinel", map[string]any{"global": drainerOn()}),
			opts: []config.Option{config.WithValueOverrides(map[string]map[string]string{
				"nvsentinel": {"node-drainer.systemNamespaces": "^(local-path-storage)$"}})},
			protected: []string{"nvsentinel", "cert-manager", "local-path-storage"},
			drained:   []string{"default"},
			wantUser:  defaultScope,
		},
		{
			name:     "operator --set-json pattern kept",
			sentinel: sentinel("nvsentinel", map[string]any{"global": drainerOn()}),
			opts: []config.Option{config.WithValueOverridesTypedPaths([]config.TypedComponentPath{{
				Component: "nvsentinel", Path: "node-drainer.systemNamespaces", Value: "^(ingress-nginx)$"}})},
			protected: []string{"nvsentinel", "cert-manager", "ingress-nginx"},
			drained:   []string{"default", "ingress"},
			wantUser:  defaultScope,
		},
		{
			name:     "--set-json userNamespaces=null gets the default",
			sentinel: sentinel("nvsentinel", map[string]any{"global": drainerOn()}),
			opts: []config.Option{config.WithValueOverridesTypedPaths([]config.TypedComponentPath{{
				Component: "nvsentinel", Path: "node-drainer.userNamespaces", Value: nil}})},
			protected: []string{"nvsentinel", "cert-manager"},
			wantUser:  defaultScope,
		},
		{
			name: "custom drain owning every node keeps userNamespaces empty",
			sentinel: sentinel("nvsentinel", map[string]any{"global": drainerOn(), "node-drainer": map[string]any{
				"userNamespaces": []any{}, "customDrain": map[string]any{"enabled": true}}}),
			protected: []string{"cert-manager"},
			wantUser:  []any{},
		},
		{
			name:      "drainer off",
			sentinel:  sentinel("nvsentinel", nil),
			wantNoKey: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(tt.opts...)))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			result := &recipe.RecipeResult{ComponentRefs: append([]recipe.ComponentRef{tt.sentinel}, talos...)}
			all, err := b.extractComponentValues(t.Context(), result)
			if err != nil {
				t.Fatalf("extractComponentValues: %v", err)
			}
			values := all[drainscope.ComponentName]
			if tt.wantNoKey {
				if _, present := values[drainscope.NodeDrainerKey]; present {
					t.Fatalf("node-drainer values = %v with node-drainer off; want none", values[drainscope.NodeDrainerKey])
				}
				return
			}
			re := renderedSystemNamespaces(t, values)
			for _, ns := range tt.protected {
				if !re.MatchString(ns) {
					t.Errorf("%q is not protected by %s", ns, re)
				}
			}
			for _, ns := range tt.drained {
				if re.MatchString(ns) {
					t.Errorf("%q is protected by %s", ns, re)
				}
			}
			if user, _ := component.GetValueByPath(values, drainscope.UserNamespacesPath); !reflect.DeepEqual(user, tt.wantUser) {
				t.Errorf("userNamespaces = %v, want %v", user, tt.wantUser)
			}
		})
	}
}

func TestExtractComponentValues_NVSentinelDrainScopeRejectsInvalidPattern(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
		"nvsentinel": {"node-drainer.systemNamespaces": "^(team"}}))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: drainscope.ComponentName,
		Namespace: "nvsentinel", Type: recipe.ComponentTypeHelm, Overrides: map[string]any{"global": drainerOn()}}}}
	_, err = b.extractComponentValues(t.Context(), result)
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

// TestExtractComponentValues_DeclaredUnionAddsNoComponents pins that the
// declared union is read-only context: extractComponentValues returns values
// for the filtered components alone, however many more the recipe declares.
func TestExtractComponentValues_DeclaredUnionAddsNoComponents(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	filtered := []recipe.ComponentRef{{Name: drainscope.ComponentName, Namespace: "nvsentinel",
		Type: recipe.ComponentTypeHelm, Overrides: map[string]any{"global": drainerOn()}}}
	declared := append([]recipe.ComponentRef{
		{Name: "cert-manager", Namespace: "cert-manager", Type: recipe.ComponentTypeHelm},
		{Name: "kai-scheduler", Namespace: "kai-scheduler", Type: recipe.ComponentTypeHelm},
	}, filtered...)

	result := (&recipe.RecipeResult{ComponentRefs: filtered}).WithDeclaredComponents(declared)
	values, err := b.extractComponentValues(t.Context(), result)
	if err != nil {
		t.Fatalf("extractComponentValues: %v", err)
	}
	if len(values) != 1 || values[drainscope.ComponentName] == nil {
		t.Fatalf("component values for %v, want nvsentinel only", slices.Sorted(maps.Keys(values)))
	}
	re := renderedSystemNamespaces(t, values[drainscope.ComponentName])
	for _, ns := range []string{"cert-manager", "kai-scheduler"} {
		if !re.MatchString(ns) {
			t.Errorf("declared but filtered %q is not protected by %s", ns, re)
		}
	}
}

// TestMake_NVSentinelDrainScopeCoversFilteredComponents runs the whole
// bundle path and requires cert-manager to stay protected when this bundle
// does not deploy it: disabled for a bring-your-own install, or left out of
// a bundlers= subset. The drain scope must follow what the recipe declares,
// not what one bundle renders, while the bundle itself still leaves the
// filtered component out.
func TestMake_NVSentinelDrainScopeCoversFilteredComponents(t *testing.T) {
	newRecipe := func(certManager map[string]any) *recipe.RecipeResult {
		return &recipe.RecipeResult{
			APIVersion: "aicr.run/v1",
			Kind:       "RecipeResult",
			ComponentRefs: []recipe.ComponentRef{
				{Name: "cert-manager", Namespace: "cert-manager", Version: "v1.20.2", Type: "helm",
					Source: "https://charts.jetstack.io", Chart: "cert-manager", Overrides: certManager},
				{Name: "nvsentinel", Namespace: "nvsentinel", Version: "v1.28.0", Type: "helm",
					Source: "oci://ghcr.io/nvidia", Chart: "nvsentinel", Overrides: map[string]any{
						"global": map[string]any{
							"faultQuarantine": map[string]any{"enabled": true},
							"nodeDrainer":     map[string]any{"enabled": true},
							"datastore":       map[string]any{"provider": "postgresql"},
						},
						"node-drainer": map[string]any{
							"deleteAfterTimeoutMinutes": 300, "evictionTimeoutInSeconds": "60",
							"drainGPUPods": false, "partialDrainEnabled": false,
						},
					}},
			},
			DeploymentOrder: []string{"cert-manager", "nvsentinel"},
		}
	}
	tests := []struct {
		name        string
		certManager map[string]any
		opts        []config.Option
	}{
		{name: "cert-manager disabled", certManager: map[string]any{"enabled": false}},
		{name: "bundlers subset", opts: []config.Option{config.WithBundlers([]string{"nvsentinel"})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(tt.opts...)))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			dir := t.TempDir()
			if _, makeErr := b.Make(t.Context(), newRecipe(tt.certManager), dir); makeErr != nil {
				t.Fatalf("Make: %v", makeErr)
			}
			// The declared union is read for the drain scope only: a component
			// this bundle filters out must not be written.
			if written, _ := filepath.Glob(filepath.Join(dir, "*-cert-manager")); len(written) != 0 {
				t.Errorf("filtered-out cert-manager was written to the bundle: %v", written)
			}
			matches, _ := filepath.Glob(filepath.Join(dir, "*-nvsentinel", "values.yaml"))
			if len(matches) != 1 {
				t.Fatalf("nvsentinel values.yaml not found under %s: %v", dir, matches)
			}
			raw, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatalf("read values: %v", err)
			}
			values := map[string]any{}
			if err := yaml.Unmarshal(raw, &values); err != nil {
				t.Fatalf("parse values: %v", err)
			}
			scope, _ := component.GetValueByPath(values, drainscope.SystemNamespacesPath)
			pattern, _ := scope.(string)
			if !regexp.MustCompile(pattern).MatchString("cert-manager") || pattern == "" {
				t.Errorf("systemNamespaces %q does not protect cert-manager", pattern)
			}
		})
	}
}
