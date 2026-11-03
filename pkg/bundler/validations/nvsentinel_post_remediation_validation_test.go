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
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// postRemediationRuleSet returns a fault-quarantine validation rule set
// requesting tests.
func postRemediationRuleSet(name string, tests ...any) map[string]any {
	return map[string]any{
		"enabled": true,
		"version": "1",
		"name":    name,
		"match": map[string]any{"all": []any{map[string]any{
			"kind": "HealthEvent",
			"expression": "event.componentClass == 'GPU' && event.isFatal == true && " +
				"event.recommendedAction in [15, 24]",
		}}},
		"tests": tests,
	}
}

// postRemediationValues returns what the nvsentinel-remediation mixin sets
// for post-remediation validation, with fault-quarantine and node-drainer
// rendering.
func postRemediationValues() map[string]any {
	return map[string]any{
		"global": map[string]any{
			"faultQuarantine": map[string]any{"enabled": true},
			"nodeDrainer":     map[string]any{"enabled": true},
		},
		"fault-quarantine": map[string]any{"validation": map[string]any{
			"enabled":  true,
			"ruleSets": []any{postRemediationRuleSet("Post-reboot validation", nodeValidationDCGMTest)},
		}},
	}
}

// postRemediationRelease returns a lifecycle-manager schedulingGate.taints
// entry for taint.
func postRemediationRelease(taint postRemediationTaint, remove bool) map[string]any {
	return map[string]any{"key": taint.key, "value": taint.value, "effect": taint.effect, "remove": remove}
}

