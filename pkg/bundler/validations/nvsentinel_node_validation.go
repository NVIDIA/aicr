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
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Value paths CheckNVSentinelNodeValidationCoherent reads under nvsentinel,
// and the lifecycle-manager chart defaults (NVSentinel v1.28.0) that apply
// when AICR's values leave them unset.
const (
	nodeValidationLifecycleEnabledPath  = "global.lifecycleManager.enabled"
	nodeValidationLifecyclePath         = "lifecycle-manager"
	nodeValidationControllerPath        = "lifecycle-manager.controllers.validationRequest.enabled"
	nodeValidationCertProviderPath      = "lifecycle-manager.webhook.certProvider"
	nodeValidationNewNodePath           = "lifecycle-manager.config.newNodeValidation"
	nodeValidationCriteriaPath          = "lifecycle-manager.config.newNodeValidation.criteria"
	nodeValidationNewNodeTestsPath      = "lifecycle-manager.config.newNodeValidation.newNodeTests"
	nodeValidationDefaultTestsPath      = "lifecycle-manager.config.defaultTests"
	nodeValidationTestsPath             = "lifecycle-manager.config.tests"
	nodeValidationProvidersPath         = "lifecycle-manager.config.providers"
	nodeValidationChartProvider         = "k8s-job-provider"
	nodeValidationFullNamePath          = "lifecycle-manager.fullNameOverride"
	nodeValidationDeploymentName        = "lifecycle-manager"
	nodeValidationOpenShiftCertProvider = "openshift-service-ca"
	nodeValidationCertManager           = "cert-manager"
	nodeValidationDCGMTest              = "dcgm-diag-test"
	nodeValidationCordonedCriterion     = "cordoned"
	nodeValidationCordonedExpression    = "has(node.spec.unschedulable) && node.spec.unschedulable"
	nodeValidationDCGMNamespace         = "gpu-operator"
	nodeValidationMixin                 = "nvsentinel-node-validation"
)

