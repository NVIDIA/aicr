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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/drainscope"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestCheckNVSentinelDrainScopeCoherent covers the gate's decision table. A
// blocked row names a substring of the message it must produce, so a row
// cannot pass on the wrong rule.
func TestCheckNVSentinelDrainScopeCoherent(t *testing.T) {
	t.Parallel()

	// timing is what the NVSentinel remediation step mixins set.
	timing := func() map[string]any {
		return map[string]any{
			"deleteAfterTimeoutMinutes": 300,
			"evictionTimeoutInSeconds":  "60",
			"drainGPUPods":              false,
			"partialDrainEnabled":       false,
		}
	}
	// pinned builds a recipe with nvsentinel and cert-manager whose emitted
	// nvsentinel values are pinned, as DefaultBundler.Make pins them: node-
	// drainer on or off, the step timing, edit applied to node-drainer's
	// values, then the bundler's drain scope step when apply is set.
	pinned := func(apply bool, drainer any, edit func(nd, global map[string]any)) *recipe.RecipeResult {
		global := map[string]any{"nodeDrainer": map[string]any{"enabled": drainer}}
		nd := timing()
		if edit != nil {
			edit(nd, global)
		}
		overrides := map[string]any{"global": global, "node-drainer": nd}
		result := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
			{Name: nvsentinelComponent, Namespace: "nvsentinel", Overrides: overrides},
			{Name: "cert-manager", Namespace: "cert-manager"},
		}}
		values := component.DeepCopyMap(overrides)
		if apply {
			// An invalid pattern fails Apply before it writes anything;
			// those rows then exercise the gate on the raw values.
			_ = drainscope.Apply(values, []string{"nvsentinel", "cert-manager"})
		}
		return result.WithResolvedValues(map[string]map[string]any{nvsentinelComponent: values})
	}
	sentinel := func(drainer any, edit func(nd, global map[string]any)) *recipe.RecipeResult {
		return pinned(true, drainer, edit)
	}
	set := func(key string, value any) func(nd, _ map[string]any) {
		return func(nd, _ map[string]any) { nd[key] = value }
	}
	unset := func(key string) func(nd, _ map[string]any) {
		return func(nd, _ map[string]any) { delete(nd, key) }
	}
	policy := func(name, ns, sel string) map[string]any {
		p := map[string]any{"name": name, "podSelector": sel, "mode": "Immediate"}
		if ns != "" {
			p["namespace"] = ns
		}
		return p
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}
	setOverride := func(path, value string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
			"nv-sentinel": {path: value}}))
	}

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantBlocked   string
	}{
		{name: "step mixin timing → passes", recipeResult: sentinel(true, nil)},
		{name: "node-drainer off, no timing → passes", recipeResult: sentinel(false, func(nd, _ map[string]any) {
			clear(nd)
		})},
		{name: "node-drainer section not a map renders → blocked",
			recipeResult: sentinel(true, func(nd, global map[string]any) {
				clear(nd)
				global["nodeDrainer"] = true
			}),
			wantBlocked: "deleteAfterTimeoutMinutes is unset"},
		{name: "no recipe → passes", recipeResult: nil},
		{name: "nvsentinel disabled → passes", recipeResult: sentinel(true, unset("deleteAfterTimeoutMinutes")),
			bundlerConfig: setOverride("enabled", "false")},
		{name: "deleteAfterTimeoutMinutes left to the chart → blocked",
			recipeResult: sentinel(true, unset("deleteAfterTimeoutMinutes")),
			wantBlocked:  "deleteAfterTimeoutMinutes is unset"},
		{name: "deleteAfterTimeoutMinutes below the floor → blocked",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", 5)),
			wantBlocked:  "deleteAfterTimeoutMinutes is 5"},
		{name: "deleteAfterTimeoutMinutes above the ceiling → blocked",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", 3000)),
			wantBlocked:  "deleteAfterTimeoutMinutes is 3000"},
		{name: "deleteAfterTimeoutMinutes fractional → blocked",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", 60.5)),
			wantBlocked:  "deleteAfterTimeoutMinutes is 60.5"},
		{name: "deleteAfterTimeoutMinutes as a string → blocked",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", "300m")),
			wantBlocked:  "deleteAfterTimeoutMinutes is \"300m\""},
		{name: "deleteAfterTimeoutMinutes at the ceiling → passes",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", 1440))},
		{name: "deleteAfterTimeoutMinutes at the floor → passes",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", 10))},
		{name: "deleteAfterTimeoutMinutes as a whole float → passes",
			recipeResult: sentinel(true, set("deleteAfterTimeoutMinutes", float64(10)))},
		{name: "deleteAfterTimeoutMinutes from --set → passes",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: setOverride("node-drainer.deleteAfterTimeoutMinutes", "120")},
		{name: "deleteAfterTimeoutMinutes typo from --set → blocked",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: setOverride("node-drainer.deleteAfterTimeoutMinutes", "12O"),
			wantBlocked:   "deleteAfterTimeoutMinutes is \"12O\""},
		{name: "evictionTimeoutInSeconds unset → blocked",
			recipeResult: sentinel(true, unset("evictionTimeoutInSeconds")),
			wantBlocked:  "evictionTimeoutInSeconds is unset"},
		{name: "evictionTimeoutInSeconds as a number → passes",
			recipeResult: sentinel(true, set("evictionTimeoutInSeconds", 120))},
		{name: "evictionTimeoutInSeconds with a unit → blocked",
			recipeResult: sentinel(true, set("evictionTimeoutInSeconds", "60s")),
			wantBlocked:  "evictionTimeoutInSeconds is \"60s\""},
		{name: "evictionTimeoutInSeconds too short → blocked",
			recipeResult: sentinel(true, set("evictionTimeoutInSeconds", "5")),
			wantBlocked:  "evictionTimeoutInSeconds is \"5\""},
		{name: "drainGPUPods unset → blocked",
			recipeResult: sentinel(true, unset("drainGPUPods")),
			wantBlocked:  "drainGPUPods is unset"},
		{name: "drainGPUPods as a string → blocked",
			recipeResult: sentinel(true, set("drainGPUPods", "false")),
			wantBlocked:  "drainGPUPods is \"false\""},
		{name: "partialDrainEnabled unset → blocked",
			recipeResult: sentinel(true, unset("partialDrainEnabled")),
			wantBlocked:  "partialDrainEnabled is unset"},
		{name: "partialDrainEnabled true without fault-quarantine's copy → blocked",
			recipeResult: sentinel(true, set("partialDrainEnabled", true)),
			wantBlocked:  "fault-quarantine.validation.partialDrainEnabled is unset"},
		{name: "partialDrainEnabled true with fault-quarantine's copy → passes",
			recipeResult:  sentinel(true, func(nd, _ map[string]any) { nd["partialDrainEnabled"] = true }),
			bundlerConfig: setOverride("fault-quarantine.validation.partialDrainEnabled", "true")},
		{name: "global partialDrainEnabled overrides node-drainer's → blocked",
			recipeResult: sentinel(true, func(_, global map[string]any) { global["partialDrainEnabled"] = true }),
			wantBlocked:  "overrides node-drainer.partialDrainEnabled=false"},
		{name: "partialDrainEnabled true with the global → passes",
			recipeResult: sentinel(true, func(nd, global map[string]any) {
				nd["partialDrainEnabled"] = true
				global["partialDrainEnabled"] = true
			})},
		{name: "global.partialDrainEnabled not a boolean → blocked",
			recipeResult: sentinel(true, func(_, global map[string]any) { global["partialDrainEnabled"] = "yes" }),
			wantBlocked:  "global.partialDrainEnabled is \"yes\""},
		{name: "systemNamespaces an extra pattern → passes",
			recipeResult: sentinel(true, set("systemNamespaces", "^(team-infra)$"))},
		{name: "fault-quarantine's copy as the string \"true\" → blocked, quoted",
			recipeResult:  sentinel(true, set("partialDrainEnabled", true)),
			bundlerConfig: setOverride("fault-quarantine.validation.partialDrainEnabled", `"true"`),
			wantBlocked:   `fault-quarantine.validation.partialDrainEnabled is "\"true\""`},
		{name: "systemNamespaces matching every namespace → blocked",
			recipeResult: sentinel(true, set("systemNamespaces", ".*")),
			wantBlocked:  "matches every namespace"},
		{name: "systemNamespaces with an empty alternative → blocked",
			recipeResult: sentinel(true, set("systemNamespaces", "^(team-infra)$|")),
			wantBlocked:  "matches every namespace"},
		{name: "systemNamespaces a deliberate prefix pattern → passes",
			recipeResult: sentinel(true, set("systemNamespaces", "^team-.*$"))},
		{name: "systemNamespaces empty → blocked",
			recipeResult: sentinel(true, set("systemNamespaces", "")),
			wantBlocked:  "protecting no namespace"},
		{name: "systemNamespaces does not compile → blocked",
			recipeResult: sentinel(true, set("systemNamespaces", "^(a|b$")),
			wantBlocked:  "fail to start"},
		{name: "systemNamespaces not a string → blocked",
			recipeResult: sentinel(true, set("systemNamespaces", []any{"a"})),
			wantBlocked:  "must be a string"},
		{name: "userNamespaces with known modes → passes",
			recipeResult: sentinel(true, set("userNamespaces", []any{
				map[string]any{"name": "training-*", "mode": "AllowCompletion"},
				map[string]any{"name": "*", "mode": "DeleteAfterTimeout"},
			}))},
		{name: "userNamespaces empty under a custom drain owning every node → passes",
			recipeResult: sentinel(true, func(nd, _ map[string]any) {
				nd["userNamespaces"] = []any{}
				nd["customDrain"] = map[string]any{"enabled": true}
			})},
		{name: "userNamespaces empty without a custom drain → blocked",
			recipeResult: sentinel(true, set("userNamespaces", []any{})),
			wantBlocked:  "marks every node drained at once"},
		{name: "userNamespaces glob that does not parse → blocked",
			recipeResult: sentinel(true, set("userNamespaces", []any{map[string]any{"name": "[", "mode": "DeleteAfterTimeout"}})),
			wantBlocked:  `userNamespaces[0].name "[" is not a valid namespace glob`},
		{name: "userNamespaces glob with a character class → passes",
			recipeResult: sentinel(true, set("userNamespaces", []any{
				map[string]any{"name": "team-[ab]*", "mode": "Immediate"},
				map[string]any{"name": "*", "mode": "DeleteAfterTimeout"}}))},
		{name: "userNamespaces without a \"*\" entry → blocked",
			recipeResult: sentinel(true, set("userNamespaces", []any{map[string]any{"name": "team-a", "mode": "Immediate"}})),
			wantBlocked:  `has no "*" entry`},
		{name: "podDrainPolicies valid, namespace omitted → passes",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{
				policy("workers", "training-*", "role=worker"), policy("rest", "", "app in (a,b)")}))},
		{name: "podDrainPolicies namespace glob that does not parse → blocked",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{policy("workers", "team-[", "role=worker")})),
			wantBlocked:  `podDrainPolicies[0].namespace "team-[" is not a valid namespace glob`},
		{name: "podDrainPolicies selector that does not parse → blocked",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{policy("workers", "", "role in (")})),
			wantBlocked:  `podDrainPolicies[0].podSelector "role in (" is not a valid label selector`},
		{name: "podDrainPolicies empty selector → blocked",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{policy("workers", "", " ")})),
			wantBlocked:  "podDrainPolicies[0].podSelector is unset"},
		{name: "podDrainPolicies selector not a string → blocked",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{map[string]any{"name": "w", "podSelector": 3, "mode": "Immediate"}})),
			wantBlocked:  "podDrainPolicies[0].podSelector is 3, want a label selector string"},
		{name: "userNamespaces and podDrainPolicies both set → blocked",
			recipeResult: sentinel(true, func(nd, _ map[string]any) {
				nd["userNamespaces"] = []any{map[string]any{"name": "*", "mode": "DeleteAfterTimeout"}}
				nd["podDrainPolicies"] = []any{policy("workers", "", "role=worker")}
			}),
			wantBlocked: "are both set"},
		{name: "podDrainPolicies with userNamespaces emptied → passes",
			recipeResult: sentinel(true, func(nd, _ map[string]any) {
				nd["userNamespaces"] = []any{}
				nd["podDrainPolicies"] = []any{policy("workers", "", "role=worker")}
			})},
		{name: "podDrainPolicies duplicate names → blocked",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{
				policy("workers", "", "role=a"), policy("workers", "", "role=b")})),
			wantBlocked: `podDrainPolicies[1].name "workers" repeats an earlier policy's`},
		{name: "userNamespaces entry without a name → blocked",
			recipeResult: sentinel(true, set("userNamespaces", []any{map[string]any{"mode": "Immediate"}})),
			wantBlocked:  "userNamespaces[0] has no name"},
		{name: "userNamespaces entry not an object → blocked",
			recipeResult: sentinel(true, set("userNamespaces", []any{"*"})),
			wantBlocked:  "userNamespaces[0] is string"},
		{name: "values without the drain scope step → blocked",
			recipeResult: pinned(false, true, nil),
			wantBlocked:  "carry no generated node-drainer.systemNamespaces"},
		{name: "values without the drain scope step leave the chart mode → blocked",
			recipeResult: pinned(false, true, nil),
			wantBlocked:  "chart's AllowCompletion applies"},
		{name: "emitted scope missing a recipe namespace → blocked",
			recipeResult: pinned(false, true, func(nd, _ map[string]any) {
				nd["systemNamespaces"] = drainscope.SystemNamespacesPattern([]string{"nvsentinel"})
				nd["userNamespaces"] = []any{map[string]any{"name": "*", "mode": "DeleteAfterTimeout"}}
			}),
			wantBlocked: "does not protect [cert-manager]"},
		{name: "operator --set pattern does not hide the emitted scope → passes",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: setOverride("node-drainer.systemNamespaces", "^(team-infra)$")},
		{name: "--set-json userNamespaces=null, replaced by the bundler's default → passes",
			recipeResult: sentinel(true, func(nd, _ map[string]any) { nd["userNamespaces"] = nil }),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{{
				Component: "nvsentinel", Path: "node-drainer.userNamespaces", Value: nil}}))},
		{name: "customDrain toggle dynamic → blocked",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: dynamic("node-drainer.customDrain.enabled"),
			wantBlocked:   "--dynamic"},
		{name: "userNamespaces misspelled mode → blocked",
			recipeResult: sentinel(true, set("userNamespaces", []any{
				map[string]any{"name": "*", "mode": "DeleteAfterTimout"}})),
			wantBlocked: `userNamespaces[0].mode is "DeleteAfterTimout"`},
		{name: "userNamespaces not a list → blocked",
			recipeResult: sentinel(true, set("userNamespaces", "*")),
			wantBlocked:  "node-drainer.userNamespaces is string, want a list"},
		{name: "podDrainPolicies entry without a mode → blocked",
			recipeResult: sentinel(true, set("podDrainPolicies", []any{
				map[string]any{"name": "workers", "podSelector": "a=b"}})),
			wantBlocked: "podDrainPolicies[0].mode is unset"},
		{name: "systemNamespaces dynamic → blocked",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: dynamic("node-drainer.systemNamespaces"),
			wantBlocked:   "--dynamic"},
		{name: "deleteAfterTimeoutMinutes dynamic → blocked",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: dynamic("node-drainer.deleteAfterTimeoutMinutes"),
			wantBlocked:   "--dynamic"},
		{name: "node-drainer section dynamic → blocked",
			recipeResult:  sentinel(true, nil),
			bundlerConfig: dynamic("node-drainer"),
			wantBlocked:   "--dynamic"},
		{name: "drain path dynamic with node-drainer off → passes",
			recipeResult:  sentinel(false, nil),
			bundlerConfig: dynamic("node-drainer.userNamespaces")},
		// CheckNVSentinelRemediationPipelineCoherent owns this guard.
		{name: "node-drainer toggle dynamic while off → left to the pipeline gate",
			recipeResult:  sentinel(false, nil),
			bundlerConfig: dynamic("global.nodeDrainer.enabled")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warnings, errs := CheckNVSentinelDrainScopeCoherent(
				t.Context(), nvsentinelComponent, tt.recipeResult, tt.bundlerConfig, nil)
			// Dynamic-guard rows block via the first return slot, the static
			// rows via the second.
			msgs := append([]string{}, warnings...)
			for _, err := range errs {
				msgs = append(msgs, err.Error())
			}
			if tt.wantBlocked == "" {
				if len(msgs) > 0 {
					t.Fatalf("want pass, got %q", msgs)
				}
				return
			}
			if joined := strings.Join(msgs, "\n"); !strings.Contains(joined, tt.wantBlocked) {
				t.Fatalf("want a message containing %q, got %q", tt.wantBlocked, msgs)
			}
		})
	}
}

// TestNVSentinelRemediationStepsPassDrainScopeGate keeps the step mixins'
// drain timing inside the bounds the gate enforces, so a mixin edit and a
// bound change cannot drift apart.
func TestNVSentinelRemediationStepsPassDrainScopeGate(t *testing.T) {
	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	for _, mixin := range []string{"nvsentinel-observe", "nvsentinel-quarantine", "nvsentinel-remediation"} {
		t.Run(mixin, func(t *testing.T) {
			values := nvsentinelRenderedValues(t, store, mixin)
			if !drainscope.NodeDrainerRenders(values) {
				t.Fatalf("%s does not enable node-drainer", mixin)
			}
			if err := drainscope.Apply(values, nil); err != nil {
				t.Fatalf("drainscope.Apply: %v", err)
			}
			if errs := drainScopeViolations(nvsentinelComponent, values); len(errs) > 0 {
				t.Errorf("%s fails the drain scope gate: %v", mixin, errs)
			}
			if errs := drainEntryViolations(nvsentinelComponent, values); len(errs) > 0 {
				t.Errorf("%s fails the drain entry checks: %v", mixin, errs)
			}
		})
	}
}
