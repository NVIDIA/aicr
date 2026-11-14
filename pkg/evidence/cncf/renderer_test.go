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

package cncf

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestRenderEmptyReport(t *testing.T) {
	r := New(WithOutputDir(t.TempDir()))
	err := r.Render(context.Background(), &ctrf.Report{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderNilReport(t *testing.T) {
	r := New(WithOutputDir(t.TempDir()))
	err := r.Render(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderNoOutputDir(t *testing.T) {
	r := New()
	err := r.Render(context.Background(), &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{{Name: "test", Status: "passed", Duration: 100}},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty output dir")
	}
}

func TestRenderSubmissionChecks(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "dra-support", Status: "passed", Duration: 5000, Stdout: []string{"DRA test passed"}},
				{Name: "gang-scheduling", Status: "failed", Duration: 8000, Message: "pods not co-scheduled"},
				{Name: "gpu-operator-health", Status: "passed", Duration: 2000}, // diagnostic, not submission
			},
		},
	}

	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	// Submission checks should produce evidence files.
	if _, err := os.Stat(filepath.Join(dir, "dra-support.md")); err != nil {
		t.Errorf("dra-support.md not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gang-scheduling.md")); err != nil {
		t.Errorf("gang-scheduling.md not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.md")); err != nil {
		t.Errorf("index.md not found: %v", err)
	}

	// Diagnostic check should NOT produce an evidence file.
	if _, err := os.Stat(filepath.Join(dir, "gpu-operator-health.md")); err == nil {
		t.Error("gpu-operator-health.md should not exist (diagnostic, not submission)")
	}
}

func TestRenderSkippedExcluded(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "dra-support", Status: "skipped", Duration: 0},
			},
		},
	}

	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	// Skipped checks should not produce evidence files.
	if _, err := os.Stat(filepath.Join(dir, "dra-support.md")); err == nil {
		t.Error("dra-support.md should not exist (skipped)")
	}
}

func TestRenderSeparateMetricsFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	// accelerator-metrics and ai-service-metrics produce separate evidence files.
	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "accelerator-metrics", Status: "passed", Duration: 3000},
				{Name: "ai-service-metrics", Status: "passed", Duration: 4000},
			},
		},
	}

	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	// Each should produce its own evidence file.
	if _, err := os.Stat(filepath.Join(dir, "accelerator-metrics.md")); err != nil {
		t.Errorf("accelerator-metrics.md not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ai-service-metrics.md")); err != nil {
		t.Errorf("ai-service-metrics.md not found: %v", err)
	}
}

// TestRenderWithNow_DeterministicTimestamp verifies that WithNow injects a
// fixed timestamp into the rendered index, which is required for
// reproducible-build (SLSA) callers that hash the output.
//
// Use a far-future date so the assertion can't false-pass against the
// current wall clock — checking only the year would silently succeed in
// 2026 even if WithNow were ignored. The expected substring includes the
// month and day, which uniquely identifies the injected value.
func TestRenderWithNow_DeterministicTimestamp(t *testing.T) {
	dir := t.TempDir()
	injected := time.Date(2099, 7, 14, 12, 30, 45, 0, time.UTC)
	r := New(WithOutputDir(dir), WithNow(injected))

	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "dra-support", Status: "passed", Duration: 1000},
			},
		},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read index.md: %v", err)
	}
	// Match the date portion of the Go default time format
	// ("2099-07-14 12:30:45 +0000 UTC"). This catches the injection
	// without pinning the exact suffix.
	want := "2099-07-14"
	if !strings.Contains(string(body), want) {
		t.Errorf("index.md does not contain injected date %q: %s", want, body)
	}
}