// CheckNVSentinelNodeValidationCoherent blocks a bundle in which NVSentinel's
// new-node validation cannot run, so a new GPU node would stay cordoned with
// nothing to release it, or be tested while it is schedulable. With
// lifecycle-manager on, it fails when:
//
//   - fullNameOverride renames the Deployment the nvsentinel health check
//     asserts by name, so a failed rollout would pass it;
//   - lifecycle-manager.controllers.validationRequest.enabled is not a
//     boolean;
//   - with that controller on, lifecycle-manager.config.tests or one of its
//     entries is not a map, a test sets a command that is not a list of
//     strings, an image that is not a string or a provider the config does
//     not define, a test AICR adds names no provider, or no test is left once
//     Helm drops the null entries, which lifecycle-manager cannot decode or
//     rejects at startup;
//   - newNodeTests or defaultTests names a test the config does not define,
//     names one twice, or both lists are empty (a null list counts as empty,
//     since Helm deletes the chart's), which lifecycle-manager rejects at
//     startup;
//   - newNodeValidation is neither a map, null nor false, which renders a
//     config lifecycle-manager cannot decode, or new-node validation runs
//     (newNodeValidation a map) with criteria that lack one named cordoned
//     with the chart's expression, so a diagnostic would run on a schedulable
//     node;
//   - the new-node tests (newNodeTests, else defaultTests) include
//     dcgm-diag-test with the chart's command, which only reaches
//     nvidia-dcgm(-dra).gpu-operator.svc:5555, and no enabled GPU Operator in
//     the gpu-operator namespace runs dcgm.enabled: true.
//
// It warns when global.dryRun is true and new-node validation runs:
// lifecycle-manager does not read dryRun, so it runs real diagnostics and
// uncordons nodes that pass. It also warns when the webhook certificate
// comes from cert-manager (any certProvider but openshift-service-ca) and
// the bundle does not deploy cert-manager: the chart renders a cert-manager
// Issuer and Certificate, so the install fails without one, which the cluster
// may already run. It rejects --dynamic on
// global.lifecycleManager, on any lifecycle-manager value while it is on, and
// on the GPU Operator paths the DCGM rule depends on.
func CheckNVSentinelNodeValidationCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
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
		"NVSentinel node validation")
	if err != nil {
		return nil, []error{err}
	}

	lifecycle := nvsentinelSubchartRenders(values, "lifecycleManager")
	guarded := []string{nodeValidationLifecycleEnabledPath}
	if lifecycle {
		guarded = append(guarded, nodeValidationLifecyclePath)
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, componentName, sentinelKeys, guarded,
		"decides whether a new GPU node is validated before it takes work, and with which tests"); len(msgs) > 0 {
		return nil, nodeValidationDynamicErrors(componentName, msgs)
	}
	if !lifecycle {
		return nil, nil
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: ", componentName)+fmt.Sprintf(format, args...)))
	}

	var warnings []string
	if msg := nodeValidationCertManagerWarning(values, recipeResult, bundlerConfig); msg != "" {
		msg = fmt.Sprintf("component %q: %s", componentName, msg)
		slog.Warn(msg, logKeyComponent, componentName)
		warnings = append(warnings, msg)
	}
	if raw, present := component.GetValueByPath(values, nodeValidationFullNamePath); present && raw != nodeValidationDeploymentName {
		fail("%s is %#v: the nvsentinel health check asserts the %s Deployment by name, so a renamed one that fails "+
			"to roll out would pass it -- leave it at the chart's %q", nodeValidationFullNamePath, raw,
			nodeValidationDeploymentName, nodeValidationDeploymentName)
	}
	controller, controllerWellFormed := nodeValidationBool(values, nodeValidationControllerPath, true)
	if !controllerWellFormed {
		raw, _ := component.GetValueByPath(values, nodeValidationControllerPath)
		fail("%s is %#v, not a boolean", nodeValidationControllerPath, raw)
		return warnings, errs
	}
	if !controller {
		return warnings, errs
	}

	defined, wellFormed := nodeValidationDefinedTests(values, fail)
	if !wellFormed {
		return warnings, errs
	}
	defaultTests := nodeValidationTestList(values, nodeValidationDefaultTestsPath, defined, fail)
	newNode, newNodeWellFormed := nodeValidationNewNodeRuns(values)
	if !newNodeWellFormed {
		raw, _ := component.GetValueByPath(values, nodeValidationNewNodePath)
		fail("%s is %#v: the chart writes it into lifecycle-manager's config, which cannot decode it, so "+
			"lifecycle-manager exits at startup -- leave it to the mixin, or turn new-node validation off with "+
			"--set-json 'nv-sentinel:%s=null'", nodeValidationNewNodePath, raw, nodeValidationNewNodePath)
		return warnings, errs
	}
	if !newNode {
		return warnings, errs
	}

	nodeValidationCriteriaViolations(values, fail)
	tests := nodeValidationTestList(values, nodeValidationNewNodeTestsPath, defined, fail)
	if len(tests) == 0 {
		tests = defaultTests
	}
	if len(tests) == 0 {
		fail("%s and %s are both empty, which lifecycle-manager rejects at startup", nodeValidationNewNodeTestsPath,
			nodeValidationDefaultTestsPath)
	}
	if dryRun, _ := nvsentinelDryRun(values); dryRun {
		msg := fmt.Sprintf("component %q: global.dryRun is true, but lifecycle-manager does not read it: "+
			"new-node validation runs real DCGM diagnostics on cordoned GPU nodes and uncordons the ones that pass",
			componentName)
		slog.Warn(msg, logKeyComponent, componentName)
		warnings = append(warnings, msg)
	}

	commandPath := nodeValidationTestsPath + "." + nodeValidationDCGMTest + ".command"
	if command, _ := component.GetValueByPath(values, commandPath); command == nil && defined[nodeValidationDCGMTest] &&
		slices.Contains(tests, nodeValidationDCGMTest) {

		dynMsgs, dcgmErr := nodeValidationDCGMViolations(ctx, recipeResult, bundlerConfig, fail)
		if dcgmErr != nil {
			return warnings, append(errs, dcgmErr)
		}
		errs = append(errs, nodeValidationDynamicErrors(componentName, dynMsgs)...)
	}
	return warnings, errs
}

