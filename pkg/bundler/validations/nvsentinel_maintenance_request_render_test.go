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
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelMaintenanceRequestChartRender renders the pinned chart with
// the maintenance-request mixin beside node validation and the quarantine
// step: lifecycle-manager runs its MaintenanceRequest controller, mounts the
// platform-connector socket it publishes through, installs the request's
// validating and mutating webhooks, and fault-quarantine carries the rule set
// that matches the events it publishes.
func TestNVSentinelMaintenanceRequestChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	out := renderNVSentinel(t, nvsentinelRenderedValues(t, store, "nvsentinel-quarantine", nodeValidationMixin,
		"nvsentinel-maintenance-request"))

	deployment := renderedObject(t, out, "Deployment", lifecycleManagerName)
	container, ok := podContainers(t, deployment)[lifecycleManagerName]
	if !ok {
		t.Fatal("lifecycle-manager Deployment has no lifecycle-manager container")
	}
	args := stringList(container["args"])
	for _, want := range []string{"--enable-validation-controller=true", "--enable-maintenance-controller=true"} {
		if !slices.Contains(args, want) {
			t.Errorf("lifecycle-manager args %v do not carry %s", args, want)
		}
	}
	mounts, _ := container["volumeMounts"].([]any)
	mountsSocket := slices.ContainsFunc(mounts, func(item any) bool {
		mount, _ := item.(map[string]any)
		return mount["mountPath"] == "/var/run/nvsentinel"
	})
	if !mountsSocket {
		t.Errorf("lifecycle-manager mounts %v, want the platform-connector socket at /var/run/nvsentinel", mounts)
	}

	for _, webhook := range []string{"vmaintenancerequest-v1.kb.io", "mmaintenancerequest-v1.kb.io"} {
		if !strings.Contains(out, "name: "+webhook) {
			t.Errorf("the %s webhook is not rendered, so MaintenanceRequests are not checked on admission", webhook)
		}
	}

	cfg := faultQuarantineConfig(t, out)
	if !strings.Contains(cfg, `name = "MaintenanceRequest ruleset"`) {
		t.Fatal("fault-quarantine config has no MaintenanceRequest ruleset, so a request would not cordon its node")
	}
	// The gate requires a restated rule set to carry these expressions
	// verbatim; the chart's quote filter writes them as Go %q strings.
	for name, expression := range map[string]string{
		"maintenanceRequestRuleExpression":     maintenanceRequestRuleExpression,
		"maintenanceRequestNodeRuleExpression": maintenanceRequestNodeRuleExpression,
	} {
		if !strings.Contains(cfg, "expression = "+strconv.Quote(expression)) {
			t.Errorf("fault-quarantine config has no rule with %s %q; update it to the chart's", name, expression)
		}
	}
}
