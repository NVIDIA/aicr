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

const (
	extRemediationTemplateKey = "external-remediation"
	extRemediationResource    = "externalremediationrequests"
	objectMonitorConfigMap    = "kubernetes-object-monitor"
	extRemediationPolicyName  = "vendor-node-repair"
)

// extRemediationActionLines are the rendered [remediationActions."external-remediation"]
// lines, exactly; upstream's contract leaves out impactedEntityScope,
// namespace and supersedingEquivalenceGroups.
var extRemediationActionLines = []string{
	`apiGroup = "nvsentinel.dgxc.nvidia.com"`,
	`version = "v1"`,
	`kind = "ExternalRemediationRequest"`,
	`scope = "Cluster"`,
	`completeConditionType = "ExternalRemediationComplete"`,
	`templateFileName = "external-remediation"`,
	`equivalenceGroup = "external-remediation"`,
}

// TestNVSentinelExternalRemediationChartRender renders the pinned chart with
// the nvsentinel-external-remediation mixin beside the remediation step and
// beside the observe step, and asserts what reaches fault-remediation: the
// action with exactly upstream's fields, the request template with the
// preserve annotation and the extrr- name, and RBAC to create requests. The
// negative controls render the remediation and observe steps and the shipped
// base without it and require none of that: the routing exists only when an
// operator composes the mixin. It also renders a declared fault's
// kubernetes-object-monitor policy (testObjectMonitorPolicyRender).
//
// Pulls the chart over the network, so -short skips it; the weekly
// .github/workflows/nvsentinel-observability-render-check.yaml (NVSentinel
// Mixin Render Checks) runs it.
func TestNVSentinelExternalRemediationChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}

	for _, step := range []string{"nvsentinel-remediation", "nvsentinel-observe"} {
		t.Run(step+"+"+extRemediationMixinName, func(t *testing.T) {
			out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, step, extRemediationMixinName))
			data, ok := faultRemediationConfigData(t, out)
			if !ok {
				t.Fatal("fault-remediation ConfigMap not rendered")
			}
			config, _ := data["config.toml"].(string)
			block, ok := remediationActionBlock(config, extRemediationMixinActionKey)
			if !ok {
				t.Fatalf(`config.toml has no [remediationActions.%q] table:\n%s`, extRemediationMixinActionKey, config)
			}
			var got []string
			for _, line := range strings.Split(block, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					got = append(got, line)
				}
			}
			if !slices.Equal(got, extRemediationActionLines) {
				t.Errorf("[remediationActions.%q] = %q, want exactly %q", extRemediationMixinActionKey, got, extRemediationActionLines)
			}
			// The COMPONENT_RESET action the step sets survives beside it.
			if _, found := remediationActionBlock(config, "COMPONENT_RESET"); !found {
				t.Error("COMPONENT_RESET action lost beside the external-remediation action")
			}

			tmpl, ok := data[extRemediationTemplateKey].(string)
			if !ok {
				t.Fatalf("fault-remediation ConfigMap has no %s key; fault-remediation refuses to start without it", extRemediationTemplateKey)
			}
			for _, want := range []string{
				"kind: ExternalRemediationRequest",
				"name: extrr-{{ .HealthEventID }}",
				`nvsentinel.nvidia.com/preserve: "true"`,
				`nodeName: {{ printf "%q" .HealthEvent.NodeName }}`,
				`customRecommendedAction: {{ printf "%q" .HealthEvent.CustomRecommendedAction }}`,
			} {
				if !strings.Contains(tmpl, want) {
					t.Errorf("%s template lacks %s:\n%s", extRemediationTemplateKey, want, tmpl)
				}
			}
			if strings.Contains(tmpl, "nvsentinel.nvidia.com/ttl") {
				t.Errorf("%s template sets a TTL; janitor would return the node mid-repair", extRemediationTemplateKey)
			}

			if verbs := faultRemediationGrant(t, out); !slices.Contains(verbs, "create") {
				t.Errorf("fault-remediation ClusterRole verbs on %s.%s = %v, want create",
					extRemediationResource, extRemediationAPIGroup, verbs)
			}
		})
	}

	for _, step := range []string{"nvsentinel-remediation", "nvsentinel-observe", ""} {
		name := step
		if name == "" {
			name = "base"
		}
		t.Run(name+" without the mixin", func(t *testing.T) {
			out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, step))
			data, rendered := faultRemediationConfigData(t, out)
			if step != "" && !rendered {
				t.Fatal("fault-remediation ConfigMap not rendered")
			}
			config, _ := data["config.toml"].(string)
			if _, ok := remediationActionBlock(config, extRemediationMixinActionKey); ok {
				t.Errorf("[remediationActions.%q] renders without %s", extRemediationMixinActionKey, extRemediationMixinName)
			}
			if _, ok := data[extRemediationTemplateKey]; ok {
				t.Errorf("the %s template renders without %s", extRemediationTemplateKey, extRemediationMixinName)
			}
			for key, v := range data {
				if s, _ := v.(string); strings.Contains(s, "ExternalRemediationRequest") {
					t.Errorf("fault-remediation ConfigMap key %s names ExternalRemediationRequest without %s", key, extRemediationMixinName)
				}
			}
			if verbs := faultRemediationGrant(t, out); verbs != nil {
				t.Errorf("fault-remediation may %v %s without %s", verbs, extRemediationResource, extRemediationMixinName)
			}
		})
	}

	testObjectMonitorPolicyRender(t, store)
}