// nodeValidationDynamicErrors turns dynamic-guard messages into errors, since
// the gate is registered at warning severity to carry its dry-run note.
func nodeValidationDynamicErrors(componentName string, msgs []string) []error {
	errs := make([]error, 0, len(msgs))
	for _, msg := range msgs {
		slog.Warn(msg, logKeyComponent, componentName)
		errs = append(errs, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, msg))
	}
	return errs
}

// nodeValidationBool reads a boolean at path, returning def when it is
// absent. wellFormed is false for a present non-boolean.
func nodeValidationBool(values map[string]any, path string, def bool) (value, wellFormed bool) {
	raw, present := component.GetValueByPath(values, path)
	if !present {
		return def, true
	}
	b, isBool := raw.(bool)
	return b, isBool
}

// nodeValidationNewNodeRuns reports whether lifecycle-manager registers its
// new-node controller: the chart default turns it on, and a null or false
// newNodeValidation leaves it out of the controller's config. Any other
// non-map renders a config the controller cannot decode.
func nodeValidationNewNodeRuns(values map[string]any) (runs, wellFormed bool) {
	raw, present := component.GetValueByPath(values, nodeValidationNewNodePath)
	if !present {
		return true, true
	}
	if _, isMap := raw.(map[string]any); isMap {
		return true, true
	}
	return false, raw == nil || raw == false
}

// nodeValidationDefinedTests returns the tests lifecycle-manager's config
// will define: the chart's dcgm-diag-test merged with AICR's
// lifecycle-manager.config.tests, where a null deletes the chart's entry as
// Helm does. It fails on a definition lifecycle-manager cannot decode or
// accept at startup; wellFormed is false when the tests value itself is not
// a map, so nothing about the tests can be read.
func nodeValidationDefinedTests(values map[string]any, fail func(string, ...any)) (defined map[string]bool, wellFormed bool) {
	raw, present := component.GetValueByPath(values, nodeValidationTestsPath)
	defined = map[string]bool{nodeValidationDCGMTest: true}
	switch {
	case present && raw == nil:
		defined = map[string]bool{}
	case present:
		tests, isMap := raw.(map[string]any)
		if !isMap {
			fail("%s is %#v, not a map of test definitions, so lifecycle-manager cannot decode its config and exits "+
				"at startup", nodeValidationTestsPath, raw)
			return nil, false
		}
		providers := nodeValidationProviders(values, fail)
		for _, name := range slices.Sorted(maps.Keys(tests)) {
			path := nodeValidationTestsPath + "." + name
			test, isMap := tests[name].(map[string]any)
			switch {
			case tests[name] == nil:
				delete(defined, name)
				continue
			case !isMap:
				fail("%s is %#v, not a test definition, so lifecycle-manager cannot decode its config and exits at "+
					"startup", path, tests[name])
				continue
			}
			nodeValidationTestViolations(path, test, name != nodeValidationDCGMTest, providers, fail)
			defined[name] = true
		}
	}
	if len(defined) == 0 {
		fail("%s defines no test once Helm drops the null entries, which lifecycle-manager rejects at startup",
			nodeValidationTestsPath)
	}
	return defined, true
}

// nodeValidationProviders returns the provider names lifecycle-manager's
// config will define: the chart's k8s-job-provider merged with AICR's
// lifecycle-manager.config.providers.
func nodeValidationProviders(values map[string]any, fail func(string, ...any)) map[string]bool {
	known := map[string]bool{nodeValidationChartProvider: true}
	raw, present := component.GetValueByPath(values, nodeValidationProvidersPath)
	if !present {
		return known
	}
	providers, isMap := raw.(map[string]any)
	if raw != nil && !isMap {
		fail("%s is %#v, not a map of providers, so lifecycle-manager cannot decode its config and exits at startup",
			nodeValidationProvidersPath, raw)
	}
	if raw == nil {
		return map[string]bool{}
	}
	for name, provider := range providers {
		if provider == nil {
			delete(known, name)
		} else {
			known[name] = true
		}
	}
	return known
}