func TestCheckNVSentinelPostRemediationValidationCoherent(t *testing.T) {
	t.Parallel()

	gpuOperator := func(namespace string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gpu-operator", Namespace: namespace, Overrides: overrides}
	}
	dcgm := func(enabled any) map[string]any {
		return map[string]any{"dcgm": map[string]any{"enabled": enabled}}
	}
	healthyGPUOperator := gpuOperator(nodeValidationDCGMNamespace, dcgm(true))

	at := func(path string, value any) map[string]any { return withPath(map[string]any{}, path, value) }
	// remediation is the remediation and node-validation mixins composed onto
	// nvsentinel, with the given values on top.
	remediation := func(overrides ...map[string]any) map[string]any {
		return mergedValues(append([]map[string]any{postRemediationValues(), nodeValidationValues()}, overrides...)...)
	}
	// withoutNodeValidation is remediation without lifecycle-manager.
	withoutNodeValidation := func(overrides ...map[string]any) map[string]any {
		return mergedValues(append([]map[string]any{postRemediationValues()}, overrides...)...)
	}
	ruleSets := func(items ...any) map[string]any { return at(postRemediationRuleSetsPath, items) }

	stubCommand := []any{"/bin/sh", "-c", "exit 0"}
	dcgmCommandPath := nodeValidationTestsPath + "." + nodeValidationDCGMTest + ".command"
	gpuBurn := at(nodeValidationTestsPath+".gpu-burn",
		map[string]any{"provider": nodeValidationChartProvider, "image": "busybox:1.38.0", "command": stubCommand})
	// broken fails several rules at once, so a case that passes with it shows
	// the gate never looked.
	broken := func(overrides ...map[string]any) map[string]any {
		return remediation(append([]map[string]any{
			at(postRemediationCordonRemovePath, false), at(postRemediationFQLabelPrefixPath, "custom.example.com/"),
			ruleSets(postRemediationRuleSet("undefined", "gpu-burn")),
		}, overrides...)...)
	}

	sets := func(key string, kv map[string]string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{key: kv}))
	}
	setJSON := func(specs ...string) *config.Config {
		paths, err := config.ParseValueOverridesJSON(specs)
		if err != nil {
			t.Fatalf("ParseValueOverridesJSON: %v", err)
		}
		return config.NewConfig(config.WithValueOverridesTypedPaths(paths))
	}
	dynamic := func(key string, paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{key: paths}))
	}

	const (
		customPrefix      = "custom.example.com/"
		wantNotBool       = postRemediationValidationEnabled + ` is "true", not a boolean`
		wantNoController  = "compose the " + nodeValidationMixin + " mixin"
		wantNoRemove      = postRemediationCordonRemovePath + " is false: lifecycle-manager does not uncordon"
		wantNoRuleSet     = "fault-quarantine creates no ValidationRequest without a rule set"
		wantNoTests       = postRemediationRuleSetsPath + "[0] names no tests"
		wantNotRuleSet    = postRemediationRuleSetsPath + `[0] is "dcgm-diag-test", not a rule set`
		wantNoActive      = postRemediationRuleSetsPath + " has no enabled rule set"
		wantEnabledNil    = postRemediationRuleSetsPath + "[0].enabled is <nil>, not a boolean"
		wantNoMatch       = postRemediationRuleSetsPath + "[0].match has no all or any rule"
		wantNotSection    = postRemediationValidationPath + " is %s, not a map"
		wantUndefined     = `requests the test "gpu-burn", which lifecycle-manager's config does not define`
		wantDCGMUndefined = `requests the test "dcgm-diag-test", which lifecycle-manager's config does not define`
		wantNoGPUOperator = "no GPU Operator component is in the recipe"
		wantGPUOpDisabled = "gpu-operator is disabled"
		wantRelocated     = `gpu-operator is deployed to "privileged-gpu-operator"`
		wantDCGMUnset     = "does not set dcgm.enabled"
		wantDCGMOff       = "runs with dcgm.enabled: false"
		wantFQPrefix      = postRemediationFQLabelPrefixPath + ` is "` + customPrefix + `" but ` +
			postRemediationLMLabelPrefixPath + ` is "` + postRemediationChartLabelPrefix + `"`
		wantLMPrefix = postRemediationFQLabelPrefixPath + ` is "` + postRemediationChartLabelPrefix + `" but ` +
			postRemediationLMLabelPrefixPath + ` is "` + customPrefix + `"`
		wantDynamic       = "--dynamic"
		wantNoDrainer     = "global.nodeDrainer.enabled is off"
		wantNotGateMap    = " is <nil>, not a map: Helm drops the chart's cordon settings"
		wantNoQuarantine  = postRemediationReadinessPath + ` does not include a criterion named "not-under-quarantine"`
		wantReadinessNull = postRemediationReadinessPath + " is <nil>, not a list of criteria"
		wantDuplicateName = postRemediationRuleSetsPath + `[1] has the name "reboot", as ` + postRemediationRuleSetsPath + "[0] does"
	)
	nvcre := postRemediationChartQuarantineTaints[0]
	releases := func(items ...any) map[string]any { return at(postRemediationReleaseTaintsPath, items) }
	quarantineRules := func(items ...any) map[string]any { return at(postRemediationQuarantineRules, items) }
	tainting := func(effect string, enabled any) map[string]any {
		rule := map[string]any{"name": "custom", "taint": map[string]any{"key": "example.com/gpu", "value": "bad", "effect": effect}}
		if enabled != nil {
			rule["enabled"] = enabled
		}
		return rule
	}
	customTaint := postRemediationTaint{key: "example.com/gpu", value: "bad", effect: "NoExecute"}
	nvcreOn := at("global.nvcreCertificationMonitor.enabled", true)
	wantNoRelease := "no entry can release it, since its value \"true\" would break every test Job"
	wantBadValue := func(i int, value string) string {
		return fmt.Sprintf("%s[%d].value is %q, which lifecycle-manager's Job template writes unquoted",
			postRemediationReleaseTaintsPath, i, value)
	}
	wantCustom := postRemediationQuarantineRules + "[0] applies the taint " + customTaint.String()
	// jobTemplate is a test Job template that writes each toleration's value
	// with valueAction, stored under file in lifecycle-manager.config.templates.
	jobTemplate := func(file, valueAction string) map[string]any {
		text := "apiVersion: batch/v1\nkind: Job\nspec:\n  template:\n    spec:\n      tolerations:\n" +
			"      {{- range .Tolerations}}\n        - key: {{.Key}}\n          value: " + valueAction + "\n" +
			"          operator: {{.Operator}}\n          effect: {{.Effect}}\n      {{- end}}\n"
		return map[string]any{"lifecycle-manager": map[string]any{"config": map[string]any{
			"templates": map[string]any{file: text}}}}
	}
	quotingTemplate := jobTemplate("k8s-job-template.yaml", "{{quote .Value}}")
	// mixedProviders requests dcgm-diag-test, on the chart's template, and
	// gpu-burn, on a provider whose template AICR's values override.
	customProvider := map[string]any{}
	for k, v := range nodeValidationChartProviderDefaults {
		customProvider[k] = v
	}
	customProvider["templateFile"] = "custom-template.yaml"
	mixedProviders := mergedValues(
		at(nodeValidationProvidersPath+".custom-provider", customProvider),
		at(nodeValidationTestsPath+".gpu-burn",
			map[string]any{"provider": "custom-provider", "image": "busybox:1.38.0", "command": []any{"true"}}),
		jobTemplate("custom-template.yaml", "{{quote .Value}}"),
		at(postRemediationRuleSetsPath, []any{postRemediationRuleSet("reboot", nodeValidationDCGMTest, "gpu-burn")}),
	)
	customOnly := mergedValues(mixedProviders,
		at(postRemediationRuleSetsPath, []any{postRemediationRuleSet("reboot", "gpu-burn")}))
	wantUnverified := postRemediationReleaseTaintsPath + " is rendered into test Jobs through k8s-job-template.yaml"
	wantUnverifiedCustom := postRemediationReleaseTaintsPath + " is rendered into test Jobs through custom-template.yaml"
	trueRelease := releases(postRemediationRelease(postRemediationTaint{"a", "true", "NoSchedule"}, true))
	typedTrue := map[string]any{"key": "a", "value": true, "effect": "NoSchedule", "remove": true}
	wantTypedValue := func(i int, value string) string {
		return fmt.Sprintf("%s[%d].value is %s, not a string: the chart writes schedulingGate into "+
			"lifecycle-manager's config with toYaml", postRemediationReleaseTaintsPath, i, value)
	}
	criterion := func(name, expression string) any { return nodeValidationCriterion(name, expression) }
	readiness := func(items ...any) map[string]any { return at(postRemediationReadinessPath, items) }
	quarantineCriterion := criterion(nodeValidationQuarantineCriterion, nodeValidationQuarantineExpression)

	tests := []struct {
		name     string
		sentinel map[string]any
		// others replaces the default healthy GPU Operator.
		others    []recipe.ComponentRef
		noOthers  bool
		cfg       *config.Config
		wantErrs  []string
		wantWarns []string
	}{
		// Inert.
		{name: "monitors only, nothing else in the recipe", sentinel: map[string]any{}, noOthers: true},
		{name: "validation off", noOthers: true, sentinel: broken(at(postRemediationValidationEnabled, false))},
		{name: "validation left to the chart default", noOthers: true,
			sentinel: withoutPath(broken(), postRemediationValidationEnabled)},
		{name: "validation off without lifecycle-manager", noOthers: true,
			sentinel: withoutNodeValidation(at(postRemediationValidationEnabled, false))},
		{name: "fault-quarantine off", noOthers: true,
			sentinel: broken(at("global.faultQuarantine.enabled", false))},
		{name: "fault-quarantine left to the chart default", noOthers: true,
			sentinel: withoutPath(broken(), "global.faultQuarantine")},
		{name: "fault-quarantine off without lifecycle-manager", noOthers: true,
			sentinel: withoutNodeValidation(at("global.faultQuarantine.enabled", false))},
		{name: "nvsentinel disabled in the recipe", noOthers: true, sentinel: broken(at("enabled", false))},
		{name: "nvsentinel disabled by --set", noOthers: true, sentinel: broken(),
			cfg: sets("nv-sentinel", map[string]string{config.ComponentEnabledKey: "false"})},

		// The mixins as shipped.
		{name: "remediation with node validation", sentinel: remediation()},
		{name: "validation on by --set", sentinel: withoutPath(remediation(), postRemediationValidationEnabled),
			cfg: sets("nv-sentinel", map[string]string{postRemediationValidationEnabled: "true"})},

		// validation.enabled.
		{name: "validation enabled a string", noOthers: true,
			sentinel: broken(at(postRemediationValidationEnabled, "true")), wantErrs: []string{wantNotBool}},
		{name: "validation enabled a string with fault-quarantine off", noOthers: true,
			sentinel: withoutNodeValidation(at(postRemediationValidationEnabled, "true"),
				at("global.faultQuarantine.enabled", false)),
			wantErrs: []string{wantNotBool}},
		{name: "validation enabled a number", noOthers: true,
			sentinel: remediation(at(postRemediationValidationEnabled, 1)),
			wantErrs: []string{postRemediationValidationEnabled + " is 1, not a boolean"}},
		{name: "validation section true", noOthers: true,
			sentinel: remediation(at(postRemediationValidationPath, true)),
			wantErrs: []string{fmt.Sprintf(wantNotSection, "true")}},
		{name: "validation section an empty list", noOthers: true,
			sentinel: remediation(at(postRemediationValidationPath, []any{})),
			wantErrs: []string{fmt.Sprintf(wantNotSection, "[]interface {}{}")}},
		{name: "validation section null", noOthers: true,
			sentinel: remediation(at(postRemediationValidationPath, nil)),
			wantErrs: []string{fmt.Sprintf(wantNotSection, "<nil>")}},
		{name: "validation section null by --set-json", noOthers: true, sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + postRemediationValidationPath + "=null"),
			wantErrs: []string{fmt.Sprintf(wantNotSection, "<nil>")}},
		{name: "validation enabled null", noOthers: true,
			sentinel: remediation(at(postRemediationValidationEnabled, nil)),
			wantErrs: []string{postRemediationValidationEnabled + " is <nil>, not a boolean"}},

		// lifecycle-manager and its ValidationRequest controller.
		{name: "validation without lifecycle-manager", sentinel: withoutNodeValidation(),
			wantErrs: []string{wantNoController}},
		{name: "validation without lifecycle-manager reports nothing else", noOthers: true,
			sentinel: withoutNodeValidation(at(postRemediationFQLabelPrefixPath, customPrefix),
				ruleSets(postRemediationRuleSet("undefined", "gpu-burn"))),
			wantErrs: []string{wantNoController}},
		{name: "lifecycle-manager off", sentinel: remediation(at(nodeValidationLifecycleEnabledPath, false)),
			wantErrs: []string{wantNoController}},
		{name: "lifecycle-manager enabled a string still renders", sentinel: remediation(at(nodeValidationLifecycleEnabledPath, "false"))},
		{name: "ValidationRequest controller off", sentinel: remediation(at(nodeValidationControllerPath, false)),
			wantErrs: []string{wantNoController}},
		{name: "ValidationRequest controller a string", sentinel: remediation(at(nodeValidationControllerPath, "true")),
			wantErrs: []string{wantNoController}},
		{name: "ValidationRequest controller left to the chart default",
			sentinel: withoutPath(remediation(), nodeValidationControllerPath)},

		// schedulingGate.cordon.remove.
		{name: "cordon.remove false", sentinel: remediation(at(postRemediationCordonRemovePath, false)),
			wantErrs: []string{wantNoRemove}},
		{name: "cordon.remove a string", sentinel: remediation(at(postRemediationCordonRemovePath, "true")),
			wantErrs: []string{postRemediationCordonRemovePath + ` is "true": lifecycle-manager does not uncordon`}},
		{name: "cordon.remove left to the chart default",
			sentinel: withoutPath(remediation(), postRemediationCordonRemovePath)},
		{name: "schedulingGate null", sentinel: remediation(at(postRemediationSchedulingGate, nil)),
			wantErrs: []string{postRemediationSchedulingGate + wantNotGateMap}},
		{name: "schedulingGate null by --set-json", sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + postRemediationSchedulingGate + "=null"),
			wantErrs: []string{postRemediationSchedulingGate + wantNotGateMap}},
		{name: "schedulingGate.cordon null", sentinel: remediation(at(postRemediationCordonPath, nil)),
			wantErrs: []string{postRemediationCordonPath + wantNotGateMap}},
		{name: "schedulingGate.cordon a string", sentinel: remediation(at(postRemediationCordonPath, "remove")),
			wantErrs: []string{postRemediationCordonPath + ` is "remove", not a map`}},
		{name: "schedulingGate left to the chart default",
			sentinel: withoutPath(remediation(), postRemediationSchedulingGate)},

		// Taints fault-quarantine keeps on the hand-over.
		{name: "the chart's NVCRE taint needs no release while its monitor is off", sentinel: remediation()},
		{name: "the chart's NVCRE taint with its monitor on cannot be released",
			sentinel: remediation(nvcreOn), wantErrs: []string{wantNoRelease}},
		{name: "a release entry valued true breaks every test Job",
			sentinel: remediation(nvcreOn, releases(postRemediationRelease(nvcre, true))),
			wantErrs: []string{wantBadValue(0, "true")}},
		{name: "a release entry valued yes or 1 breaks every test Job",
			sentinel: remediation(releases(postRemediationRelease(postRemediationTaint{"a", "yes", "NoSchedule"}, true),
				postRemediationRelease(postRemediationTaint{"b", "1", "NoSchedule"}, true))),
			wantErrs: []string{wantBadValue(0, "yes"), wantBadValue(1, "1")}},
		{name: "a release entry with a typed bool or number value stops lifecycle-manager",
			sentinel: remediation(releases(typedTrue, map[string]any{"key": "b", "value": 1, "effect": "NoSchedule", "remove": true})),
			wantErrs: []string{wantTypedValue(0, "true"), wantTypedValue(1, "1")}},
		{name: "a release entry with a typed bool value by --set-json stops lifecycle-manager", sentinel: remediation(),
			cfg: setJSON(`nv-sentinel:` + postRemediationReleaseTaintsPath +
				`=[{"key":"a","value":true,"effect":"NoSchedule","remove":true}]`),
			wantErrs: []string{wantTypedValue(0, "true")}},
		{name: "a typed value stops lifecycle-manager even with a template that quotes it",
			sentinel: remediation(quotingTemplate, releases(typedTrue)),
			wantErrs: []string{wantTypedValue(0, "true")}, wantWarns: []string{wantUnverified}},
		{name: "a release entry with no value is fine",
			sentinel: remediation(releases(postRemediationRelease(postRemediationTaint{"a", "", "NoSchedule"}, true)))},
		{name: "a custom Job template leaves a string value unverified, with a warning",
			sentinel: remediation(quotingTemplate, trueRelease), wantWarns: []string{wantUnverified}},
		{name: "a custom Job template that writes the value unquoted is not rendered either",
			sentinel:  remediation(jobTemplate("k8s-job-template.yaml", "{{.Value}}"), trueRelease),
			wantWarns: []string{wantUnverified}},
		{name: "a custom Job template lets the NVCRE taint be listed, with a warning",
			sentinel:  remediation(nvcreOn, quotingTemplate, releases(postRemediationRelease(nvcre, true))),
			wantWarns: []string{wantUnverified}},
		{name: "a custom Job template with nothing to release warns of nothing",
			sentinel: remediation(quotingTemplate)},
		{name: "a template override no requested test uses leaves the chart's rule in force",
			sentinel: remediation(jobTemplate("unused-template.yaml", "{{quote .Value}}"), trueRelease),
			wantErrs: []string{wantBadValue(0, "true")}},
		{name: "a test on the chart's template beside one on a custom template keeps the chart's rule",
			sentinel: remediation(mixedProviders, trueRelease),
			wantErrs: []string{wantBadValue(0, "true")}, wantWarns: []string{wantUnverifiedCustom}},
		{name: "only tests on a custom template leave the value unverified",
			sentinel: remediation(customOnly, trueRelease), wantWarns: []string{wantUnverifiedCustom}},
		{name: "a custom Job template does not excuse a taint left unreleased",
			sentinel: remediation(quotingTemplate, quarantineRules(tainting("NoExecute", true)), trueRelease),
			wantErrs: []string{wantCustom}, wantWarns: []string{wantUnverified}},
		{name: "a custom rule set's NoExecute taint released",
			sentinel: remediation(quarantineRules(tainting("NoExecute", nil)), releases(postRemediationRelease(customTaint, true)))},
		{name: "a custom rule set's NoExecute taint not released",
			sentinel: remediation(quarantineRules(tainting("NoExecute", true))), wantErrs: []string{wantCustom}},
		{name: "a custom rule set's taint released with remove false",
			sentinel: remediation(quarantineRules(tainting("NoExecute", nil)), releases(postRemediationRelease(customTaint, false))),
			wantErrs: []string{wantCustom}},
		{name: "a custom rule set's taint released with another value",
			sentinel: remediation(quarantineRules(tainting("NoExecute", nil)),
				releases(postRemediationRelease(postRemediationTaint{customTaint.key, "other", customTaint.effect}, true))),
			wantErrs: []string{wantCustom}},
		{name: "a custom rule set's PreferNoSchedule taint needs no release",
			sentinel: remediation(quarantineRules(tainting("PreferNoSchedule", nil)))},
		{name: "a disabled rule set's taint needs no release",
			sentinel: remediation(quarantineRules(tainting("NoExecute", false)))},
		{name: "rule sets without taints need no release",
			sentinel: remediation(quarantineRules(map[string]any{"name": "cordon only"}))},
		{name: "rule sets null need no release, even with the NVCRE monitor on",
			sentinel: remediation(nvcreOn, at(postRemediationQuarantineRules, nil))},
		{name: "--dynamic fault-quarantine rule sets", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", postRemediationQuarantineRules), wantErrs: []string{wantDynamic}},
		{name: "--dynamic NVCRE monitor toggle with the chart's rule sets", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", postRemediationNVCREMonitorPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic NVCRE monitor toggle with AICR's own rule sets",
			sentinel: remediation(quarantineRules(map[string]any{"name": "cordon only"})),
			cfg:      dynamic("nv-sentinel", postRemediationNVCREMonitorPath)},
		{name: "--dynamic NVCRE monitor toggle while validation is off", noOthers: true,
			sentinel: broken(at(postRemediationValidationEnabled, false)),
			cfg:      dynamic("nv-sentinel", postRemediationNVCREMonitorPath)},

		// node-drainer.
		{name: "node-drainer off", sentinel: remediation(at("global.nodeDrainer.enabled", false)),
			wantErrs: []string{wantNoDrainer}},
		{name: "node-drainer left to the chart default", sentinel: withoutPath(remediation(), "global.nodeDrainer"),
			wantErrs: []string{wantNoDrainer}},

		// readinessCriteria.
		{name: "readinessCriteria left to the chart default", sentinel: remediation()},
		{name: "readinessCriteria keeping only not-under-quarantine", sentinel: remediation(readiness(quarantineCriterion))},
		{name: "readinessCriteria with not-under-quarantine reflowed",
			sentinel: remediation(readiness(criterion(nodeValidationQuarantineCriterion,
				"!(has(node.metadata.annotations) &&\n  \"quarantineHealthEvent\" in node.metadata.annotations)")))},
		{name: "readinessCriteria without not-under-quarantine",
			sentinel: remediation(readiness(criterion("gpu-allocatable", "true"))), wantErrs: []string{wantNoQuarantine}},
		{name: "readinessCriteria with another not-under-quarantine expression",
			sentinel: remediation(readiness(criterion(nodeValidationQuarantineCriterion, "true"))),
			wantErrs: []string{wantNoQuarantine}},
		{name: "readinessCriteria empty", sentinel: remediation(readiness()), wantErrs: []string{wantNoQuarantine}},
		{name: "readinessCriteria null", sentinel: remediation(at(postRemediationReadinessPath, nil)),
			wantErrs: []string{wantReadinessNull}},
		{name: "readinessCriteria null by --set-json", sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + postRemediationReadinessPath + "=null"),
			wantErrs: []string{wantReadinessNull}},

		// ruleSets.
		{name: "ruleSets left to the chart default", sentinel: withoutPath(remediation(), postRemediationRuleSetsPath)},
		{name: "chart-default ruleSets still needs DCGM", noOthers: true,
			sentinel: withoutPath(remediation(), postRemediationRuleSetsPath), wantErrs: []string{wantNoGPUOperator}},
		{name: "ruleSets null", sentinel: remediation(at(postRemediationRuleSetsPath, nil)),
			wantErrs: []string{postRemediationRuleSetsPath + " is <nil>: " + wantNoRuleSet}},
		{name: "ruleSets null by --set-json", sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + postRemediationRuleSetsPath + "=null"),
			wantErrs: []string{wantNoRuleSet}},
		{name: "ruleSets empty", sentinel: remediation(ruleSets()),
			wantErrs: []string{postRemediationRuleSetsPath + " is []interface {}{}: " + wantNoRuleSet}},
		{name: "ruleSets empty by --set-json", sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + postRemediationRuleSetsPath + "=[]"),
			wantErrs: []string{wantNoRuleSet}},
		{name: "ruleSets a single rule set, not a list",
			sentinel: remediation(at(postRemediationRuleSetsPath, postRemediationRuleSet("one", nodeValidationDCGMTest))),
			wantErrs: []string{wantNoRuleSet}},
		{name: "ruleSets a string", sentinel: remediation(at(postRemediationRuleSetsPath, nodeValidationDCGMTest)),
			wantErrs: []string{postRemediationRuleSetsPath + ` is "dcgm-diag-test": ` + wantNoRuleSet}},
		{name: "a rule set not a map", sentinel: remediation(ruleSets(nodeValidationDCGMTest)),
			wantErrs: []string{wantNotRuleSet, wantNoActive}},
		{name: "a rule set not a map beside a valid one",
			sentinel: remediation(ruleSets(nodeValidationDCGMTest, postRemediationRuleSet("reboot", nodeValidationDCGMTest))),
			wantErrs: []string{wantNotRuleSet}},
		{name: "a rule set with no tests", sentinel: remediation(ruleSets(postRemediationRuleSet("reboot"))),
			wantErrs: []string{wantNoTests}},
		{name: "a rule set without a tests key",
			sentinel: remediation(ruleSets(withoutPath(postRemediationRuleSet("reboot"), "tests"))),
			wantErrs: []string{wantNoTests}},
		{name: "a rule set whose tests is a string, not a list",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot"), "tests", nodeValidationDCGMTest))),
			wantErrs: []string{wantNoTests}},
		{name: "a test name not a string", sentinel: remediation(ruleSets(postRemediationRuleSet("reboot", 7))),
			wantErrs: []string{postRemediationRuleSetsPath + "[0].tests[0] is 7, not a test name"}},
		{name: "a test name empty",
			sentinel: remediation(ruleSets(postRemediationRuleSet("reboot", nodeValidationDCGMTest, ""))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].tests[1] is "", not a test name`}},
		{name: "a disabled rule set is skipped",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("off", "gpu-burn"), "enabled", false),
				postRemediationRuleSet("reboot", nodeValidationDCGMTest)))},
		{name: "a disabled rule set with no tests is skipped",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("off"), "enabled", false),
				postRemediationRuleSet("reboot", nodeValidationDCGMTest)))},
		{name: "a rule set without an enabled key is enabled, as the chart renders it",
			sentinel: remediation(ruleSets(withoutPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "enabled")))},
		{name: "a rule set without an enabled key is checked",
			sentinel: remediation(ruleSets(withoutPath(postRemediationRuleSet("reboot", "gpu-burn"), "enabled"))),
			wantErrs: []string{wantUndefined}},
		{name: "a rule set enabled null",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "enabled", nil))),
			wantErrs: []string{wantEnabledNil, wantNoActive}},
		{name: "a rule set enabled a string",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "enabled", "false"))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].enabled is "false", not a boolean`, wantNoActive}},
		{name: "a rule set enabled a string beside a valid one",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("off", "gpu-burn"), "enabled", "true"),
				postRemediationRuleSet("reboot", nodeValidationDCGMTest))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].enabled is "true", not a boolean`}},
		{name: "a rule set with an empty match",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{}))),
			wantErrs: []string{wantNoMatch}},
		{name: "a rule set with empty all and any lists",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"all": []any{}, "any": []any{}}))),
			wantErrs: []string{wantNoMatch}},
		{name: "a rule set with no match key",
			sentinel: remediation(ruleSets(withoutPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match"))),
			wantErrs: []string{postRemediationRuleSetsPath + "[0].match is <nil>, not a map of all and any rules"}},
		{name: "a rule set whose match is a string",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match", "GPU"))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].match is "GPU", not a map of all and any rules`}},
		{name: "a rule set matching on any",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"any": []any{map[string]any{"kind": "HealthEvent", "expression": "event.isFatal"}}})))},
		{name: "a rule set whose match.all is a map, not a list",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"all": map[string]any{"kind": "HealthEvent", "expression": "event.isFatal"}}))),
			wantErrs: []string{postRemediationRuleSetsPath + "[0].match.all is", wantNoMatch}},
		{name: "a match rule not a map",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"all": []any{"event.isFatal"}}))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].match.all[0] is "event.isFatal", not a rule`}},
		{name: "a Node match rule",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"any": []any{map[string]any{"kind": "Node", "expression": "true"}}}))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].match.any[0].kind is "Node"`}},
		{name: "a match rule with no expression",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"all": []any{map[string]any{"kind": "HealthEvent", "expression": ""}}}))),
			wantErrs: []string{postRemediationRuleSetsPath + `[0].match.all[0].expression is "", not an expression`}},
		{name: "a disabled rule set's empty match is not checked",
			sentinel: remediation(ruleSets(withPath(withPath(postRemediationRuleSet("off"), "enabled", false), "match",
				map[string]any{}), postRemediationRuleSet("reboot", nodeValidationDCGMTest)))},
		{name: "every rule set disabled",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("vm", nodeValidationDCGMTest), "enabled", false),
				withPath(postRemediationRuleSet("bm", nodeValidationDCGMTest), "enabled", false))),
			wantErrs: []string{wantNoActive}},
		{name: "two rule sets requesting the same test",
			sentinel: remediation(ruleSets(postRemediationRuleSet("vm", nodeValidationDCGMTest),
				postRemediationRuleSet("bm", nodeValidationDCGMTest)))},
		{name: "a disabled rule set with no tests sharing an enabled one's name",
			sentinel: remediation(ruleSets(postRemediationRuleSet("reboot", nodeValidationDCGMTest),
				withPath(postRemediationRuleSet("reboot"), "enabled", false))),
			wantErrs: []string{wantDuplicateName}},
		{name: "two enabled rule sets sharing a name",
			sentinel: remediation(ruleSets(postRemediationRuleSet("reboot", nodeValidationDCGMTest),
				postRemediationRuleSet("reboot", nodeValidationDCGMTest))),
			wantErrs: []string{wantDuplicateName}},
		{name: "two unnamed rule sets",
			sentinel: remediation(ruleSets(withoutPath(postRemediationRuleSet("", nodeValidationDCGMTest), "name"),
				withPath(postRemediationRuleSet("", nodeValidationDCGMTest), "name", nil))),
			wantErrs: []string{postRemediationRuleSetsPath + `[1] has the name ""`}},
		{name: "a numeric name collides with its quoted form",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("", nodeValidationDCGMTest), "name", 7),
				postRemediationRuleSet("7", nodeValidationDCGMTest))),
			wantErrs: []string{postRemediationRuleSetsPath + `[1] has the name "7"`}},
		{name: "a rule set matching on any beside a null all",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"all": nil, "any": []any{map[string]any{"kind": "HealthEvent", "expression": "event.isFatal"}}})))},
		{name: "a rule set whose only rule list is null",
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("reboot", nodeValidationDCGMTest), "match",
				map[string]any{"all": nil}))),
			wantErrs: []string{wantNoMatch}},

		// Tests lifecycle-manager defines.
		{name: "a test lifecycle-manager does not define",
			sentinel: remediation(ruleSets(postRemediationRuleSet("reboot", "gpu-burn"))),
			wantErrs: []string{wantUndefined}},
		{name: "an undefined test named twice is reported once",
			sentinel: remediation(ruleSets(postRemediationRuleSet("vm", "gpu-burn"), postRemediationRuleSet("bm", "gpu-burn"))),
			wantErrs: []string{wantUndefined}},
		{name: "a custom test lifecycle-manager defines, no DCGM needed", noOthers: true,
			sentinel: remediation(ruleSets(postRemediationRuleSet("reboot", "gpu-burn")), gpuBurn)},
		{name: "dcgm-diag-test deleted", sentinel: remediation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest, nil)),
			wantErrs: []string{wantDCGMUndefined}},
		{name: "dcgm-diag-test deleted by --set-json", sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + nodeValidationTestsPath + "." + nodeValidationDCGMTest + "=null"),
			wantErrs: []string{wantDCGMUndefined}},
		{name: "dcgm-diag-test deleted beside a custom test",
			sentinel: remediation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest, nil), gpuBurn),
			wantErrs: []string{wantDCGMUndefined}},
		{name: "every test deleted", sentinel: remediation(at(nodeValidationTestsPath, nil)),
			wantErrs: []string{wantDCGMUndefined}},
		{name: "tests not a map is left to the node-validation gate", noOthers: true,
			sentinel: remediation(at(nodeValidationTestsPath, true))},

		// The DCGM hostengine the chart's dcgm-diag-test calls.
		{name: "no GPU Operator", noOthers: true, sentinel: remediation(), wantErrs: []string{wantNoGPUOperator}},
		{name: "GPU Operator disabled", sentinel: remediation(),
			others:   []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, map[string]any{"enabled": false})},
			wantErrs: []string{wantGPUOpDisabled}},
		{name: "GPU Operator disabled by --set", sentinel: remediation(),
			cfg:      sets("gpuoperator", map[string]string{config.ComponentEnabledKey: "false"}),
			wantErrs: []string{wantGPUOpDisabled}},
		{name: "GPU Operator relocated", sentinel: remediation(),
			others:   []recipe.ComponentRef{gpuOperator("privileged-gpu-operator", dcgm(true))},
			wantErrs: []string{wantRelocated}},
		{name: "GPU Operator namespace unset", sentinel: remediation(),
			others: []recipe.ComponentRef{gpuOperator("", dcgm(true))}},
		{name: "dcgm.enabled false", sentinel: remediation(),
			others:   []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, dcgm(false))},
			wantErrs: []string{wantDCGMOff}},
		{name: "dcgm.enabled absent", sentinel: remediation(),
			others:   []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, nil)},
			wantErrs: []string{wantDCGMUnset}},
		{name: "dcgm.enabled true by --set", sentinel: remediation(),
			others: []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, nil)},
			cfg:    sets("gpuoperator", map[string]string{"dcgm.enabled": "true"})},
		{name: "overridden dcgm-diag-test command skips the DCGM check", noOthers: true,
			sentinel: remediation(at(dcgmCommandPath, stubCommand))},
		{name: "overridden dcgm-diag-test command by --set-json skips the DCGM check", noOthers: true,
			sentinel: remediation(),
			cfg:      setJSON(`nv-sentinel:` + dcgmCommandPath + `=["/bin/sh","-c","exit 0"]`)},
		{name: "dcgm-diag-test only in a disabled rule set needs no DCGM", noOthers: true,
			sentinel: remediation(ruleSets(withPath(postRemediationRuleSet("off", nodeValidationDCGMTest), "enabled", false),
				postRemediationRuleSet("reboot", "gpu-burn")), gpuBurn)},

		// Cordon label prefixes.
		{name: "fault-quarantine label prefix differs",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, customPrefix)), wantErrs: []string{wantFQPrefix}},
		{name: "lifecycle-manager label prefix differs",
			sentinel: remediation(at(postRemediationLMLabelPrefixPath, customPrefix)), wantErrs: []string{wantLMPrefix}},
		{name: "lifecycle-manager label prefix differs by --set", sentinel: remediation(),
			cfg:      sets("nv-sentinel", map[string]string{postRemediationLMLabelPrefixPath: customPrefix}),
			wantErrs: []string{wantLMPrefix}},
		{name: "fault-quarantine label prefix differs from the chart default",
			sentinel: withoutPath(remediation(at(postRemediationFQLabelPrefixPath, customPrefix)), postRemediationLMLabelPrefixPath),
			wantErrs: []string{wantFQPrefix}},
		{name: "both label prefixes changed together",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, customPrefix), at(postRemediationLMLabelPrefixPath, customPrefix))},
		{name: "fault-quarantine label prefix restating the chart's",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, postRemediationChartLabelPrefix))},
		{name: "both label prefixes left to the chart default",
			sentinel: withoutPath(remediation(), postRemediationLMLabelPrefixPath)},
		{name: "fault-quarantine label prefix null",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, nil)),
			wantErrs: []string{postRemediationFQLabelPrefixPath + " is <nil>, not a non-empty label prefix"}},
		{name: "fault-quarantine label prefix empty",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, "")),
			wantErrs: []string{postRemediationFQLabelPrefixPath + ` is "", not a non-empty label prefix`}},
		{name: "fault-quarantine label prefix a number",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, 7)),
			wantErrs: []string{postRemediationFQLabelPrefixPath + " is 7, not a non-empty label prefix"}},
		{name: "lifecycle-manager label prefix null",
			sentinel: remediation(at(postRemediationLMLabelPrefixPath, nil)),
			wantErrs: []string{postRemediationLMLabelPrefixPath + " is <nil>, not a non-empty label prefix"}},
		{name: "lifecycle-manager label prefix a number by --set-json", sentinel: remediation(),
			cfg:      setJSON("nv-sentinel:" + postRemediationLMLabelPrefixPath + "=7"),
			wantErrs: []string{postRemediationLMLabelPrefixPath + " is 7, not a non-empty label prefix"}},
		{name: "both label prefixes null",
			sentinel: remediation(at(postRemediationFQLabelPrefixPath, nil), at(postRemediationLMLabelPrefixPath, nil)),
			wantErrs: []string{postRemediationFQLabelPrefixPath + " is <nil>", postRemediationLMLabelPrefixPath + " is <nil>"}},

		// Findings accumulate.
		{name: "every finding at once", noOthers: true,
			sentinel: remediation(at(postRemediationCordonRemovePath, false), at(postRemediationFQLabelPrefixPath, customPrefix),
				ruleSets(nodeValidationDCGMTest, postRemediationRuleSet("custom", 7, "gpu-burn"),
					postRemediationRuleSet("reboot", nodeValidationDCGMTest))),
			wantErrs: []string{wantNoRemove, wantFQPrefix, wantNotRuleSet,
				postRemediationRuleSetsPath + "[1].tests[0] is 7, not a test name", wantUndefined, wantNoGPUOperator}},

		// --dynamic on a verified path.
		{name: "--dynamic validation toggle while off", noOthers: true, sentinel: map[string]any{},
			cfg: dynamic("nv-sentinel", postRemediationValidationEnabled), wantErrs: []string{wantDynamic}},
		{name: "--dynamic validation toggle while on", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", postRemediationValidationEnabled), wantErrs: []string{wantDynamic}},
		{name: "--dynamic validation section", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", postRemediationValidationPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic fault-quarantine section", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", "fault-quarantine"), wantErrs: []string{wantDynamic}},
		{name: "--dynamic validation ruleSets", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", postRemediationRuleSetsPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic validation ruleSets while off", noOthers: true,
			sentinel: remediation(at(postRemediationValidationEnabled, false)),
			cfg:      dynamic("nv-sentinel", postRemediationRuleSetsPath)},
		{name: "--dynamic fault-quarantine label prefix", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", postRemediationFQLabelPrefixPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic fault-quarantine label prefix while off", noOthers: true,
			sentinel: remediation(at(postRemediationValidationEnabled, false)),
			cfg:      dynamic("nv-sentinel", postRemediationFQLabelPrefixPath)},
		{name: "--dynamic on the canonical component name", sentinel: remediation(),
			cfg: dynamic(nvsentinelComponent, postRemediationFQLabelPrefixPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic on two verified paths", sentinel: remediation(),
			cfg:      dynamic("nv-sentinel", postRemediationValidationEnabled, postRemediationFQLabelPrefixPath),
			wantErrs: []string{wantDynamic, wantDynamic}},
		{name: "--dynamic GPU Operator dcgm.enabled", sentinel: remediation(),
			cfg: dynamic("gpuoperator", "dcgm.enabled"), wantErrs: []string{wantDynamic}},
		{name: "--dynamic GPU Operator dcgm section", sentinel: remediation(),
			cfg: dynamic("gpu-operator", "dcgm"), wantErrs: []string{wantDynamic}},
		{name: "--dynamic GPU Operator dcgm.enabled with an overridden command",
			sentinel: remediation(at(dcgmCommandPath, stubCommand)), cfg: dynamic("gpuoperator", "dcgm.enabled")},
		{name: "--dynamic GPU Operator dcgm.enabled with validation off",
			sentinel: remediation(at(postRemediationValidationEnabled, false)), cfg: dynamic("gpuoperator", "dcgm.enabled")},
		{name: "--dynamic on an unverified fault-quarantine path", sentinel: remediation(),
			cfg: dynamic("nv-sentinel", "fault-quarantine.circuitBreaker.percentage")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refs := []recipe.ComponentRef{{Name: nvsentinelComponent, Overrides: tt.sentinel}}
			switch {
			case tt.others != nil:
				refs = append(refs, tt.others...)
			case !tt.noOthers:
				refs = append(refs, healthyGPUOperator)
			}
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelPostRemediationValidationCoherent(t.Context(), nvsentinelComponent,
				&recipe.RecipeResult{ComponentRefs: refs}, cfg, nil)
			assertDatastoreFindings(t, "warning", warnings, tt.wantWarns)
			got := make([]string, 0, len(errs))
			for _, err := range errs {
				got = append(got, err.Error())
				errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			}
			assertDatastoreFindings(t, "error", got, tt.wantErrs)
		})
	}
}

