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
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// What the render test reads in the chart's output: fault-quarantine's
// objects, the ValidationRequest group and resource it creates on
// unquarantine, the mixin's rule set name, the recommended actions it
// validates (RESTART_VM and RESTART_BM), and the config section header.
const (
	faultQuarantineName       = "fault-quarantine"
	validationRequestGroup    = "nvsentinel.nvidia.com"
	validationRequestResource = "validationrequests"
	postRebootRuleSetName     = "Post-reboot validation"
	postRebootActions         = "15, 24"
	validationRuleSetHeader   = "[[validation.ruleSets]]"
)

var (
	recommendedActionsRe = regexp.MustCompile(`event\.recommendedAction in \[([0-9, ]*)\]`)
	quarantineTaintRe    = regexp.MustCompile(`\[rule-sets\.taint\]\s*key = "([^"]*)"\s*value = "([^"]*)"\s*effect = "([^"]*)"`)
)

// faultQuarantineConfig returns fault-quarantine's rendered config.toml.
func faultQuarantineConfig(t *testing.T, rendered string) string {
	t.Helper()
	configMap := renderedObject(t, rendered, "ConfigMap", faultQuarantineName)
	data, _ := configMap["data"].(map[string]any)
	raw, _ := data["config.toml"].(string)
	if raw == "" {
		t.Fatal("fault-quarantine ConfigMap has no config.toml")
	}
	return raw
}

// grantsValidationRequestCreate reports whether the fault-quarantine
// ClusterRole lets it create ValidationRequests.
func grantsValidationRequestCreate(t *testing.T, rendered string) bool {
	t.Helper()
	role := renderedObject(t, rendered, "ClusterRole", faultQuarantineName)
	rules, _ := role["rules"].([]any)
	for _, item := range rules {
		rule, _ := item.(map[string]any)
		if slices.Contains(stringList(rule["apiGroups"]), validationRequestGroup) &&
			slices.Contains(stringList(rule["resources"]), validationRequestResource) &&
			slices.Contains(stringList(rule["verbs"]), "create") {

			return true
		}
	}
	return false
}

// TestNVSentinelPostRemediationValidationChartRender renders the pinned
// nvsentinel chart with the remediation step as shipped (remediation,
// health-events-analyzer and node-validation mixins) and asserts what #3089
// promises reaches fault-quarantine: a [validation] section that is on, with
// the post-reboot rule set requesting dcgm-diag-test for RESTART_VM and
// RESTART_BM only, and RBAC to create the ValidationRequest. Without the
// grant, creation fails and the repaired node stays quarantined.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelNodeValidationChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelPostRemediationValidationChartRender`.
func TestNVSentinelPostRemediationValidationChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store,
		"nvsentinel-remediation", healthEventsAnalyzerMixinName, nodeValidationMixin))

	renderedObject(t, out, "Deployment", lifecycleManagerName)
	cfg := faultQuarantineConfig(t, out)
	_, validation, found := strings.Cut(cfg, "\n[validation]\n")
	if !found {
		t.Fatalf("fault-quarantine config.toml has no [validation] section; a repaired node returns untested:\n%s", cfg)
	}
	header, ruleSetsTOML, _ := strings.Cut(validation, validationRuleSetHeader)
	for _, line := range []string{
		"enabled = true",
		`apiGroup = "` + validationRequestGroup + `"`,
		`kind = "ValidationRequest"`,
		`resource = "` + validationRequestResource + `"`,
		"partialDrainEnabled = false",
	} {
		if !strings.Contains(header, line) {
			t.Errorf("fault-quarantine [validation] does not carry %s:\n%s", line, header)
		}
	}

	ruleSets := strings.Split(validationRuleSetHeader+ruleSetsTOML, validationRuleSetHeader)[1:]
	if len(ruleSets) != 1 {
		t.Fatalf("fault-quarantine renders %d validation rule sets, want only %q:\n%s",
			len(ruleSets), postRebootRuleSetName, validation)
	}
	ruleSet := ruleSets[0]
	if m := ruleSetName.FindStringSubmatch(ruleSet); m == nil || m[1] != postRebootRuleSetName {
		t.Errorf("validation rule set name = %v, want %q", m, postRebootRuleSetName)
	}
	for _, line := range []string{"enabled = true", `tests = ["` + nodeValidationDCGMTest + `"]`} {
		if !strings.Contains(ruleSet, line) {
			t.Errorf("validation rule set %q does not carry %s:\n%s", postRebootRuleSetName, line, ruleSet)
		}
	}
	actions := recommendedActionsRe.FindAllStringSubmatch(ruleSet, -1)
	if len(actions) != 1 || actions[0][1] != postRebootActions {
		t.Errorf("validation rule set %q matches recommendedAction %v, want only [%s] (RESTART_VM, RESTART_BM)",
			postRebootRuleSetName, actions, postRebootActions)
	}
	// The gate assumes these are the chart's only quarantine taints when AICR
	// leaves fault-quarantine.ruleSets unset; a chart bump that adds or changes
	// one fails here rather than leaving a node tainted after it passes.
	matches := quarantineTaintRe.FindAllStringSubmatch(cfg, -1)
	chartTaints := make([]postRemediationTaint, 0, len(matches))
	for _, m := range matches {
		chartTaints = append(chartTaints, postRemediationTaint{key: m[1], value: m[2], effect: m[3]})
	}
	if !slices.Equal(chartTaints, postRemediationChartQuarantineTaints) {
		t.Errorf("fault-quarantine rule sets apply taints %v, want %v (postRemediationChartQuarantineTaints)",
			chartTaints, postRemediationChartQuarantineTaints)
	}
	if !grantsValidationRequestCreate(t, out) {
		t.Errorf("fault-quarantine ClusterRole does not grant create on %s.%s; ValidationRequest creation fails "+
			"and the repaired node stays quarantined", validationRequestResource, validationRequestGroup)
	}

	// Negative control: quarantine alone leaves validation off, so both
	// assertions above would fail on it.
	quarantine := renderNVSentinel(t, nvsentinelRenderedValues(t, store, "nvsentinel-quarantine"))
	if strings.Contains(faultQuarantineConfig(t, quarantine), "[validation]") {
		t.Error("nvsentinel-quarantine renders a [validation] section; only the remediation step validates")
	}
	if grantsValidationRequestCreate(t, quarantine) {
		t.Errorf("nvsentinel-quarantine grants create on %s", validationRequestResource)
	}
}
