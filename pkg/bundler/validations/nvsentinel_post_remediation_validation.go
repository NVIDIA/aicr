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
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Value paths CheckNVSentinelPostRemediationValidationCoherent reads under
// nvsentinel, the chart default (NVSentinel v1.28.0) both fault-quarantine and
// lifecycle-manager write their cordon labels under, and the only rule kind a
// validation rule set evaluates.
const (
	postRemediationValidationPath    = "fault-quarantine.validation"
	postRemediationValidationEnabled = "fault-quarantine.validation.enabled"
	postRemediationRuleSetsPath      = "fault-quarantine.validation.ruleSets"
	postRemediationFQLabelPrefixPath = "fault-quarantine.labelPrefix"
	postRemediationSchedulingGate    = "lifecycle-manager.config.schedulingGate"
	postRemediationCordonPath        = "lifecycle-manager.config.schedulingGate.cordon"
	postRemediationLMLabelPrefixPath = "lifecycle-manager.config.schedulingGate.cordon.labelPrefix"
	postRemediationCordonRemovePath  = "lifecycle-manager.config.schedulingGate.cordon.remove"
	postRemediationReadinessPath     = "lifecycle-manager.config.readinessCriteria"
	postRemediationQuarantineRules   = "fault-quarantine.ruleSets"
	postRemediationReleaseTaintsPath = "lifecycle-manager.config.schedulingGate.taints"
	postRemediationChartLabelPrefix  = "k8saas.nvidia.com/"
	postRemediationRuleKind          = "HealthEvent"
	postRemediationNVCRETaintValue   = "true"
)

// postRemediationChartQuarantineTaints are the taints the chart's own
// fault-quarantine rule sets apply (NVSentinel v1.28.0: the NVCRE
// certification rule set's), used when AICR's values leave ruleSets unset.
// TestNVSentinelPostRemediationValidationChartRender pins them to the chart.
var postRemediationChartQuarantineTaints = []postRemediationTaint{
	{key: "nvsentinel.dgxc.nvidia.com/nvcre-cert-failed", value: postRemediationNVCRETaintValue, effect: "NoSchedule"},
}

// postRemediationTaint is a taint as fault-quarantine applies it and
// lifecycle-manager matches it: on key, value and effect together.
type postRemediationTaint struct{ key, value, effect string }

func (t postRemediationTaint) String() string { return t.key + "=" + t.value + ":" + t.effect }

