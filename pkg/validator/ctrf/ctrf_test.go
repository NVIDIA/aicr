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

package ctrf

import (
	"encoding/json"
	"testing"
	"time"
)

const testPhase = "deployment"

func TestExitCodeToCTRFStatus(t *testing.T) {
	tests := []struct {
		name     string
		code     int32
		expected string
	}{
		{"exit 0 is passed", 0, StatusPassed},
		{"exit 1 is failed", 1, StatusFailed},
		{"exit 2 is skipped", 2, StatusSkipped},
		{"exit 137 is other", 137, StatusOther},
		{"exit -1 is other", -1, StatusOther},
		{"exit 255 is other", 255, StatusOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExitCodeToCTRFStatus(tt.code)
			if got != tt.expected {
				t.Errorf("ExitCodeToCTRFStatus(%d) = %q, want %q", tt.code, got, tt.expected)
			}
		})
	}
}

func TestValidatorResultCTRFStatus(t *testing.T) {
	r := &ValidatorResult{ExitCode: 1}
	if got := r.CTRFStatus(); got != StatusFailed {
		t.Errorf("CTRFStatus() = %q, want %q", got, StatusFailed)
	}
}

func TestBuilderEmpty(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)
	report := b.Build()

	if report.ReportFormat != ReportFormatCTRF {
		t.Errorf("ReportFormat = %q, want %q", report.ReportFormat, ReportFormatCTRF)
	}
	if report.SpecVersion != SpecVersion {
		t.Errorf("SpecVersion = %q, want %q", report.SpecVersion, SpecVersion)
	}
	if report.GeneratedBy != "aicr" {
		t.Errorf("GeneratedBy = %q, want %q", report.GeneratedBy, "aicr")
	}
	if report.Results.Tool.Name != "aicr" {
		t.Errorf("Tool.Name = %q, want %q", report.Results.Tool.Name, "aicr")
	}
	if report.Results.Tool.Version != "1.0.0" {
		t.Errorf("Tool.Version = %q, want %q", report.Results.Tool.Version, "1.0.0")
	}
	if report.Results.Summary.Tests != 0 {
		t.Errorf("Summary.Tests = %d, want 0", report.Results.Summary.Tests)
	}
	if len(report.Results.Tests) != 0 {
		t.Errorf("Tests length = %d, want 0", len(report.Results.Tests))
	}
}

func TestBuilderAddResult(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)

	b.AddResult(&ValidatorResult{
		Name:           "gpu-operator-health",
		Phase:          testPhase,
		ExitCode:       0,
		Duration:       15 * time.Second,
		Stdout:         []string{"All pods running"},
		TerminationMsg: "",
	})

	b.AddResult(&ValidatorResult{
		Name:           "expected-resources",
		Phase:          testPhase,
		ExitCode:       1,
		Duration:       8 * time.Second,
		Stdout:         []string{"Ready: 6/8"},
		TerminationMsg: "DaemonSet nvidia-driver: expected 8, got 6",
	})

	b.AddResult(&ValidatorResult{
		Name:     "optional-check",
		Phase:    testPhase,
		ExitCode: 2,
		Duration: 1 * time.Second,
	})

	b.AddResult(&ValidatorResult{
		Name:           "crashed-check",
		Phase:          testPhase,
		ExitCode:       137,
		Duration:       30 * time.Second,
		TerminationMsg: "OOMKilled",
	})

	report := b.Build()

	if report.Results.Summary.Tests != 4 {
		t.Errorf("Summary.Tests = %d, want 4", report.Results.Summary.Tests)
	}
	if report.Results.Summary.Passed != 1 {
		t.Errorf("Summary.Passed = %d, want 1", report.Results.Summary.Passed)
	}
	if report.Results.Summary.Failed != 1 {
		t.Errorf("Summary.Failed = %d, want 1", report.Results.Summary.Failed)
	}
	if report.Results.Summary.Skipped != 1 {
		t.Errorf("Summary.Skipped = %d, want 1", report.Results.Summary.Skipped)
	}
	if report.Results.Summary.Other != 1 {
		t.Errorf("Summary.Other = %d, want 1", report.Results.Summary.Other)
	}

	// Verify individual test entries
	tests := report.Results.Tests

	if tests[0].Name != "gpu-operator-health" || tests[0].Status != StatusPassed {
		t.Errorf("tests[0] = {%q, %q}, want {%q, %q}", tests[0].Name, tests[0].Status, "gpu-operator-health", StatusPassed)
	}
	if tests[0].Duration != 15000 {
		t.Errorf("tests[0].Duration = %d, want 15000", tests[0].Duration)
	}
	if len(tests[0].Suite) != 1 || tests[0].Suite[0] != testPhase {
		t.Errorf("tests[0].Suite = %v, want [testPhase]", tests[0].Suite)
	}
	if tests[0].Message != "" {
		t.Errorf("tests[0].Message = %q, want empty", tests[0].Message)
	}

	if tests[1].Name != "expected-resources" || tests[1].Status != StatusFailed {
		t.Errorf("tests[1] = {%q, %q}, want {%q, %q}", tests[1].Name, tests[1].Status, "expected-resources", StatusFailed)
	}
	if tests[1].Message != "DaemonSet nvidia-driver: expected 8, got 6" {
		t.Errorf("tests[1].Message = %q, want failure message", tests[1].Message)
	}

	if tests[2].Status != StatusSkipped {
		t.Errorf("tests[2].Status = %q, want %q", tests[2].Status, StatusSkipped)
	}

	if tests[3].Status != StatusOther {
		t.Errorf("tests[3].Status = %q, want %q", tests[3].Status, StatusOther)
	}
}

