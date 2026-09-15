// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"reflect"
	"testing"
)

func testPins() []Pin {
	return []Pin{
		{Component: "nvsentinel", Chart: "nvsentinel", Repository: "oci://ghcr.io/nvidia",
			Version: "v1.20.0", Datasource: "docker", DepName: "ghcr.io/nvidia/nvsentinel", Annotated: true},
		{Component: "prometheus-adapter", Chart: "prometheus-community/prometheus-adapter",
			Repository: "https://prometheus-community.github.io/helm-charts", Version: "5.3.0",
			Datasource: "helm", DepName: "prometheus-adapter", Annotated: true},
		{Component: "prometheus-adapter-ocp", Chart: "prometheus-community/prometheus-adapter",
			Repository: "https://prometheus-community.github.io/helm-charts", Version: "5.3.0",
			Datasource: "helm", DepName: "prometheus-adapter", Annotated: true},
		{Component: "cert-manager", Chart: "jetstack/cert-manager", Repository: "https://charts.jetstack.io",
			Version: "v1.20.2", Datasource: "helm", DepName: "cert-manager", Annotated: true},
		{Component: "dranet"}, // manifest-only: must not appear anywhere
	}
}

func TestBuildReportCollapsesTwins(t *testing.T) {
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0", Latest: "v1.23.0", UpdateType: "minor"},
		"prometheus-adapter":        {Current: "5.3.0", Latest: "5.4.0", UpdateType: "minor"},
		"cert-manager":              {Current: "v1.20.2"},
	}
	got, err := BuildReport(testPins(), lookups, Meta{Commit: "abc1234", RunURL: "https://example/run/1"})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if got.Summary != (Summary{Tracked: 4, Behind: 2, Unresolved: 0}) {
		t.Errorf("summary = %+v, want {4 2 0}", got.Summary)
	}
	if len(got.Drift) != 2 {
		t.Fatalf("drift rows = %d, want 2 (the -ocp twin collapses)", len(got.Drift))
	}
	var adapter *Row
	for i := range got.Drift {
		if got.Drift[i].Chart == "prometheus-community/prometheus-adapter" {
			adapter = &got.Drift[i]
		}
	}
	if adapter == nil {
		t.Fatal("prometheus-adapter row missing")
	}
	want := []string{"prometheus-adapter", "prometheus-adapter-ocp"}
	if !reflect.DeepEqual(adapter.Components, want) {
		t.Errorf("components = %v, want %v", adapter.Components, want)
	}
	if !reflect.DeepEqual(got.Current, []string{"cert-manager"}) {
		t.Errorf("current = %v, want [cert-manager]", got.Current)
	}
}

func TestBuildReportMissingLookupIsUnresolved(t *testing.T) {
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0", Latest: "v1.23.0", UpdateType: "minor"},
		"prometheus-adapter":        {Current: "5.3.0"},
		// cert-manager absent: Renovate never reported it.
	}
	got, err := BuildReport(testPins(), lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if got.Summary.Unresolved != 1 || len(got.Unresolved) != 1 {
		t.Fatalf("unresolved = %d, want 1", got.Summary.Unresolved)
	}
	if got.Unresolved[0].Components[0] != "cert-manager" {
		t.Errorf("unresolved component = %q, want cert-manager", got.Unresolved[0].Components[0])
	}
	for _, c := range got.Current {
		if c == "cert-manager" {
			t.Fatal("an unresolved pin was counted as current; the report must fail loud, not clean")
		}
	}
}

func TestBuildReportRejectsEmptyLookups(t *testing.T) {
	// Every pin unresolved means the report shape changed or the run failed.
	// Reporting "0 behind" would be a silent false negative.
	if _, err := BuildReport(testPins(), map[string]Lookup{}, Meta{}); err == nil {
		t.Fatal("want error when no pin resolved, got nil")
	}
}

func TestBuildReportIsDeterministic(t *testing.T) {
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0", Latest: "v1.23.0", UpdateType: "minor"},
		"prometheus-adapter":        {Current: "5.3.0", Latest: "5.4.0", UpdateType: "minor"},
		"cert-manager":              {Current: "v1.20.2"},
	}
	a, err := BuildReport(testPins(), lookups, Meta{Commit: "abc1234"})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	b, err := BuildReport(testPins(), lookups, Meta{Commit: "abc1234"})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("two builds over identical input differ; ordering is not deterministic")
	}
}