// CheckNVSentinelPostRemediationValidationCoherent blocks a bundle in which
// post-remediation validation (fault-quarantine.validation) would hold a
// repaired node cordoned with nothing to release it, or release it untested.
// It fails when fault-quarantine.validation is set to anything but a map,
// which the chart cannot render, or its enabled is not a boolean. When
// validation is on and fault-quarantine renders, it also fails when:
//
//   - lifecycle-manager or its ValidationRequest controller does not run:
//     fault-quarantine hands the node's cordon to a ValidationRequest nothing
//     reconciles;
//   - node-drainer does not run: fault-quarantine validates only a node whose
//     drain completed, so every node is released untested;
//   - lifecycle-manager.config.schedulingGate or its cordon is set to
//     anything but a map, which drops the chart's release settings, or
//     cordon.remove is set to anything but true, so a node that passes is
//     never uncordoned;
//   - lifecycle-manager.config.readinessCriteria is set without one named
//     not-under-quarantine with the chart's expression: a node quarantined
//     again while its test runs would pass, be uncordoned, and have the new
//     quarantine canceled;
//   - ruleSets is null, empty, not a list or has no enabled rule set, so
//     fault-quarantine creates no ValidationRequest and a repaired node
//     returns untested; two rule sets, disabled ones included, share a name,
//     since fault-quarantine looks a matched rule set's tests up by name and
//     the last wins; a rule set's enabled is not a boolean; an enabled rule
//     set's match has no rule, so it matches no event, or a rule that is not
//     a HealthEvent expression; or an enabled rule set names no test, or a
//     test lifecycle-manager's config does not define, which its admission
//     webhook rejects, leaving the node quarantined;
//   - a rule set requests dcgm-diag-test with the chart's command and no
//     enabled GPU Operator in gpu-operator runs dcgm.enabled: true;
//   - either cordon label prefix is set to anything but a non-empty string,
//     or fault-quarantine and lifecycle-manager write their cordon labels
//     under different prefixes, so lifecycle-manager's release leaves
//     fault-quarantine's labels on the node;
//   - an enabled fault-quarantine rule set, the chart's own when ruleSets is
//     unset, applies a NoSchedule or NoExecute taint that
//     lifecycle-manager.config.schedulingGate.taints does not list with
//     remove: true: fault-quarantine keeps its taints on the hand-over and
//     lifecycle-manager lifts only the listed ones, so a node that passes is
//     uncordoned but stays closed to work.
//
// It rejects --dynamic on fault-quarantine.validation.enabled, on
// fault-quarantine.validation, fault-quarantine.labelPrefix and
// fault-quarantine.ruleSets while validation is on, and on the GPU Operator's
// enabled and dcgm.enabled while the DCGM rule applies.
func CheckNVSentinelPostRemediationValidationCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
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
		"NVSentinel post-remediation validation")
	if err != nil {
		return nil, []error{err}
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}

	if section, present := component.GetValueByPath(values, postRemediationValidationPath); present {
		if _, isMap := section.(map[string]any); !isMap {
			fail("%s is %#v, not a map: the chart reads %s.enabled from it whenever fault-quarantine renders, so the "+
				"bundle fails to render -- remove it or restate the section the nvsentinel-remediation mixin sets",
				postRemediationValidationPath, section, postRemediationValidationPath)
			return nil, errs
		}
	}
	enabled, wellFormed := nodeValidationBool(values, postRemediationValidationEnabled, false)
	if !wellFormed {
		raw, _ := component.GetValueByPath(values, postRemediationValidationEnabled)
		fail("%s is %#v, not a boolean: the chart reads it as a template condition, so whether a repaired node is "+
			"validated would depend on how Helm coerces it", postRemediationValidationEnabled, raw)
		return nil, errs
	}
	guarded := []string{postRemediationValidationEnabled}
	if enabled {
		guarded = append(guarded, postRemediationValidationPath, postRemediationFQLabelPrefixPath,
			postRemediationQuarantineRules)
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, guarded,
		"decides whether a repaired node is validated before it returns to work"); len(msgs) > 0 {
		return nil, nodeValidationDynamicErrors(componentName, msgs)
	}
	if !enabled || !nvsentinelSubchartRenders(values, "faultQuarantine") {
		return nil, nil
	}

	controller, controllerWellFormed := nodeValidationBool(values, nodeValidationControllerPath, true)
	if !nvsentinelSubchartRenders(values, "lifecycleManager") || !controllerWellFormed || !controller {
		fail("%s is true, so fault-quarantine keeps a repaired node cordoned until a ValidationRequest passes, but "+
			"lifecycle-manager's ValidationRequest controller does not run (%s is off, or %s is not true), so the "+
			"node never returns to service -- compose the %s mixin", postRemediationValidationEnabled,
			nodeValidationLifecycleEnabledPath, nodeValidationControllerPath, nodeValidationMixin)
		return nil, errs
	}
	if !nvsentinelSubchartRenders(values, "nodeDrainer") {
		fail("%s is true, but global.nodeDrainer.enabled is off: fault-quarantine validates only a node whose drain "+
			"completed, so with no drain every node returns to service untested -- turn node-drainer on, as every "+
			"remediation step does, or turn validation off", postRemediationValidationEnabled)
	}
	postRemediationSchedulingGateViolations(values, fail)
	if err = postRemediationReadinessViolations(ctx, values, fail); err != nil {
		return nil, append(errs, err)
	}
	postRemediationLabelPrefixViolations(values, fail)
	if err = postRemediationTaintViolations(ctx, values, fail); err != nil {
		return nil, append(errs, err)
	}

	defined, testsWellFormed, err := nodeValidationDefinedTests(ctx, values, func(string, ...any) {})
	if err != nil {
		return nil, append(errs, err)
	}
	tests, err := postRemediationRuleSetTests(ctx, values, fail)
	if err != nil {
		return nil, append(errs, err)
	}
	for _, test := range tests {
		if testsWellFormed && !defined[test] {
			fail("%s requests the test %q, which lifecycle-manager's config does not define, so its admission "+
				"webhook rejects the ValidationRequest and the repaired node stays quarantined -- define it under "+
				"%s.%s or remove it", postRemediationRuleSetsPath, test, nodeValidationTestsPath, test)
		}
	}
	commandPath := nodeValidationTestsPath + "." + nodeValidationDCGMTest + ".command"
	if command, _ := component.GetValueByPath(values, commandPath); command == nil && defined[nodeValidationDCGMTest] &&
		slices.Contains(tests, nodeValidationDCGMTest) {

		// Prefixed so it reads apart from CheckNVSentinelNodeValidationCoherent's
		// finding on the same hostengine.
		dcgmFail := func(format string, args ...any) {
			fail("post-reboot validation: "+format, args...)
		}
		dynMsgs, dcgmErr := nodeValidationDCGMViolations(ctx, recipeResult, bundlerConfig, dcgmFail)
		if dcgmErr != nil {
			return nil, append(errs, dcgmErr)
		}
		errs = append(errs, nodeValidationDynamicErrors(componentName, dynMsgs)...)
	}
	return nil, errs
}

