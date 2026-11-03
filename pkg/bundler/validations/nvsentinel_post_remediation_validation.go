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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"
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
	postRemediationTemplatesPath     = "lifecycle-manager.config.templates"
	postRemediationNVCREMonitorPath  = "global.nvcreCertificationMonitor.enabled"
	postRemediationProbe             = "probe"
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
//   - a lifecycle-manager.config.schedulingGate.taints entry's value is not
//     a string in the tolerations the requested tests' Job templates render,
//     which breaks every test Job; or an enabled fault-quarantine rule set
//     applies a NoSchedule or NoExecute taint that list does not release with
//     remove: true, so a node that passes is uncordoned but stays closed to
//     work. With ruleSets unset, the chart's NVCRE certification taint counts
//     while its monitor runs.
//
// It rejects --dynamic on fault-quarantine.validation.enabled, on
// fault-quarantine.validation, fault-quarantine.labelPrefix and
// fault-quarantine.ruleSets while validation is on, on
// global.nvcreCertificationMonitor.enabled while validation is on and the
// chart's default fault-quarantine rule sets apply, and on the GPU Operator's
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
		// The chart's default rule sets apply the NVCRE taint only while its
		// monitor runs, so the toggle decides which taints must be released.
		if _, present := component.GetValueByPath(values, postRemediationQuarantineRules); !present {
			guarded = append(guarded, postRemediationNVCREMonitorPath)
		}
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

	defined, testsWellFormed, err := nodeValidationDefinedTests(ctx, values, func(string, ...any) {})
	if err != nil {
		return nil, append(errs, err)
	}
	tests, err := postRemediationRuleSetTests(ctx, values, fail)
	if err != nil {
		return nil, append(errs, err)
	}
	if err = postRemediationTaintViolations(ctx, values, tests, fail); err != nil {
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

// postRemediationTaintViolations fails on two ways the release of a validated
// node can go wrong. Each schedulingGate.taints entry's value must stay a
// string in the tolerations of every test Job the requested tests render: the
// chart's Job template (NVSentinel v1.28.0) writes it unquoted, so "true" or
// "1" turns into a bool or number, the API server rejects the Job, and every
// validation fails. A template AICR's values override is rendered instead. And
// each NoSchedule or NoExecute taint an enabled fault-quarantine rule set
// applies must be listed there with remove: true, since fault-quarantine keeps
// its taints on the hand-over and lifecycle-manager lifts only the listed
// ones. The chart's own rule sets apply only the NVCRE certification taint,
// and only while its monitor runs.
func postRemediationTaintViolations(ctx context.Context, values map[string]any, tests []string, fail func(string, ...any)) error {
	type release struct {
		taint  postRemediationTaint
		remove bool
	}
	raw, _ := component.GetValueByPath(values, postRemediationReleaseTaintsPath)
	items, _ := raw.([]any)
	releases := make([]release, 0, len(items))
	taints := make([]postRemediationTaint, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "checking "+postRemediationReleaseTaintsPath)
		}
		entry, _ := item.(map[string]any)
		key, _ := entry["key"].(string)
		effect, _ := entry["effect"].(string)
		taint := postRemediationTaint{key, postRemediationQuoted(entry["value"]), effect}
		releases = append(releases, release{taint, entry["remove"] == true})
		taints = append(taints, taint)
	}
	renders, chartTemplate := postRemediationJobRenders(values, tests)
	broken, err := postRemediationBrokenTolerations(ctx, renders, chartTemplate, taints)
	if err != nil {
		return err
	}
	for i, taint := range taints {
		if broken[i] {
			fail("%s[%d].value is %q, which lifecycle-manager's Job template writes unquoted into every test Job's "+
				"tolerations, where it is not a string, so the API server rejects every test Job, every validation "+
				"fails and every validated node stays cordoned", postRemediationReleaseTaintsPath, i, taint.value)
		}
	}

	type applied struct {
		source string
		taint  postRemediationTaint
	}
	var required []applied
	rules, present := component.GetValueByPath(values, postRemediationQuarantineRules)
	if !present && nvsentinelSubchartRenders(values, "nvcreCertificationMonitor") {
		for _, taint := range postRemediationChartQuarantineTaints {
			required = append(required, applied{"the chart's default " + postRemediationQuarantineRules +
				", with global.nvcreCertificationMonitor on,", taint})
		}
	}
	ruleSets, _ := rules.([]any)
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

	for _, req := range required {
		if slices.Contains(releases, release{req.taint, true}) {
			continue
		}
		fix := fmt.Sprintf("add {key: %q, value: %q, effect: %q, remove: true}", req.taint.key, req.taint.value,
			req.taint.effect)
		// Rendered beside the entries already listed, as lifecycle-manager would
		// render it once added.
		withIt := append(slices.Clone(taints), req.taint)
		broken, err := postRemediationBrokenTolerations(ctx, renders, chartTemplate, withIt)
		if err != nil {
			return err
		}
		if broken[len(withIt)-1] {
			fix = fmt.Sprintf("no entry can release it, since its value %q would break every test Job -- change the "+
				"taint's value, or turn the taint or validation off", req.taint.value)
		}
		fail("%s applies the taint %s, which fault-quarantine keeps when it hands a node to validation, but %s "+
			"does not list it with remove: true, so lifecycle-manager uncordons a node that passes and leaves it "+
			"tainted, closed to work -- %s", req.source, req.taint, postRemediationReleaseTaintsPath, fix)
	}
	return nil
}