// TestCheckNVSentinelPostRemediationValidationShippedMixins runs the gate on
// the values the shipped mixins resolve to: remediation passes beside
// node validation and blocks without it.
func TestCheckNVSentinelPostRemediationValidationShippedMixins(t *testing.T) {
	t.Parallel()
	store, err := recipe.LoadMetadataStoreFor(t.Context(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	gpuOperator := recipe.ComponentRef{Name: "gpu-operator", Namespace: nodeValidationDCGMNamespace,
		Overrides: map[string]any{"dcgm": map[string]any{"enabled": true}}}

	tests := []struct {
		name     string
		mixins   []string
		wantErrs []string
	}{
		{name: "remediation with node validation",
			mixins: []string{"nvsentinel-remediation", healthEventsAnalyzerMixinName, nodeValidationMixin}},
		{name: "remediation without node validation",
			mixins:   []string{"nvsentinel-remediation", healthEventsAnalyzerMixinName},
			wantErrs: []string{"compose the " + nodeValidationMixin + " mixin"}},
		{name: "quarantine leaves validation off", mixins: []string{"nvsentinel-quarantine"}},
		{name: "node validation alone", mixins: []string{nodeValidationMixin}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values := nvsentinelRenderedValues(t, store, tt.mixins...)
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: nvsentinelComponent, Overrides: values}, gpuOperator,
			}}
			_, errs := CheckNVSentinelPostRemediationValidationCoherent(t.Context(), nvsentinelComponent, rr,
				config.NewConfig(), nil)
			got := make([]string, 0, len(errs))
			for _, err := range errs {
				got = append(got, err.Error())
			}
			assertDatastoreFindings(t, "error", got, tt.wantErrs)
		})
	}
}

