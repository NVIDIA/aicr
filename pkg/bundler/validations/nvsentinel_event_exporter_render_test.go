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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	eventExporterName = "event-exporter"
	// eventExporterImage is the image docs/user/container-images.md
	// discloses for the mixin. The static BOM cannot see a mixin-gated
	// subchart, so this assertion is what keeps that note honest.
	eventExporterImage = "ghcr.io/nvidia/nvsentinel/event-exporter:v1.28.0"
)

// TestNVSentinelEventExporterChartRender renders the pinned nvsentinel chart
// with the remediation step and the event exporter mixin composed, plus the
// per-deployment values, and asserts what #2621 promises reaches the
// workload: the pinned image, the OIDC Secret mounted from oidcSecretName,
// the Percona datastore wiring, and a config carrying the sink, OIDC client,
// metadata, the filter and certificate checks on.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelHealthEventsAnalyzerChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelEventExporterChartRender`.
func TestNVSentinelEventExporterChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	helmtest.RequireBinary(t)

	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	values := nvsentinelRenderedValues(t, store, "nvsentinel-remediation", "nvsentinel-event-exporter")
	mergeValuesForRender(values, map[string]any{eventExporterName: map[string]any{"exporter": map[string]any{
		"metadata": map[string]any{"cluster": "train-1", "environment": "staging", "region": "us-west-2"},
		"sink":     map[string]any{"endpoint": "https://events.corp.internal/v1/events"},
		"oidc": map[string]any{
			"tokenUrl": "https://idp.corp.internal/oauth2/token", "clientId": "nvsentinel", "scope": "events:write",
		},
	}}})
	out := renderNVSentinel(t, values)

	deployment := renderedObject(t, out, "Deployment", eventExporterName)
	containers := podContainers(t, deployment)
	exporter, ok := containers[eventExporterName]
	if !ok || len(containers) != 1 {
		t.Fatalf("event-exporter Deployment containers = %d, want the one event-exporter container", len(containers))
	}
	if got := exporter["image"]; got != eventExporterImage {
		t.Errorf("event-exporter image = %v, want %s", got, eventExporterImage)
	}
	spec, _ := deployment["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	volumes := map[string]map[string]any{}
	for _, v := range podSpec["volumes"].([]any) {
		vol, _ := v.(map[string]any)
		name, _ := vol["name"].(string)
		volumes[name] = vol
	}
	oidcSecret, _ := volumes["oidc-secret"]["secret"].(map[string]any)
	if oidcSecret["secretName"] != "event-exporter-oidc-secret" || oidcSecret["optional"] != false {
		t.Errorf("oidc-secret volume = %v, want the required Secret event-exporter-oidc-secret", oidcSecret)
	}
	certSecret, _ := volumes["mongo-app-client-cert"]["secret"].(map[string]any)
	if certSecret["secretName"] != "nvsentinel-mongodb-app-client" {
		t.Errorf("mongo-app-client-cert volume = %v, want the nvsentinel-mongodb-app-client Secret", certSecret)
	}
	var uriSecret bool
	envFrom, _ := exporter["envFrom"].([]any)
	for _, e := range envFrom {
		entry, _ := e.(map[string]any)
		if ref, _ := entry["secretRef"].(map[string]any); ref["name"] == "nvsentinel-mongodb-uri" {
			uriSecret = true
		}
	}
	if !uriSecret {
		t.Errorf("event-exporter envFrom %v does not read the nvsentinel-mongodb-uri Secret", envFrom)
	}

	configMap := renderedObject(t, out, "ConfigMap", eventExporterName)
	data, _ := configMap["data"].(map[string]any)
	toml, _ := data["config.toml"].(string)
	for _, want := range []string{
		`cluster = "train-1"`,
		`environment = "staging"`,
		`region = "us-west-2"`,
		`endpoint = "https://events.corp.internal/v1/events"`,
		`token_url = "https://idp.corp.internal/oauth2/token"`,
		`client_id = "nvsentinel"`,
		`scope = "events:write"`,
		`expression = "event.recommendedAction != 'NONE' || event.isHealthy == true"`,
	} {
		if !strings.Contains(toml, want) {
			t.Errorf("config.toml does not contain %q:\n%s", want, toml)
		}
	}
	if strings.Contains(toml, "insecure_skip_verify = true") {
		t.Errorf("config.toml turns off a certificate check:\n%s", toml)
	}
}

// podContainers returns a pod template's init containers and containers by
// name.
func podContainers(t *testing.T, workload map[string]any) map[string]map[string]any {
	t.Helper()
	spec, _ := workload["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	out := map[string]map[string]any{}
	for _, key := range []string{"initContainers", "containers"} {
		list, _ := podSpec[key].([]any)
		for _, c := range list {
			container, _ := c.(map[string]any)
			name, _ := container["name"].(string)
			out[name] = container
		}
	}
	return out
}
