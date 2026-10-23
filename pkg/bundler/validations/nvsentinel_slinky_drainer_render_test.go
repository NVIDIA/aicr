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

// renderedCustomDrain matches node-drainer's rendered [customDrain] TOML
// table, up to the next table header or the end of the file.
var renderedCustomDrain = regexp.MustCompile(`(?s)\[customDrain\](.*?)(?:\n\s*\[|\z)`)

// TestNVSentinelSlinkyDrainerChartRender renders the pinned nvsentinel chart
// with nvsentinel-quarantine and nvsentinel-slinky-drainer composed and
// asserts node-drainer is wired to the plugin: the [customDrain] table
// carries the DrainRequest contract and a whole-seconds timeout, no
// [[userNamespaces]] entry survives (node-drainer refuses to start with one
// alongside an empty customDrain.nodeSelector), the plugin's template
// ConfigMap is mounted where the table says, and node-drainer may create
// DrainRequests. The chart builds each of these from values, so a chart bump
// that renamed a key would leave the values test green and fail here.
//
// Pulls the chart over the network, so -short skips it; the weekly
// .github/workflows/nvsentinel-observability-render-check.yaml (NVSentinel
// Mixin Render Checks) runs it.
func TestNVSentinelSlinkyDrainerChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, "nvsentinel-quarantine", slinkyDrainerMixinName))

	cm, ok := renderedDocument(out, "ConfigMap", "node-drainer")
	if !ok {
		t.Fatal("node-drainer ConfigMap not rendered")
	}
	m := renderedCustomDrain.FindStringSubmatch(cm)
	if m == nil {
		t.Fatalf("node-drainer config has no [customDrain] table:\n%s", cm)
	}
	for _, want := range []string{
		`enabled = true`,
		`templateMountPath = "/etc/drain-template"`,
		`templateFileName = "drain-template.yaml"`,
		`namespace = "nvsentinel"`,
		`apiGroup = "nvsentinel.nvidia.com"`,
		`version = "v1alpha1"`,
		`kind = "DrainRequest"`,
		`statusConditionType = "DrainComplete"`,
		`statusConditionStatus = "True"`,
		`timeout = "1800"`,
	} {
		if !strings.Contains(m[1], want) {
			t.Errorf("rendered [customDrain] lacks %s:\n%s", want, m[1])
		}
	}
	if strings.Contains(cm, "[[userNamespaces]]") {
		t.Errorf("node-drainer config still carries [[userNamespaces]]; node-drainer refuses to start with an empty customDrain.nodeSelector:\n%s", cm)
	}

	deployment, ok := renderedDocument(out, "Deployment", "node-drainer")
	if !ok {
		t.Fatal("node-drainer Deployment not rendered")
	}
	for _, want := range []string{"name: slinky-drain-template", "mountPath: /etc/drain-template"} {
		if !strings.Contains(deployment, want) {
			t.Errorf("node-drainer Deployment lacks %q; node-drainer would not find the drain template", want)
		}
	}

	role := renderedObject(t, out, "ClusterRole", "node-drainer")
	rules, _ := role["rules"].([]any)
	canCreate := false
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if slices.Contains(stringList(rule["apiGroups"]), "nvsentinel.nvidia.com") &&
			slices.Contains(stringList(rule["resources"]), "drainrequests") &&
			slices.Contains(stringList(rule["verbs"]), "create") {

			canCreate = true
		}
	}
	if !canCreate {
		t.Error("node-drainer ClusterRole cannot create nvsentinel.nvidia.com drainrequests")
	}

	// Negative control: quarantine alone keeps eviction-based draining.
	base := renderNVSentinel(t, nvsentinelRenderedValues(t, store, "nvsentinel-quarantine"))
	if cm, ok := renderedDocument(base, "ConfigMap", "node-drainer"); !ok {
		t.Error("node-drainer ConfigMap not rendered with nvsentinel-quarantine alone")
	} else if m := renderedCustomDrain.FindStringSubmatch(cm); m != nil && strings.Contains(m[1], "enabled = true") {
		t.Errorf("customDrain renders enabled without the %s mixin", slinkyDrainerMixinName)
	}
}
