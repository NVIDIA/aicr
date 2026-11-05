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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Value paths CheckNVSentinelMaintenanceRequestCoherent reads under
// nvsentinel, the metadata key lifecycle-manager (NVSentinel v1.28.0) sets on
// every event a MaintenanceRequest publishes, and the rule kind and the
// HealthEvent and Node rule expressions of the chart's "MaintenanceRequest
// ruleset".
const (
	maintenanceRequestControllerPath = "lifecycle-manager.controllers.maintenanceRequest.enabled"
	maintenanceRequestRuleSetsPath   = "fault-quarantine.ruleSets"
	maintenanceRequestMetadataKey    = "maintenanceRequestName"
	maintenanceRequestRuleKind       = "HealthEvent"
	maintenanceRequestRuleExpression = "event.agent == 'lifecycle-manager' && event.metadata != null && " +
		"'" + maintenanceRequestMetadataKey + "' in event.metadata"
	maintenanceRequestNodeRuleExpression = `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && ` +
		`node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") && ` +
		`!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && ` +
		`node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")`
)

// CheckNVSentinelMaintenanceRequestCoherent blocks a bundle in which a
// MaintenanceRequest would be accepted but do nothing. lifecycle-manager's
// controller (lifecycle-manager.controllers.maintenanceRequest.enabled)
// publishes the request's health event, and NVSentinel's pipeline does the
// rest: fault-quarantine cordons the node and node-drainer drains it. Each
// piece renders on its own toggle and none checks the others, so the request
// is admitted and its HealthEventEmitted condition goes True while the node
// stays schedulable. It fails when the toggle is not a boolean, and, with the
// controller on, when:
//
//   - lifecycle-manager does not render (global.lifecycleManager.enabled),
//     so nothing runs the controller or its webhook, which is installed with
//     it;
//   - fault-quarantine or node-drainer does not render, so the node is not
//     cordoned or not drained. Their datastore is
//     CheckNVSentinelRemediationPipelineCoherent's to require;
//   - global.dryRun is true, so fault-quarantine only logs the cordon, or
//     not a boolean, so the binary's parser decides;
//   - fault-quarantine.ruleSets is set without a rule set shaped as the
//     chart's "MaintenanceRequest ruleset", so the event matches no rule set
//     that cordons and is stored without a cordon, or a rule set's enabled is
//     not a boolean. Unset, the chart's own rule sets include one.
//
// It rejects --dynamic on the controller toggle, and while the controller is
// on, on every other path it reads.
func CheckNVSentinelMaintenanceRequestCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	sentinelRef := recipeResult.GetComponentRef(componentName)
	if sentinelRef == nil {
		return nil, nil
	}
	sentinelKeys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(sentinelRef, bundlerConfig, sentinelKeys) {
		return nil, nil
	}
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, sentinelKeys,
		"NVSentinel MaintenanceRequest")
	if err != nil {
		return nil, []error{err}
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}

	enabled, wellFormed := nodeValidationBool(values, maintenanceRequestControllerPath, false)
	if !wellFormed {
		raw, _ := component.GetValueByPath(values, maintenanceRequestControllerPath)
		fail("%s is %#v, not a boolean: the chart reads it as a template condition, so whether the controller "+
			"runs would depend on how Helm coerces it", maintenanceRequestControllerPath, raw)
		return nil, errs
	}
	guarded := []string{maintenanceRequestControllerPath}
	if enabled {
		guarded = append(guarded, nodeValidationLifecycleEnabledPath, nvsentinelFaultQuarantineEnabledPath,
			nvsentinelNodeDrainerEnabledPath, nvsentinelDryRunPath, maintenanceRequestRuleSetsPath)
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, guarded,
		"decides whether a MaintenanceRequest cordons and drains its node"); len(msgs) > 0 {
		return nil, nodeValidationDynamicErrors(componentName, msgs)
	}
	if !enabled {
		return nil, nil
	}

	if !nvsentinelSubchartRenders(values, "lifecycleManager") {
		fail("%s is true, but %s is off, so nothing runs the MaintenanceRequest controller or its webhook -- "+
			"compose the %s mixin, which turns lifecycle-manager on", maintenanceRequestControllerPath,
			nodeValidationLifecycleEnabledPath, nodeValidationMixin)
	}
	for _, stage := range []struct{ key, path, does string }{
		{"faultQuarantine", nvsentinelFaultQuarantineEnabledPath, "cordons"},
		{"nodeDrainer", nvsentinelNodeDrainerEnabledPath, "drains"},
	} {
		if !nvsentinelSubchartRenders(values, stage.key) {
			fail("%s is true, but %s is off, so nothing %s the node a MaintenanceRequest names, though the request "+
				"is accepted -- compose a quarantine or remediation step mixin", maintenanceRequestControllerPath,
				stage.path, stage.does)
		}
	}
	dryRun, dryRunWellFormed := nvsentinelDryRun(values)
	if !dryRunWellFormed {
		raw, _ := component.GetValueByPath(values, nvsentinelDryRunPath)
		fail("%s is true, but %s is %#v, not a boolean: the chart passes it to fault-quarantine's --dry-run "+
			"unchecked, so whether a MaintenanceRequest cordons its node is left to the binary's parser -- set it "+
			"to true or false", maintenanceRequestControllerPath, nvsentinelDryRunPath, raw)
	}
	if dryRun {
		fail("%s is true, but %s is true, so fault-quarantine only logs the cordon a MaintenanceRequest asks for "+
			"-- compose a quarantine or remediation step instead of the observe step",
			maintenanceRequestControllerPath, nvsentinelDryRunPath)
	}
	if err := maintenanceRequestRuleSetViolations(ctx, values, fail); err != nil {
		return nil, append(errs, err)
	}
	return nil, errs
}

