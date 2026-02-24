// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/checks"

	// Import conformance checks to register them.
	_ "github.com/NVIDIA/aicr/pkg/validator/checks/conformance"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name        string
		result      *validator.ValidationResult
		wantErr     bool
		errContains string
		wantFiles   []string // expected files in output dir
		wantAbsent  []string // files that should NOT exist
	}{
		{
			name: "full result with passing checks",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusPass,
						Checks: []validator.CheckResult{
							{Name: "dra-support", Status: validator.ValidationStatusPass, Reason: "all healthy", Duration: 5 * time.Second},
							{Name: "gang-scheduling", Status: validator.ValidationStatusPass, Duration: 3 * time.Second},
							{Name: "accelerator-metrics", Status: validator.ValidationStatusPass, Duration: 2 * time.Second},
							{Name: "ai-service-metrics", Status: validator.ValidationStatusPass, Duration: 1 * time.Second},
						},
					},
				},
			},
			wantFiles: []string{"index.md", "dra-support.md", "gang-scheduling.md", "accelerator-metrics.md"},
		},
		{
			name: "identity mismatch: test names resolved",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusPass,
						Checks: []validator.CheckResult{
							{Name: "TestDRASupport", Status: validator.ValidationStatusPass},
							{Name: "TestGangScheduling", Status: validator.ValidationStatusPass},
						},
					},
				},
			},
			wantFiles: []string{"index.md", "dra-support.md", "gang-scheduling.md"},
		},
		{
			name: "skipped checks excluded from evidence",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusPass,
						Checks: []validator.CheckResult{
							{Name: "dra-support", Status: validator.ValidationStatusSkipped, Reason: "skipped - no-cluster mode (test mode)"},
							{Name: "gang-scheduling", Status: validator.ValidationStatusPass, Duration: 3 * time.Second},
						},
					},
				},
			},
			wantFiles:  []string{"index.md", "gang-scheduling.md"},
			wantAbsent: []string{"dra-support.md"},
		},
		{
			name: "non-submission checks excluded",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusPass,
						Checks: []validator.CheckResult{
							{Name: "platform-health", Status: validator.ValidationStatusPass},
							{Name: "gpu-operator-health", Status: validator.ValidationStatusPass},
							{Name: "dra-support", Status: validator.ValidationStatusPass},
						},
					},
				},
			},
			wantFiles:  []string{"index.md", "dra-support.md"},
			wantAbsent: []string{"platform-health.md", "gpu-operator-health.md"},
		},
		{
			name: "shared EvidenceFile grouping",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusPass,
						Checks: []validator.CheckResult{
							{Name: "accelerator-metrics", Status: validator.ValidationStatusPass, Duration: 2 * time.Second},
							{Name: "ai-service-metrics", Status: validator.ValidationStatusPass, Duration: 1 * time.Second},
						},
					},
				},
			},
			wantFiles: []string{"index.md", "accelerator-metrics.md"},
		},
		{
			name: "no conformance phase",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"readiness": {Status: validator.ValidationStatusPass},
				},
			},
			wantErr:     true,
			errContains: "no conformance phase",
		},
		{
			name: "all checks skipped produces error",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusPass,
						Checks: []validator.CheckResult{
							{Name: "dra-support", Status: validator.ValidationStatusSkipped},
							{Name: "gang-scheduling", Status: validator.ValidationStatusSkipped},
						},
					},
				},
			},
			wantErr:     true,
			errContains: "no submission checks found",
		},
		{
			name: "failing check produces FAIL evidence",
			result: &validator.ValidationResult{
				RunID: "20260223-120000-test",
				Phases: map[string]*validator.PhaseResult{
					"conformance": {
						Status: validator.ValidationStatusFail,
						Checks: []validator.CheckResult{
							{Name: "dra-support", Status: validator.ValidationStatusFail, Reason: "DRA driver not found"},
						},
					},
				},
			},
			wantFiles: []string{"index.md", "dra-support.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			r := New(WithOutputDir(dir))

			err := r.Render(context.Background(), tt.result)

			if (err != nil) != tt.wantErr {
				t.Errorf("Render() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Render() error = %v, should contain %q", err, tt.errContains)
				}
				return
			}

			// Verify expected files exist.
			for _, f := range tt.wantFiles {
				path := filepath.Join(dir, f)
				info, err := os.Stat(path)
				if err != nil {
					t.Errorf("expected file %s not found: %v", f, err)
					continue
				}
				if info.Size() == 0 {
					t.Errorf("expected file %s is empty", f)
				}
			}

			// Verify absent files.
			for _, f := range tt.wantAbsent {
				path := filepath.Join(dir, f)
				if _, err := os.Stat(path); err == nil {
					t.Errorf("file %s should not exist but does", f)
				}
			}
		})
	}
}

