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

package ctrf

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigMapName(t *testing.T) {
	tests := []struct {
		name     string
		runID    string
		phase    string
		expected string
	}{
		{"deployment phase", "20260305-abc123", "deployment", "aicr-ctrf-20260305-abc123-deployment"},
		{"conformance phase", "run1", "conformance", "aicr-ctrf-run1-conformance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConfigMapName(tt.runID, tt.phase)
			if got != tt.expected {
				t.Errorf("ConfigMapName(%q, %q) = %q, want %q", tt.runID, tt.phase, got, tt.expected)
			}
		})
	}
}

func buildTestReport() *Report {
	b := NewBuilder("aicr", "0.8.12", "deployment")
	b.AddResult(&ValidatorResult{
		Name:     "gpu-operator-health",
		Phase:    "deployment",
		ExitCode: 0,
		Duration: 15 * time.Second,
		Stdout:   []string{"All pods running"},
	})
	b.AddResult(&ValidatorResult{
		Name:           "expected-resources",
		Phase:          "deployment",
		ExitCode:       1,
		Duration:       8 * time.Second,
		TerminationMsg: "DaemonSet check failed",
	})
	return b.Build()
}

func TestWriteAndReadCTRFConfigMap(t *testing.T) {
	requireEnvtest(t)
	ns := createUniqueNamespace(t)
	ctx := context.Background()
	runID := "20260305-abc123"
	phase := testPhase

	report := buildTestReport()

	// Write
	if writeErr := WriteCTRFConfigMap(ctx, testClientset, ns, runID, phase, report); writeErr != nil {
		t.Fatalf("WriteCTRFConfigMap failed: %v", writeErr)
	}

	// Verify ConfigMap exists with correct labels
	cmName := ConfigMapName(runID, phase)
	cm, err := testClientset.CoreV1().ConfigMaps(ns).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}
	if cm.Labels["aicr.nvidia.com/run-id"] != runID {
		t.Errorf("label run-id = %q, want %q", cm.Labels["aicr.nvidia.com/run-id"], runID)
	}
	if cm.Labels["aicr.nvidia.com/phase"] != phase {
		t.Errorf("label phase = %q, want %q", cm.Labels["aicr.nvidia.com/phase"], phase)
	}
	if cm.Labels["aicr.nvidia.com/report-type"] != "ctrf" {
		t.Errorf("label report-type = %q, want %q", cm.Labels["aicr.nvidia.com/report-type"], "ctrf")
	}

	// Read back
	decoded, err := ReadCTRFConfigMap(ctx, testClientset, ns, runID, phase)
	if err != nil {
		t.Fatalf("ReadCTRFConfigMap failed: %v", err)
	}

	if decoded.ReportFormat != ReportFormatCTRF {
		t.Errorf("ReportFormat = %q, want %q", decoded.ReportFormat, ReportFormatCTRF)
	}
	if decoded.Results.Summary.Tests != 2 {
		t.Errorf("Summary.Tests = %d, want 2", decoded.Results.Summary.Tests)
	}
	if decoded.Results.Summary.Passed != 1 {
		t.Errorf("Summary.Passed = %d, want 1", decoded.Results.Summary.Passed)
	}
	if decoded.Results.Summary.Failed != 1 {
		t.Errorf("Summary.Failed = %d, want 1", decoded.Results.Summary.Failed)
	}
	if len(decoded.Results.Tests) != 2 {
		t.Fatalf("Tests length = %d, want 2", len(decoded.Results.Tests))
	}
	if decoded.Results.Tests[0].Name != "gpu-operator-health" {
		t.Errorf("Tests[0].Name = %q, want %q", decoded.Results.Tests[0].Name, "gpu-operator-health")
	}
}

func TestWriteCTRFConfigMapUpdateExisting(t *testing.T) {
	requireEnvtest(t)
	ns := createUniqueNamespace(t)
	ctx := context.Background()
	runID := "run1"
	phase := testPhase

	// Write first version
	report1 := buildTestReport()
	if err := WriteCTRFConfigMap(ctx, testClientset, ns, runID, phase, report1); err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Write second version (should update, not fail)
	b := NewBuilder("aicr", "0.9.0", "deployment")
	b.AddResult(&ValidatorResult{
		Name:     "new-check",
		Phase:    "deployment",
		ExitCode: 0,
		Duration: 1 * time.Second,
	})
	report2 := b.Build()

	if err := WriteCTRFConfigMap(ctx, testClientset, ns, runID, phase, report2); err != nil {
		t.Fatalf("second write (update) failed: %v", err)
	}

	// Read back — should be the second version
	decoded, err := ReadCTRFConfigMap(ctx, testClientset, ns, runID, phase)
	if err != nil {
		t.Fatalf("ReadCTRFConfigMap failed: %v", err)
	}
	if decoded.Results.Summary.Tests != 1 {
		t.Errorf("Summary.Tests = %d, want 1 (from second write)", decoded.Results.Summary.Tests)
	}
	if decoded.Results.Tests[0].Name != "new-check" {
		t.Errorf("Tests[0].Name = %q, want %q", decoded.Results.Tests[0].Name, "new-check")
	}
}

func TestReadCTRFConfigMapNotFound(t *testing.T) {
	requireEnvtest(t)
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	_, err := ReadCTRFConfigMap(ctx, testClientset, ns, "nonexistent", "deployment")
	if err == nil {
		t.Fatal("expected error for nonexistent ConfigMap")
	}
}

func TestDeleteCTRFConfigMap(t *testing.T) {
	requireEnvtest(t)
	ns := createUniqueNamespace(t)
	ctx := context.Background()
	runID := "run1"
	phase := testPhase

	// Write a ConfigMap
	report := buildTestReport()
	if err := WriteCTRFConfigMap(ctx, testClientset, ns, runID, phase, report); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Delete it
	if err := DeleteCTRFConfigMap(ctx, testClientset, ns, runID, phase); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	// Verify it's gone
	_, err := ReadCTRFConfigMap(ctx, testClientset, ns, runID, phase)
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestDeleteCTRFConfigMapNotFound(t *testing.T) {
	requireEnvtest(t)
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	// Should not error when ConfigMap doesn't exist
	if err := DeleteCTRFConfigMap(ctx, testClientset, ns, "nonexistent", "deployment"); err != nil {
		t.Fatalf("delete of nonexistent ConfigMap should not error, got: %v", err)
	}
}
