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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckNVSentinelMaintenanceRequestCoherent(t *testing.T) {
	t.Parallel()

	at := func(path string, value any) map[string]any { return withPath(map[string]any{}, path, value) }
	mrOn := at(maintenanceRequestControllerPath, true)
	pipeline := mergedValues(at(nvsentinelFaultQuarantineEnabledPath, true), at(nvsentinelNodeDrainerEnabledPath, true))
	// composed is nvsentinel-node-validation, a quarantine or remediation
	// step's pipeline and nvsentinel-maintenance-request, with values on top.
	composed := func(overrides ...map[string]any) map[string]any {
		return mergedValues(append([]map[string]any{nodeValidationValues(), pipeline, mrOn}, overrides...)...)
	}
	mrRule := func(expression string) map[string]any {
		return map[string]any{"kind": maintenanceRequestRuleKind, "expression": expression}
	}
	mrRuleSetWith := func(matchKey string, cordon any, rules ...any) map[string]any {
		ruleSet := map[string]any{"name": "MaintenanceRequest ruleset", "match": map[string]any{matchKey: rules}}
		if cordon != nil {
			ruleSet["cordon"] = cordon
		}
		return ruleSet
	}
	cordons := map[string]any{"shouldCordon": true}
	mrRuleSet := mrRuleSetWith("all", cordons, mrRule(maintenanceRequestRuleExpression))
	gpuRuleSet := map[string]any{"name": "GPU fatal error ruleset", "match": map[string]any{"all": []any{
		map[string]any{"kind": maintenanceRequestRuleKind, "expression": "event.agent == 'gpu-health-monitor'"},
	}}}
	withEnabled := func(ruleSet map[string]any, enabled any) map[string]any {
		out := map[string]any{"enabled": enabled}
		for k, v := range ruleSet {
			out[k] = v
		}
		return out
	}
	disabled := func(ruleSet map[string]any) map[string]any { return withEnabled(ruleSet, false) }
	nodeRule := func(expression string) map[string]any {
		return map[string]any{"kind": "Node", "expression": expression}
	}
	ruleSets := func(items ...any) map[string]any { return at(maintenanceRequestRuleSetsPath, items) }
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}

	const (
		wantNotBool    = maintenanceRequestControllerPath + ` is "true", not a boolean`
		wantNoLM       = "compose the " + nodeValidationMixin + " mixin, which turns lifecycle-manager on"
		wantNoFQ       = nvsentinelFaultQuarantineEnabledPath + " is off, so nothing cordons"
		wantNoDrainer  = nvsentinelNodeDrainerEnabledPath + " is off, so nothing drains"
		wantDryRun     = nvsentinelDryRunPath + " is true, so fault-quarantine only logs the cordon"
		wantDryRunStr  = nvsentinelDryRunPath + ` is "true", not a boolean`
		wantNoRuleSet  = maintenanceRequestRuleSetsPath + ` is set without the chart's "MaintenanceRequest ruleset"`
		wantEnabledStr = maintenanceRequestRuleSetsPath + `[1] (MaintenanceRequest ruleset) has enabled "false", not a boolean`
		wantEnabledNil = maintenanceRequestRuleSetsPath + `[1] (MaintenanceRequest ruleset) has enabled <nil>, not a boolean`
		wantGPUEnabled = maintenanceRequestRuleSetsPath + `[0] (GPU fatal error ruleset) has enabled "true", not a boolean`
		wantDynamic    = "--dynamic"
		wantNotBoolSet = maintenanceRequestControllerPath + ` is "yes", not a boolean`
	)

	tests := []struct {
		name     string
		sentinel map[string]any
		cfg      *config.Config
		wantErrs []string
	}{
		// Inert.
		{name: "controller left to the chart default, pipeline off", sentinel: nodeValidationValues()},
		{name: "controller off, pipeline off", sentinel: mergedValues(at(maintenanceRequestControllerPath, false))},
		{name: "nvsentinel disabled in the recipe", sentinel: mergedValues(mrOn, at("enabled", false))},
		{name: "nvsentinel disabled by --set", sentinel: mrOn,
			cfg: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {config.ComponentEnabledKey: "false"}}))},

		// The mixins as composed.
		{name: "maintenance requests on node validation and a step", sentinel: composed()},
		{name: "new-node validation turned off beside maintenance requests",
			sentinel: composed(at(nodeValidationNewNodePath, nil))},
		{name: "controller on by --set", sentinel: mergedValues(nodeValidationValues(), pipeline),
			cfg: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {maintenanceRequestControllerPath: "true"}}))},

		// The toggle.
		{name: "toggle a string", sentinel: at(maintenanceRequestControllerPath, "true"), wantErrs: []string{wantNotBool}},
		{name: "toggle a string by --set-string", sentinel: nodeValidationValues(),
			cfg: func() *config.Config {
				paths, err := config.ParseValueOverridesJSON([]string{`nv-sentinel:` + maintenanceRequestControllerPath + `="yes"`})
				if err != nil {
					t.Fatalf("ParseValueOverridesJSON: %v", err)
				}
				return config.NewConfig(config.WithValueOverridesTypedPaths(paths))
			}(), wantErrs: []string{wantNotBoolSet}},

		// What the controller needs.
		{name: "lifecycle-manager off", sentinel: composed(at(nodeValidationLifecycleEnabledPath, false)),
			wantErrs: []string{wantNoLM}},
		{name: "lifecycle-manager left to the chart default", sentinel: mergedValues(pipeline, mrOn),
			wantErrs: []string{wantNoLM}},
		{name: "fault-quarantine off", sentinel: composed(at(nvsentinelFaultQuarantineEnabledPath, false)),
			wantErrs: []string{wantNoFQ}},
		{name: "node-drainer off", sentinel: composed(at(nvsentinelNodeDrainerEnabledPath, false)),
			wantErrs: []string{wantNoDrainer}},
		{name: "controller alone", sentinel: mrOn, wantErrs: []string{wantNoLM, wantNoFQ, wantNoDrainer}},
		{name: "dry run, as the observe step sets", sentinel: composed(at(nvsentinelDryRunPath, true)),
			wantErrs: []string{wantDryRun}},
		{name: "dry run off", sentinel: composed(at(nvsentinelDryRunPath, false))},
		{name: "dry run a string", sentinel: composed(at(nvsentinelDryRunPath, "true")),
			wantErrs: []string{wantDryRunStr}},
		{name: "dry run null", sentinel: composed(at(nvsentinelDryRunPath, nil)),
			wantErrs: []string{nvsentinelDryRunPath + " is <nil>, not a boolean"}},

		// fault-quarantine.ruleSets.
		{name: "rule sets kept to the chart's", sentinel: composed()},
		{name: "rule sets restated with the maintenance one", sentinel: composed(ruleSets(gpuRuleSet, mrRuleSet))},
		{name: "rule sets restated without the maintenance one", sentinel: composed(ruleSets(gpuRuleSet)),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the maintenance rule set disabled", sentinel: composed(ruleSets(gpuRuleSet, disabled(mrRuleSet))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "rule sets null", sentinel: composed(at(maintenanceRequestRuleSetsPath, nil)),
			wantErrs: []string{wantNoRuleSet}},
		{name: "rule sets empty", sentinel: composed(ruleSets()), wantErrs: []string{wantNoRuleSet}},
		{name: "the maintenance rule under match.any",
			sentinel: composed(ruleSets(gpuRuleSet, mrRuleSetWith("any", cordons, mrRule(maintenanceRequestRuleExpression)))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the chart's rule set with its Node rule",
			sentinel: composed(ruleSets(gpuRuleSet, mrRuleSetWith("all", cordons, mrRule(maintenanceRequestRuleExpression),
				nodeRule(maintenanceRequestNodeRuleExpression))))},
		{name: "the Node rule folded across lines, as the chart's YAML writes it",
			sentinel: composed(ruleSets(mrRuleSetWith("all", cordons, mrRule(maintenanceRequestRuleExpression),
				nodeRule(strings.Replace(maintenanceRequestNodeRuleExpression, ") && ", ") &&\n  ", 1)))))},
		{name: "an extra false rule beside the chart's under match.all",
			sentinel: composed(ruleSets(mrRuleSetWith("all", cordons, mrRule(maintenanceRequestRuleExpression), mrRule("false")))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "a Node rule other than the chart's",
			sentinel: composed(ruleSets(mrRuleSetWith("all", cordons, mrRule(maintenanceRequestRuleExpression),
				nodeRule("node.metadata.name == 'x'")))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "a match.any beside the chart's match.all",
			sentinel: composed(ruleSets(map[string]any{"name": "MaintenanceRequest ruleset", "cordon": cordons,
				"match": map[string]any{"all": []any{mrRule(maintenanceRequestRuleExpression)}, "any": []any{mrRule("false")}}})),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the maintenance rule set enabled as a string",
			sentinel: composed(ruleSets(gpuRuleSet, withEnabled(mrRuleSet, "false"))),
			wantErrs: []string{wantEnabledStr, wantNoRuleSet}},
		{name: "the maintenance rule set enabled null",
			sentinel: composed(ruleSets(gpuRuleSet, withEnabled(mrRuleSet, nil))),
			wantErrs: []string{wantEnabledNil, wantNoRuleSet}},
		{name: "the maintenance rule set enabled true",
			sentinel: composed(ruleSets(gpuRuleSet, withEnabled(mrRuleSet, true)))},
		{name: "another rule set enabled as a string",
			sentinel: composed(ruleSets(withEnabled(gpuRuleSet, "true"), mrRuleSet)),
			wantErrs: []string{wantGPUEnabled}},
		{name: "the maintenance rule set does not cordon",
			sentinel: composed(ruleSets(mrRuleSetWith("all", map[string]any{"shouldCordon": false},
				mrRule(maintenanceRequestRuleExpression)))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the maintenance rule set has no cordon block",
			sentinel: composed(ruleSets(mrRuleSetWith("all", nil, mrRule(maintenanceRequestRuleExpression)))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "shouldCordon a string",
			sentinel: composed(ruleSets(mrRuleSetWith("all", map[string]any{"shouldCordon": "true"},
				mrRule(maintenanceRequestRuleExpression)))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the expression short-circuited to false",
			sentinel: composed(ruleSets(mrRuleSetWith("all", cordons, mrRule("false && "+maintenanceRequestRuleExpression)))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the expression only names the metadata key",
			sentinel: composed(ruleSets(mrRuleSetWith("all", cordons, mrRule("'"+maintenanceRequestMetadataKey+"' == ''")))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "the chart's expression under a Node rule",
			sentinel: composed(ruleSets(mrRuleSetWith("all", cordons,
				map[string]any{"kind": "Node", "expression": maintenanceRequestRuleExpression}))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "rule sets ignored with the controller off",
			sentinel: mergedValues(nodeValidationValues(), pipeline, ruleSets(gpuRuleSet))},

		// --dynamic.
		{name: "--dynamic controller toggle while off", sentinel: nodeValidationValues(),
			cfg: dynamic(maintenanceRequestControllerPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic controller toggle while on", sentinel: composed(),
			cfg: dynamic(maintenanceRequestControllerPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic lifecycle-manager toggle", sentinel: composed(),
			cfg: dynamic(nodeValidationLifecycleEnabledPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic fault-quarantine toggle", sentinel: composed(),
			cfg: dynamic(nvsentinelFaultQuarantineEnabledPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic node-drainer toggle", sentinel: composed(),
			cfg: dynamic(nvsentinelNodeDrainerEnabledPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic dry run", sentinel: composed(), cfg: dynamic(nvsentinelDryRunPath),
			wantErrs: []string{wantDynamic}},
		{name: "--dynamic rule sets", sentinel: composed(), cfg: dynamic(maintenanceRequestRuleSetsPath),
			wantErrs: []string{wantDynamic}},
		{name: "--dynamic fault-quarantine section", sentinel: composed(), cfg: dynamic("fault-quarantine"),
			wantErrs: []string{wantDynamic}},
		{name: "--dynamic rule sets while the controller is off", sentinel: nodeValidationValues(),
			cfg: dynamic(maintenanceRequestRuleSetsPath)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: nvsentinelComponent, Overrides: tt.sentinel},
			}}
			warnings, errs := CheckNVSentinelMaintenanceRequestCoherent(t.Context(), nvsentinelComponent, rr, cfg, nil)
			if len(warnings) != 0 {
				t.Errorf("got warnings %q, want none", warnings)
			}
			got := make([]string, 0, len(errs))
			for _, err := range errs {
				got = append(got, err.Error())
				errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			}
			assertDatastoreFindings(t, "error", got, tt.wantErrs)
		})
	}
}