// nodeValidationTestViolations fails on a test definition lifecycle-manager
// cannot decode or accept at startup: a command that is not a list of
// strings, an image that is not a string, or a provider that is null or not
// one the config defines. A test AICR defines from scratch must name its
// provider; the chart's dcgm-diag-test keeps its own unless it is nulled.
func nodeValidationTestViolations(path string, test map[string]any, needsProvider bool, providers map[string]bool, fail func(string, ...any)) {
	if command, present := test["command"]; present && command != nil {
		items, isList := command.([]any)
		ok := isList
		for _, item := range items {
			if _, isString := item.(string); !isString {
				ok = false
			}
		}
		switch {
		case !ok:
			fail("%s.command is %#v, not a list of strings, so lifecycle-manager cannot decode its config and exits "+
				"at startup -- pass it as a list: --set-json 'nv-sentinel:%s.command=[\"sh\",\"-c\",\"...\"]'",
				path, command, path)
		case len(items) == 0:
			fail("%s.command is empty, so the test Job runs the image's entrypoint instead of a test -- set the "+
				"command, or leave it unset to keep the chart's", path)
		}
	}
	if image, present := test["image"]; present && image != nil {
		if _, isString := image.(string); !isString {
			fail("%s.image is %#v, not a string, so lifecycle-manager cannot decode its config and exits at startup",
				path, image)
		}
	}
	provider, present := test["provider"]
	switch {
	case present && provider == nil:
		fail("%s.provider is null, which Helm drops along with the chart's value, so lifecycle-manager rejects "+
			"the test at startup -- set it to %s or leave it unset", path, nodeValidationChartProvider)
		return
	case !present:
		if needsProvider {
			fail("%s names no provider, which lifecycle-manager rejects at startup -- set %s.provider to %s",
				path, path, nodeValidationChartProvider)
		}
		return
	}
	if name, isString := provider.(string); !isString || !providers[name] {
		fail("%s.provider is %#v, not a provider %s defines, which lifecycle-manager rejects at startup",
			path, provider, nodeValidationProvidersPath)
	}
}

// nodeValidationCriteriaViolations fails unless the effective criteria keep
// one named cordoned with the chart's expression, which is what stops a
// diagnostic running on a schedulable node.
func nodeValidationCriteriaViolations(values map[string]any, fail func(string, ...any)) {
	raw, present := component.GetValueByPath(values, nodeValidationCriteriaPath)
	if !present {
		return
	}
	criteria, _ := raw.([]any)
	for _, item := range criteria {
		criterion, _ := item.(map[string]any)
		name, _ := criterion["name"].(string)
		expression, _ := criterion["expression"].(string)
		if name == nodeValidationCordonedCriterion &&
			strings.Join(strings.Fields(expression), " ") == nodeValidationCordonedExpression {

			return
		}
	}
	why := "a new GPU node that is still schedulable would run the diagnostic while it takes work"
	if len(criteria) == 0 {
		why = "an empty list matches every node"
	}
	fail("%s does not include a criterion named %q with the chart's expression (%s): lifecycle-manager never "+
		"cordons a node itself, so without it %s -- keep the criterion the %s mixin sets",
		nodeValidationCriteriaPath, nodeValidationCordonedCriterion, nodeValidationCordonedExpression, why,
		nodeValidationMixin)
}

// nodeValidationTestList returns the test names in the list at path: the
// chart's [dcgm-diag-test] when it is absent, and none when it is null, which
// Helm treats as deleting the chart's list. It fails on a name
// lifecycle-manager would reject at startup.
func nodeValidationTestList(values map[string]any, path string, defined map[string]bool, fail func(string, ...any)) []string {
	raw, present := component.GetValueByPath(values, path)
	items := []any{nodeValidationDCGMTest}
	switch {
	case present && raw == nil:
		return nil
	case present:
		list, isList := raw.([]any)
		if !isList {
			fail("%s is %v, not a list of test names", path, raw)
			return nil
		}
		items = list
	}
	out := make([]string, 0, len(items))
	for i, item := range items {
		name, ok := item.(string)
		switch {
		case !ok:
			fail("%s[%d] is %#v, not a test name", path, i, item)
			continue
		case slices.Contains(out, name):
			fail("%s names %q twice, which lifecycle-manager rejects at startup", path, name)
		}
		if !defined[name] {
			fail("%s names the test %q, which is not defined, so lifecycle-manager rejects its config at startup "+
				"and crash-loops -- define it under %s.%s, with a provider, or remove it",
				path, name, nodeValidationTestsPath, name)
		}
		out = append(out, name)
	}
	return out
}

