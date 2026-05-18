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
		name     string
		wantNil  bool
		wantFile string
	}{
		{"dra-support", false, "dra-support.md"},
		{"gang-scheduling", false, "gang-scheduling.md"},
		{"accelerator-metrics", false, "accelerator-metrics.md"},
		{"ai-service-metrics", false, "ai-service-metrics.md"},
		{"gpu-operator-health", true, ""}, // diagnostic
		{"platform-health", true, ""},     // diagnostic
		{"nonexistent", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := GetRequirement(tt.name)
			if tt.wantNil && meta != nil {
				t.Errorf("expected nil for %q, got %+v", tt.name, meta)
			}
			if !tt.wantNil && meta == nil {
				t.Fatalf("expected non-nil for %q", tt.name)
			}
			if !tt.wantNil && meta.File != tt.wantFile {
				t.Errorf("File = %q, want %q", meta.File, tt.wantFile)
			}
		})
	}
}
