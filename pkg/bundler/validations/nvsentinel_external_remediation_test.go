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
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestCheckNVSentinelExternalRemediationCoherent covers every rule, an ERR
// action added with --set under another key, the chart's own action keys, and
// the --dynamic guard.
func TestCheckNVSentinelExternalRemediationCoherent(t *testing.T) {
	t.Parallel()

	on := map[string]any{"enabled": true}
	remediation := func() map[string]any {
		return map[string]any{"dryRun": false, "faultQuarantine": on, "nodeDrainer": on, "faultRemediation": on}
	}
	observe := func() map[string]any {
		g := remediation()
		g["dryRun"] = true
		return g
	}
	quarantine := func() map[string]any {
		return map[string]any{"dryRun": false, "faultQuarantine": on, "nodeDrainer": on}
	}
	action := func() map[string]any {
		return map[string]any{
			"apiGroup":              "nvsentinel.dgxc.nvidia.com",
			"version":               "v1",
			"kind":                  "ExternalRemediationRequest",
			"scope":                 "Cluster",
			"completeConditionType": "ExternalRemediationComplete",
			"templateFileName":      "external-remediation",
			"equivalenceGroup":      "external-remediation",
		}
	}
	with := func(m map[string]any, key string, value any) map[string]any {
		m[key] = value
		return m
	}
	without := func(m map[string]any, key string) map[string]any {
		delete(m, key)
		return m
	}
	// policy is a declared fault: an enabled, fatal Node-condition policy.
	policy := func(name string, event map[string]any) map[string]any {
		healthEvent := map[string]any{
			"componentClass":          "Node",
			"isFatal":                 true,
			"message":                 "repair requested",
			"recommendedAction":       "CUSTOM",
			"customRecommendedAction": "external-remediation",
		}
		for k, v := range event {
			healthEvent[k] = v
		}
		return map[string]any{
			"name": name, "enabled": true,
			"resource":    map[string]any{"group": "", "version": "v1", "kind": "Node"},
			"predicate":   map[string]any{"expression": "true"},
			"healthEvent": healthEvent,
		}
	}
	// recipeOf builds nvsentinel with the step's global toggles, the given ERR
	// actions (nil for none) with the mixin's template, and object-monitor
	// policies (none leaves the monitor off).
	recipeOf := func(global map[string]any, actions map[string]any, policies ...map[string]any) *recipe.RecipeResult {
		overrides := map[string]any{"global": global}
		if actions != nil {
			overrides["fault-remediation"] = map[string]any{"maintenance": map[string]any{
				"actions":   actions,
				"templates": map[string]any{"external-remediation": "kind: ExternalRemediationRequest\n"},
			}}
		}
		if len(policies) > 0 {
			global["kubernetesObjectMonitor"] = map[string]any{"enabled": true}
			list := make([]any, 0, len(policies))
			for _, p := range policies {
				list = append(list, p)
			}
			overrides["kubernetes-object-monitor"] = map[string]any{"policies": list}
		}
		return &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent, Overrides: overrides}}}
	}
	mixin := func() map[string]any { return map[string]any{"external-remediation": action()} }
	// withTemplate sets template key to text in the recipe's ERR templates.
	withTemplate := func(r *recipe.RecipeResult, key, text string) *recipe.RecipeResult {
		fr := r.ComponentRefs[0].Overrides["fault-remediation"].(map[string]any)
		fr["maintenance"].(map[string]any)["templates"].(map[string]any)[key] = text
		return r
	}
	declared := func(event map[string]any) map[string]any { return policy("platform-node-repair", event) }
	set := func(overrides map[string]string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{"nv-sentinel": overrides}))
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}

	const (
		wantNoRemediation = "nothing creates the request"
		wantContract      = `ExternalRemediationRequest action "external-remediation" has `
		wantNamespace     = `sets namespace "nvsentinel" -- the CRD is cluster-scoped, and NVSentinel's external remediation contract says not to set namespace`
		wantEntityScope   = `sets impactedEntityScope "GPU_UUID"`
		wantTemplate      = "does not define -- fault-remediation refuses to start"
		wantTemplateKind  = "has no line declaring kind: ExternalRemediationRequest -- fault-remediation creates whatever kind the template renders"
		gpuResetTemplate  = "apiVersion: {{ .ApiGroup }}/{{ .Version }}\nkind: GPUReset\nmetadata:\n  name: maintenance-{{ .HealthEventID }}\n"
		wantNoCustomName  = "refuses to load its config"
		wantUnrouted      = "labels the node remediation-failed without repair"
		wantNotTOMLBool   = "is invalid TOML -- the chart prints it into the monitor's config as is"
		wantMonitorOff    = "global.kubernetesObjectMonitor.enabled is not true"
		wantNoPolicy      = `no enabled kubernetes-object-monitor policy emits recommendedAction CUSTOM with customRecommendedAction "external-remediation"`
		wantNotFatal      = "fault-quarantine quarantines only fatal"
		wantNotExecute    = "fault-quarantine processes only EXECUTE_REMEDIATION events"
		wantDynamic       = "decides whether a health event routes to an ExternalRemediationRequest"
	)

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantWarnings  []string
		wantErrs      []string
	}{
		{
			name:         "stock nvsentinel",
			recipeResult: recipeOf(map[string]any{}, nil),
		},
		{
			name:         "remediation step without the mixin",
			recipeResult: recipeOf(remediation(), nil),
		},
		{
			name:         "mixin on the remediation step with a declared fault",
			recipeResult: recipeOf(remediation(), mixin(), declared(nil)),
		},
		{
			name:         "mixin on the observe step with a declared fault, as a rehearsal",
			recipeResult: recipeOf(observe(), mixin(), declared(nil)),
		},
		{
			name:         "rule 1: mixin on the quarantine step",
			recipeResult: recipeOf(quarantine(), mixin(), declared(nil)),
			wantErrs:     []string{wantNoRemediation},
		},
		{
			name:         "rule 1: mixin alone",
			recipeResult: recipeOf(map[string]any{}, mixin()),
			wantErrs:     []string{wantNoRemediation},
		},
		{
			name:         "rule 2: apiGroup",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "apiGroup", "janitor.dgxc.nvidia.com")}, declared(nil)),
			wantErrs:     []string{wantContract + `apiGroup "janitor.dgxc.nvidia.com", not "nvsentinel.dgxc.nvidia.com"`},
		},
		{
			name:         "rule 2: version",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "version", "v1alpha1")}, declared(nil)),
			wantErrs:     []string{wantContract + `version "v1alpha1", not "v1"`},
		},
		{
			name:         "rule 2: scope",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "scope", "Namespaced")}, declared(nil)),
			wantErrs:     []string{wantContract + `scope "Namespaced", not "Cluster"`},
		},
		{
			name:         "rule 2: scope unset renders as Cluster",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": without(action(), "scope")}, declared(nil)),
		},
		{
			name:         "rule 2: the mixin's key with another kind is still an ERR action",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "kind", "ExternalRemediationRequests")}, declared(nil)),
			wantErrs:     []string{wantContract + `kind "ExternalRemediationRequests", not "ExternalRemediationRequest"`},
		},
		{
			name:         "rule 2: namespace",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "namespace", "nvsentinel")}, declared(nil)),
			wantErrs:     []string{wantNamespace},
		},
		{
			name:         "rule 2: empty namespace is not rendered",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "namespace", "")}, declared(nil)),
		},
		{
			name:         "rule 2: completeConditionType unset renders as NodeReady",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": without(action(), "completeConditionType")}, declared(nil)),
			wantErrs:     []string{wantContract + `completeConditionType "NodeReady", not "ExternalRemediationComplete"`},
		},
		{
			name:         "rule 2: impactedEntityScope",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "impactedEntityScope", "GPU_UUID")}, declared(nil)),
			wantErrs:     []string{wantEntityScope},
		},
		{
			name:         "rule 2: equivalenceGroup",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "equivalenceGroup", "restart")}, declared(nil)),
			wantErrs:     []string{wantContract + `equivalenceGroup "restart", not "external-remediation"`},
		},
		{
			name:         "rule 2: template the values do not define",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": with(action(), "templateFileName", "nvidia-reboot.yaml")}, declared(nil)),
			wantErrs:     []string{wantTemplate},
		},
		{
			name: "rule 2: template nulled",
			recipeResult: func() *recipe.RecipeResult {
				r := recipeOf(remediation(), mixin(), declared(nil))
				fr := r.ComponentRefs[0].Overrides["fault-remediation"].(map[string]any)
				fr["maintenance"].(map[string]any)["templates"] = map[string]any{"external-remediation": nil}
				return r
			}(),
			wantErrs: []string{wantTemplate},
		},
		{
			name:         "rule 2: templateFileName set to the remediation step's gpureset template",
			recipeResult: withTemplate(recipeOf(remediation(), mixin(), declared(nil)), "gpureset", gpuResetTemplate),
			bundlerConfig: set(map[string]string{
				"fault-remediation.maintenance.actions.external-remediation.templateFileName": "gpureset",
			}),
			wantErrs: []string{`templateFileName "gpureset", whose template in fault-remediation.maintenance.templates ` + wantTemplateKind},
		},
		{
			name: "rule 2: a template without a kind line",
			recipeResult: withTemplate(recipeOf(remediation(), mixin(), declared(nil)), "external-remediation",
				"apiVersion: {{ .ApiGroup }}/{{ .Version }}\nmetadata:\n  name: extrr-{{ .HealthEventID }}\n  kind: ExternalRemediationRequests\n"),
			wantErrs: []string{wantTemplateKind},
		},
		{
			name: "rule 2: a quoted, padded kind line",
			recipeResult: withTemplate(recipeOf(remediation(), mixin(), declared(nil)), "external-remediation",
				"apiVersion: {{ .ApiGroup }}/{{ .Version }}\r\nkind:  'ExternalRemediationRequest' \r\n"),
		},
		{
			name:         "rule 2 keyed on kind: an ERR action under another key, added with --set",
			recipeResult: recipeOf(remediation(), nil),
			bundlerConfig: set(map[string]string{
				"fault-remediation.maintenance.actions.vendor-repair.kind":     "ExternalRemediationRequest",
				"fault-remediation.maintenance.actions.vendor-repair.apiGroup": "nvsentinel.dgxc.nvidia.com",
				"fault-remediation.maintenance.actions.vendor-repair.version":  "v1",
			}),
			wantWarnings: []string{`action "vendor-repair" is present, but ` + wantMonitorOff},
			wantErrs: []string{
				`action "vendor-repair" has completeConditionType "NodeReady"`,
				`action "vendor-repair" has equivalenceGroup "", not "external-remediation"`,
				`action "vendor-repair" has templateFileName ""`,
			},
		},
		{
			name:         "an ERR action under another key beside the mixin's",
			recipeResult: recipeOf(remediation(), map[string]any{"external-remediation": action(), "vendor-repair": action()}, declared(nil)),
			wantWarnings: []string{`no enabled kubernetes-object-monitor policy emits recommendedAction CUSTOM with customRecommendedAction "vendor-repair"`},
		},
		{
			name:          "an action of another kind added with --set is not an ERR action",
			recipeResult:  recipeOf(remediation(), nil),
			bundlerConfig: set(map[string]string{"fault-remediation.maintenance.actions.vendor-repair.kind": "RebootNode"}),
		},
		{
			name:         "rule 3a: a disabled CUSTOM policy without a custom name, no mixin",
			recipeResult: recipeOf(map[string]any{}, nil, with(declared(map[string]any{"customRecommendedAction": ""}), "enabled", false)),
			wantErrs:     []string{wantNoCustomName},
		},
		{
			name:         "rule 3a: a CUSTOM policy with a null custom name",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"customRecommendedAction": nil})),
			wantWarnings: []string{wantNoPolicy},
			wantErrs:     []string{wantNoCustomName},
		},
		{
			name:         "rule 3b: enabled unset",
			recipeResult: recipeOf(remediation(), mixin(), without(declared(nil), "enabled")),
			wantWarnings: []string{wantNoPolicy},
			wantErrs:     []string{`policy "platform-node-repair" has enabled "", which ` + wantNotTOMLBool},
		},
		{
			name:         "rule 3b: enabled a string the TOML does not read as a boolean",
			recipeResult: recipeOf(remediation(), mixin(), with(declared(nil), "enabled", "yes")),
			wantWarnings: []string{wantNoPolicy},
			wantErrs:     []string{`has enabled "yes"`},
		},
		{
			name:         "rule 3b: enabled the string true renders as a TOML boolean",
			recipeResult: recipeOf(remediation(), mixin(), with(declared(nil), "enabled", "true")),
		},
		{
			name:         "rule 3b: isFatal unset",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"isFatal": nil})),
			wantErrs:     []string{`has healthEvent.isFatal "", which ` + wantNotTOMLBool},
		},
		{
			name:         "rule 3b: isFatal capitalized, on a disabled policy without the mixin",
			recipeResult: recipeOf(map[string]any{}, nil, with(declared(map[string]any{"isFatal": "True"}), "enabled", false)),
			wantErrs:     []string{`has healthEvent.isFatal "True"`},
		},
		{
			name:         "rule 3b: both unset",
			recipeResult: recipeOf(quarantine(), nil, without(declared(map[string]any{"isFatal": nil}), "enabled")),
			wantErrs:     []string{`has enabled ""`, `has healthEvent.isFatal ""`},
		},
		{
			name:         "rule 7: a policy naming an action that does not exist",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"customRecommendedAction": "external-remediaton"})),
			wantWarnings: []string{wantNoPolicy, wantUnrouted},
		},
		{
			name:         "rule 7: a CUSTOM policy without the mixin",
			recipeResult: recipeOf(remediation(), nil, declared(nil)),
			wantWarnings: []string{wantUnrouted},
		},
		{
			name:         "rule 7: a CUSTOM policy on the observe step without the mixin",
			recipeResult: recipeOf(observe(), nil, declared(nil)),
			wantWarnings: []string{wantUnrouted},
		},
		{
			name:         "rule 7: a CUSTOM policy on the quarantine step, where nothing remediates",
			recipeResult: recipeOf(quarantine(), nil, declared(nil)),
		},
		{
			name:         "rule 7: a disabled policy is not routed",
			recipeResult: recipeOf(remediation(), nil, with(declared(nil), "enabled", false)),
		},
		{
			name:         "rule 7: a custom name the chart's actions define",
			recipeResult: recipeOf(remediation(), nil, declared(map[string]any{"customRecommendedAction": "RESTART_VM"})),
		},
		{
			name:         "rule 7: a chart action nulled by the values",
			recipeResult: recipeOf(remediation(), map[string]any{"RESTART_VM": nil}, declared(map[string]any{"customRecommendedAction": "RESTART_VM"})),
			wantWarnings: []string{wantUnrouted},
		},
		{
			name:         "rule 7: the monitor off leaves its policies inert",
			recipeResult: recipeOf(remediation(), nil, declared(map[string]any{"customRecommendedAction": ""})),
			bundlerConfig: set(map[string]string{
				"global.kubernetesObjectMonitor.enabled": "false",
			}),
		},
		{
			name:         "rule 5: the monitor is off",
			recipeResult: recipeOf(remediation(), mixin()),
			wantWarnings: []string{wantMonitorOff},
		},
		{
			name:         "rule 5: the only routing policy is disabled",
			recipeResult: recipeOf(remediation(), mixin(), with(declared(nil), "enabled", false)),
			wantWarnings: []string{wantNoPolicy},
		},
		{
			name:         "rule 5: an ERR action keyed by a built-in RecommendedAction name",
			recipeResult: recipeOf(remediation(), map[string]any{"RESTART_BM": action()}),
		},
		{
			name:         "rule 5: an ERR action keyed NONE is not exempt, since fault-remediation skips NONE events",
			recipeResult: recipeOf(remediation(), map[string]any{"NONE": action()}),
			wantWarnings: []string{`action "NONE" is present, but ` + wantMonitorOff},
		},
		{
			name:         "rule 5: the policy routes to another action",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"recommendedAction": "CONTACT_SUPPORT"})),
			wantWarnings: []string{wantNoPolicy},
		},
		{
			name:         "rule 6: isFatal false",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"isFatal": false})),
			wantWarnings: []string{wantNotFatal},
		},
		{
			name:         "rule 6: isFatal false with a forced quarantine",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"isFatal": false, "quarantineOverrides": map[string]any{"force": true}})),
		},
		{
			name:         "rule 6: isFatal false with an unforced quarantine override",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"isFatal": false, "quarantineOverrides": map[string]any{"force": false}})),
			wantWarnings: []string{wantNotFatal},
		},
		{
			name:         "rule 6: the policy is STORE_ONLY",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"processingStrategy": "STORE_ONLY"})),
			wantWarnings: []string{wantNotExecute},
		},
		{
			name:         "rule 6: the policy is STORE_AND_ANALYSE",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"processingStrategy": "STORE_AND_ANALYSE"})),
			wantWarnings: []string{`processingStrategy STORE_AND_ANALYSE -- ` + wantNotExecute},
		},
		{
			name:         "rule 6: the policy is UNSPECIFIED, which is stored as EXECUTE_REMEDIATION",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"processingStrategy": "UNSPECIFIED"})),
		},
		{
			name:          "rule 6: the monitor is STORE_ONLY",
			recipeResult:  recipeOf(remediation(), mixin(), declared(nil)),
			bundlerConfig: set(map[string]string{"kubernetes-object-monitor.processingStrategy": "STORE_ONLY"}),
			wantWarnings:  []string{wantNotExecute},
		},
		{
			name:          "rule 6: the monitor is UNSPECIFIED",
			recipeResult:  recipeOf(remediation(), mixin(), declared(nil)),
			bundlerConfig: set(map[string]string{"kubernetes-object-monitor.processingStrategy": "UNSPECIFIED"}),
		},
		{
			name:          "rule 6: the policy's strategy overrides the monitor's",
			recipeResult:  recipeOf(remediation(), mixin(), declared(map[string]any{"processingStrategy": "EXECUTE_REMEDIATION"})),
			bundlerConfig: set(map[string]string{"kubernetes-object-monitor.processingStrategy": "STORE_ONLY"}),
		},
		{
			name: "rule 6: a STORE_ONLY policy routing by a built-in name to an ERR action",
			recipeResult: recipeOf(remediation(), map[string]any{"REPLACE_VM": action()},
				declared(map[string]any{"recommendedAction": "REPLACE_VM", "processingStrategy": "STORE_ONLY"})),
			wantWarnings: []string{`action "REPLACE_VM" with processingStrategy STORE_ONLY -- ` + wantNotExecute},
		},
		{
			name: "rule 6: a non-fatal policy routing by a built-in name to an ERR action",
			recipeResult: recipeOf(remediation(), map[string]any{"REPLACE_VM": action()},
				declared(map[string]any{"recommendedAction": "REPLACE_VM", "isFatal": false})),
			wantWarnings: []string{wantNotFatal},
		},
		{
			name:         "rule 6: both",
			recipeResult: recipeOf(remediation(), mixin(), declared(map[string]any{"isFatal": false, "processingStrategy": "STORE_ONLY"})),
			wantWarnings: []string{wantNotFatal, wantNotExecute},
		},
		{
			name:         "rule 6 is not checked without live remediation",
			recipeResult: recipeOf(quarantine(), mixin(), declared(map[string]any{"isFatal": false})),
			wantErrs:     []string{wantNoRemediation},
		},
		{
			name:         "rules 1 and 2 together",
			recipeResult: recipeOf(quarantine(), map[string]any{"external-remediation": with(with(action(), "version", "v2"), "impactedEntityScope", "GPU_UUID")}),
			wantErrs:     []string{wantNoRemediation, wantContract + `version "v2"`, wantEntityScope},
		},
		{
			name:          "rule 4: the template dynamic",
			recipeResult:  recipeOf(remediation(), mixin(), declared(nil)),
			bundlerConfig: dynamic("fault-remediation.maintenance.templates.external-remediation"),
			wantErrs:      []string{wantDynamic},
		},
		{
			name:          "rule 4: the fault-remediation toggle dynamic",
			recipeResult:  recipeOf(remediation(), mixin(), declared(nil)),
			bundlerConfig: dynamic("global.faultRemediation.enabled"),
			wantErrs:      []string{wantDynamic},
		},
		{
			name:          "rule 4: the policies dynamic with a CUSTOM policy and no mixin",
			recipeResult:  recipeOf(remediation(), nil, declared(map[string]any{"customRecommendedAction": "RESTART_VM"})),
			bundlerConfig: dynamic("kubernetes-object-monitor.policies"),
			wantErrs:      []string{wantDynamic},
		},
		{
			name:          "rule 4: the monitor toggle dynamic",
			recipeResult:  recipeOf(remediation(), mixin(), declared(nil)),
			bundlerConfig: dynamic("global.kubernetesObjectMonitor.enabled"),
			wantErrs:      []string{wantDynamic},
		},
		{
			name:          "rule 4: the monitor's strategy dynamic",
			recipeResult:  recipeOf(remediation(), mixin(), declared(nil)),
			bundlerConfig: dynamic("kubernetes-object-monitor.processingStrategy"),
			wantErrs:      []string{wantDynamic},
		},
		{
			name:          "rule 4: maintenance dynamic without an ERR action or CUSTOM policy",
			recipeResult:  recipeOf(remediation(), nil),
			bundlerConfig: dynamic("fault-remediation.maintenance"),
		},
		{
			name: "nvsentinel disabled",
			recipeResult: func() *recipe.RecipeResult {
				r := recipeOf(quarantine(), mixin())
				r.ComponentRefs[0].Overrides["enabled"] = false
				return r
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelExternalRemediationCoherent(t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			assertFindings(t, "warning", warnings, tt.wantWarnings)
			got := make([]string, 0, len(errs))
			for _, err := range errs {
				errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
				got = append(got, err.Error())
			}
			assertFindings(t, "error", got, tt.wantErrs)
		})
	}
}