// nodeValidationCertManagerWarning reports when the webhook certificate is
// cert-manager's and this bundle deploys no cert-manager: the Deployment
// mounts the certificate Secret and does not start without it, unless the
// cluster already runs a cert-manager this bundle does not manage.
func nodeValidationCertManagerWarning(values map[string]any, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config) string {
	if raw, _ := component.GetValueByPath(values, nodeValidationCertProviderPath); raw == nodeValidationOpenShiftCertProvider {
		return ""
	}
	union := declaredUnionView(recipeResult)
	ref := union.GetComponentRef(nodeValidationCertManager)
	if ref != nil && !componentDisabled(ref, bundlerConfig, componentOverrideKeys(nodeValidationCertManager, union.DataProvider())) {
		return ""
	}
	return fmt.Sprintf("lifecycle-manager's admission webhook takes its certificate from cert-manager unless %s is %s, "+
		"and this bundle deploys no cert-manager: unless the cluster already runs one, the nvsentinel install fails on "+
		"lifecycle-manager's Issuer and Certificate (or, with the CRDs but no controller, lifecycle-manager never "+
		"starts and every ValidationRequest is rejected) -- deploy cert-manager, or on OpenShift set %s=%s",
		nodeValidationCertProviderPath, nodeValidationOpenShiftCertProvider, nodeValidationCertProviderPath,
		nodeValidationOpenShiftCertProvider)
}

// nodeValidationDCGMViolations fails unless an enabled GPU Operator in the
// gpu-operator namespace serves the standalone DCGM hostengine Service the
// chart's dcgm-diag-test command calls. dynMsgs are the --dynamic refusals on
// the GPU Operator paths that decision depends on.
func nodeValidationDCGMViolations(ctx context.Context, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, fail func(string, ...any)) (dynMsgs []string, err error) {
	union := declaredUnionView(recipeResult)
	gpuName, gpuOperator, gpuKeys := preflightDCGMServingGPUOperator(union, bundlerConfig, union.DataProvider())
	problem := func(reason string) {
		fail("%s runs with the chart's command, which calls DCGM only at nvidia-dcgm-dra.%s.svc:%s or "+
			"nvidia-dcgm.%s.svc:%s, but %s -- every test fails and the node stays cordoned. Deploy gpu-operator with "+
			"dcgm.enabled: true in %s, or override %s.%s.command",
			nodeValidationDCGMTest, nodeValidationDCGMNamespace, gpuOperatorDCGMPort, nodeValidationDCGMNamespace,
			gpuOperatorDCGMPort, reason, nodeValidationDCGMNamespace, nodeValidationTestsPath, nodeValidationDCGMTest)
	}
	if gpuOperator == nil {
		problem("no GPU Operator component is in the recipe")
		return nil, nil
	}
	if componentDisabled(gpuOperator, bundlerConfig, gpuKeys) {
		problem(gpuName + " is disabled")
		return nil, nil
	}
	namespace := gpuOperator.Namespace
	if namespace == "" {
		namespace = nodeValidationDCGMNamespace
	}
	if namespace != nodeValidationDCGMNamespace {
		problem(fmt.Sprintf("%s is deployed to %q", gpuName, namespace))
		return nil, nil
	}
	if msgs := nvsentinelDynamicGuardViolations(bundlerConfig, gpuName, gpuKeys, gpuOperatorDCGMGuardPaths,
		"decides whether the DCGM hostengine node validation depends on is deployed at all"); len(msgs) > 0 {
		return msgs, nil
	}
	gpuValues, err := effectiveComponentValues(ctx, union, bundlerConfig, gpuName, gpuKeys, "NVSentinel node validation DCGM")
	if err != nil {
		return nil, err
	}
	dcgmEnabled, why := ownershipToggle(gpuValues, "dcgm")
	switch {
	case why != "":
		problem(why)
	case dcgmEnabled == nil:
		problem(gpuName + " does not set dcgm.enabled, and the chart defaults it to false")
	case !*dcgmEnabled:
		problem(gpuName + " runs with dcgm.enabled: false, so the standalone hostengine Service is never created")
	}
	return nil, nil
}