// TestRenderWithNow_FreshTimestampPerCall confirms the wall-clock path is
// captured per Render call and not memoized on the Renderer: two
// non-deterministic renders separated by a sleep must produce index.md
// outputs that differ in the embedded timestamp. Asserting on the
// rendered bytes (not on a Renderer field) is what actually catches a
// regression where the timestamp gets memoized across calls.
func TestRenderWithNow_FreshTimestampPerCall(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "dra-support", Status: "passed", Duration: 1000},
			},
		},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render 1 failed: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read first index.md: %v", err)
	}
	// Sleep so the rendered Go-formatted time (second precision) observably
	// advances even on fast hosts.
	time.Sleep(1100 * time.Millisecond)
	if err = r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render 2 failed: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read second index.md: %v", err)
	}
	if string(first) == string(second) {
		t.Errorf("expected different index.md across renders in non-deterministic mode; got identical output:\n%s", first)
	}
}

func TestGetRequirement(t *testing.T) {
	tests := []struct {
		name           string
		version        Version
		wantNil        bool
		wantFile       string
		wantLevel      Level
		wantSupporting bool
	}{
		{"dra-support", Version135, false, "dra-support.md", LevelMust, false},
		{"dra-support", Version136, false, "dra-support.md", "", true}, // retired at v1.36: supporting evidence
		{"dra-support", Version137, false, "dra-support.md", "", true},
		{"gang-scheduling", Version137, false, "gang-scheduling.md", LevelMust, false},
		{"accelerator-metrics", Version136, false, "accelerator-metrics.md", LevelMust, false},
		{"ai-service-metrics", Version136, false, "ai-service-metrics.md", LevelMust, false},
		{"secure-accelerator-access", Version137, false, "secure-accelerator-access.md", LevelMust, false},
		{"slinky-slurm-gpu-access", Version137, false, "secure-accelerator-access.md", LevelMust, false},
		{"driver-runtime-management", Version137, false, "driver-runtime-management.md", LevelShould, false}, // MUST only from v1.38 (KAR-0001)
		{"driver-runtime-management", Version135, false, "driver-runtime-management.md", LevelShould, false},
		{"workload-sandboxing", Version137, false, "workload-sandboxing.md", LevelShould, false}, // MUST only from v1.38 (KAR-0020)
		{"workload-sandboxing", Version136, false, "workload-sandboxing.md", "", true},           // not a requirement before v1.37: supporting evidence
		{"gpu-operator-health", Version137, true, "", "", false},                                 // diagnostic
		{"platform-health", Version137, true, "", "", false},                                     // diagnostic
		{"nonexistent", Version137, true, "", "", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.version)+"/"+tt.name, func(t *testing.T) {
			meta, err := GetRequirement(tt.version, tt.name)
			if err != nil {
				t.Fatalf("GetRequirement: %v", err)
			}
			if tt.wantNil {
				if meta != nil {
					t.Errorf("expected nil for %q, got %+v", tt.name, meta)
				}
				return
			}
			if meta == nil {
				t.Fatalf("expected non-nil for %q", tt.name)
			}
			if meta.File != tt.wantFile {
				t.Errorf("File = %q, want %q", meta.File, tt.wantFile)
			}
			if meta.Level != tt.wantLevel || meta.Supporting != tt.wantSupporting {
				t.Errorf("Level/Supporting = %q/%v, want %q/%v", meta.Level, meta.Supporting, tt.wantLevel, tt.wantSupporting)
			}
		})
	}

	// An unsupported version is refused even for a submission check, and a
	// non-submission check reports nil without consulting the checklist.
	if _, err := GetRequirement("v1.38", "gang-scheduling"); err == nil {
		t.Error("expected an error for an unsupported version")
	}
	if meta, err := GetRequirement("v1.38", "platform-health"); err != nil || meta != nil {
		t.Errorf("diagnostic check at unsupported version = (%v, %v), want (nil, nil)", meta, err)
	}
}

