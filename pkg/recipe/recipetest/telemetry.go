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

// Package recipetest provides recipe-data fixtures for tests in other
// packages.
package recipetest

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// PromQLServerComponent is the component the fixtures declare the server.
const PromQLServerComponent = "kube-prometheus-stack"

// Paths the fixture server owns. They are not the chart's, so a fixture's
// scope never mixes with the embedded registry's.
const (
	FixtureSMNamespaceSelectorPath = "promqlFixture.serviceMonitorNamespaceSelector"
	FixturePMNamespaceSelectorPath = "promqlFixture.podMonitorNamespaceSelector"
	FixtureSMSelectorPath          = "promqlFixture.serviceMonitorSelector"
	FixturePMSelectorPath          = "promqlFixture.podMonitorSelector"
	FixtureScrapeClassesPath       = "promqlFixture.scrapeClasses"
)

// FixturePromQLServer declares a server owning the Fixture*Path paths.
func FixturePromQLServer() recipe.TelemetryConfig {
	return recipe.TelemetryConfig{PromQLServer: recipe.PromQLServerConfig{
		NamespaceSelectorPaths: []string{FixtureSMNamespaceSelectorPath, FixturePMNamespaceSelectorPath},
		MonitorSelectorPaths:   []string{FixtureSMSelectorPath, FixturePMSelectorPath},
		ScrapeClassesPath:      FixtureScrapeClassesPath,
	}}
}

// FixturePromQLConsumer declares a consumer of series.
func FixturePromQLConsumer(series []string, monitorsFrom ...string) recipe.TelemetryConfig {
	return recipe.TelemetryConfig{PromQL: recipe.PromQLConsumerConfig{MonitorsFrom: monitorsFrom, Series: series}}
}

// DefaultPromQLFixture is ADR-026 Decision 3's shape: the stack serves
// prometheus-adapter, reading gpu-operator, and dynamo-platform, reading
// itself.
func DefaultPromQLFixture() map[string]recipe.TelemetryConfig {
	return map[string]recipe.TelemetryConfig{
		PromQLServerComponent: FixturePromQLServer(),
		"prometheus-adapter":  FixturePromQLConsumer([]string{"DCGM_FI_DEV_GPU_UTIL"}, "gpu-operator"),
		"dynamo-platform":     FixturePromQLConsumer([]string{"dynamo_frontend_requests_total"}, "dynamo-platform"),
	}
}

// WriteTelemetryRegistry writes a --data registry.yaml whose entries are the
// named components' embedded entries plus their telemetry, and returns its
// directory.
func WriteTelemetryRegistry(t testing.TB, telemetry map[string]recipe.TelemetryConfig) string {
	t.Helper()
	embedded, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("load embedded registry: %v", err)
	}
	external := recipe.ComponentRegistry{APIVersion: embedded.APIVersion, Kind: embedded.Kind}
	for _, name := range embedded.Names() {
		if decl, ok := telemetry[name]; ok {
			entry := *embedded.Get(name)
			entry.Telemetry = decl
			external.Components = append(external.Components, entry)
		}
	}
	if len(external.Components) != len(telemetry) {
		t.Fatalf("telemetry names a component the embedded registry lacks: %v", telemetry)
	}
	data, err := yaml.Marshal(&external)
	if err != nil {
		t.Fatalf("marshal telemetry registry: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, recipe.RegistryFileName), data, 0o600); err != nil {
		t.Fatalf("write telemetry registry: %v", err)
	}
	return dir
}

// TelemetryDataProvider layers WriteTelemetryRegistry's directory over the
// embedded catalog.
func TelemetryDataProvider(t testing.TB, telemetry map[string]recipe.TelemetryConfig) recipe.DataProvider {
	t.Helper()
	layered, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: WriteTelemetryRegistry(t, telemetry)})
	if err != nil {
		t.Fatalf("layer telemetry registry: %v", err)
	}
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	return layered
}
