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
	"encoding/json"
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
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0", Candidates: []Candidate{{Version: "v1.23.0", UpdateType: "minor"}}},
		"prometheus-adapter":        {Current: "5.3.0", Candidates: []Candidate{{Version: "5.4.0", UpdateType: "minor"}}},
		"cert-manager":              {Current: "v1.20.2"},
	}
	got, err := BuildReport(testPins(), lookups, Meta{Commit: "abc1234", RunURL: "https://example/run/1"})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	// No fixture carries a publish date, so both drift rows are unscreened.
	if got.Summary != (Summary{Tracked: 4, Behind: 2, Unscreened: 2}) {
		t.Errorf("summary = %+v, want {Tracked:4 Behind:2 Unscreened:2}", got.Summary)
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
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0", Candidates: []Candidate{{Version: "v1.23.0", UpdateType: "minor"}}},
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
		"ghcr.io/nvidia/nvsentinel": {Current: ""},
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
	// nvsentinel carries multiple candidates: Alternatives is the newest
	// ordering in the artifact, so the determinism this test names has to cover
	// it rather than only the single-candidate rows it covered before.
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0",
			Candidates: []Candidate{
				{Version: "v1.20.3", UpdateType: "patch"},
				{Version: "v1.23.0", UpdateType: "minor"},
			}},
		"prometheus-adapter": {Current: "5.3.0", Candidates: []Candidate{{Version: "5.4.0", UpdateType: "minor"}}},
		"cert-manager":       {Current: "v1.20.2"},
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