// assertFindings requires got to match want one to one, in order, by
// substring.
func assertFindings(t *testing.T, kind string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d %ss %q, want %d matching %q", len(got), kind, got, len(want), want)
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("%s %d = %q, want it to contain %q", kind, i, got[i], w)
		}
	}
}

// TestCheckNVSentinelExternalRemediationCoherentEntry covers the nil,
// condition-filtered and no-nvsentinel entry paths.
func TestCheckNVSentinelExternalRemediationCoherentEntry(t *testing.T) {
	t.Parallel()
	if w, e := CheckNVSentinelExternalRemediationCoherent(t.Context(), nvsentinelComponent, nil, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("nil recipe: got %v %v, want nothing", w, e)
	}
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	if w, e := CheckNVSentinelExternalRemediationCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("no nvsentinel: got %v %v, want nothing", w, e)
	}
	r = &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent}}}
	r.Criteria = &recipe.Criteria{Service: recipe.CriteriaServiceEKS}
	if w, e := CheckNVSentinelExternalRemediationCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(),
		map[string][]string{"service": {"gke"}}); w != nil || e != nil {
		t.Errorf("conditions not met: got %v %v, want nothing", w, e)
	}
}

// TestCheckNVSentinelExternalRemediationCoherentCanceled stops reading the
// monitor's policies once the context is canceled.
func TestCheckNVSentinelExternalRemediationCoherentCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	values := map[string]any{"kubernetes-object-monitor": map[string]any{"policies": []any{map[string]any{"name": "p"}}}}
	if _, err := objectMonitorPolicies(ctx, values); err == nil {
		t.Fatal("objectMonitorPolicies on a canceled context returned no error")
	} else {
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeCanceled)
	}
}