func TestRenderSharedFileContent(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	result := &validator.ValidationResult{
		RunID: "test-shared",
		Phases: map[string]*validator.PhaseResult{
			"conformance": {
				Checks: []validator.CheckResult{
					{Name: "accelerator-metrics", Status: validator.ValidationStatusPass, Reason: "DCGM metrics found", Duration: 2 * time.Second},
					{Name: "ai-service-metrics", Status: validator.ValidationStatusPass, Reason: "Prometheus active", Duration: 1 * time.Second},
				},
			},
		},
	}

	if err := r.Render(context.Background(), result); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	// Read the combined file and verify both checks appear.
	content, err := os.ReadFile(filepath.Join(dir, "accelerator-metrics.md"))
	if err != nil {
		t.Fatalf("failed to read accelerator-metrics.md: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "accelerator-metrics") {
		t.Error("accelerator-metrics.md should contain accelerator-metrics check")
	}
	if !strings.Contains(s, "ai-service-metrics") {
		t.Error("accelerator-metrics.md should contain ai-service-metrics check")
	}
	if !strings.Contains(s, "DCGM metrics found") {
		t.Error("accelerator-metrics.md should contain DCGM metrics reason")
	}
	if !strings.Contains(s, "Prometheus active") {
		t.Error("accelerator-metrics.md should contain Prometheus reason")
	}
}

func TestRenderFailingCheckContent(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	result := &validator.ValidationResult{
		RunID: "test-fail",
		Phases: map[string]*validator.PhaseResult{
			"conformance": {
				Checks: []validator.CheckResult{
					{Name: "dra-support", Status: validator.ValidationStatusFail, Reason: "DRA driver controller not found"},
				},
			},
		},
	}

	if err := r.Render(context.Background(), result); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "dra-support.md"))
	if err != nil {
		t.Fatalf("failed to read dra-support.md: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "FAIL") {
		t.Error("dra-support.md should contain FAIL status")
	}
	if !strings.Contains(s, "DRA driver controller not found") {
		t.Error("dra-support.md should contain failure reason")
	}
}

func TestRenderNoOutputDir(t *testing.T) {
	r := New() // no output dir
	err := r.Render(context.Background(), &validator.ValidationResult{})
	if err == nil {
		t.Fatal("expected error for missing output dir")
	}
	if !strings.Contains(err.Error(), "evidence output directory not set") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRenderIndexContent(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	result := &validator.ValidationResult{
		RunID: "test-index",
		Phases: map[string]*validator.PhaseResult{
			"conformance": {
				Checks: []validator.CheckResult{
					{Name: "dra-support", Status: validator.ValidationStatusPass},
					{Name: "gang-scheduling", Status: validator.ValidationStatusFail, Reason: "KAI not found"},
				},
			},
		},
	}

	if err := r.Render(context.Background(), result); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "index.md"))
	if err != nil {
		t.Fatalf("failed to read index.md: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "dra-support.md") {
		t.Error("index.md should link to dra-support.md")
	}
	if !strings.Contains(s, "gang-scheduling.md") {
		t.Error("index.md should link to gang-scheduling.md")
	}
	if !strings.Contains(s, "PASS") {
		t.Error("index.md should contain PASS")
	}
	if !strings.Contains(s, "FAIL") {
		t.Error("index.md should contain FAIL")
	}
	if !strings.Contains(s, "test-index") {
		t.Error("index.md should contain run ID")
	}
}

func TestRenderWithArtifacts(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	result := &validator.ValidationResult{
		RunID: "test-artifacts",
		Phases: map[string]*validator.PhaseResult{
			"conformance": {
				Checks: []validator.CheckResult{
					{
						Name:     "dra-support",
						Status:   validator.ValidationStatusPass,
						Reason:   "DRA controller healthy",
						Duration: 5 * time.Second,
						Artifacts: []checks.Artifact{
							{Label: "DRA Controller Pods", Data: "NAME                   READY   STATUS\ndra-controller-abc12   1/1     Running"},
							{Label: "ResourceSlice Count", Data: "Total ResourceSlices: 8"},
						},
					},
				},
			},
		},
	}

	if err := r.Render(context.Background(), result); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "dra-support.md"))
	if err != nil {
		t.Fatalf("failed to read dra-support.md: %v", err)
	}

	s := string(content)

	// Verify artifact labels are present.
	if !strings.Contains(s, "#### DRA Controller Pods") {
		t.Error("evidence should contain artifact label 'DRA Controller Pods'")
	}
	if !strings.Contains(s, "#### ResourceSlice Count") {
		t.Error("evidence should contain artifact label 'ResourceSlice Count'")
	}

	// Verify artifact data is present.
	if !strings.Contains(s, "dra-controller-abc12") {
		t.Error("evidence should contain artifact data")
	}
	if !strings.Contains(s, "Total ResourceSlices: 8") {
		t.Error("evidence should contain ResourceSlice count data")
	}

	// Verify the reason is also present (artifacts don't replace reason).
	if !strings.Contains(s, "DRA controller healthy") {
		t.Error("evidence should still contain the reason text")
	}
}

func TestRenderWithoutArtifacts(t *testing.T) {
	dir := t.TempDir()
	r := New(WithOutputDir(dir))

	result := &validator.ValidationResult{
		RunID: "test-no-artifacts",
		Phases: map[string]*validator.PhaseResult{
			"conformance": {
				Checks: []validator.CheckResult{
					{
						Name:     "dra-support",
						Status:   validator.ValidationStatusPass,
						Reason:   "all healthy",
						Duration: 3 * time.Second,
					},
				},
			},
		},
	}

	if err := r.Render(context.Background(), result); err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "dra-support.md"))
	if err != nil {
		t.Fatalf("failed to read dra-support.md: %v", err)
	}

	s := string(content)

	// Verify basic content is present.
	if !strings.Contains(s, "dra-support") {
		t.Error("evidence should contain check name")
	}
	if !strings.Contains(s, "all healthy") {
		t.Error("evidence should contain reason")
	}

	// Verify no artifact headers appear.
	if strings.Contains(s, "####") {
		t.Error("evidence without artifacts should not contain #### headers")
	}
}