// faultRemediationGrant returns the verbs fault-remediation's ClusterRole
// grants on externalremediationrequests in the ERR API group, or nil. The
// janitor's ClusterRole grants them unconditionally, so it is not read.
func faultRemediationGrant(t *testing.T, rendered string) []string {
	t.Helper()
	if _, ok := renderedDocument(rendered, "ClusterRole", "fault-remediation"); !ok {
		return nil
	}
	role := renderedObject(t, rendered, "ClusterRole", "fault-remediation")
	rules, _ := role["rules"].([]any)
	var verbs []string
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if slices.Contains(stringList(rule["apiGroups"]), extRemediationAPIGroup) &&
			slices.Contains(stringList(rule["resources"]), extRemediationResource) {

			verbs = append(verbs, stringList(rule["verbs"])...)
		}
	}
	return verbs
}

// faultRemediationConfigData returns the data of fault-remediation's
// ConfigMap, and whether it rendered.
func faultRemediationConfigData(t *testing.T, rendered string) (map[string]any, bool) {
	t.Helper()
	if _, ok := renderedDocument(rendered, "ConfigMap", "fault-remediation"); !ok {
		return nil, false
	}
	data, _ := renderedObject(t, rendered, "ConfigMap", "fault-remediation")["data"].(map[string]any)
	return data, true
}

// testObjectMonitorPolicyRender renders a declared CUSTOM policy through the
// pinned kubernetes-object-monitor chart and pins what the gate's rule 3b
// relies on: enabled and isFatal are printed into the TOML verbatim, so an
// unset isFatal leaves the value empty, and customRecommendedAction is printed
// quoted (charts/kubernetes-object-monitor/templates/configmap.yaml:28, 50,
// 53-55 at v1.28.0).
func testObjectMonitorPolicyRender(t *testing.T, store *recipe.MetadataStore) {
	tests := []struct {
		name  string
		event map[string]any
		want  []string
	}{
		{
			name:  "declared fault",
			event: map[string]any{"isFatal": true},
			want: []string{
				`enabled = true`,
				`isFatal = true`,
				`customRecommendedAction = "` + extRemediationMixinActionKey + `"`,
			},
		},
		{
			name:  "isFatal unset",
			event: map[string]any{},
			want:  []string{`enabled = true`, `isFatal = `},
		},
	}
	for _, tt := range tests {
		t.Run("kubernetes-object-monitor policy: "+tt.name, func(t *testing.T) {
			event := map[string]any{
				"componentClass":          "Node",
				"message":                 "vendor repair requested",
				"recommendedAction":       recommendedActionCustom,
				"customRecommendedAction": extRemediationMixinActionKey,
			}
			for k, v := range tt.event {
				event[k] = v
			}
			values := nvsentinelRenderedValues(t, store, "nvsentinel-remediation", extRemediationMixinName)
			global, _ := values["global"].(map[string]any)
			if global == nil {
				t.Fatal("resolved nvsentinel values have no global map")
			}
			global["kubernetesObjectMonitor"] = map[string]any{"enabled": true}
			values["kubernetes-object-monitor"] = map[string]any{"policies": []any{map[string]any{
				"name":            extRemediationPolicyName,
				"enabled":         true,
				"resource":        map[string]any{"group": "", "version": "v1", "kind": "Node"},
				"predicate":       map[string]any{"expression": "true"},
				"nodeAssociation": map[string]any{"expression": "resource.metadata.name"},
				"healthEvent":     event,
			}}}

			cm := renderedObject(t, renderNVSentinel(t, values), "ConfigMap", objectMonitorConfigMap)
			data, _ := cm["data"].(map[string]any)
			config, _ := data["config.toml"].(string)
			block, ok := renderedPolicyBlock(config, extRemediationPolicyName)
			if !ok {
				t.Fatalf("config.toml has no policy %q:\n%s", extRemediationPolicyName, config)
			}
			for _, line := range tt.want {
				if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(line) + `$`).MatchString(block) {
					t.Errorf("policy %q lacks the line %q:\n%s", extRemediationPolicyName, line, block)
				}
			}
		})
	}
}