// postRemediationUnquotedString reports whether value stays the same string
// when written unquoted as a YAML value and decoded as lifecycle-manager
// decodes its rendered Job. An empty value decodes to null, which a string
// field reads as empty.
func postRemediationUnquotedString(value string) bool {
	if value == "" {
		return true
	}
	var doc map[string]any
	if err := sigsyaml.Unmarshal([]byte("value: "+value), &doc); err != nil {
		return false
	}
	decoded, isString := doc["value"].(string)
	return isString && decoded == value
}

// postRemediationJobRender is one test Job rendering lifecycle-manager would
// perform: a test, the template AICR's values give its provider, and the
// fields of the test's context that come from AICR's values.
type postRemediationJobRender struct {
	file, text, test, image     string
	command                     []string
	env                         []corev1.EnvVar
	timeoutSeconds              int64
	bandwidthGBps, goodputRatio *string
}

// postRemediationJobRenders returns a rendering for each requested test whose
// template AICR's values override: the test's provider (the chart's
// k8s-job-provider for dcgm-diag-test), that provider's templateFile, and the
// entry of that name under config.templates. chartTemplate reports that a test
// renders through the chart's own template, or that no test resolves to one.
// A test whose provider or template does not resolve is skipped; the other
// checks report it.
func postRemediationJobRenders(values map[string]any, tests []string) (renders []postRemediationJobRender, chartTemplate bool) {
	mapAt := func(path string) map[string]any {
		raw, _ := component.GetValueByPath(values, path)
		m, _ := raw.(map[string]any)
		return m
	}
	testDefs, providers, overrides := mapAt(nodeValidationTestsPath), mapAt(nodeValidationProvidersPath),
		mapAt(postRemediationTemplatesPath)
	chartFile, _ := nodeValidationChartProviderDefaults["templateFile"].(string)
	chartTimeout, _ := nodeValidationChartProviderDefaults["timeoutSeconds"].(int)
	optional := func(raw any) *string {
		if raw == nil {
			return nil
		}
		s := fmt.Sprint(raw)
		return &s
	}
	for _, test := range tests {
		def, _ := testDefs[test].(map[string]any)
		provider, _ := def["provider"].(string)
		if _, set := def["provider"]; !set && test == nodeValidationDCGMTest {
			provider = nodeValidationChartProvider
		}
		providerDef, _ := providers[provider].(map[string]any)
		file, _ := providerDef["templateFile"].(string)
		if _, set := providerDef["templateFile"]; !set && provider == nodeValidationChartProvider {
			file = chartFile
		}
		text, overridden := overrides[file].(string)
		if !overridden {
			chartTemplate = chartTemplate || (file == chartFile && file != "")
			continue
		}
		render := postRemediationJobRender{file: file, text: text, test: test,
			bandwidthGBps: optional(def["bandwidthGBps"]), goodputRatio: optional(def["goodputRatio"])}
		render.image, _ = def["image"].(string)
		for _, arg := range asSlice(def["command"]) {
			if s, isString := arg.(string); isString {
				render.command = append(render.command, s)
			}
		}
		if raw, err := json.Marshal(def["env"]); err == nil {
			_ = json.Unmarshal(raw, &render.env)
		}
		switch t := providerDef["timeoutSeconds"].(type) {
		case int:
			render.timeoutSeconds = int64(t)
		case int64:
			render.timeoutSeconds = t
		case float64:
			render.timeoutSeconds = int64(t)
		default:
			if provider == nodeValidationChartProvider {
				render.timeoutSeconds = int64(chartTimeout)
			}
		}
		renders = append(renders, render)
	}
	if len(renders) == 0 {
		chartTemplate = true
	}
	return renders, chartTemplate
}