// TestCheckNVSentinelPostRemediationValidationSubset reads the GPU Operator
// from the declared union, so a bundlers= subset that leaves it out of this
// bundle still passes.
func TestCheckNVSentinelPostRemediationValidationSubset(t *testing.T) {
	t.Parallel()
	sentinel := recipe.ComponentRef{Name: nvsentinelComponent,
		Overrides: mergedValues(postRemediationValues(), nodeValidationValues())}
	declared := []recipe.ComponentRef{
		sentinel,
		{Name: "gpu-operator", Namespace: nodeValidationDCGMNamespace,
			Overrides: map[string]any{"dcgm": map[string]any{"enabled": true}}},
	}
	rr := (&recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{sentinel}}).WithDeclaredComponents(declared)
	warnings, errs := CheckNVSentinelPostRemediationValidationCoherent(t.Context(), nvsentinelComponent, rr,
		config.NewConfig(), nil)
	if len(warnings) != 0 || len(errs) != 0 {
		t.Errorf("bundlers= subset: got warnings %q errors %v, want none", warnings, errs)
	}
}

func TestCheckNVSentinelPostRemediationValidationNoNVSentinel(t *testing.T) {
	t.Parallel()
	cfg := config.NewConfig()
	if w, e := CheckNVSentinelPostRemediationValidationCoherent(t.Context(), nvsentinelComponent, nil, cfg, nil); w != nil || e != nil {
		t.Errorf("nil recipe: got %v %v, want nothing", w, e)
	}
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	if w, e := CheckNVSentinelPostRemediationValidationCoherent(t.Context(), nvsentinelComponent, r, cfg, nil); w != nil || e != nil {
		t.Errorf("no nvsentinel: got %v %v, want nothing", w, e)
	}
}