// TestShippedExternalRemediationMixinPassesGate runs the gate over the shipped
// mixins' resolved values: with each remediation step and the mixins composed
// beside it, the only finding is that no fault is declared, since no shipped
// source emits CUSTOM.
func TestShippedExternalRemediationMixinPassesGate(t *testing.T) {
	t.Parallel()
	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	for _, mixins := range [][]string{
		{"nvsentinel-remediation", "nvsentinel-health-events-analyzer", extRemediationMixinName},
		{"nvsentinel-remediation", "nvsentinel-health-events-analyzer", "nvsentinel-object-monitor", extRemediationMixinName},
		{"nvsentinel-observe", extRemediationMixinName},
	} {
		t.Run(strings.Join(mixins, "+"), func(t *testing.T) {
			t.Parallel()
			values := nvsentinelRenderedValues(t, store, mixins...)
			r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: nvsentinelComponent, Overrides: values}}}
			warnings, errs := CheckNVSentinelExternalRemediationCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil)
			if len(errs) > 0 {
				t.Fatalf("errors: %v", errs)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], `action "external-remediation" is present`) {
				t.Errorf("warnings = %q, want only that nothing routes to the action", warnings)
			}
		})
	}
}

// TestExtRemediationTemplateKind matches the kind line of the shipped mixin's
// request template and of hand-written ones.
func TestExtRemediationTemplateKind(t *testing.T) {
	t.Parallel()
	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	mixin, ok := store.Mixins[extRemediationMixinName]
	if !ok {
		t.Fatalf("%s mixin not present", extRemediationMixinName)
	}
	var shipped string
	for _, ref := range mixin.Spec.ComponentRefs {
		if ref.Name == nvsentinelComponent {
			shipped = renderedString(mapValue(ref.Overrides, extRemediationTemplatesPath+"."+extRemediationMixinActionKey))
		}
	}
	if shipped == "" {
		t.Fatalf("%s carries no %s template", extRemediationMixinName, extRemediationMixinActionKey)
	}

	tests := []struct {
		name     string
		template string
		want     bool
	}{
		{"the mixin's template", shipped, true},
		{"double-quoted", "kind: \"ExternalRemediationRequest\"\n", true},
		{"indented with trailing blanks", "  kind:\tExternalRemediationRequest  \n", true},
		{"CRLF line ends", "apiVersion: x/v1\r\nkind: ExternalRemediationRequest\r\n", true},
		{"another kind", "kind: GPUReset\n", false},
		{"a longer kind", "kind: ExternalRemediationRequests\n", false},
		{"a commented-out line", "# kind: ExternalRemediationRequest\n", false},
		{"another key", "apiKind: ExternalRemediationRequest\n", false},
		{"no kind", "apiVersion: x/v1\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := extRemediationTemplateKind.MatchString(tt.template); got != tt.want {
				t.Errorf("match %q = %v, want %v", tt.template, got, tt.want)
			}
		})
	}
}
