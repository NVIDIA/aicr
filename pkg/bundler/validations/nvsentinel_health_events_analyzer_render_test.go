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
	healthEventsAnalyzerMixinName = "nvsentinel-health-events-analyzer"
	// healthEventsAnalyzerImage is the image docs/user/container-images.md
	// discloses for the mixin. The static BOM cannot see a mixin-gated
	// subchart, so this assertion is what keeps that note honest.
	healthEventsAnalyzerImage = "ghcr.io/nvidia/nvsentinel/health-events-analyzer:v1.28.0"
	healthEventsAnalyzerName  = "health-events-analyzer"
)

var analyzerRuleNameRe = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"\s*$`)

// analyzerRuleConfig returns the rendered analyzer rule config.
func analyzerRuleConfig(t *testing.T, rendered string) string {
	t.Helper()
	configMap := renderedObject(t, rendered, "ConfigMap", healthEventsAnalyzerName+"-config")
	data, _ := configMap["data"].(map[string]any)
	config, _ := data["config.toml"].(string)
	if config == "" {
		t.Fatal("health-events-analyzer-config has no config.toml")
	}
	return config
}

// TestNVSentinelHealthEventsAnalyzerChartRender renders the pinned nvsentinel
// chart with the remediation step and the analyzer mixin composed, and asserts
// what #2618 promises reaches the workload: the pinned image, STORE_ONLY on the
// container's own argument, the Percona datastore wiring, every rule in the
// config evaluated, and the service account admitted by the platform
// connector. It also renders with every rule toggle the mixin names switched
// off: a rule still evaluated then is one the chart added and the mixin does
// not name, so the rule set would no longer be chosen in the mixin.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelSlurmDrainMonitorChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelHealthEventsAnalyzerChartRender`.
func TestNVSentinelHealthEventsAnalyzerChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	values := nvsentinelRenderedValues(t, store, "nvsentinel-remediation", healthEventsAnalyzerMixinName)
	out := renderNVSentinel(t, values)

	deployment := renderedObject(t, out, "Deployment", healthEventsAnalyzerName)
	spec, _ := deployment["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("health-events-analyzer Deployment has %d containers, want 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	if got := container["image"]; got != healthEventsAnalyzerImage {
		t.Errorf("health-events-analyzer image = %v, want %s", got, healthEventsAnalyzerImage)
	}
	if args := stringList(container["args"]); !slices.Contains(args, "--processing-strategy=STORE_ONLY") {
		t.Errorf("health-events-analyzer args %v do not carry --processing-strategy=STORE_ONLY", args)
	}
	if inits, _ := podSpec["initContainers"].([]any); len(inits) != 0 {
		t.Errorf("health-events-analyzer renders %d init containers on MongoDB; the image note lists none", len(inits))
	}

	// The datastore the remediation step wires: the URI Secret and the x509
	// client certificate nvsentinel-mongodb issues.
	volumes, _ := podSpec["volumes"].([]any)
	var certSecret string
	for _, v := range volumes {
		vol, _ := v.(map[string]any)
		if vol["name"] == "mongo-app-client-cert" {
			secret, _ := vol["secret"].(map[string]any)
			certSecret, _ = secret["secretName"].(string)
		}
	}
	if certSecret != "nvsentinel-mongodb-app-client" {
		t.Errorf("mongo-app-client-cert volume mounts Secret %q, want nvsentinel-mongodb-app-client", certSecret)
	}
	var uriSecret bool
	envFrom, _ := container["envFrom"].([]any)
	for _, e := range envFrom {
		entry, _ := e.(map[string]any)
		ref, _ := entry["secretRef"].(map[string]any)
		if ref["name"] == "nvsentinel-mongodb-uri" {
			uriSecret = true
		}
	}
	if !uriSecret {
		t.Errorf("health-events-analyzer envFrom %v does not read the nvsentinel-mongodb-uri Secret", envFrom)
	}

	config := analyzerRuleConfig(t, out)
	rules := strings.Count(config, "[[rules]]")
	if rules == 0 {
		t.Fatal("rendered config has no [[rules]]")
	}
	if got := strings.Count(config, "evaluate_rule = true"); got != rules {
		t.Errorf("%d of %d rules are evaluated, want all", got, rules)
	}
	// The rendered rules are exactly the ones the mixin names: one toggle,
	// enable<Rule>Rule, per rule.
	hea, _ := values["health-events-analyzer"].(map[string]any)
	named := make([]string, 0, len(hea))
	for key := range hea {
		if rule, ok := strings.CutPrefix(key, "enable"); ok && strings.HasSuffix(rule, "Rule") {
			named = append(named, strings.TrimSuffix(rule, "Rule"))
		}
	}
	matches := analyzerRuleNameRe.FindAllStringSubmatch(config, -1)
	rendered := make([]string, 0, len(matches))
	for _, m := range matches {
		rendered = append(rendered, m[1])
	}
	slices.Sort(named)
	slices.Sort(rendered)
	if !slices.Equal(rendered, named) {
		t.Errorf("rendered rules %v, want the rules %s names %v", rendered, healthEventsAnalyzerMixinName, named)
	}
	if !slices.Contains(rendered, "MultipleRemediations") {
		t.Error("rendered config has no MultipleRemediations rule")
	}

	// The platform connector authenticates publishers; a service account it
	// does not list has every derived event rejected.
	const publisher = "system:serviceaccount:nvsentinel:health-events-analyzer"
	if !strings.Contains(out, publisher) {
		t.Errorf("platform connector does not admit %s; every derived event would be rejected", publisher)
	}

	// Every rule toggle the mixin names, switched off.
	off := map[string]any{}
	mergeValuesForRender(off, values)
	offHEA, _ := off["health-events-analyzer"].(map[string]any)
	for key := range hea {
		if strings.HasPrefix(key, "enable") && strings.HasSuffix(key, "Rule") {
			offHEA[key] = false
		}
	}
	offConfig := analyzerRuleConfig(t, renderNVSentinel(t, off))
	if got := strings.Count(offConfig, "evaluate_rule = false"); got != rules {
		t.Errorf("with every toggle the mixin names off, %d of %d rules are off; the chart has a rule %s does not name",
			got, rules, healthEventsAnalyzerMixinName)
	}

	// Negative control: the remediation step alone must not pull the analyzer.
	base := renderNVSentinel(t, nvsentinelRenderedValues(t, store, "nvsentinel-remediation"))
	if strings.Contains(base, healthEventsAnalyzerImage) {
		t.Errorf("%s renders without the %s mixin", healthEventsAnalyzerImage, healthEventsAnalyzerMixinName)
	}
}