// postRemediationSchedulingGateViolations fails when lifecycle-manager would
// not release a node that passes: a schedulingGate or cordon that is not a
// map, null included, drops the chart's settings, and lifecycle-manager then
// neither uncordons the node nor removes the cordon labels; and a
// cordon.remove that is not true.
func postRemediationSchedulingGateViolations(values map[string]any, fail func(string, ...any)) {
	for _, path := range []string{postRemediationSchedulingGate, postRemediationCordonPath} {
		raw, present := component.GetValueByPath(values, path)
		if _, isMap := raw.(map[string]any); present && !isMap {
			fail("%s is %#v, not a map: Helm drops the chart's cordon settings, so lifecycle-manager does not "+
				"uncordon a node that passes and every repaired node stays cordoned -- remove it", path, raw)
			return
		}
	}
	if remove, ok := nodeValidationBool(values, postRemediationCordonRemovePath, true); !ok || !remove {
		raw, _ := component.GetValueByPath(values, postRemediationCordonRemovePath)
		fail("%s is %#v: lifecycle-manager does not uncordon a node that passes, so every repaired node stays "+
			"cordoned -- leave it at the chart's true", postRemediationCordonRemovePath, raw)
	}
}

// postRemediationReadinessViolations fails unless lifecycle-manager's
// readiness criteria keep not-under-quarantine. lifecycle-manager re-checks
// readiness while a test runs and fails the attempt on a violation; without
// the criterion, a node fault-quarantine quarantines again mid-test passes
// and is uncordoned, which fault-quarantine reads as a manual uncordon that
// cancels the new quarantine. The chart's own criteria, used when the list is
// absent, include it.
func postRemediationReadinessViolations(ctx context.Context, values map[string]any, fail func(string, ...any)) error {
	raw, present := component.GetValueByPath(values, postRemediationReadinessPath)
	if !present {
		return nil
	}
	const why = "lifecycle-manager re-checks readiness while a test runs, so without it a node quarantined again " +
		"mid-test passes, is uncordoned, and the new quarantine is canceled"
	criteria, isList := raw.([]any)
	if !isList {
		fail("%s is %#v, not a list of criteria that keeps %q: %s -- keep the chart's criterion",
			postRemediationReadinessPath, raw, nodeValidationQuarantineCriterion, why)
		return nil
	}
	for _, item := range criteria {
		if err := ctx.Err(); err != nil {
			return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+postRemediationReadinessPath)
		}
		criterion, _ := item.(map[string]any)
		name, _ := criterion["name"].(string)
		expression, _ := criterion["expression"].(string)
		if name == nodeValidationQuarantineCriterion &&
			strings.Join(strings.Fields(expression), " ") == nodeValidationQuarantineExpression {

			return nil
		}
	}
	fail("%s does not include a criterion named %q with the expression %s: %s -- keep the chart's criterion",
		postRemediationReadinessPath, nodeValidationQuarantineCriterion, nodeValidationQuarantineExpression, why)
	return nil
}