func TestBuilderAddSkipped(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", "conformance")
	b.AddSkipped("dra-support", "conformance", "skipped - no-cluster mode")

	report := b.Build()

	if report.Results.Summary.Tests != 1 {
		t.Errorf("Summary.Tests = %d, want 1", report.Results.Summary.Tests)
	}
	if report.Results.Summary.Skipped != 1 {
		t.Errorf("Summary.Skipped = %d, want 1", report.Results.Summary.Skipped)
	}

	tr := report.Results.Tests[0]
	if tr.Name != "dra-support" {
		t.Errorf("Name = %q, want %q", tr.Name, "dra-support")
	}
	if tr.Status != StatusSkipped {
		t.Errorf("Status = %q, want %q", tr.Status, StatusSkipped)
	}
	if tr.Message != "skipped - no-cluster mode" {
		t.Errorf("Message = %q, want %q", tr.Message, "skipped - no-cluster mode")
	}
	if len(tr.Suite) != 1 || tr.Suite[0] != "conformance" {
		t.Errorf("Suite = %v, want [conformance]", tr.Suite)
	}
}

func TestBuilderSetEnvironment(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)
	b.SetEnvironment(&Environment{
		AppName:         "aicr",
		AppVersion:      "1.0.0",
		TestEnvironment: "eks-h100-training",
	})

	report := b.Build()

	if report.Results.Environment == nil {
		t.Fatal("Environment is nil")
	}
	if report.Results.Environment.TestEnvironment != "eks-h100-training" {
		t.Errorf("TestEnvironment = %q, want %q", report.Results.Environment.TestEnvironment, "eks-h100-training")
	}
}