// TestPostRemediationRuleSetTestsHonorsCancellation pins that a long
// user-supplied rule set list is abandoned once the bundle's context ends,
// rather than checked in full.
func TestPostRemediationRuleSetTestsHonorsCancellation(t *testing.T) {
	t.Parallel()
	items := make([]any, 100_000)
	for i := range items {
		items[i] = postRemediationRuleSet(fmt.Sprintf("rule-set-%d", i), fmt.Sprintf("test-%d", i))
	}
	values := withPath(map[string]any{}, postRemediationRuleSetsPath, items)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var findings int
	tests, err := postRemediationRuleSetTests(ctx, values, func(string, ...any) { findings++ })
	if err == nil {
		t.Fatalf("postRemediationRuleSetTests returned %d tests and %d findings with a canceled context, want an error",
			len(tests), findings)
	}
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeCanceled)
}

// TestPostRemediationInnerLoopsHonorCancellation pins that a long readiness
// criteria list or match rule list is abandoned once the context ends.
func TestPostRemediationInnerLoopsHonorCancellation(t *testing.T) {
	t.Parallel()
	items := make([]any, 100_000)
	for i := range items {
		items[i] = map[string]any{"name": fmt.Sprintf("c-%d", i), "kind": postRemediationRuleKind, "expression": "true"}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	noop := func(string, ...any) {}

	readiness := withPath(map[string]any{}, postRemediationReadinessPath, items)
	if err := postRemediationReadinessViolations(ctx, readiness, noop); err == nil {
		t.Error("postRemediationReadinessViolations: got nil with a canceled context, want an error")
	} else {
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeCanceled)
	}
	if err := postRemediationMatchViolations(ctx, map[string]any{"all": items}, "ruleSets[0]", noop); err == nil {
		t.Error("postRemediationMatchViolations: got nil with a canceled context, want an error")
	} else {
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeCanceled)
	}
}