// At v1.36 and later dra_support is no longer an upstream requirement: its
// page still renders, labeled as supporting evidence, and the index lists
// it under Supporting Evidence rather than Results.
func TestRenderDRASupportIsSupportingFromV136(t *testing.T) {
	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "dra-support", Status: "passed", Duration: 5000, Stdout: []string{"ResourceSlices validated"}},
				{Name: "gang-scheduling", Status: "passed", Duration: 8000},
			},
		},
	}
	tests := []struct {
		version        Version
		wantIndex      []string
		wantPage       []string
		wantNotInIndex []string
	}{
		{
			version:        Version135,
			wantIndex:      []string{"**Checklist:** v1.35", "| `dra_support` | MUST |", "| `gang_scheduling` | MUST |"},
			wantPage:       []string{"**Requirement:** `dra_support` (MUST)"},
			wantNotInIndex: []string{"## Supporting Evidence"},
		},
		{
			version:   Version136,
			wantIndex: []string{"**Checklist:** v1.36", "| `gang_scheduling` | MUST |", "## Supporting Evidence", "| `dra_support` | DRA Support"},
			wantPage: []string{"**Checklist:** v1.36",
				"**Supporting evidence:** `dra_support` is not a requirement of the v1.36 checklist"},
			wantNotInIndex: []string{"| `dra_support` | MUST |"},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			dir := t.TempDir()
			r := New(WithOutputDir(dir), WithConformanceVersion(tt.version))
			if r.Version() != tt.version {
				t.Fatalf("Version() = %q", r.Version())
			}
			if err := r.Render(context.Background(), report); err != nil {
				t.Fatalf("Render: %v", err)
			}
			index, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.wantIndex {
				if !strings.Contains(string(index), want) {
					t.Errorf("index.md missing %q:\n%s", want, index)
				}
			}
			for _, bad := range tt.wantNotInIndex {
				if strings.Contains(string(index), bad) {
					t.Errorf("index.md must not contain %q:\n%s", bad, index)
				}
			}
			page, err := os.ReadFile(filepath.Join(dir, "dra-support.md")) //nolint:gosec // test-controlled path
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.wantPage {
				if !strings.Contains(string(page), want) {
					t.Errorf("dra-support.md missing %q:\n%s", want, page)
				}
			}
		})
	}
}

func TestRenderDefaultsToLatestVersion(t *testing.T) {
	r := New(WithOutputDir(t.TempDir()))
	if r.Version() != LatestVersion {
		t.Errorf("default Version() = %q, want %q", r.Version(), LatestVersion)
	}
}

func TestRenderRefusesUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir), WithConformanceVersion("v1.38"))
	err := r.Render(context.Background(), &ctrf.Report{
		Results: ctrf.Results{Tests: []ctrf.TestResult{{Name: "gang-scheduling", Status: "passed"}}},
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported checklist version")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("evidence dir not left empty: %v", entries)
	}
}

// Vendor names stay out of page titles: the implementation a check observed
// belongs in the check output, not in the requirement heading.
func TestRenderTitlesAreVendorNeutral(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))
	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "inference-gateway", Status: "passed", Stdout: []string{"GatewayClass agentgateway Accepted"}},
				{Name: "robust-controller", Status: "passed", Stdout: []string{"operator: kubeflow-trainer"}},
			},
		},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	gw, _ := os.ReadFile(filepath.Join(dir, "inference-gateway.md")) //nolint:gosec // test-controlled path
	op, _ := os.ReadFile(filepath.Join(dir, "robust-operator.md"))   //nolint:gosec // test-controlled path
	if !strings.HasPrefix(string(gw), "# Inference API Gateway\n") {
		t.Errorf("gateway title line: %q", strings.SplitN(string(gw), "\n", 2)[0])
	}
	if !strings.HasPrefix(string(op), "# Robust AI Operator\n") {
		t.Errorf("operator title line: %q", strings.SplitN(string(op), "\n", 2)[0])
	}
	// The observed implementation survives in the evidence body.
	if !strings.Contains(string(gw), "GatewayClass agentgateway Accepted") {
		t.Error("check output dropped from the gateway page")
	}
}