// postRemediationTaintViolations fails on each NoSchedule or NoExecute taint
// an enabled fault-quarantine rule set applies that
// lifecycle-manager.config.schedulingGate.taints does not list with remove:
// true. fault-quarantine keeps its taints when it hands a node to validation,
// and lifecycle-manager lifts only the listed ones, so the node would pass,
// be uncordoned and stay closed to work.
func postRemediationTaintViolations(ctx context.Context, values map[string]any, fail func(string, ...any)) error {
	type applied struct {
		source string
		taint  postRemediationTaint
	}
	var required []applied
	raw, present := component.GetValueByPath(values, postRemediationQuarantineRules)
	if !present {
		for _, taint := range postRemediationChartQuarantineTaints {
			required = append(required, applied{"the chart's default " + postRemediationQuarantineRules, taint})
		}
	}
	ruleSets, _ := raw.([]any)
	for i, item := range ruleSets {
		if err := ctx.Err(); err != nil {
			return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+postRemediationQuarantineRules)
		}
		ruleSet, _ := item.(map[string]any)
		taint, isMap := ruleSet["taint"].(map[string]any)
		if !isMap || ruleSet["enabled"] == false {
			continue
		}
		t := postRemediationTaint{
			key:    postRemediationQuoted(taint["key"]),
			value:  postRemediationQuoted(taint["value"]),
			effect: postRemediationQuoted(taint["effect"]),
		}
		if t.effect == "NoSchedule" || t.effect == "NoExecute" {
			required = append(required, applied{fmt.Sprintf("%s[%d]", postRemediationQuarantineRules, i), t})
		}
	}
	if len(required) == 0 {
		return nil
	}

	releases, _ := component.GetValueByPath(values, postRemediationReleaseTaintsPath)
	entries, _ := releases.([]any)
	for _, req := range required {
		released := false
		for _, item := range entries {
			if err := ctx.Err(); err != nil {
				return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+postRemediationReleaseTaintsPath)
			}
			entry, _ := item.(map[string]any)
			key, _ := entry["key"].(string)
			value, _ := entry["value"].(string)
			effect, _ := entry["effect"].(string)
			if entry["remove"] == true && (postRemediationTaint{key, value, effect}) == req.taint {
				released = true
				break
			}
		}
		if !released {
			fail("%s applies the taint %s, which fault-quarantine keeps when it hands a node to validation, but %s "+
				"does not list it with remove: true, so lifecycle-manager uncordons a node that passes and leaves it "+
				"tainted, closed to work -- add {key: %q, value: %q, effect: %q, remove: true}", req.source, req.taint,
				postRemediationReleaseTaintsPath, req.taint.key, req.taint.value, req.taint.effect)
		}
	}
	return nil
}

// postRemediationRuleSetTests returns the tests post-remediation validation
// can request: the chart's reboot rule set's dcgm-diag-test when ruleSets is
// absent. It fails on a rule set list that requests none.
func postRemediationRuleSetTests(ctx context.Context, values map[string]any, fail func(string, ...any)) ([]string, error) {
	raw, present := component.GetValueByPath(values, postRemediationRuleSetsPath)
	if !present {
		return []string{nodeValidationDCGMTest}, nil
	}
	ruleSets, isList := raw.([]any)
	if !isList || len(ruleSets) == 0 {
		fail("%s is %#v: fault-quarantine creates no ValidationRequest without a rule set, so a repaired node "+
			"returns to service untested -- restate the rule sets the nvsentinel-remediation mixin sets",
			postRemediationRuleSetsPath, raw)
		return nil, nil
	}
	seen := map[string]bool{}
	names := map[string]int{}
	var tests []string
	active := 0
	for i, item := range ruleSets {
		if err := ctx.Err(); err != nil {
			return nil, aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+postRemediationRuleSetsPath)
		}
		path := fmt.Sprintf("%s[%d]", postRemediationRuleSetsPath, i)
		ruleSet, isMap := item.(map[string]any)
		if !isMap {
			fail("%s is %#v, not a rule set", path, item)
			continue
		}
		name := postRemediationQuoted(ruleSet["name"])
		if first, dup := names[name]; dup {
			fail("%s has the name %q, as %s[%d] does: fault-quarantine looks a matched rule set's tests up by name, "+
				"disabled rule sets included, so the later one's tests replace the earlier one's -- give each rule "+
				"set its own name", path, name, postRemediationRuleSetsPath, first)
		} else {
			names[name] = i
		}
		enabled := true
		if raw, present := ruleSet["enabled"]; present {
			isBool := false
			if enabled, isBool = raw.(bool); !isBool {
				fail("%s.enabled is %#v, not a boolean: the chart writes it into fault-quarantine's config unquoted, "+
					"so whether the rule set applies would depend on how it parses", path, raw)
				continue
			}
		}
		if !enabled {
			continue
		}
		active++
		if err := postRemediationMatchViolations(ctx, ruleSet["match"], path, fail); err != nil {
			return nil, err
		}
		testNames, _ := ruleSet["tests"].([]any)
		if len(testNames) == 0 {
			fail("%s names no tests, so a node it matches is released untested", path)
		}
		for j, item := range testNames {
			if err := ctx.Err(); err != nil {
				return nil, aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+path+".tests")
			}
			test, isString := item.(string)
			if !isString || test == "" {
				fail("%s.tests[%d] is %#v, not a test name", path, j, item)
				continue
			}
			if !seen[test] {
				seen[test] = true
				tests = append(tests, test)
			}
		}
	}
	if active == 0 {
		fail("%s has no enabled rule set, so fault-quarantine requests no test and a repaired node returns to "+
			"service untested", postRemediationRuleSetsPath)
	}
	return tests, nil
}

