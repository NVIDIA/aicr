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

package recipe

import (
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func telemetryTestServer() PromQLServerConfig {
	return PromQLServerConfig{
		NamespaceSelectorPaths: []string{"p.smNamespaces", "p.pmNamespaces"},
		MonitorSelectorPaths:   []string{"p.monitors"},
		ScrapeClassesPath:      "p.scrapeClasses",
	}
}

func TestComponentRegistryValidate_Telemetry(t *testing.T) {
	tests := []struct {
		name     string
		consumer PromQLConsumerConfig
		server   PromQLServerConfig
		second   *PromQLServerConfig // a second server on another component
		wantErr  string
	}{
		{
			name:     "consumer and server pass",
			consumer: PromQLConsumerConfig{MonitorsFrom: []string{"exporter"}, Series: []string{"DCGM_FI_DEV_GPU_UTIL"}},
			server:   telemetryTestServer(),
		},
		{
			name:   "several servers pass; the bundler rejects more than one enabled",
			server: telemetryTestServer(),
			second: new(telemetryTestServer()),
		},
		{
			name:     "series without monitorsFrom",
			consumer: PromQLConsumerConfig{Series: []string{"up"}},
			wantErr:  "telemetry.promql.monitorsFrom is required",
		},
		{
			name:     "monitorsFrom without series",
			consumer: PromQLConsumerConfig{MonitorsFrom: []string{"exporter"}},
			wantErr:  "telemetry.promql.series is required",
		},
		{
			name:     "monitorsFrom names an unknown component",
			consumer: PromQLConsumerConfig{MonitorsFrom: []string{"missing"}, Series: []string{"up"}},
			wantErr:  `names "missing", which is not a registry component`,
		},
		{
			name:     "pattern that does not compile",
			consumer: PromQLConsumerConfig{MonitorsFrom: []string{"exporter"}, Series: []string{"dcgm_(?!x)"}},
			wantErr:  "is not valid RE2",
		},
		{
			name:    "server missing a path kind",
			server:  PromQLServerConfig{NamespaceSelectorPaths: []string{"p.ns"}, ScrapeClassesPath: "p.sc"},
			wantErr: "needs namespaceSelectorPaths, monitorSelectorPaths and scrapeClassesPath",
		},
		{
			name: "malformed server path",
			server: PromQLServerConfig{
				NamespaceSelectorPaths: []string{"p..ns"}, MonitorSelectorPaths: []string{"p.m"}, ScrapeClassesPath: "p.sc",
			},
			wantErr: `path "p..ns" is not a well-formed dotted path`,
		},
		{
			name: "overlapping server paths",
			server: PromQLServerConfig{
				NamespaceSelectorPaths: []string{"p.ns"}, MonitorSelectorPaths: []string{"p.ns.matchLabels"}, ScrapeClassesPath: "p.sc",
			},
			wantErr: "overlap",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &ComponentRegistry{Components: []ComponentConfig{
				{Name: "exporter", DisplayName: "Exporter"},
				{Name: "subject", DisplayName: "Subject", Telemetry: TelemetryConfig{PromQL: tt.consumer, PromQLServer: tt.server}},
			}}
			if tt.second != nil {
				registry.Components[0].Telemetry.PromQLServer = *tt.second
			}
			errs := registry.Validate()
			if tt.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("Validate() = %v, want no errors", errs)
				}
				return
			}
			if len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.wantErr) || !strings.Contains(errs[0].Error(), `"subject"`) {
				t.Fatalf("Validate() = %v, want one error naming the component and containing %q", errs, tt.wantErr)
			}
			errorstest.WantReportedCode(t, errs[0], errors.ErrCodeInvalidRequest)
		})
	}
}

func TestValidatePromQLSeriesPattern(t *testing.T) {
	tests := []struct {
		pattern string
		wantErr string
	}{
		{pattern: "dynamo_frontend_(requests_started|requests)_total"},
		{pattern: " ", wantErr: "series pattern is empty"},
		{pattern: "foo_(bar", wantErr: "is not valid RE2"},
		{pattern: "foo}}", wantErr: "Helm template delimiter"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			err := ValidatePromQLSeriesPattern(tt.pattern)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidatePromQLSeriesPattern(%q) = %v, want nil", tt.pattern, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidatePromQLSeriesPattern(%q) = %v, want an error containing %q", tt.pattern, err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

func TestTelemetryConfig_Declarations(t *testing.T) {
	consumer := TelemetryConfig{PromQL: PromQLConsumerConfig{Series: []string{"up"}}}
	server := TelemetryConfig{PromQLServer: telemetryTestServer()}
	if (TelemetryConfig{}).PromQL.IsDeclared() || (TelemetryConfig{}).PromQLServer.IsDeclared() {
		t.Error("an empty TelemetryConfig declares something")
	}
	if !consumer.PromQL.IsDeclared() || consumer.PromQLServer.IsDeclared() {
		t.Error("a consumer declaration is not exactly a consumer")
	}
	if !server.PromQLServer.IsDeclared() || server.PromQL.IsDeclared() {
		t.Error("a server declaration is not exactly a server")
	}
	want := []string{"p.smNamespaces", "p.pmNamespaces", "p.monitors", "p.scrapeClasses"}
	if got := server.PromQLServer.OwnedPaths(); !slices.Equal(got, want) {
		t.Errorf("OwnedPaths() = %v, want %v", got, want)
	}
}

// TestLoadComponentRegistry_Telemetry pins the load-time check, which is what
// an external --data registry gets: it never reaches Validate().
func TestLoadComponentRegistry_Telemetry(t *testing.T) {
	const header = "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents:\n" +
		"- name: exporter\n  displayName: Exporter\n"
	const consumer = "- name: consumer\n  displayName: Consumer\n  telemetry:\n    promql:\n      series: [up]\n"
	tests := []struct {
		name    string
		entry   string
		wantErr string
	}{
		{name: "valid declaration parses", entry: consumer + "      monitorsFrom: [exporter]\n"},
		{name: "unknown monitorsFrom fails the load", entry: consumer + "      monitorsFrom: [missing]\n",
			wantErr: "not a registry component"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dp := newInMemoryProvider("registry-telemetry-"+tt.name,
				map[string][]byte{"registry.yaml": []byte(header + tt.entry)})
			t.Cleanup(func() { EvictCachedRegistry(dp) })

			reg, err := GetComponentRegistryFor(dp)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GetComponentRegistryFor() error = %v, want one containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("GetComponentRegistryFor() error = %v", err)
			}
			if got := reg.Get("consumer").Telemetry.PromQL.MonitorsFrom; !slices.Equal(got, []string{"exporter"}) {
				t.Errorf("consumer monitorsFrom = %v", got)
			}
		})
	}
}