// On a Slinky Slurm recipe secure-accelerator-access skips, and the
// secure_accelerator_access page must come from slinky-slurm-gpu-access alone.
func TestRenderSlurmGPUAccessFillsSecureAcceleratorAccessPage(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))
	report := &ctrf.Report{
		Results: ctrf.Results{
			Tests: []ctrf.TestResult{
				{Name: "secure-accelerator-access", Status: "skipped"},
				{Name: "slinky-slurm-gpu-access", Status: "passed", Duration: 4000,
					Stdout: []string{"--- Slinky Slurm GPU access and isolation ---"}},
			},
		},
	}
	if err := r.Render(context.Background(), report); err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "secure-accelerator-access.md")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("secure-accelerator-access.md not rendered: %v", err)
	}
	for _, want := range []string{
		"# Secure Accelerator Access\n",
		"**Requirement:** `secure_accelerator_access` (MUST)",
		"### slinky-slurm-gpu-access",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("secure-accelerator-access.md missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(string(body), "### secure-accelerator-access") {
		t.Errorf("skipped secure-accelerator-access must not appear on the page:\n%s", body)
	}
}

// The index lists the whole checklist, not only what ran: a requirement no
// result evidenced appears as NOT TESTED with its level, no page and no link.
// The verdicts come from AggregateReport; see aggregate_test.go.
func TestRenderIndexListsUntestedRequirements(t *testing.T) {
	allPassing := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "dra-support", Status: "passed"},
		{Name: "gang-scheduling", Status: "passed"},
		{Name: "accelerator-metrics", Status: "passed"},
		{Name: "ai-service-metrics", Status: "passed"},
		{Name: "inference-gateway", Status: "passed"},
		{Name: "pod-autoscaling", Status: "passed"},
		{Name: "cluster-autoscaling", Status: "passed", Extra: stamped},
		{Name: "robust-controller", Status: "passed"},
		{Name: "secure-accelerator-access", Status: "passed"},
	}}}
	tests := []struct {
		version  Version
		wantRows []string
		absent   []string
	}{
		{Version136, []string{
			// driver_runtime_management has a check from #3615; nothing ran it
			// in this report, so it is not run rather than uncovered.
			"| `driver_runtime_management` | SHOULD | | NOT TESTED (not run) | |",
			"| `gpu_sharing` | SHOULD | | NOT TESTED (no AICR check) | |",
			"| `virtualized_accelerator` | SHOULD | | NOT TESTED (no AICR check) | |",
			"| `advanced_inference_ingress` | SHOULD | | NOT TESTED (no AICR check) | |",
			"| `high_performance_networking` | SHOULD | | NOT TESTED (no AICR check) | |",
			"| `disaggregated_inference` | SHOULD | | NOT TESTED (no AICR check) | |",
		}, []string{"`workload_sandboxing`", "| `gang_scheduling` | MUST | | NOT TESTED"}},
		{Version137, []string{
			// workload_sandboxing has a check from #3615; nothing ran it in
			// this report, so it is not run rather than uncovered.
			"| `workload_sandboxing` | SHOULD | | NOT TESTED (not run) | |",
			"| `disaggregated_inference` | SHOULD | | NOT TESTED (no AICR check) | |",
		}, []string{"| `gang_scheduling` | MUST | | NOT TESTED"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			dir := t.TempDir()
			if err := New(WithOutputDir(dir), WithConformanceVersion(tt.version)).Render(context.Background(), allPassing); err != nil {
				t.Fatal(err)
			}
			index, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.wantRows {
				if !strings.Contains(string(index), want) {
					t.Errorf("index.md missing %q:\n%s", want, index)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(string(index), bad) {
					t.Errorf("index.md must not contain %q:\n%s", bad, index)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "workload-sandboxing.md")); err == nil {
				t.Error("an untested requirement must not get a page")
			}
		})
	}

	// A MUST whose check exists but produced no result reads "not run".
	dir := t.TempDir()
	partial := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{{Name: "gang-scheduling", Status: "passed"}}}}
	if err := New(WithOutputDir(dir), WithConformanceVersion(Version137)).Render(context.Background(), partial); err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	for _, want := range []string{
		"| `secure_accelerator_access` | MUST | | NOT TESTED (not run) | |",
		"| `robust_controller` | MUST | | NOT TESTED (not run) | |",
		"| `workload_sandboxing` | SHOULD | | NOT TESTED (not run) | |",
		"| `gpu_sharing` | SHOULD | | NOT TESTED (no AICR check) | |",
	} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.md missing %q:\n%s", want, index)
		}
	}

	// A report with no submission-required checks still writes nothing.
	empty := t.TempDir()
	if err := New(WithOutputDir(empty)).Render(context.Background(), &ctrf.Report{Results: ctrf.Results{
		Tests: []ctrf.TestResult{{Name: "platform-health", Status: "passed"}}}}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("report without submission checks wrote %v", entries)
	}
}