// postRemediationQuoted returns what fault-quarantine reads for a value the
// chart quotes into its config, such as a rule set's name or taint: an absent
// or null value is the empty string, and any other value its printed form.
func postRemediationQuoted(raw any) string {
	if raw == nil {
		return ""
	}
	return fmt.Sprint(raw)
}

// postRemediationMatchViolations fails on an enabled rule set's match that
// fault-quarantine (NVSentinel v1.28.0) cannot use: one with no all or any
// rule builds no evaluator and matches no event, and a rule of any kind but
// HealthEvent stops fault-quarantine at startup, since validation rule sets
// get no Node informer. A null all or any is skipped, as the chart does.
func postRemediationMatchViolations(ctx context.Context, raw any, path string, fail func(string, ...any)) error {
	match, isMap := raw.(map[string]any)
	if !isMap {
		fail("%s.match is %#v, not a map of all and any rules: the chart reads match.all and match.any from it, so "+
			"the bundle fails to render", path, raw)
		return nil
	}
	rules := 0
	for _, key := range []string{"all", "any"} {
		value := match[key]
		if value == nil {
			continue
		}
		list, isList := value.([]any)
		if !isList {
			fail("%s.match.%s is %#v, not a list of rules", path, key, value)
			continue
		}
		for i, item := range list {
			if err := ctx.Err(); err != nil {
				return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+path+".match")
			}
			rules++
			rule, isRule := item.(map[string]any)
			if !isRule {
				fail("%s.match.%s[%d] is %#v, not a rule", path, key, i, item)
				continue
			}
			if kind := rule["kind"]; kind != postRemediationRuleKind {
				fail("%s.match.%s[%d].kind is %#v: a validation rule set evaluates only %s rules, and any other kind "+
					"stops fault-quarantine at startup", path, key, i, kind, postRemediationRuleKind)
			}
			if expression, isString := rule["expression"].(string); !isString || expression == "" {
				fail("%s.match.%s[%d].expression is %#v, not an expression", path, key, i, rule["expression"])
			}
		}
	}
	if rules == 0 {
		fail("%s.match has no all or any rule, so fault-quarantine builds no evaluator for it and it matches no "+
			"event: a node it should hold is released untested", path)
	}
	return nil
}

// postRemediationLabelPrefixViolations fails when fault-quarantine and
// lifecycle-manager write the cordon labels under different prefixes:
// lifecycle-manager removes only its own on release.
func postRemediationLabelPrefixViolations(values map[string]any, fail func(string, ...any)) {
	prefix := func(path, why string) (string, bool) {
		raw, present := component.GetValueByPath(values, path)
		if !present {
			return postRemediationChartLabelPrefix, true
		}
		v, isString := raw.(string)
		if !isString || v == "" {
			fail("%s is %#v, not a non-empty label prefix: %s", path, raw, why)
			return "", false
		}
		return v, true
	}
	fq, fqOK := prefix(postRemediationFQLabelPrefixPath, "fault-quarantine's chart quotes it into its config, so "+
		"null leaves the prefix empty and any other value becomes a prefix lifecycle-manager does not share")
	lm, lmOK := prefix(postRemediationLMLabelPrefixPath, "lifecycle-manager's chart writes it into its config "+
		"as YAML, so null leaves the prefix empty and any other value is a config lifecycle-manager cannot decode")
	if fqOK && lmOK && fq != lm {
		fail("%s is %q but %s is %q: lifecycle-manager releases a repaired node and removes only the cordon labels "+
			"under its own prefix, so fault-quarantine's stay on the node", postRemediationFQLabelPrefixPath, fq,
			postRemediationLMLabelPrefixPath, lm)
	}
}