// asSlice returns raw as a list, or nil when it is not one.
func asSlice(raw any) []any {
	list, _ := raw.([]any)
	return list
}

// postRemediationBrokenTolerations reports, for each taint, whether a test
// Job lifecycle-manager renders for it carries its toleration value as
// something other than a string, which the API server rejects. Every
// rendering gets all the taints at once, as lifecycle-manager passes them, so
// a template that treats one key differently is judged on that key. The
// chart's template, and an overridden one that cannot be evaluated within
// postRemediationRender's limits, are judged by the chart's unquoted write.
func postRemediationBrokenTolerations(ctx context.Context, renders []postRemediationJobRender, chartTemplate bool, taints []postRemediationTaint) ([]bool, error) {
	broken := make([]bool, len(taints))
	unquoted := func() {
		for i, taint := range taints {
			broken[i] = broken[i] || !postRemediationUnquotedString(taint.value)
		}
	}
	if chartTemplate {
		unquoted()
	}
	for _, render := range renders {
		rendered, evaluated, err := postRemediationRender(ctx, render, taints)
		if err != nil {
			return nil, err
		}
		if !evaluated {
			unquoted()
			continue
		}
		badKeys := map[string]bool{}
		var walk func(any)
		walk = func(node any) {
			switch n := node.(type) {
			case map[string]any:
				if key, isString := n["key"].(string); isString {
					if v := n["value"]; v != nil {
						if _, isString := v.(string); !isString {
							badKeys[key] = true
						}
					}
				}
				for _, child := range n {
					walk(child)
				}
			case []any:
				for _, child := range n {
					walk(child)
				}
			}
		}
		walk(rendered)
		for i, taint := range taints {
			broken[i] = broken[i] || badKeys[taint.key]
		}
	}
	return broken, nil
}

