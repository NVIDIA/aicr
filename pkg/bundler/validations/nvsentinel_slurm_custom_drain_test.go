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
	"maps"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestCheckNVSentinelSlurmCustomDrainCoherent covers the gate's decision
// table. A blocked row names a substring of the message it must produce, so a
// row cannot pass on the wrong rule.
func TestCheckNVSentinelSlurmCustomDrainCoherent(t *testing.T) {
	t.Parallel()

	// customDrain is what the nvsentinel-slinky-drainer mixin sets.
	customDrain := func() map[string]any {
		return map[string]any{
			"enabled":               true,
			"templateConfigMapName": "slinky-drain-template",
			"templateMountPath":     "/etc/drain-template",
			"templateFileName":      "drain-template.yaml",
			"namespace":             "nvsentinel",
			"apiGroup":              "nvsentinel.nvidia.com",
			"version":               "v1alpha1",
			"kind":                  "DrainRequest",
			"resource":              "drainrequests",
			"statusConditionType":   "DrainComplete",
			"statusConditionStatus": "True",
			"timeout":               "1800",
		}
	}
	// sentinel builds an nvsentinel ref: drainer runs node-drainer, dryRun
	// sets global.dryRun when non-nil, drain is the customDrain block (nil
	// leaves it off), and edit adjusts node-drainer's values last.
	sentinel := func(drainer bool, dryRun any, drain map[string]any, edit func(nd map[string]any)) recipe.ComponentRef {
		global := map[string]any{"nodeDrainer": map[string]any{"enabled": drainer}}
		if dryRun != nil {
			global["dryRun"] = dryRun
		}
		nd := map[string]any{}
		if drain != nil {
			nd["customDrain"] = drain
			nd["userNamespaces"] = []any{}
		}
		if edit != nil {
			edit(nd)
		}
		return recipe.ComponentRef{Name: nvsentinelComponent, Namespace: "nvsentinel",
			Overrides: map[string]any{"global": global, "node-drainer": nd}}
	}
	with := func(key string, value any) func(nd map[string]any) {
		return func(nd map[string]any) {
			cd := maps.Clone(nd["customDrain"].(map[string]any))
			cd[key] = value
			nd["customDrain"] = cd
		}
	}
	// remediating turns on fault-remediation, as nvsentinel-remediation does.
	remediating := func(ref recipe.ComponentRef) recipe.ComponentRef {
		global := maps.Clone(ref.Overrides["global"].(map[string]any))
		global["faultRemediation"] = map[string]any{"enabled": true}
		overrides := maps.Clone(ref.Overrides)
		overrides["global"] = global
		ref.Overrides = overrides
		return ref
	}
	slinky := recipe.ComponentRef{Name: slinkySlurmComponent, Namespace: "slurm"}
	slinkyDisabled := recipe.ComponentRef{Name: slinkySlurmComponent, Namespace: "slurm",
		Overrides: map[string]any{"enabled": false}}
	plugin := func(watched string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: slinkyDrainerComponent, Namespace: "nvsentinel",
			Overrides: map[string]any{"slinkyNamespace": watched}}
	}
	crds := recipe.ComponentRef{Name: slinkyDrainerCRDsComponent, Namespace: "nvsentinel"}
	// resultNoCRDs is the recipe as given; result adds slinky-drainer-crds,
	// which the mixin always composes beside the plugin.
	resultNoCRDs := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return resultNoCRDs(append(refs, crds)...)
	}
	// pinned is what a cloud Slurm bundle passes: requireNodeSelector
	// forces both selectors, which keeps Slurm's control pods off slurmd's
	// nodes.
	pinned := []config.Option{
		config.WithSystemNodeSelector(map[string]string{"nodeGroup": "system"}),
		config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"}),
	}
	pinnedConfig := func(opts ...config.Option) *config.Config {
		return config.NewConfig(append(append([]config.Option{}, pinned...), opts...)...)
	}
	dynamic := func(key string, paths ...string) *config.Config {
		return pinnedConfig(config.WithDynamicValues(map[string][]string{key: paths}))
	}
	// slinkyWith is a slinky-slurm ref in namespace slurm with overrides.
	slinkyWith := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: slinkySlurmComponent, Namespace: "slurm", Overrides: overrides}
	}
	// setJSON pairs paths with values as --set-json slinkyslurm:<path>=<value>.
	setJSON := func(pathValues ...any) config.Option {
		var paths []config.TypedComponentPath
		for i := 0; i+1 < len(pathValues); i += 2 {
			paths = append(paths, config.TypedComponentPath{Component: "slinkyslurm",
				Path: pathValues[i].(string), Value: pathValues[i+1]})
		}
		return config.WithValueOverridesTypedPaths(paths)
	}
	podSpec := func(selector map[string]any) map[string]any {
		return map[string]any{"podSpec": map[string]any{"nodeSelector": selector}}
	}
	ok := sentinel(true, false, customDrain(), nil)

	type gateCase struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantBlocked   string
	}
	tests := make([]gateCase, 0, 80)
	tests = append(tests, []gateCase{
		{name: "nil recipe result → skipped", recipeResult: nil},
		{name: "no nvsentinel ref → skipped", recipeResult: result()},
		{
			// Every shipped recipe: no step, so node-drainer is off.
			name:         "shipped Slurm recipe, node-drainer off → passes",
			recipeResult: result(sentinel(false, nil, nil, nil), slinky),
		},
		{
			name:         "non-Slurm recipe with node-drainer live → passes",
			recipeResult: result(sentinel(true, false, nil, nil)),
		},
		{
			// nvsentinel-observe on a Slurm leaf: dry run evicts nothing.
			name:         "Slurm, node-drainer in dry run, no plugin → passes",
			recipeResult: result(sentinel(true, true, nil, nil), slinky),
		},
		{
			// nvsentinel-quarantine on a Slurm leaf: AllowCompletion evicts
			// nothing and the step promises no repair.
			name:         "Slurm, quarantine live, customDrain off → passes",
			recipeResult: result(sentinel(true, false, nil, nil), slinky),
		},
		{
			name:         "Slurm, remediation live, customDrain off → blocked",
			recipeResult: result(remediating(sentinel(true, false, nil, nil)), slinky),
			wantBlocked:  "compose the nvsentinel-slinky-drainer mixin",
		},
		{
			// A malformed dryRun counts as live.
			name:         "Slurm, remediation with non-bool dryRun, customDrain off → blocked",
			recipeResult: result(remediating(sentinel(true, "true", nil, nil)), slinky),
			wantBlocked:  "compose the nvsentinel-slinky-drainer mixin",
		},
		{
			// nvsentinel-observe on a Slurm leaf.
			name:         "Slurm, remediation in dry run, customDrain off → passes",
			recipeResult: result(remediating(sentinel(true, true, nil, nil)), slinky),
		},
		{
			// The chart's --dry-run=1 parses as true, and node-drainer creates
			// DrainRequests in dry run.
			name:         "customDrain with a non-bool dryRun → blocked",
			recipeResult: result(sentinel(true, 1, customDrain(), nil), slinky, plugin("slurm")),
			wantBlocked:  "is not a boolean",
		},
		{
			name:         "Slurm, remediation live, slinky-slurm disabled, customDrain off → passes",
			recipeResult: result(remediating(sentinel(true, false, nil, nil)), slinkyDisabled),
		},
		{
			// The shape nvsentinel-quarantine or -remediation plus the
			// nvsentinel-slinky-drainer mixin produce on a Slurm leaf.
			name:         "Slurm, customDrain coherent → passes",
			recipeResult: result(ok, slinky, plugin("slurm")),
		},
		{
			name:         "customDrain without node-drainer → blocked",
			recipeResult: result(sentinel(false, nil, customDrain(), nil), slinky, plugin("slurm")),
			wantBlocked:  "nothing creates DrainRequests",
		},
		{
			name:         "customDrain in dry run → blocked",
			recipeResult: result(sentinel(true, true, customDrain(), nil), slinky, plugin("slurm")),
			wantBlocked:  "even in dry run",
		},
		{
			name:         "customDrain without slinky-slurm → blocked",
			recipeResult: result(ok, plugin("slurm")),
			wantBlocked:  "no Slurm to drain through",
		},
		{
			name:         "customDrain with slinky-slurm disabled → blocked",
			recipeResult: result(ok, slinkyDisabled, plugin("slurm")),
			wantBlocked:  "no Slurm to drain through",
		},
		{
			name:         "customDrain without the plugin → blocked",
			recipeResult: result(ok, slinky),
			wantBlocked:  "without its drain template ConfigMap",
		},
		{
			name: "customDrain with the plugin disabled → blocked",
			recipeResult: result(ok, slinky, recipe.ComponentRef{Name: slinkyDrainerComponent, Namespace: "nvsentinel",
				Overrides: map[string]any{"enabled": false, "slinkyNamespace": "slurm"}}),
			wantBlocked: "without its drain template ConfigMap",
		},
		{
			name:         "customDrain without slinky-drainer-crds → blocked",
			recipeResult: resultNoCRDs(ok, slinky, plugin("slurm")),
			wantBlocked:  "exit at startup without the DrainRequest CRD",
		},
		{
			name: "customDrain with slinky-drainer-crds disabled → blocked",
			recipeResult: resultNoCRDs(ok, slinky, plugin("slurm"), recipe.ComponentRef{Name: slinkyDrainerCRDsComponent,
				Namespace: "nvsentinel", Overrides: map[string]any{"enabled": false}}),
			wantBlocked: "exit at startup without the DrainRequest CRD",
		},
		{
			name:          "slinky-drainer-crds toggle dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("slinkydrainercrds", "enabled"),
			wantBlocked:   "--dynamic",
		},
		{
			name: "plugin in another namespace than NVSentinel → blocked",
			recipeResult: result(ok, slinky, recipe.ComponentRef{Name: slinkyDrainerComponent, Namespace: "drainer",
				Overrides: map[string]any{"slinkyNamespace": "slurm"}}),
			wantBlocked: "mounts the drain template ConfigMap from its own namespace",
		},
		{
			name:         "plugin watching the wrong Slinky namespace → blocked",
			recipeResult: result(ok, slinky, plugin("slinky")),
			wantBlocked:  "mark every drain complete",
		},
		{
			name:         "plugin Slinky namespace unset → blocked",
			recipeResult: result(ok, slinky, recipe.ComponentRef{Name: slinkyDrainerComponent, Namespace: "nvsentinel"}),
			wantBlocked:  "mark every drain complete",
		},
		{
			name:         "slinky-slurm namespace unset, plugin watching the default → passes",
			recipeResult: result(ok, recipe.ComponentRef{Name: slinkySlurmComponent}, plugin("slurm")),
		},
		{
			name:         "slinky-slurm namespace unset, plugin watching another → blocked",
			recipeResult: result(ok, recipe.ComponentRef{Name: slinkySlurmComponent}, plugin("hpc")),
			wantBlocked:  "mark every drain complete",
		},
		{
			name: "slinky-slurm in a custom namespace the plugin watches → passes",
			recipeResult: result(ok, recipe.ComponentRef{Name: slinkySlurmComponent, Namespace: "hpc"},
				plugin("hpc")),
		},
		{
			name:         "chart-default timeout 30m → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("timeout", "30m")), slinky, plugin("slurm")),
			wantBlocked:  "whole number of seconds",
		},
		{
			name:         "zero timeout → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("timeout", "0")), slinky, plugin("slurm")),
			wantBlocked:  "whole number of seconds",
		},
		{
			name: "integer timeout (--set without quotes) → passes",
			recipeResult: result(sentinel(true, false, customDrain(), with("timeout", float64(900))), slinky,
				plugin("slurm")),
		},
		{
			name: "fractional timeout → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("timeout", 900.5)), slinky,
				plugin("slurm")),
			wantBlocked: "whole number of seconds",
		},
		{
			// quote prints a float64 of 1e6 as "1e+06", which node-drainer
			// cannot parse.
			name: "float timeout printed in exponent form → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("timeout", float64(1000000))), slinky,
				plugin("slurm")),
			wantBlocked: "whole number of seconds",
		},
		{
			// --set gives an int64; Helm reads it back as a float64 and quote
			// prints "1e+06".
			name: "integer timeout of a million seconds → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("timeout", int64(1000000))), slinky,
				plugin("slurm")),
			wantBlocked: "whole number of seconds",
		},
		{
			name: "missing timeout → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), func(nd map[string]any) {
				delete(nd["customDrain"].(map[string]any), "timeout")
			}), slinky, plugin("slurm")),
			wantBlocked: "whole number of seconds",
		},
		{
			name:         "wrong CRD kind → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("kind", "Drain")), slinky, plugin("slurm")),
			wantBlocked:  "node-drainer.customDrain.kind",
		},
		{
			name: "chart-default template ConfigMap name → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("templateConfigMapName", "drain-template")),
				slinky, plugin("slurm")),
			wantBlocked: "node-drainer.customDrain.templateConfigMapName",
		},
		{
			name: "status condition matched case-insensitively → passes",
			recipeResult: result(sentinel(true, false, customDrain(), with("statusConditionStatus", "true")),
				slinky, plugin("slurm")),
		},
		{
			name: "status condition False → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("statusConditionStatus", "False")),
				slinky, plugin("slurm")),
			wantBlocked: "DrainComplete=True",
		},
		{
			name: "requests in another namespace → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("namespace", "default")),
				slinky, plugin("slurm")),
			wantBlocked: "node-drainer.customDrain.namespace",
		},
		{
			name: "empty template mount path → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("templateMountPath", "")),
				slinky, plugin("slurm")),
			wantBlocked: "cannot read the drain template",
		},
		{
			// The chart's userNamespaces default is a catch-all entry, which
			// node-drainer rejects alongside an empty nodeSelector.
			name: "userNamespaces left at chart default → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), func(nd map[string]any) {
				delete(nd, "userNamespaces")
			}), slinky, plugin("slurm")),
			wantBlocked: "must both be empty lists",
		},
		{
			name: "podDrainPolicies set with an empty nodeSelector → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), func(nd map[string]any) {
				nd["podDrainPolicies"] = []any{map[string]any{"name": "x"}}
			}), slinky, plugin("slurm")),
			wantBlocked: "must both be empty lists",
		},
		{
			// A scope could leave slurmd nodes on pod eviction, so it is
			// rejected even where node-drainer itself would start.
			name: "nodeSelector set, userNamespaces non-empty → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), func(nd map[string]any) {
				cd := maps.Clone(nd["customDrain"].(map[string]any))
				cd["nodeSelector"] = "slurm.example.com/worker=true"
				nd["customDrain"] = cd
				nd["userNamespaces"] = []any{map[string]any{"name": "*", "mode": "AllowCompletion"}}
			}), slinky, plugin("slurm")),
			wantBlocked: "have its jobs evicted",
		},
		{
			name: "nodeSelector set with nothing left to evict by → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("nodeSelector", "slurm.example.com/worker=true")),
				slinky, plugin("slurm")),
			wantBlocked: "have its jobs evicted",
		},
		{
			name: "nodeSelector unparseable → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("nodeSelector", "[")),
				slinky, plugin("slurm")),
			wantBlocked: "have its jobs evicted",
		},
		{
			name: "nodeSelector not a string → blocked",
			recipeResult: result(sentinel(true, false, customDrain(),
				with("nodeSelector", map[string]any{"slurm.example.com/worker": "true"})), slinky, plugin("slurm")),
			wantBlocked: "have its jobs evicted",
		},
		{
			// node-drainer trims the selector, so blank is unscoped.
			name:         "nodeSelector blank → passes",
			recipeResult: result(sentinel(true, false, customDrain(), with("nodeSelector", "  ")), slinky, plugin("slurm")),
		},
		{
			name:         "nodeSelector null → passes",
			recipeResult: result(sentinel(true, false, customDrain(), with("nodeSelector", nil)), slinky, plugin("slurm")),
		},
		{
			name:         "customDrain enabled via a non-bool Helm treats as true, no plugin → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with("enabled", "yes")), slinky),
			wantBlocked:  "without its drain template ConfigMap",
		},
		{
			name:         "slinky-slurm disabled via --set with customDrain on → blocked",
			recipeResult: result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"slinkyslurm": {"enabled": "false"},
			})),
			wantBlocked: "no Slurm to drain through",
		},
		{
			name:          "plugin namespace changed via --set → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: setJSONOverride(t, `slinkydrainer:slinkyNamespace="slinky"`),
			wantBlocked:   "mark every drain complete",
		},
		{
			name:          "customDrain dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("nv-sentinel", "node-drainer.customDrain.timeout"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "remediation toggle dynamic on a Slurm recipe → blocked",
			recipeResult:  result(sentinel(false, nil, nil, nil), slinky),
			bundlerConfig: dynamic("nv-sentinel", "global.faultRemediation.enabled"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "customDrain toggle dynamic on a monitoring-only Slurm recipe → blocked",
			recipeResult:  result(sentinel(false, nil, nil, nil), slinky),
			bundlerConfig: dynamic("nv-sentinel", "node-drainer.customDrain.enabled"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "dryRun dynamic on a monitoring-only Slurm recipe → passes",
			recipeResult:  result(sentinel(false, nil, nil, nil), slinky),
			bundlerConfig: dynamic("nv-sentinel", "global.dryRun"),
		},
		{
			name:          "userNamespaces dynamic on a quarantine-only Slurm recipe → passes",
			recipeResult:  result(sentinel(true, false, nil, nil), slinky),
			bundlerConfig: dynamic("nv-sentinel", "node-drainer.userNamespaces"),
		},
		{
			name:          "dryRun dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("nv-sentinel", "global.dryRun"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "userNamespaces dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("nv-sentinel", "node-drainer.userNamespaces"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "slinky-slurm toggle dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("slinkyslurm", "enabled"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "plugin namespace dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("slinkydrainer", "slinkyNamespace"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "Slurm control pods unpinned (no --system-node-selector) → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"})),
			wantBlocked:   "controller.podSpec.nodeSelector renders no node selector",
		},
		{
			// The single-node Kind leaf's opt-out: an explicit empty selector
			// keeps the injected one out.
			name: "controller selector opted out with an empty map → blocked",
			recipeResult: result(ok, slinkyWith(map[string]any{"controller": podSpec(map[string]any{})}),
				plugin("slurm")),
			wantBlocked: "controller.podSpec.nodeSelector renders no node selector",
		},
		{
			// The Kind leaf's workflow: overlay opt-out, then --set-json.
			name: "opted-out selectors pinned with --set-json → passes",
			recipeResult: result(ok, slinkyWith(map[string]any{
				"controller": podSpec(map[string]any{}), "restapi": podSpec(map[string]any{})}), plugin("slurm")),
			bundlerConfig: pinnedConfig(setJSON(
				"controller.podSpec.nodeSelector", map[string]any{"nodeGroup": "system"},
				"restapi.podSpec.nodeSelector", map[string]any{"nodeGroup": "system"})),
		},
		{
			name: "opted-out selector nulled again with --set-json → blocked",
			recipeResult: result(ok, slinkyWith(map[string]any{"controller": podSpec(map[string]any{})}),
				plugin("slurm")),
			bundlerConfig: pinnedConfig(setJSON("controller.podSpec.nodeSelector", nil)),
			wantBlocked:   "controller.podSpec.nodeSelector renders no node selector",
		},
		{
			// A bare `nodeSelector:` opts out like {}.
			name: "controller selector opted out with null → blocked",
			recipeResult: result(ok, slinkyWith(map[string]any{"controller": podSpec(nil)}),
				plugin("slurm")),
			wantBlocked: "controller.podSpec.nodeSelector renders no node selector",
		},
		{
			// --set-json {} merges into the injected selector.
			name:          "--set-json empty selector on a pinned path → passes",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: pinnedConfig(setJSON("controller.podSpec.nodeSelector", map[string]any{})),
		},
		{
			// --system-node-selector replaces an overlay selector.
			name: "overlay puts the controller on GPU nodes, injection replaces it → passes",
			recipeResult: result(ok, slinkyWith(map[string]any{
				"controller": podSpec(map[string]any{"nodeGroup": "gpu"})}), plugin("slurm")),
		},
		{
			name: "overlay puts slurmd on system nodes, injection replaces it → passes",
			recipeResult: result(ok, slinkyWith(map[string]any{"nodesets": map[string]any{
				"slinky": podSpec(map[string]any{"nodeGroup": "system"})}}), plugin("slurm")),
		},
		{
			// --set lands before injection, which replaces it.
			name:         "--set control selector with system == accelerated → blocked",
			recipeResult: result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(
				config.WithSystemNodeSelector(map[string]string{"pool": "all"}),
				config.WithAcceleratedNodeSelector(map[string]string{"pool": "all"}),
				config.WithValueOverrides(map[string]map[string]string{"slinkyslurm": {
					"controller.podSpec.nodeSelector.pool": "cpu"}})),
			wantBlocked: "overlap",
		},
		{
			name:         "control selector a superset of slurmd's → blocked",
			recipeResult: result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(
				config.WithSystemNodeSelector(map[string]string{"nodeGroup": "gpu", "zone": "a"}),
				config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"})),
			wantBlocked: "overlap",
		},
		{
			name:         "control selector a subset of slurmd's → blocked",
			recipeResult: result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(
				config.WithSystemNodeSelector(map[string]string{"nodeGroup": "gpu"}),
				config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu", "zone": "a"})),
			wantBlocked: "overlap",
		},
		{
			// One node can carry both labels.
			name:         "control and slurmd selectors on different keys → blocked",
			recipeResult: result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(
				config.WithSystemNodeSelector(map[string]string{"role": "system"}),
				config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"})),
			wantBlocked: "overlap",
		},
		{
			name:         "selectors conflict on one key and differ on another → passes",
			recipeResult: result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(
				config.WithSystemNodeSelector(map[string]string{"nodeGroup": "system", "role": "system"}),
				config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"})),
		},
		{
			name:          "slurmd unpinned (no --accelerated-node-selector) → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(config.WithSystemNodeSelector(map[string]string{"nodeGroup": "system"})),
			wantBlocked:   "nodesets.slinky.podSpec.nodeSelector renders no node selector",
		},
		{
			name:          "slurmd selector cleared with --set-json null → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: pinnedConfig(setJSON("nodesets.slinky.podSpec.nodeSelector", nil)),
			wantBlocked:   "nodesets.slinky.podSpec.nodeSelector renders no node selector",
		},
		{
			name:          "system selector equal to the slurmd selector → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: config.NewConfig(config.WithSystemNodeSelector(map[string]string{"pool": "all"}), config.WithAcceleratedNodeSelector(map[string]string{"pool": "all"})),
			wantBlocked:   "overlap",
		},
		{
			name: "accounting disabled, its selector opted out → passes",
			recipeResult: result(ok, slinkyWith(map[string]any{"accounting": map[string]any{
				"enabled": false, "podSpec": map[string]any{"nodeSelector": map[string]any{}}}}), plugin("slurm")),
		},
		{
			name: "accounting enabled, its selector opted out → blocked",
			recipeResult: result(ok, slinkyWith(map[string]any{"accounting": map[string]any{
				"enabled": true, "podSpec": map[string]any{"nodeSelector": map[string]any{}}}}), plugin("slurm")),
			wantBlocked: "accounting.podSpec.nodeSelector renders no node selector",
		},
		{
			name: "MariaDB in the Slurm namespace opted out → blocked",
			recipeResult: result(ok, slinky, plugin("slurm"), recipe.ComponentRef{Name: slurmAccountingMariaDBComponent,
				Namespace: "slurm", Overrides: map[string]any{"mariadb": map[string]any{"nodeSelector": map[string]any{}}}}),
			wantBlocked: "mariadb.nodeSelector renders no node selector",
		},
		{
			name: "MariaDB in another namespace opted out → passes",
			recipeResult: result(ok, slinky, plugin("slurm"), recipe.ComponentRef{Name: slurmAccountingMariaDBComponent,
				Namespace: "db", Overrides: map[string]any{"mariadb": map[string]any{"nodeSelector": map[string]any{}}}}),
		},
		{
			name: "MariaDB selector dynamic with customDrain → blocked",
			recipeResult: result(ok, slinky, plugin("slurm"), recipe.ComponentRef{Name: slurmAccountingMariaDBComponent,
				Namespace: "slurm"}),
			bundlerConfig: dynamic("slurmaccountingmariadb", "mariadb.nodeSelector"),
			wantBlocked:   "--dynamic",
		},
		{
			name: "MariaDB toggle dynamic with customDrain → blocked",
			recipeResult: result(ok, slinky, plugin("slurm"), recipe.ComponentRef{Name: slurmAccountingMariaDBComponent,
				Namespace: "slurm"}),
			bundlerConfig: dynamic("slurmaccountingmariadb", "enabled"),
			wantBlocked:   "--dynamic",
		},
		{
			name: "MariaDB selector dynamic without customDrain → passes",
			recipeResult: result(sentinel(false, nil, nil, nil), slinky, recipe.ComponentRef{
				Name: slurmAccountingMariaDBComponent, Namespace: "slurm"}),
			bundlerConfig: dynamic("slurmaccountingmariadb", "mariadb.nodeSelector"),
		},
		{
			name:          "accounting toggle dynamic with customDrain → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("slinkyslurm", "accounting.enabled"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "dryRun dynamic with live remediation and customDrain off → blocked",
			recipeResult:  result(remediating(sentinel(true, false, nil, nil)), slinky),
			bundlerConfig: dynamic("nv-sentinel", "global.dryRun"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "control-pod selector dynamic → blocked",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("slinkyslurm", "controller.podSpec.nodeSelector"),
			wantBlocked:   "--dynamic",
		},
		{
			name:          "control-pod selector dynamic without customDrain → passes",
			recipeResult:  result(sentinel(false, nil, nil, nil), slinky),
			bundlerConfig: dynamic("slinkyslurm", "controller.podSpec.nodeSelector"),
		},
		{
			name:          "unrelated --dynamic path does not trip the guard → passes",
			recipeResult:  result(ok, slinky, plugin("slurm")),
			bundlerConfig: dynamic("nv-sentinel", "global.auditLogging.maxSizeMB"),
		},
		{
			name: "bundlers= subset excluding slinky-slurm and the plugin → passes",
			recipeResult: (&recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{ok}}).
				WithDeclaredComponents([]recipe.ComponentRef{ok, slinky, plugin("slurm"), crds}),
		},
	}...)

	for _, c := range slinkyDrainerCustomDrainContract {
		key := strings.TrimPrefix(c.path, nodeDrainerCustomDrainPath+".")
		tests = append(tests, gateCase{
			name:         "contract value " + key + " changed → blocked",
			recipeResult: result(sentinel(true, false, customDrain(), with(key, "other")), slinky, plugin("slurm")),
			wantBlocked:  c.path,
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = pinnedConfig()
			}
			warnings, errs := CheckNVSentinelSlurmCustomDrainCoherent(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			// Dynamic-guard rows block via the first return slot, the static
			// rows via the second.
			var msgs []string
			msgs = append(msgs, warnings...)
			for _, err := range errs {
				msgs = append(msgs, err.Error())
			}
			if tt.wantBlocked == "" {
				if len(msgs) > 0 {
					t.Fatalf("want pass, got %q", msgs)
				}
				return
			}
			joined := strings.Join(msgs, "\n")
			if !strings.Contains(joined, tt.wantBlocked) {
				t.Fatalf("want a message containing %q, got %q", tt.wantBlocked, msgs)
			}
		})
	}
}