// Alternatives carries the candidates Latest beat, so a reviewer sees the
// smaller step when one exists. Latest itself is never repeated there: the row
// already names it, and duplicating it would read as two separate options.
func TestBuildReportSurfacesAlternatives(t *testing.T) {
	pins := []Pin{{
		Component: "kube-prometheus-stack", Chart: "kube-prometheus-stack",
		Repository: "https://prometheus-community.github.io/helm-charts",
		Datasource: "helm", Version: "84.4.0", DepName: "kube-prometheus-stack",
	}}
	lookups := map[string]Lookup{"kube-prometheus-stack": {
		DepName: "kube-prometheus-stack", Current: "84.4.0",
		Candidates: []Candidate{
			{Version: "84.5.0", UpdateType: "minor"},
			{Version: "91.5.2", UpdateType: "major"},
		},
	}}

	r, err := BuildReport(pins, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(r.Drift) != 1 {
		t.Fatalf("got %d drift rows, want 1", len(r.Drift))
	}
	got := r.Drift[0]
	if got.Latest != "91.5.2" || got.UpdateType != "major" {
		t.Errorf("headline changed: got %q/%q, want 91.5.2/major", got.Latest, got.UpdateType)
	}
	want := []Candidate{{Version: "84.5.0", UpdateType: "minor"}}
	if len(got.Alternatives) != len(want) || got.Alternatives[0] != want[0] {
		t.Errorf("got alternatives %+v, want %+v", got.Alternatives, want)
	}
}

// The common case is one candidate, which is the one Latest already names.
// Alternatives must stay absent there so those rows keep the bytes they have
// always had -- a diffable artifact is the point of committing it. In the
// 2026-09-28 report that is 25 of the 26 drift rows.
func TestBuildReportOmitsAlternativesWhenOnlyOneCandidate(t *testing.T) {
	pins := []Pin{{
		Component: "kueue", Chart: "kueue", Repository: "oci://registry.k8s.io/kueue/charts",
		Datasource: "docker", Version: "0.19.3", DepName: "kueue",
	}}
	lookups := map[string]Lookup{"kueue": {
		DepName: "kueue", Current: "0.19.3",
		Candidates: []Candidate{{Version: "0.19.6", UpdateType: "patch"}},
	}}

	r, err := BuildReport(pins, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(r.Drift[0].Alternatives) != 0 {
		t.Errorf("got alternatives %+v, want none", r.Drift[0].Alternatives)
	}
	blob, err := json.Marshal(r.Drift[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "alternatives") {
		t.Errorf("alternatives key present in JSON for a single-candidate row: %s", blob)
	}
}

func kaiPin() Pin {
	return Pin{
		Component: "kai-scheduler", Chart: "kai-scheduler", Repository: "oci://ghcr.io/kai-scheduler/kai-scheduler",
		Datasource: "docker", Version: "v0.16.9", DepName: "ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler",
	}
}

// The 2026-09-28 report proposed kai-scheduler v0.20.1, a version upstream
// retracted, and hid both v0.16.10 and the v0.18.x line upstream marks Latest
// (#2791). With one candidate per release line and publish dates attached,
// the row must propose the maintained line and keep the retracted one visible
// beside it rather than dropping it.
func TestBuildReportOffersMaintainedLineOverPredatingOne(t *testing.T) {
	lookups := map[string]Lookup{kaiPin().DepName: {
		Current: "v0.16.9", CurrentPublished: mustTime(t, "2026-08-16T10:29:50Z"),
		Candidates: []Candidate{
			{Version: "v0.16.11", UpdateType: "patch", Published: mustTime(t, "2026-10-11T10:16:51Z")},
			{Version: "v0.17.2", UpdateType: "minor", Published: mustTime(t, "2026-09-16T12:00:00Z")},
			{Version: "v0.18.5", UpdateType: "minor", Published: mustTime(t, "2026-10-09T12:00:00Z")},
			{Version: "v0.20.1", UpdateType: "minor", Published: mustTime(t, "2025-11-30T13:27:49Z")},
		},
	}}
	r, err := BuildReport([]Pin{kaiPin()}, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if r.Summary != (Summary{Tracked: 1, Behind: 1}) {
		t.Errorf("summary = %+v, want {Tracked:1 Behind:1}", r.Summary)
	}
	if len(r.Drift) != 1 {
		t.Fatalf("drift rows = %d, want 1", len(r.Drift))
	}
	row := r.Drift[0]
	if row.Latest != "v0.18.5" || row.UpdateType != "minor" {
		t.Errorf("latest = %s %s, want minor v0.18.5", row.UpdateType, row.Latest)
	}
	if !row.LatestPublished.Equal(mustTime(t, "2026-10-09T12:00:00Z")) {
		t.Errorf("latestPublished = %v, want v0.18.5's date", row.LatestPublished)
	}
	if got := versions(row.Alternatives); !reflect.DeepEqual(got, []string{"patch v0.16.11"}) {
		t.Errorf("alternatives = %v, want [patch v0.16.11]", got)
	}
	if got := versions(row.Predating); !reflect.DeepEqual(got, []string{"minor v0.20.1"}) {
		t.Errorf("predating = %v, want [minor v0.20.1]", got)
	}
	if row.Unscreened != "" {
		t.Errorf("unscreened = %q, want empty: every offer was dated", row.Unscreened)
	}
}

// agentgateway as the 2026-09-16 report saw it: the only candidate was the
// kgateway-lineage v2.2.1. Nothing is proposed, so the chart must not count as
// behind, must not read as current, and must stay visible for a reviewer.
func TestBuildReportWithholdsChartWhoseEveryCandidatePredates(t *testing.T) {
	pin := Pin{
		Component: "agentgateway", Chart: "agentgateway", Repository: "oci://cr.agentgateway.dev/charts",
		Datasource: "docker", Version: "v1.5.0", DepName: "cr.agentgateway.dev/charts/agentgateway",
	}
	lookups := map[string]Lookup{pin.DepName: {
		Current: "v1.5.0", CurrentPublished: mustTime(t, "2026-08-27T17:26:27Z"),
		Candidates: []Candidate{
			{Version: "v2.2.1", UpdateType: "major", Published: mustTime(t, "2026-02-17T22:12:11Z")},
		},
	}}
	r, err := BuildReport([]Pin{pin}, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if r.Summary != (Summary{Tracked: 1, Withheld: 1}) {
		t.Errorf("summary = %+v, want {Tracked:1 Withheld:1}", r.Summary)
	}
	if len(r.Drift) != 0 || len(r.Current) != 0 {
		t.Errorf("drift = %v, current = %v; a withheld chart is neither", r.Drift, r.Current)
	}
	if len(r.Withheld) != 1 {
		t.Fatalf("withheld rows = %d, want 1", len(r.Withheld))
	}
	row := r.Withheld[0]
	if got := versions(row.Predating); !reflect.DeepEqual(got, []string{"major v2.2.1"}) {
		t.Errorf("predating = %v, want [major v2.2.1]", got)
	}
	blob, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err = json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"latest", "updateType", "latestPublished", "alternatives"} {
		if _, ok := fields[key]; ok {
			t.Errorf("withheld row carries %q, which would read as a proposal: %s", key, blob)
		}
	}
	if !strings.Contains(string(blob), `"currentPublished":"2026-08-27T17:26:27Z"`) {
		t.Errorf("withheld row must show the date its candidates were compared to: %s", blob)
	}
}

// Without publish dates -- a registry outage, or a chart pushed without the
// created annotation -- nothing can be withheld. The row must say so rather
// than read like a screened one.
func TestBuildReportMarksUndatedRowUnscreened(t *testing.T) {
	lookups := map[string]Lookup{kaiPin().DepName: {
		Current:    "v0.16.9",
		Candidates: []Candidate{{Version: "v0.20.1", UpdateType: "minor"}},
	}}
	r, err := BuildReport([]Pin{kaiPin()}, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	if len(r.Drift) != 1 || r.Drift[0].Latest != "v0.20.1" {
		t.Fatalf("drift = %+v, want v0.20.1 offered unchecked", r.Drift)
	}
	if !strings.Contains(r.Drift[0].Unscreened, "no publish date for current v0.16.9") {
		t.Errorf("unscreened = %q, want it to name the undated current version", r.Drift[0].Unscreened)
	}
	if r.Summary.Unscreened != 1 {
		t.Errorf("summary.unscreened = %d, want 1: a registry outage must show in the counts", r.Summary.Unscreened)
	}
}

// Every tracked pin lands in exactly one of drift, withheld, current, or
// unresolved; the twins share a row, so rows are counted by component.
func TestBuildReportPartitionsEveryTrackedPin(t *testing.T) {
	pins := append(testPins(), kaiPin())
	lookups := map[string]Lookup{
		"ghcr.io/nvidia/nvsentinel": {Current: "v1.20.0", CurrentPublished: mustTime(t, "2026-08-01T00:00:00Z"),
			Candidates: []Candidate{{Version: "v1.23.0", UpdateType: "minor", Published: mustTime(t, "2025-01-01T00:00:00Z")}}},
		"prometheus-adapter": {Current: "5.3.0", Candidates: []Candidate{{Version: "5.4.0", UpdateType: "minor"}}},
		"cert-manager":       {Current: "v1.20.2"},
		// kai-scheduler absent: unresolved.
	}
	r, err := BuildReport(pins, lookups, Meta{})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	seen := map[string]int{}
	for _, row := range r.Drift {
		for _, c := range row.Components {
			seen[c]++
		}
	}
	for _, row := range r.Withheld {
		for _, c := range row.Components {
			seen[c]++
		}
	}
	for _, c := range r.Current {
		seen[c]++
	}
	for _, u := range r.Unresolved {
		for _, c := range u.Components {
			seen[c]++
		}
	}
	for _, p := range pins {
		if p.Repository == "" {
			continue
		}
		if seen[p.Component] != 1 {
			t.Errorf("component %q appears %d times across the report, want exactly 1", p.Component, seen[p.Component])
		}
	}
	if r.Summary != (Summary{Tracked: 5, Behind: 1, Unresolved: 1, Withheld: 1, Unscreened: 1}) {
		t.Errorf("summary = %+v", r.Summary)
	}
}