func TestBuilderNoEnvironment(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)
	report := b.Build()

	if report.Results.Environment != nil {
		t.Errorf("Environment should be nil when not set, got %+v", report.Results.Environment)
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	b := NewBuilder("aicr", "0.8.12", testPhase)
	b.AddResult(&ValidatorResult{
		Name:     "gpu-operator-health",
		Phase:    testPhase,
		ExitCode: 0,
		Duration: 15 * time.Second,
		Stdout:   []string{"All pods running", "ClusterPolicy: ready"},
	})
	b.AddResult(&ValidatorResult{
		Name:           "expected-resources",
		Phase:          testPhase,
		ExitCode:       1,
		Duration:       8 * time.Second,
		TerminationMsg: "DaemonSet check failed",
		Stdout:         []string{"Ready: 6/8"},
	})

	original := b.Build()

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	// Verify top-level fields survive round-trip
	if decoded.ReportFormat != ReportFormatCTRF {
		t.Errorf("ReportFormat = %q, want %q", decoded.ReportFormat, ReportFormatCTRF)
	}
	if decoded.SpecVersion != SpecVersion {
		t.Errorf("SpecVersion = %q, want %q", decoded.SpecVersion, SpecVersion)
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

	// Verify test entries survive round-trip
	if len(decoded.Results.Tests) != 2 {
		t.Fatalf("Tests length = %d, want 2", len(decoded.Results.Tests))
	}
	if decoded.Results.Tests[0].Stdout[0] != "All pods running" {
		t.Errorf("Tests[0].Stdout[0] = %q, want %q", decoded.Results.Tests[0].Stdout[0], "All pods running")
	}
	if decoded.Results.Tests[1].Message != "DaemonSet check failed" {
		t.Errorf("Tests[1].Message = %q, want %q", decoded.Results.Tests[1].Message, "DaemonSet check failed")
	}
}

func TestReportJSONOmitsEmptyOptionalFields(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)
	b.AddResult(&ValidatorResult{
		Name:     "simple-check",
		Phase:    testPhase,
		ExitCode: 0,
		Duration: 1 * time.Second,
	})

	report := b.Build()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	results := raw["results"].(map[string]any)
	tests := results["tests"].([]any)
	test := tests[0].(map[string]any)

	if _, exists := test["message"]; exists {
		t.Error("message should be omitted when empty")
	}
	if _, exists := test["stdout"]; exists {
		t.Error("stdout should be omitted when empty")
	}
	if _, exists := results["environment"]; exists {
		t.Error("environment should be omitted when nil")
	}
}

func TestBuilderTimestamps(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)
	report := b.Build()

	if report.Timestamp == "" {
		t.Error("Timestamp should not be empty")
	}
	if report.Results.Summary.Start == 0 {
		t.Error("Summary.Start should not be zero")
	}
	if report.Results.Summary.Stop == 0 {
		t.Error("Summary.Stop should not be zero")
	}
	if report.Results.Summary.Stop < report.Results.Summary.Start {
		t.Errorf("Summary.Stop (%d) should be >= Summary.Start (%d)",
			report.Results.Summary.Stop, report.Results.Summary.Start)
	}
}

func TestMergeReportsSingleReport(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", "deployment")
	b.AddResult(&ValidatorResult{
		Name:     "v1",
		Phase:    "deployment",
		ExitCode: 0,
		Duration: 5 * time.Second,
	})
	original := b.Build()

	merged := MergeReports("aicr", "1.0.0", []*Report{original})
	if merged != original {
		t.Error("MergeReports with single report should return the same pointer")
	}
}

func TestMergeReportsMultiple(t *testing.T) {
	b1 := NewBuilder("aicr", "1.0.0", "deployment")
	b1.AddResult(&ValidatorResult{Name: "v1", Phase: "deployment", ExitCode: 0, Duration: 5 * time.Second})
	b1.AddResult(&ValidatorResult{Name: "v2", Phase: "deployment", ExitCode: 1, Duration: 3 * time.Second})
	r1 := b1.Build()

	b2 := NewBuilder("aicr", "1.0.0", "performance")
	b2.AddResult(&ValidatorResult{Name: "v3", Phase: "performance", ExitCode: 0, Duration: 10 * time.Second})
	b2.AddSkipped("v4", "performance", "skipped")
	r2 := b2.Build()

	merged := MergeReports("aicr", "1.0.0", []*Report{r1, r2})

	if merged.ReportFormat != ReportFormatCTRF {
		t.Errorf("ReportFormat = %q, want %q", merged.ReportFormat, ReportFormatCTRF)
	}
	if merged.Results.Summary.Tests != 4 {
		t.Errorf("Summary.Tests = %d, want 4", merged.Results.Summary.Tests)
	}
	if merged.Results.Summary.Passed != 2 {
		t.Errorf("Summary.Passed = %d, want 2", merged.Results.Summary.Passed)
	}
	if merged.Results.Summary.Failed != 1 {
		t.Errorf("Summary.Failed = %d, want 1", merged.Results.Summary.Failed)
	}
	if merged.Results.Summary.Skipped != 1 {
		t.Errorf("Summary.Skipped = %d, want 1", merged.Results.Summary.Skipped)
	}
	if len(merged.Results.Tests) != 4 {
		t.Errorf("Tests length = %d, want 4", len(merged.Results.Tests))
	}

	// Earliest start, latest stop
	if merged.Results.Summary.Start != r1.Results.Summary.Start {
		t.Errorf("Summary.Start = %d, want %d (earliest)", merged.Results.Summary.Start, r1.Results.Summary.Start)
	}
	if merged.Results.Summary.Stop < r2.Results.Summary.Stop {
		t.Errorf("Summary.Stop = %d, should be >= %d (latest)", merged.Results.Summary.Stop, r2.Results.Summary.Stop)
	}

	// Timestamp from first report
	if merged.Timestamp != r1.Timestamp {
		t.Errorf("Timestamp = %q, want %q (first report)", merged.Timestamp, r1.Timestamp)
	}
}