// TestNVSentinelPostRemediationValidationRegistrySeverity runs the gate through
// RunValidations with its registry entry: the note on a custom Job template
// stays a warning, and a finding still blocks.
func TestNVSentinelPostRemediationValidationRegistrySeverity(t *testing.T) {
	t.Parallel()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	var validations []recipe.ComponentValidationConfig
	for _, v := range registry.Get(nvsentinelComponent).Validations {
		if v.Function == "CheckNVSentinelPostRemediationValidationCoherent" {
			validations = append(validations, v)
		}
	}
	if len(validations) != 1 {
		t.Fatalf("registry declares the gate %d times, want 1", len(validations))
	}

	customTemplate := map[string]any{"lifecycle-manager": map[string]any{"config": map[string]any{
		"templates": map[string]any{"k8s-job-template.yaml": "kind: Job\n"}}}}
	release := func(value any) map[string]any {
		return withPath(map[string]any{}, postRemediationReleaseTaintsPath,
			[]any{map[string]any{"key": "a", "value": value, "effect": "NoSchedule", "remove": true}})
	}
	gpuOperator := recipe.ComponentRef{Name: "gpu-operator", Namespace: nodeValidationDCGMNamespace,
		Overrides: map[string]any{"dcgm": map[string]any{"enabled": true}}}

	tests := []struct {
		name               string
		value              any
		wantWarns, wantErr int
	}{
		{name: "a string value on a custom template is a warning", value: "true", wantWarns: 1},
		{name: "a typed value on a custom template still blocks", value: true, wantWarns: 1, wantErr: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			values := mergedValues(postRemediationValues(), nodeValidationValues(), customTemplate, release(tt.value))
			rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				{Name: nvsentinelComponent, Overrides: values}, gpuOperator,
			}}
			warnings, errs := RunValidations(t.Context(), nvsentinelComponent, validations, rr, config.NewConfig())
			if len(warnings) != tt.wantWarns || len(errs) != tt.wantErr {
				t.Errorf("got %d warnings %q and %d errors %v, want %d and %d",
					len(warnings), warnings, len(errs), errs, tt.wantWarns, tt.wantErr)
			}
		})
	}
}

// TestPostRemediationTaintViolationsHonorsContext checks that large unmatched
// release and quarantine lists stop at an ended context instead of finishing.
func TestPostRemediationTaintViolationsHonorsContext(t *testing.T) {
	t.Parallel()
	const n = 600
	rules := make([]any, n)
	releases := make([]any, n)
	for i := range n {
		rules[i] = map[string]any{"name": fmt.Sprintf("r%d", i),
			"taint": map[string]any{"key": fmt.Sprintf("q%d", i), "value": "x", "effect": "NoSchedule"}}
		releases[i] = map[string]any{"key": fmt.Sprintf("k%d", i), "value": "v", "effect": "NoSchedule", "remove": true}
	}
	values := withPath(withPath(map[string]any{}, postRemediationQuarantineRules, rules),
		postRemediationReleaseTaintsPath, releases)

	findings := 0
	if _, err := postRemediationTaintViolations(t.Context(), values, []string{nodeValidationDCGMTest},
		func(string, ...any) { findings++ }); err != nil || findings != n {
		t.Fatalf("live context: err %v, %d findings, want nil and %d", err, findings, n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := postRemediationTaintViolations(ctx, values, []string{nodeValidationDCGMTest}, func(string, ...any) {})
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeCanceled)
}
