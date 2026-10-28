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
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nodeValidationValues returns what the nvsentinel-node-validation mixin sets.
func nodeValidationValues() map[string]any {
	return map[string]any{
		"global": map[string]any{"lifecycleManager": map[string]any{"enabled": true}},
		"lifecycle-manager": map[string]any{
			"controllers": map[string]any{
				"validationRequest":  map[string]any{"enabled": true},
				"maintenanceRequest": map[string]any{"enabled": false},
			},
			"config": map[string]any{
				"schedulingGate": map[string]any{"cordon": map[string]any{
					"remove": true, "labelPrefix": lifecycleManagerLabelPrefix,
				}},
				"newNodeValidation": map[string]any{
					"criteria": []any{
						nodeValidationCriterion("recently-joined", `has(node.metadata.creationTimestamp) && `+
							`now() - timestamp(node.metadata.creationTimestamp) < duration("30m")`),
						nodeValidationCriterion("gpu-present", `has(node.metadata.labels) && `+
							`node.metadata.labels["nvidia.com/gpu.present"] == "true"`),
						nodeValidationCriterion(nodeValidationCordonedCriterion, nodeValidationCordonedExpression),
					},
					"newNodeTests": []any{nodeValidationDCGMTest},
				},
			},
		},
	}
}

func nodeValidationCriterion(name, expression string) map[string]any {
	return map[string]any{"name": name, "expression": expression}
}

// mergedValues deep-merges each layer over the previous one, as composing
// mixins does.
func mergedValues(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, layer := range layers {
		mergeValuesForRender(out, layer)
	}
	return out
}

// withPath sets the dotted path in values, creating maps on the way.
func withPath(values map[string]any, path string, value any) map[string]any {
	keys := strings.Split(path, ".")
	cur := values
	for _, key := range keys[:len(keys)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[key] = next
		}
		cur = next
	}
	cur[keys[len(keys)-1]] = value
	return values
}

// withoutPath deletes the dotted path from values, leaving the chart default.
func withoutPath(values map[string]any, path string) map[string]any {
	keys := strings.Split(path, ".")
	cur := values
	for _, key := range keys[:len(keys)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return values
		}
		cur = next
	}
	delete(cur, keys[len(keys)-1])
	return values
}