// postRemediationRender renders one test Job the way lifecycle-manager
// (NVSentinel v1.28.0, internal/controller/validationrequest_provider.go)
// does, with the taints as tolerations after its fixed unschedulable one, and
// decodes the result. Placeholders stand in for the fields that come from the
// ValidationRequest. evaluated is false, so the caller falls back to the
// chart's unquoted write, when the template is larger than
// defaults.MaxNVSentinelJobTemplateBytes, does not parse, falls outside
// postRemediationTemplateSteps, runs past
// defaults.NVSentinelJobTemplateRenderTimeout, writes or has its functions
// produce more than defaults.MaxNVSentinelJobTemplateBytes, fails or does not
// decode. err is set only when ctx ends.
//
// The work is bounded three ways: postRemediationTemplateSteps caps what the
// template evaluates before it runs, the functions charge what they produce to
// a budget, and the functions and the writer stop the execution once the
// render's deadline passes, so an abandoned execution ends at its next call or
// write and evaluates at most the step budget in between.
func postRemediationRender(ctx context.Context, render postRemediationJobRender, taints []postRemediationTaint) (rendered any, evaluated bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "rendering "+render.file)
	}
	if len(render.text) > defaults.MaxNVSentinelJobTemplateBytes {
		return nil, false, nil
	}
	renderCtx, cancel := context.WithTimeout(ctx, defaults.NVSentinelJobTemplateRenderTimeout)
	defer cancel()
	budget := &postRemediationBudget{ctx: renderCtx, remaining: defaults.MaxNVSentinelJobTemplateBytes}
	funcs := template.FuncMap{
		"join": func(elems []string, sep string) (string, error) {
			size := len(sep) * max(len(elems)-1, 0)
			for _, e := range elems {
				size += len(e)
			}
			if err := budget.charge(size); err != nil {
				return "", err
			}
			return strings.Join(elems, sep), nil
		},
		"quote": func(s string) (string, error) {
			// JSON escaping at most sextuples a byte (\u00XX).
			if err := budget.charge(6*len(s) + 2); err != nil {
				return "", err
			}
			b, err := json.Marshal(s)
			return string(b), err
		},
	}
	tmpl, parseErr := template.New(render.file).Funcs(funcs).Parse(render.text)
	if parseErr != nil {
		return nil, false, nil
	}
	type toleration struct{ Key, Value, Operator, Effect string }
	type node struct{ NodeName string }
	type test struct {
		Name                        string
		BandwidthGBps, GoodputRatio *string
	}
	tolerations := []toleration{{Key: corev1.TaintNodeUnschedulable, Operator: string(corev1.TolerationOpExists),
		Effect: string(corev1.TaintEffectNoSchedule)}}
	for _, taint := range taints {
		operator := corev1.TolerationOpExists
		if taint.value != "" {
			operator = corev1.TolerationOpEqual
		}
		tolerations = append(tolerations, toleration{taint.key, taint.value, string(operator), taint.effect})
	}
	data := struct {
		ValidationRequestName, TestGroupName, Namespace string
		TimeoutSeconds                                  int64
		Nodes                                           []node
		Tests                                           []test
		Image                                           string
		Command                                         []string
		Env                                             []corev1.EnvVar
		Tolerations                                     []toleration
	}{
		ValidationRequestName: postRemediationProbe, TestGroupName: postRemediationProbe,
		Namespace: postRemediationProbe, TimeoutSeconds: render.timeoutSeconds,
		Nodes: []node{{NodeName: postRemediationProbe}},
		Tests: []test{{Name: render.test, BandwidthGBps: render.bandwidthGBps, GoodputRatio: render.goodputRatio}},
		Image: render.image, Command: render.command, Env: render.env, Tolerations: tolerations,
	}
	lengths := map[string]int{"Tolerations": len(data.Tolerations), "Nodes": len(data.Nodes),
		"Tests": len(data.Tests), "Command": len(data.Command), "Env": len(data.Env)}
	if _, bounded := postRemediationTemplateSteps(tmpl, lengths); !bounded {
		return nil, false, nil
	}

	out := &postRemediationCappedWriter{budget: budget}
	done := make(chan error, 1)
	go func() { done <- tmpl.Execute(out, data) }()
	select {
	case execErr := <-done:
		if execErr != nil {
			if err := ctx.Err(); err != nil {
				return nil, false, aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "rendering "+render.file)
			}
			return nil, false, nil
		}
	case <-renderCtx.Done():
		if err := ctx.Err(); err != nil {
			return nil, false, aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "rendering "+render.file)
		}
		return nil, false, nil
	}
	if err := sigsyaml.Unmarshal(out.Bytes(), &rendered); err != nil {
		return nil, false, nil
	}
	return rendered, true, nil
}

// postRemediationBudget is what one rendering may still produce, through its
// functions and its writer together, before its deadline.
type postRemediationBudget struct {
	ctx       context.Context
	remaining int
}

// charge spends n bytes, failing once the render's deadline has passed or
// the budget is spent; the failure stops the template execution.
func (b *postRemediationBudget) charge(n int) error {
	if err := b.ctx.Err(); err != nil {
		return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "rendering a test Job template")
	}
	if n > b.remaining {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "rendering a test Job template exceeds its size limit")
	}
	b.remaining -= n
	return nil
}

// postRemediationCappedWriter is a buffer whose writes are charged to the
// render's budget, so a write past it or past the deadline stops the template
// execution writing to it.
type postRemediationCappedWriter struct {
	bytes.Buffer
	budget *postRemediationBudget
}

func (w *postRemediationCappedWriter) Write(p []byte) (int, error) {
	if err := w.budget.charge(len(p)); err != nil {
		return 0, err
	}
	return w.Buffer.Write(p)
}

// postRemediationRangeFields are the TemplateContext lists a bounded template
// may range over, and postRemediationTemplateFuncs the functions it may call:
// lifecycle-manager's own two, both metered, and text/template's builtins
// whose cost is bounded by their arguments.
var (
	postRemediationRangeFields   = []string{"Tolerations", "Nodes", "Tests", "Command", "Env"}
	postRemediationTemplateFuncs = []string{"join", "quote", "and", "or", "not", "eq", "ne", "lt", "le", "gt",
		"ge", "len", "index"}
)

