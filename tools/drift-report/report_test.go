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
	"strings"
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

func TestBuildReportDivergentDuplicateDepNameIsUnresolved(t *testing.T) {
	// prometheus-adapter and prometheus-adapter-ocp (see testPins) share one
	// depName. Renovate emits one raw dep entry per regex match, so
	// ParseRenovateReport's map keeps only the last one it saw; here that
	// surviving entry reports currentValue 5.4.0, which disagrees with both
	// pins' declared 5.3.0. A join to the wrong entry must be reported as
	// unknown, never folded into Current or Drift.
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0"},
		"prometheus-adapter":        {Current: "5.4.0"},
		"cert-manager":              {Current: "v1.20.2"},
	}
	got, err := BuildReport(testPins(), lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(got.Drift) != 0 {
		t.Fatalf("drift = %v, want none: a currentValue mismatch must never report an update", got.Drift)
	}
	for _, c := range got.Current {
		if c == "prometheus-adapter" || c == "prometheus-adapter-ocp" {
			t.Fatalf("component %q counted as current despite a currentValue mismatch", c)
		}
	}
	if got.Summary.Unresolved != 1 || len(got.Unresolved) != 1 {
		t.Fatalf("unresolved rows = %d, want 1 (the twins share a groupKey and collapse)", got.Summary.Unresolved)
	}
	want := []string{"prometheus-adapter", "prometheus-adapter-ocp"}
	if !reflect.DeepEqual(got.Unresolved[0].Components, want) {
		t.Errorf("unresolved components = %v, want %v", got.Unresolved[0].Components, want)
	}
	if !strings.Contains(got.Unresolved[0].Reason, "duplicate depName") {
		t.Errorf("reason = %q, want it to call out the duplicate depName", got.Unresolved[0].Reason)
	}
}

func TestBuildReportEmptyCurrentIsUnresolved(t *testing.T) {
	// ParseRenovateReport can produce this exact shape: dep.Updates is a
	// present-but-empty array (Renovate ran the lookup) while
	// dep.CurrentValue is absent, so Lookup.Current, .Latest, and .Problem
	// are all "". Nothing here trips the currentValue-mismatch case (Current
	// is empty, not disagreeing) or sets a Problem, so an empty Current must
	// be caught on its own — otherwise it falls straight into "Latest == ''
	// means current" and a pin Renovate never actually resolved is reported
	// as up to date.
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "", Latest: ""},
		"prometheus-adapter":        {Current: "5.3.0"},
		"cert-manager":              {Current: "v1.20.2"},
	}
	got, err := BuildReport(testPins(), lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	for _, c := range got.Current {
		if c == "nvsentinel" {
			t.Fatal("nvsentinel counted as current despite an empty currentValue; Renovate never resolved it")
		}
	}
	var found bool
	for _, u := range got.Unresolved {
		for _, c := range u.Components {
			if c == "nvsentinel" {
				found = true
				if !strings.Contains(u.Reason, "no currentValue") {
					t.Errorf("reason = %q, want it to name the missing currentValue", u.Reason)
				}
			}
		}
	}
	if !found {
		t.Fatal("nvsentinel not found in unresolved")
	}
	if got.Summary.Unresolved != 1 || len(got.Unresolved) != 1 {
		t.Fatalf("unresolved rows = %d, want 1", got.Summary.Unresolved)
	}
}

func TestBuildReportTotalOutageAllUnresolved(t *testing.T) {
	// The realistic outage shape: every registry-chart dep resolves (Renovate
	// ran) but every resolution carries a Problem (each lookup itself failed).
	// resolved reaches the tracked count, so the "zero resolved" fail-closed
	// guard deliberately does not fire — the report must still come back
	// all-unresolved and 0-behind, not silently clean.
	pins := []Pin{
		{Component: "nvsentinel", Chart: "nvsentinel", Repository: "oci://ghcr.io/nvidia",
			Version: "v1.20.0", Datasource: "docker", DepName: "ghcr.io/nvidia/nvsentinel", Annotated: true},
		{Component: "cert-manager", Chart: "jetstack/cert-manager", Repository: "https://charts.jetstack.io",
			Version: "v1.20.2", Datasource: "helm", DepName: "cert-manager", Annotated: true},
		{Component: "dranet"}, // manifest-only: must not appear anywhere
	}
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Problem: "registry unreachable"},
		"cert-manager":              {Problem: "registry unreachable"},
	}
	got, err := BuildReport(pins, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	const wantTracked = 2
	if got.Summary.Tracked != wantTracked {
		t.Fatalf("tracked = %d, want %d", got.Summary.Tracked, wantTracked)
	}
	if got.Summary.Unresolved != wantTracked {
		t.Errorf("unresolved = %d, want %d (every tracked pin)", got.Summary.Unresolved, wantTracked)
	}
	if got.Summary.Behind != 0 {
		t.Errorf("behind = %d, want 0", got.Summary.Behind)
	}
	if len(got.Current) != 0 {
		t.Errorf("current = %v, want none: a total lookup outage must never read as a clean fleet", got.Current)
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