func TestMergeReportsEmpty(t *testing.T) {
	merged := MergeReports("aicr", "1.0.0", []*Report{})
	if merged == nil {
		t.Fatal("MergeReports with empty slice should not return nil")
	}
	if merged.Results.Summary.Tests != 0 {
		t.Errorf("Summary.Tests = %d, want 0", merged.Results.Summary.Tests)
	}
	if merged.Timestamp == "" {
		t.Error("Timestamp should be set even for empty merge")
	}
}

func TestMergeReportsNilInSlice(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", "deployment")
	b.AddResult(&ValidatorResult{Name: "v1", Phase: "deployment", ExitCode: 0, Duration: 1 * time.Second})
	r := b.Build()

	merged := MergeReports("aicr", "1.0.0", []*Report{nil, r, nil})
	if merged.Results.Summary.Tests != 1 {
		t.Errorf("Summary.Tests = %d, want 1", merged.Results.Summary.Tests)
	}
	if merged.Results.Summary.Passed != 1 {
		t.Errorf("Summary.Passed = %d, want 1", merged.Results.Summary.Passed)
	}
	if len(merged.Results.Tests) != 1 {
		t.Errorf("Tests length = %d, want 1", len(merged.Results.Tests))
	}
}

func TestMergeReportsEnvironmentFromLast(t *testing.T) {
	r1 := &Report{
		ReportFormat: ReportFormatCTRF,
		SpecVersion:  SpecVersion,
		Timestamp:    "2026-01-01T00:00:00Z",
		Results: Results{
			Summary: Summary{Tests: 1, Passed: 1, Start: 1000, Stop: 2000},
			Tests:   []TestResult{{Name: "v1", Status: StatusPassed}},
			Environment: &Environment{
				TestEnvironment: "env-first",
			},
		},
	}
	r2 := &Report{
		ReportFormat: ReportFormatCTRF,
		SpecVersion:  SpecVersion,
		Timestamp:    "2026-01-01T00:01:00Z",
		Results: Results{
			Summary: Summary{Tests: 1, Passed: 1, Start: 2000, Stop: 3000},
			Tests:   []TestResult{{Name: "v2", Status: StatusPassed}},
			Environment: &Environment{
				TestEnvironment: "env-second",
			},
		},
	}

	merged := MergeReports("aicr", "1.0.0", []*Report{r1, r2})
	if merged.Results.Environment == nil {
		t.Fatal("Environment should not be nil")
	}
	if merged.Results.Environment.TestEnvironment != "env-second" {
		t.Errorf("Environment.TestEnvironment = %q, want %q (last non-nil wins)",
			merged.Results.Environment.TestEnvironment, "env-second")
	}
}

func TestBuilderStdoutNotCapturedWhenEmpty(t *testing.T) {
	b := NewBuilder("aicr", "1.0.0", testPhase)
	b.AddResult(&ValidatorResult{
		Name:     "no-output",
		Phase:    testPhase,
		ExitCode: 0,
		Stdout:   []string{},
	})

	report := b.Build()
	if report.Results.Tests[0].Stdout != nil {
		t.Errorf("Stdout should be nil for empty slice, got %v", report.Results.Tests[0].Stdout)
	}
}