// A requirement every check of which was skipped gets no page, and the index
// records why it is not tested: the skip reason code rides the result cell.
func TestRenderSkippedReasonReachesIndex(t *testing.T) {
	dir := t.TempDir()
	report := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "gang-scheduling", Status: "passed"},
		{Name: "pod-autoscaling", Status: "skipped", Extra: map[string]string{"skipReason": "no-metrics-api"}},
		{Name: "robust-controller", Status: "other", Message: "validator crashed"},
	}}}
	agg, err := New(WithOutputDir(dir), WithConformanceVersion(Version137)).RenderWithResult(context.Background(), report)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Passed() {
		t.Error("Passed() = true with a skipped and a crashed MUST")
	}
	index, err := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"**Applicable MUSTs:** FAILED",
		"| `gang_scheduling` | MUST | Gang Scheduling | PASSED | [gang-scheduling.md](gang-scheduling.md) |",
		"| `pod_autoscaling` | MUST | | NOT TESTED (pod-autoscaling skipped: no-metrics-api) | |",
		"| `robust_controller` | MUST | Robust AI Operator | NOT TESTED (robust-controller other) | [robust-operator.md](robust-operator.md) |",
	} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.md missing %q:\n%s", want, index)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pod-autoscaling.md")); err == nil {
		t.Error("a requirement whose only check was skipped must not get a page")
	}
	page, _ := os.ReadFile(filepath.Join(dir, "gang-scheduling.md")) //nolint:gosec // test-controlled path
	if !strings.Contains(string(page), "**Result:** PASSED\n**Proof:** gang-scheduling") {
		t.Errorf("gang-scheduling.md lacks the proof line:\n%s", page)
	}
}

// A not-applicable declaration renders from configuration and overrides a
// check that ran, whose result stays on the page as an observation.
func TestRenderNotApplicable(t *testing.T) {
	dir := t.TempDir()
	report := &ctrf.Report{Results: ctrf.Results{Tests: []ctrf.TestResult{
		{Name: "cluster-autoscaling", Status: "failed", Message: "no node group scaled"},
		{Name: "gang-scheduling", Status: "passed"},
	}}}
	r := New(WithOutputDir(dir), WithConformanceVersion(Version137),
		WithNotApplicable(map[string]string{"cluster_autoscaling": "fixed-size | bare-metal\ncluster"}))
	agg, err := r.RenderWithResult(context.Background(), report)
	if err != nil {
		t.Fatal(err)
	}
	ca := agg.Requirement("cluster_autoscaling")
	if ca.Status != StatusNotApplicable || ca.Reason != "fixed-size | bare-metal\ncluster" || ca.Applicable() {
		t.Errorf("cluster_autoscaling = %+v", *ca)
	}
	for _, m := range agg.IncompleteMusts() {
		if m.ID == "cluster_autoscaling" {
			t.Error("a not-applicable MUST must not be reported incomplete")
		}
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	// The justification is escaped for the table cell, and left verbatim on
	// the page, which is not a table.
	want := "| `cluster_autoscaling` | MUST | Cluster Autoscaling | NOT APPLICABLE (fixed-size \\| bare-metal cluster) | [cluster-autoscaling.md](cluster-autoscaling.md) |"
	if !strings.Contains(string(index), want) {
		t.Errorf("index.md missing %q:\n%s", want, index)
	}
	page, _ := os.ReadFile(filepath.Join(dir, "cluster-autoscaling.md")) //nolint:gosec // test-controlled path
	for _, want := range []string{"**Result:** NOT APPLICABLE (fixed-size | bare-metal\ncluster)", "### cluster-autoscaling", "no node group scaled"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("cluster-autoscaling.md missing %q:\n%s", want, page)
		}
	}

	// Any other id is refused before anything is written.
	empty := t.TempDir()
	r = New(WithOutputDir(empty), WithNotApplicable(map[string]string{"gang_scheduling": "we do not gang"}))
	if _, err := r.RenderWithResult(context.Background(), report); err == nil {
		t.Fatal("expected an error for a non-conditional requirement")
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("evidence dir not left empty: %v", entries)
	}
}