func TestCheckNVSentinelNodeValidationCoherent(t *testing.T) {
	t.Parallel()

	gpuOperator := func(namespace string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gpu-operator", Namespace: namespace, Overrides: overrides}
	}
	dcgm := func(enabled any) map[string]any {
		return map[string]any{"dcgm": map[string]any{"enabled": enabled}}
	}
	healthyGPUOperator := gpuOperator(nodeValidationDCGMNamespace, dcgm(true))
	certManager := recipe.ComponentRef{Name: nodeValidationCertManager, Namespace: nodeValidationCertManager}
	disabledCertManager := recipe.ComponentRef{Name: nodeValidationCertManager,
		Overrides: map[string]any{"enabled": false}}
	onlyCertManager := []recipe.ComponentRef{certManager}

	// nodeValidation is the mixin composed onto nvsentinel, with the given
	// values on top.
	nodeValidation := func(overrides ...map[string]any) map[string]any {
		return mergedValues(append([]map[string]any{nodeValidationValues()}, overrides...)...)
	}
	at := func(path string, value any) map[string]any { return withPath(map[string]any{}, path, value) }

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

	stubCommand := []any{"/bin/sh", "-c", "exit 0"}
	dcgmCommandPath := nodeValidationTestsPath + "." + nodeValidationDCGMTest + ".command"
	gpuBurn := map[string]any{"provider": "k8s-job-provider", "image": "busybox:1.38.0", "command": stubCommand}
	// broken fails several rules at once, so a case that passes with it shows
	// the gate never looked.
	broken := func(overrides ...map[string]any) map[string]any {
		return nodeValidation(append([]map[string]any{
			at(nodeValidationCriteriaPath, []any{}), at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
		}, overrides...)...)
	}

	const (
		wantNotBool       = "not a boolean"
		wantUndefined     = `names the test "gpu-burn", which is not defined`
		wantNotTestList   = "not a list of test names"
		wantBothEmpty     = "are both empty, which lifecycle-manager rejects at startup"
		wantNoCordoned    = `does not include a criterion named "cordoned" with the chart's expression`
		wantNoGPUOperator = "no GPU Operator component is in the recipe"
		wantGPUOpDisabled = "gpu-operator is disabled"
		wantRelocated     = `gpu-operator is deployed to "privileged-gpu-operator"`
		wantDCGMUnset     = "does not set dcgm.enabled"
		wantDCGMOff       = "runs with dcgm.enabled: false"
		wantDCGMNotBool   = "dcgm.enabled=yes (string) is not a boolean"
		wantNoCertManager = "this bundle deploys no cert-manager"
		wantDryRun        = "global.dryRun is true, but lifecycle-manager does not read it"
		wantDynamic       = "--dynamic"
	)

	tests := []struct {
		name     string
		sentinel map[string]any
		// others replaces the default healthy GPU Operator and cert-manager.
		others       []recipe.ComponentRef
		noOthers     bool
		cfg          *config.Config
		wantWarnings []string
		wantErrs     []string
	}{
		// Inert.
		{name: "monitors only, nothing else in the recipe", sentinel: map[string]any{}, noOthers: true},
		{name: "lifecycle-manager off", noOthers: true,
			sentinel: broken(at(nodeValidationLifecycleEnabledPath, false))},
		{name: "nvsentinel disabled in the recipe", noOthers: true, sentinel: broken(at("enabled", false))},
		{name: "nvsentinel disabled by --set", noOthers: true, sentinel: broken(),
			cfg: sets("nv-sentinel", map[string]string{config.ComponentEnabledKey: "false"})},
		{name: "ValidationRequest controller off", others: onlyCertManager,
			sentinel: broken(at(nodeValidationControllerPath, false))},
		{name: "newNodeValidation null turns discovery off", others: onlyCertManager,
			sentinel: broken(at(nodeValidationNewNodePath, nil), at("global.dryRun", true))},
		{name: "newNodeValidation null still needs cert-manager", noOthers: true,
			sentinel: nodeValidation(at(nodeValidationNewNodePath, nil)), wantWarnings: []string{wantNoCertManager}},
		{name: "dry run without lifecycle-manager is silent", noOthers: true, sentinel: at("global.dryRun", true)},

		// The mixin as shipped.
		{name: "node validation", sentinel: nodeValidation()},
		{name: "lifecycle-manager on by --set", sentinel: withoutPath(nodeValidation(), nodeValidationLifecycleEnabledPath),
			cfg: sets("nv-sentinel", map[string]string{nodeValidationLifecycleEnabledPath: "true"})},

		// ValidationRequest controller.
		{name: "ValidationRequest controller a string",
			sentinel: nodeValidation(at(nodeValidationControllerPath, "true")),
			wantErrs: []string{nodeValidationControllerPath + ` is "true", ` + wantNotBool}},
		{name: "ValidationRequest controller left to the chart default",
			sentinel: withoutPath(nodeValidation(), nodeValidationControllerPath)},

		// newNodeTests.
		{name: "newNodeTests names an undefined test",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"})),
			wantErrs: []string{wantUndefined}},
		{name: "newNodeTests names a defined test, no DCGM needed", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationTestsPath+".gpu-burn", gpuBurn))},
		{name: "newNodeTests empty falls back to defaultTests",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{}))},
		{name: "newNodeTests empty by --set-json falls back to defaultTests", sentinel: nodeValidation(),
			cfg: setJSON("nv-sentinel:" + nodeValidationNewNodeTestsPath + "=[]")},
		{name: "newNodeTests and defaultTests both empty",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{}), at(nodeValidationDefaultTestsPath, []any{})),
			wantErrs: []string{wantBothEmpty}},
		{name: "empty newNodeTests runs the DCGM defaultTests", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{})),
			wantErrs: []string{wantNoGPUOperator}},
		{name: "newNodeTests names a test twice",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{nodeValidationDCGMTest, nodeValidationDCGMTest})),
			wantErrs: []string{`names "dcgm-diag-test" twice`}},
		{name: "defaultTests names an undefined test",
			sentinel: nodeValidation(at(nodeValidationDefaultTestsPath, []any{"gpu-burn"})),
			wantErrs: []string{wantUndefined}},
		{name: "defaultTests checked with new-node validation off", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodePath, nil), at(nodeValidationDefaultTestsPath, []any{"gpu-burn"})),
			wantErrs: []string{wantUndefined}},
		{name: "null dcgm-diag-test command deletes the chart's diagnostic", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".command", nil)),
			wantErrs: []string{"." + nodeValidationDCGMTest + ".command is null", wantNoGPUOperator}},
		{name: "null dcgm-diag-test image", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand), at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".image", nil)),
			wantErrs: []string{"." + nodeValidationDCGMTest + " sets no image"}},
		{name: "empty dcgm-diag-test image", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand), at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".image", " ")),
			wantErrs: []string{"." + nodeValidationDCGMTest + ".image is empty"}},
		{name: "an added test without an image", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"provider": "k8s-job-provider", "command": stubCommand})),
			wantErrs: []string{".gpu-burn sets no image"}},
		{name: "providers null leaves the chart's test without a provider",
			sentinel: nodeValidation(at(nodeValidationProvidersPath, nil)),
			wantErrs: []string{"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "a provider entry that is a boolean",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider", true)),
			wantErrs: []string{nodeValidationProvidersPath + ".k8s-job-provider is true, not a provider definition",
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "a provider entry that is a string",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider", "job")),
			wantErrs: []string{`.k8s-job-provider is "job", not a provider definition`,
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "a provider entry that is a list",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider", []any{"job"})),
			wantErrs: []string{"not a provider definition",
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "a provider entry that is an empty map", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand), at(nodeValidationProvidersPath+".k8s-job-provider", map[string]any{}))},
		{name: "the chart's provider nulled",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider", nil)),
			wantErrs: []string{"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "newNodeTests not a list",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, nodeValidationDCGMTest)),
			wantErrs: []string{wantNotTestList}},
		{name: "fullNameOverride renames the Deployment",
			sentinel: nodeValidation(at(nodeValidationFullNamePath, "renamed-lifecycle")),
			wantErrs: []string{nodeValidationFullNamePath + ` is "renamed-lifecycle"`}},
		{name: "fullNameOverride emptied falls back to a release-derived name",
			sentinel: nodeValidation(at(nodeValidationFullNamePath, "")),
			wantErrs: []string{nodeValidationFullNamePath + ` is ""`}},
		{name: "fullNameOverride restating the chart's name",
			sentinel: nodeValidation(at(nodeValidationFullNamePath, nodeValidationDeploymentName))},
		{name: "custom command as a string", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, "exit 0")),
			wantErrs: []string{".command is \"exit 0\", not a list of strings"}},
		{name: "custom command with a non-string item", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, []any{"sh", 7})),
			wantErrs: []string{"not a list of strings"}},
		{name: "custom command empty", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, []any{})),
			wantErrs: []string{".command is empty, so the test Job runs the image's entrypoint"}},
		{name: "a test image that is not a string", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand), at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".image", 7)),
			wantErrs: []string{".image is 7, not a string"}},
		{name: "newNodeValidation true", sentinel: nodeValidation(at(nodeValidationNewNodePath, true)),
			wantErrs: []string{nodeValidationNewNodePath + " is true: the chart writes it"}},
		{name: "newNodeValidation a string", sentinel: nodeValidation(at(nodeValidationNewNodePath, "on")),
			wantErrs: []string{nodeValidationNewNodePath + ` is "on": the chart writes it`}},
		{name: "newNodeValidation a list", sentinel: nodeValidation(at(nodeValidationNewNodePath, []any{1})),
			wantErrs: []string{"the chart writes it into lifecycle-manager's config"}},
		{name: "newNodeValidation false turns discovery off", others: onlyCertManager,
			sentinel: broken(at(nodeValidationNewNodePath, false))},
		{name: "tests not a map",
			sentinel: nodeValidation(at(nodeValidationTestsPath, true)),
			wantErrs: []string{nodeValidationTestsPath + " is true, not a map of test definitions"}},
		{name: "a test definition not a map",
			sentinel: nodeValidation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest, true)),
			wantErrs: []string{"." + nodeValidationDCGMTest + " is true, not a test definition",
				"defines no test once Helm drops the null entries",
				nodeValidationDefaultTestsPath + ` names the test "dcgm-diag-test", which is not defined`,
				nodeValidationNewNodeTestsPath + ` names the test "dcgm-diag-test", which is not defined`}},
		{name: "tests null deletes every test",
			sentinel: nodeValidation(at(nodeValidationTestsPath, nil)),
			wantErrs: []string{nodeValidationTestsPath + " is null, which Helm treats as deleting every test",
				nodeValidationDefaultTestsPath + ` names the test "dcgm-diag-test", which is not defined`,
				nodeValidationNewNodeTestsPath + ` names the test "dcgm-diag-test", which is not defined`}},
		{name: "dcgm-diag-test null deletes the chart's test", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest, nil),
				at(nodeValidationTestsPath+".gpu-burn", gpuBurn)),
			wantErrs: []string{nodeValidationDefaultTestsPath + ` names the test "dcgm-diag-test", which is not defined`,
				nodeValidationNewNodeTestsPath + ` names the test "dcgm-diag-test", which is not defined`}},
		{name: "an added test without a provider",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"image": "busybox:1.38.0", "command": stubCommand})),
			wantErrs: []string{".gpu-burn names no provider"}},
		{name: "dcgm-diag-test provider nulled",
			sentinel: nodeValidation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".provider", nil)),
			wantErrs: []string{"." + nodeValidationDCGMTest + ".provider is null"}},
		{name: "an added test with a null provider",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"provider": nil, "image": "busybox:1.38.0", "command": stubCommand})),
			wantErrs: []string{".gpu-burn.provider is null"}},
		{name: "dcgm-diag-test provider restated", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand),
				at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".provider", nodeValidationChartProvider))},
		{name: "a test naming an undefined provider", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"provider": "nvcre", "image": "busybox:1.38.0", "command": stubCommand})),
			wantErrs: []string{`.gpu-burn.provider is "nvcre", not a provider`}},
		{name: "a test naming a provider AICR defines", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationProvidersPath+".nvcre", validProvider()),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"provider": "nvcre", "image": "busybox:1.38.0", "command": stubCommand}))},
		{name: "providers not a map",
			sentinel: nodeValidation(at(nodeValidationProvidersPath, "k8s-job-provider"), at(nodeValidationTestsPath+".gpu-burn", gpuBurn)),
			wantErrs: []string{nodeValidationProvidersPath + ` is "k8s-job-provider", not a map of providers`,
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`,
				`.gpu-burn.provider is "k8s-job-provider", not a provider`}},
		{name: "the chart provider's templateFile nulled",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider.templateFile", nil)),
			wantErrs: []string{".k8s-job-provider.templateFile is <nil>, not a non-empty string",
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "the chart provider's timeout set to 0",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider.timeoutSeconds", 0)),
			wantErrs: []string{".k8s-job-provider.timeoutSeconds is 0, not a whole number above 0",
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "the chart provider's retries negative",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider.retries", -1)),
			wantErrs: []string{".k8s-job-provider.retries is -1, not a whole number of at least 0",
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "the chart provider's failed condition type nulled",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider.failedCondition.type", nil)),
			wantErrs: []string{".k8s-job-provider.failedCondition.type is <nil>, not a non-empty string",
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "the chart provider's success status not a condition status",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider.successfulCondition.status", "Passed")),
			wantErrs: []string{`.k8s-job-provider.successfulCondition.status is "Passed", not one of True, False, Unknown`,
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "an added provider without retries", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationProvidersPath+".nvcre", validProvider()),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"provider": "nvcre", "image": "busybox:1.38.0", "command": stubCommand}))},
		{name: "the chart provider's batching flag a string",
			sentinel: nodeValidation(at(nodeValidationProvidersPath+".k8s-job-provider.supportsTestBatching", "yes")),
			wantErrs: []string{`.k8s-job-provider.supportsTestBatching is "yes", not a boolean`,
				"." + nodeValidationDCGMTest + `.provider is "k8s-job-provider", not a provider`}},
		{name: "the chart provider tuned, as the e2e does", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand),
				at(nodeValidationProvidersPath+".k8s-job-provider.retries", 0),
				at(nodeValidationProvidersPath+".k8s-job-provider.timeoutSeconds", 180))},
		{name: "an added provider missing its fields",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{"gpu-burn"}),
				at(nodeValidationProvidersPath+".nvcre", map[string]any{"kind": "Job"}),
				at(nodeValidationTestsPath+".gpu-burn", map[string]any{"provider": "nvcre", "image": "busybox:1.38.0", "command": stubCommand})),
			wantErrs: []string{".nvcre.apiGroup is <nil>", ".nvcre.version is <nil>", ".nvcre.resource is <nil>",
				".nvcre.templateFile is <nil>", ".nvcre.timeoutSeconds is <nil>", ".nvcre.successfulCondition.type is <nil>",
				".nvcre.successfulCondition.status is <nil>", ".nvcre.failedCondition.type is <nil>",
				".nvcre.failedCondition.status is <nil>", `.gpu-burn.provider is "nvcre", not a provider`}},
		{name: "null newNodeTests and null defaultTests are both empty",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, nil), at(nodeValidationDefaultTestsPath, nil)),
			wantErrs: []string{wantBothEmpty}},
		{name: "empty newNodeTests with null defaultTests are both empty",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{}), at(nodeValidationDefaultTestsPath, nil)),
			wantErrs: []string{wantBothEmpty}},
		{name: "null newNodeTests runs a custom defaultTests, no DCGM needed", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, nil), at(nodeValidationDefaultTestsPath, []any{"gpu-burn"}),
				at(nodeValidationTestsPath+".gpu-burn", gpuBurn))},
		{name: "newNodeTests names a non-string",
			sentinel: nodeValidation(at(nodeValidationNewNodeTestsPath, []any{7})),
			wantErrs: []string{nodeValidationNewNodeTestsPath + "[0] is 7, not a test name"}},
		{name: "newNodeTests left to the chart default",
			sentinel: withoutPath(nodeValidation(), nodeValidationNewNodeTestsPath)},

		// New-node criteria.
		{name: "criteria without cordoned",
			sentinel: nodeValidation(at(nodeValidationCriteriaPath, []any{
				nodeValidationCriterion("recently-joined", "true"), nodeValidationCriterion("gpu-present", "true"),
			})),
			wantErrs: []string{wantNoCordoned}},
		{name: "criteria empty", sentinel: nodeValidation(at(nodeValidationCriteriaPath, []any{})),
			wantErrs: []string{wantNoCordoned}},
		{name: "cordoned expression under another name",
			sentinel: nodeValidation(at(nodeValidationCriteriaPath, []any{
				nodeValidationCriterion("is-cordoned", nodeValidationCordonedExpression),
			})),
			wantErrs: []string{wantNoCordoned}},
		{name: "cordoned name with a different expression",
			sentinel: nodeValidation(at(nodeValidationCriteriaPath, []any{
				nodeValidationCriterion(nodeValidationCordonedCriterion, "true"),
			})),
			wantErrs: []string{wantNoCordoned}},
		{name: "cordoned expression reflowed",
			sentinel: nodeValidation(at(nodeValidationCriteriaPath, []any{
				nodeValidationCriterion(nodeValidationCordonedCriterion,
					"has(node.spec.unschedulable)\n  &&\tnode.spec.unschedulable\n"),
			}))},
		{name: "criteria left to the chart default",
			sentinel: withoutPath(nodeValidation(), nodeValidationCriteriaPath)},
		{name: "newNodeValidation left to the chart default",
			sentinel: withoutPath(nodeValidation(), nodeValidationNewNodePath)},

		// The DCGM hostengine dcgm-diag-test calls.
		{name: "no GPU Operator", others: onlyCertManager, sentinel: nodeValidation(),
			wantErrs: []string{wantNoGPUOperator}},
		{name: "chart-default newNodeTests still needs DCGM", others: onlyCertManager,
			sentinel: withoutPath(nodeValidation(), nodeValidationNewNodeTestsPath),
			wantErrs: []string{wantNoGPUOperator}},
		{name: "GPU Operator disabled", sentinel: nodeValidation(),
			others: []recipe.ComponentRef{
				gpuOperator(nodeValidationDCGMNamespace, map[string]any{"enabled": false}), certManager,
			},
			wantErrs: []string{wantGPUOpDisabled}},
		{name: "GPU Operator disabled by --set", sentinel: nodeValidation(),
			cfg:      sets("gpuoperator", map[string]string{config.ComponentEnabledKey: "false"}),
			wantErrs: []string{wantGPUOpDisabled}},
		{name: "OCP GPU Operator serves DCGM", sentinel: nodeValidation(),
			others: []recipe.ComponentRef{
				gpuOperator(nodeValidationDCGMNamespace, map[string]any{"enabled": false}),
				{Name: "gpu-operator-ocp", Namespace: nodeValidationDCGMNamespace, Overrides: dcgm(true)},
				certManager,
			}},
		{name: "OCP GPU Operator with DCGM off", sentinel: nodeValidation(),
			others: []recipe.ComponentRef{
				gpuOperator(nodeValidationDCGMNamespace, map[string]any{"enabled": false}),
				{Name: "gpu-operator-ocp", Namespace: nodeValidationDCGMNamespace, Overrides: dcgm(false)},
				certManager,
			},
			wantErrs: []string{wantDCGMOff}},
		{name: "GPU Operator relocated", sentinel: nodeValidation(),
			others:   []recipe.ComponentRef{gpuOperator("privileged-gpu-operator", dcgm(true)), certManager},
			wantErrs: []string{wantRelocated}},
		{name: "GPU Operator namespace unset", sentinel: nodeValidation(),
			others: []recipe.ComponentRef{gpuOperator("", dcgm(true)), certManager}},
		{name: "dcgm.enabled false", sentinel: nodeValidation(),
			others:   []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, dcgm(false)), certManager},
			wantErrs: []string{wantDCGMOff}},
		{name: "dcgm.enabled absent", sentinel: nodeValidation(),
			others:   []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, nil), certManager},
			wantErrs: []string{wantDCGMUnset}},
		{name: "dcgm.enabled a string", sentinel: nodeValidation(),
			others:   []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, dcgm("yes")), certManager},
			wantErrs: []string{wantDCGMNotBool}},
		{name: "dcgm.enabled true by --set", sentinel: nodeValidation(),
			others: []recipe.ComponentRef{gpuOperator(nodeValidationDCGMNamespace, nil), certManager},
			cfg:    sets("gpuoperator", map[string]string{"dcgm.enabled": "true"})},
		{name: "overridden dcgm-diag-test command skips the DCGM check", others: onlyCertManager,
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand))},
		{name: "overridden dcgm-diag-test image alone keeps the DCGM check", others: onlyCertManager,
			sentinel: nodeValidation(at(nodeValidationTestsPath+"."+nodeValidationDCGMTest+".image", "busybox:1.38.0")),
			wantErrs: []string{wantNoGPUOperator}},

		// cert-manager.
		{name: "cert-manager absent", others: []recipe.ComponentRef{healthyGPUOperator}, sentinel: nodeValidation(),
			wantWarnings: []string{wantNoCertManager}},
		{name: "cert-manager disabled", others: []recipe.ComponentRef{healthyGPUOperator, disabledCertManager},
			sentinel: nodeValidation(), wantWarnings: []string{wantNoCertManager}},
		{name: "cert-manager disabled by --set", sentinel: nodeValidation(),
			cfg:          sets("certmanager", map[string]string{config.ComponentEnabledKey: "false"}),
			wantWarnings: []string{wantNoCertManager}},
		{name: "cert-manager named explicitly as the provider", others: []recipe.ComponentRef{healthyGPUOperator},
			sentinel:     nodeValidation(at(nodeValidationCertProviderPath, "cert-manager")),
			wantWarnings: []string{wantNoCertManager}},
		{name: "OpenShift service CA needs no cert-manager", others: []recipe.ComponentRef{healthyGPUOperator},
			sentinel: nodeValidation(at(nodeValidationCertProviderPath, "openshift-service-ca"))},

		// Findings accumulate.
		{name: "every new-node finding at once", noOthers: true,
			sentinel:     broken(),
			wantWarnings: []string{wantNoCertManager}, wantErrs: []string{wantNoCordoned, wantUndefined}},

		// Dry run.
		{name: "dry run with new-node validation warns", sentinel: nodeValidation(at("global.dryRun", true)),
			wantWarnings: []string{wantDryRun}},
		{name: "dry run with new-node validation still blocks what it must",
			sentinel:     nodeValidation(at("global.dryRun", true), at(nodeValidationCriteriaPath, []any{})),
			wantWarnings: []string{wantDryRun}, wantErrs: []string{wantNoCordoned}},
		{name: "dry run false is silent", sentinel: nodeValidation(at("global.dryRun", false))},

		// --dynamic on a verified path.
		{name: "--dynamic lifecycle-manager toggle while off", noOthers: true, sentinel: map[string]any{},
			cfg: dynamic("nv-sentinel", nodeValidationLifecycleEnabledPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic global.lifecycleManager section while off", noOthers: true, sentinel: map[string]any{},
			cfg: dynamic("nv-sentinel", "global.lifecycleManager"), wantErrs: []string{wantDynamic}},
		{name: "--dynamic lifecycle-manager value while off", noOthers: true, sentinel: map[string]any{},
			cfg: dynamic("nv-sentinel", nodeValidationNewNodeTestsPath)},
		{name: "--dynamic lifecycle-manager toggle while on", sentinel: nodeValidation(),
			cfg: dynamic("nv-sentinel", nodeValidationLifecycleEnabledPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic lifecycle-manager value", sentinel: nodeValidation(),
			cfg: dynamic("nv-sentinel", nodeValidationNewNodeTestsPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic lifecycle-manager section", sentinel: nodeValidation(),
			cfg: dynamic("nv-sentinel", nodeValidationLifecyclePath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic lifecycle-manager value while the controller is off", noOthers: true,
			sentinel: nodeValidation(at(nodeValidationControllerPath, false)),
			cfg:      dynamic("nv-sentinel", nodeValidationControllerPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic on the canonical component name", sentinel: nodeValidation(),
			cfg: dynamic(nvsentinelComponent, nodeValidationCriteriaPath), wantErrs: []string{wantDynamic}},
		{name: "--dynamic GPU Operator dcgm.enabled", sentinel: nodeValidation(),
			cfg: dynamic("gpuoperator", "dcgm.enabled"), wantErrs: []string{wantDynamic}},
		{name: "--dynamic GPU Operator enabled", sentinel: nodeValidation(),
			cfg: dynamic("gpu-operator", "enabled"), wantErrs: []string{wantDynamic}},
		{name: "--dynamic GPU Operator dcgm.enabled with an overridden command",
			sentinel: nodeValidation(at(dcgmCommandPath, stubCommand)),
			cfg:      dynamic("gpuoperator", "dcgm.enabled")},
		{name: "--dynamic GPU Operator dcgm.enabled without node validation", sentinel: map[string]any{},
			cfg: dynamic("gpuoperator", "dcgm.enabled")},
		{name: "--dynamic on an unverified nvsentinel path", sentinel: nodeValidation(),
			cfg: dynamic("nv-sentinel", "fault-quarantine.labelPrefix")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			refs := []recipe.ComponentRef{{Name: nvsentinelComponent, Overrides: tt.sentinel}}
			switch {
			case tt.others != nil:
				refs = append(refs, tt.others...)
			case !tt.noOthers:
				refs = append(refs, healthyGPUOperator, certManager)
			}
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelNodeValidationCoherent(t.Context(), nvsentinelComponent,
				&recipe.RecipeResult{ComponentRefs: refs}, cfg, nil)
			assertDatastoreFindings(t, "warning", warnings, tt.wantWarnings)
			got := make([]string, 0, len(errs))
			for _, err := range errs {
				got = append(got, err.Error())
				if code := errorstest.ReportedCode(err); code != aicrerrors.ErrCodeInvalidRequest {
					t.Errorf("error %q reports code %s, want %s", err, code, aicrerrors.ErrCodeInvalidRequest)
				}
			}
			assertDatastoreFindings(t, "error", got, tt.wantErrs)
		})
	}
}

// TestCheckNVSentinelNodeValidationCoherentSubset reads the GPU Operator and
// cert-manager from the declared union, so a bundlers= subset that leaves
// them out of this bundle still passes.
func TestCheckNVSentinelNodeValidationCoherentSubset(t *testing.T) {
	t.Parallel()
	sentinel := recipe.ComponentRef{Name: nvsentinelComponent, Overrides: nodeValidationValues()}
	declared := []recipe.ComponentRef{
		sentinel,
		{Name: "gpu-operator", Namespace: nodeValidationDCGMNamespace,
			Overrides: map[string]any{"dcgm": map[string]any{"enabled": true}}},
		{Name: nodeValidationCertManager},
	}
	rr := (&recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{sentinel}}).WithDeclaredComponents(declared)
	warnings, errs := CheckNVSentinelNodeValidationCoherent(t.Context(), nvsentinelComponent, rr, config.NewConfig(), nil)
	if len(warnings) != 0 || len(errs) != 0 {
		t.Errorf("bundlers= subset: got warnings %q errors %v, want none", warnings, errs)
	}
}

func TestCheckNVSentinelNodeValidationCoherentNoNVSentinel(t *testing.T) {
	t.Parallel()
	if w, e := CheckNVSentinelNodeValidationCoherent(t.Context(), nvsentinelComponent, nil, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("nil recipe: got %v %v, want nothing", w, e)
	}
	r := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	if w, e := CheckNVSentinelNodeValidationCoherent(t.Context(), nvsentinelComponent, r, config.NewConfig(), nil); w != nil || e != nil {
		t.Errorf("no nvsentinel: got %v %v, want nothing", w, e)
	}
}

// TestNodeValidationTestListHonorsCancellation pins that a long user-supplied
// list is abandoned once the bundle's context ends, rather than checked in
// full.
func TestNodeValidationTestListHonorsCancellation(t *testing.T) {
	t.Parallel()
	items := make([]any, 100_000)
	for i := range items {
		items[i] = fmt.Sprintf("test-%d", i)
	}
	values := map[string]any{"lifecycle-manager": map[string]any{"config": map[string]any{"defaultTests": items}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var findings int
	tests, err := nodeValidationTestList(ctx, values, nodeValidationDefaultTestsPath, map[string]bool{},
		func(string, ...any) { findings++ })
	if err == nil {
		t.Fatalf("nodeValidationTestList returned %d tests and %d findings with a canceled context, want an error",
			len(tests), findings)
	}
	if code := errorstest.ReportedCode(err); code != aicrerrors.ErrCodeCanceled {
		t.Errorf("error code = %s, want %s", code, aicrerrors.ErrCodeCanceled)
	}
}

// validProvider returns a complete provider definition, as the chart's
// k8s-job-provider is.
func validProvider() map[string]any {
	return map[string]any{
		"apiGroup": "batch", "version": "v1", "kind": "Job", "resource": "jobs",
		"templateFile": "k8s-job-template.yaml", "timeoutSeconds": 600,
		"successfulCondition": map[string]any{"type": "Complete", "status": "True"},
		"failedCondition":     map[string]any{"type": "Failed", "status": "True"},
	}
}