// maintenanceRequestRuleSetViolations fails when fault-quarantine.ruleSets is
// set but no rule set has the chart's "MaintenanceRequest ruleset" shape: an
// enabled field that is a boolean when set and not false, cordon.shouldCordon
// true, no match.any, and a match.all holding the chart's HealthEvent rule and
// nothing but, optionally, the chart's Node rule. The rule sets replace the
// chart's, so without one a request's event matches nothing that cordons.
// Expressions are compared with whitespace collapsed, not evaluated, so any
// other rule, even one that would still match, is rejected rather than
// trusted. It also fails on any rule set whose enabled is set to a
// non-boolean, which the chart writes unquoted into fault-quarantine's TOML.
func maintenanceRequestRuleSetViolations(ctx context.Context, values map[string]any, fail func(string, ...any)) error {
	raw, present := component.GetValueByPath(values, maintenanceRequestRuleSetsPath)
	if !present {
		return nil
	}
	ruleSets, _ := raw.([]any)
	found := false
	for i, item := range ruleSets {
		if err := ctx.Err(); err != nil {
			return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+maintenanceRequestRuleSetsPath)
		}
		ruleSet, _ := item.(map[string]any)
		if ruleSet == nil {
			continue
		}
		enabled, hasEnabled := ruleSet["enabled"]
		if _, isBool := enabled.(bool); hasEnabled && !isBool {
			fail("%s[%d] (%v) has enabled %#v, not a boolean: the chart writes it unquoted into fault-quarantine's "+
				"config, so it does not mean what it says", maintenanceRequestRuleSetsPath, i, ruleSet["name"], enabled)
			continue
		}
		if !found && enabled != false && maintenanceRequestRuleSetMatches(ruleSet) {
			found = true
		}
	}
	if !found {
		fail("%s is set without the chart's \"MaintenanceRequest ruleset\": an enabled rule set with "+
			"cordon.shouldCordon true, no match.any, and a match.all holding only the %s rule %q and, optionally, "+
			"the chart's Node rule, so a MaintenanceRequest's event matches no rule set that cordons and is stored "+
			"without a cordon -- keep that rule set in the list as the chart ships it",
			maintenanceRequestRuleSetsPath, maintenanceRequestRuleKind, maintenanceRequestRuleExpression)
	}
	return nil
}

// maintenanceRequestRuleSetMatches reports whether ruleSet cordons and matches
// exactly the chart's MaintenanceRequest rules.
func maintenanceRequestRuleSetMatches(ruleSet map[string]any) bool {
	cordon, _ := ruleSet["cordon"].(map[string]any)
	if cordon["shouldCordon"] != true {
		return false
	}
	match, _ := ruleSet["match"].(map[string]any)
	if anyRules, _ := match["any"].([]any); len(anyRules) > 0 {
		return false
	}
	rules, _ := match["all"].([]any)
	healthEventRule := false
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		expression, _ := rule["expression"].(string)
		expression = strings.Join(strings.Fields(expression), " ")
		switch {
		case rule["kind"] == maintenanceRequestRuleKind && expression == maintenanceRequestRuleExpression:
			healthEventRule = true
		case rule["kind"] == "Node" && expression == maintenanceRequestNodeRuleExpression:
		default:
			return false
		}
	}
	return healthEventRule
}