// postRemediationTemplateSteps bounds the evaluation steps a template takes
// over lists of the given lengths: each node, pipeline argument and range
// iteration is one step, multiplied by the iterations of every range around
// it, with both
// branches of an if or with counted. bounded is false when the template
// defines or invokes another template, calls a function outside
// postRemediationTemplateFuncs, ranges over anything but a
// postRemediationRangeFields list (an integer included), or exceeds
// defaults.MaxNVSentinelJobTemplateSteps. Work that writes nothing and calls
// no function is bounded by this alone.
func postRemediationTemplateSteps(tmpl *template.Template, lengths map[string]int) (steps int, bounded bool) {
	if len(tmpl.Templates()) > 1 || tmpl.Tree == nil {
		return 0, false
	}
	bounded = true
	var visit func(node parse.Node, multiplier int)
	add := func(n, multiplier int) {
		if !bounded {
			return
		}
		steps += n * multiplier
		if steps > defaults.MaxNVSentinelJobTemplateSteps {
			bounded = false
		}
	}
	var pipeSteps func(pipe *parse.PipeNode) int
	argSteps := func(arg parse.Node) int {
		switch a := arg.(type) {
		case *parse.IdentifierNode:
			if !slices.Contains(postRemediationTemplateFuncs, a.Ident) {
				bounded = false
			}
			return 1
		case *parse.PipeNode:
			return pipeSteps(a)
		case *parse.ChainNode:
			if p, isPipe := a.Node.(*parse.PipeNode); isPipe {
				return 1 + pipeSteps(p)
			}
			return 1
		case *parse.FieldNode, *parse.VariableNode, *parse.DotNode, *parse.NilNode, *parse.BoolNode,
			*parse.NumberNode, *parse.StringNode:
			return 1
		default:
			bounded = false
			return 1
		}
	}
	pipeSteps = func(pipe *parse.PipeNode) int {
		if pipe == nil {
			return 0
		}
		n := 1
		for _, cmd := range pipe.Cmds {
			for _, arg := range cmd.Args {
				n += argSteps(arg)
			}
		}
		return n
	}
	branch := func(n *parse.BranchNode, multiplier, bodyMultiplier int) {
		add(pipeSteps(n.Pipe), multiplier)
		if n.List != nil {
			visit(n.List, bodyMultiplier)
		}
		if n.ElseList != nil {
			visit(n.ElseList, multiplier)
		}
	}
	visit = func(node parse.Node, multiplier int) {
		if !bounded {
			return
		}
		switch n := node.(type) {
		case *parse.ListNode:
			for _, child := range n.Nodes {
				visit(child, multiplier)
			}
		case *parse.TextNode, *parse.CommentNode, *parse.BreakNode, *parse.ContinueNode:
			add(1, multiplier)
		case *parse.ActionNode:
			add(pipeSteps(n.Pipe), multiplier)
		case *parse.IfNode:
			branch(&n.BranchNode, multiplier, multiplier)
		case *parse.WithNode:
			branch(&n.BranchNode, multiplier, multiplier)
		case *parse.RangeNode:
			field, ranged := postRemediationRangedList(n.Pipe)
			if !ranged {
				bounded = false
				return
			}
			iterations := multiplier * max(lengths[field], 1)
			// Each iteration is a step, even with an empty body.
			add(1, iterations)
			branch(&n.BranchNode, multiplier, iterations)
		default:
			bounded = false
		}
	}
	visit(tmpl.Root, 1)
	return steps, bounded
}

// postRemediationRangedList returns the data list a range pipeline names, as
// .Tolerations or $.Tolerations, when it names one of
// postRemediationRangeFields and nothing else.
func postRemediationRangedList(pipe *parse.PipeNode) (string, bool) {
	if pipe == nil || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		return "", false
	}
	var field string
	switch arg := pipe.Cmds[0].Args[0].(type) {
	case *parse.FieldNode:
		if len(arg.Ident) == 1 {
			field = arg.Ident[0]
		}
	case *parse.VariableNode:
		if len(arg.Ident) == 2 && arg.Ident[0] == "$" {
			field = arg.Ident[1]
		}
	}
	return field, slices.Contains(postRemediationRangeFields, field)
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