// TestRenderObserved pins how an observation-only pass renders (#3681): the
// index row and the page read OBSERVED with the check named, the index
// header reads FAILED since the MUST is not established, no Proof line is
// written, and the check's own CTRF status stays passed on the page.
func TestRenderObserved(t *testing.T) {
	// Every other MUST passed, so the observed one alone decides the run.
	report := reportOf(withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{
		Name: "cluster-autoscaling", Status: "passed", Extra: map[string]string{"observationOnly": "true"},
		Stdout: []string{"OBSERVED — AKS cluster with 2 GPU node(s)"}})...)

	dir := t.TempDir()
	agg, err := New(WithOutputDir(dir), WithConformanceVersion(Version137)).RenderWithResult(context.Background(), report)
	if err != nil {
		t.Fatal(err)
	}
	ca := agg.Requirement("cluster_autoscaling")
	if ca.Status != StatusObserved || !ca.Applicable() || ca.Proof != nil {
		t.Errorf("cluster_autoscaling = %+v", *ca)
	}
	if ids := incompleteIDs(agg); !slices.Equal(ids, []string{"cluster_autoscaling"}) {
		t.Errorf("IncompleteMusts = %v, want the observed MUST alone", ids)
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	for _, want := range []string{
		"**Applicable MUSTs:** FAILED",
		"| `cluster_autoscaling` | MUST | Cluster Autoscaling | OBSERVED (cluster-autoscaling observed, not demonstrated) | [cluster-autoscaling.md](cluster-autoscaling.md) |",
		"| `gang_scheduling` | MUST | Gang Scheduling | PASSED | [gang-scheduling.md](gang-scheduling.md) |",
	} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.md missing %q:\n%s", want, index)
		}
	}
	page, _ := os.ReadFile(filepath.Join(dir, "cluster-autoscaling.md")) //nolint:gosec // test-controlled path
	for _, want := range []string{"**Result:** OBSERVED (cluster-autoscaling observed, not demonstrated)", "- **Status:** PASSED", "OBSERVED — AKS cluster"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("cluster-autoscaling.md missing %q:\n%s", want, page)
		}
	}
	if strings.Contains(string(page), "**Proof:**") {
		t.Errorf("an observed requirement must not claim a proof:\n%s", page)
	}

	// The declaration still takes precedence: the same report renders NOT
	// APPLICABLE with the justification and the MUST is not counted.
	dir = t.TempDir()
	agg, err = New(WithOutputDir(dir), WithConformanceVersion(Version137),
		WithNotApplicable(map[string]string{"cluster_autoscaling": "fixed-size cluster"})).RenderWithResult(context.Background(), report)
	if err != nil {
		t.Fatal(err)
	}
	if !agg.Passed() {
		t.Errorf("Passed() = false with the observed MUST declared not applicable: %v", incompleteIDs(agg))
	}
	index, _ = os.ReadFile(filepath.Join(dir, "index.md")) //nolint:gosec // test-controlled path
	want := "| `cluster_autoscaling` | MUST | Cluster Autoscaling | NOT APPLICABLE (fixed-size cluster) | [cluster-autoscaling.md](cluster-autoscaling.md) |"
	if !strings.Contains(string(index), want) || !strings.Contains(string(index), "**Applicable MUSTs:** PASSED") {
		t.Errorf("index.md missing %q or the PASSED header:\n%s", want, index)
	}
}
