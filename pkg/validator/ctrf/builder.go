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
	"time"
)

// ValidatorResult is the outcome of running a single validator container.
// Populated by the job package after extracting exit code, termination message,
// and stdout from the completed pod.
type ValidatorResult struct {
	// Name is the validator name from the catalog entry.
	Name string

	// Phase is the validation phase from the catalog entry.
	Phase string

	// ExitCode is the container exit code. -1 indicates the container never ran.
	ExitCode int32

	// TerminationMsg is the content of /dev/termination-log.
	TerminationMsg string

	// Stdout contains the standard output lines from the container.
	Stdout []string

	// Duration is the wall-clock execution time.
	Duration time.Duration

	// StartTime is when the container started.
	StartTime time.Time

	// CompletionTime is when the container finished.
	CompletionTime time.Time
}

// CTRFStatus maps the exit code to a CTRF status string.
func (r *ValidatorResult) CTRFStatus() string {
	return ExitCodeToCTRFStatus(r.ExitCode)
}

// ExitCodeToCTRFStatus maps a container exit code to a CTRF status string.
func ExitCodeToCTRFStatus(code int32) string {
	switch code {
	case 0:
		return StatusPassed
	case 1:
		return StatusFailed
	case 2:
		return StatusSkipped
	default:
		return StatusOther
	}
}

// MergeReports combines multiple CTRF reports into a single report.
// It aggregates test results, summaries, and picks the earliest timestamp.
func MergeReports(toolName, toolVersion string, reports []*Report) *Report {
	if len(reports) == 1 && reports[0] != nil { //nolint:gosec // Index is safe: len==1 guarantees bounds
		return reports[0] //nolint:gosec // Index is safe: len==1 guarantees bounds
	}

	merged := &Report{
		ReportFormat: ReportFormatCTRF,
		SpecVersion:  SpecVersion,
		GeneratedBy:  toolName,
		Results: Results{
			Tool: Tool{Name: toolName, Version: toolVersion},
		},
	}

	for _, r := range reports {
		if r == nil {
			continue
		}
		merged.Results.Tests = append(merged.Results.Tests, r.Results.Tests...)
		merged.Results.Summary.Tests += r.Results.Summary.Tests
		merged.Results.Summary.Passed += r.Results.Summary.Passed
		merged.Results.Summary.Failed += r.Results.Summary.Failed
		merged.Results.Summary.Skipped += r.Results.Summary.Skipped
		merged.Results.Summary.Pending += r.Results.Summary.Pending
		merged.Results.Summary.Other += r.Results.Summary.Other

		if merged.Results.Summary.Start == 0 || r.Results.Summary.Start < merged.Results.Summary.Start {
			merged.Results.Summary.Start = r.Results.Summary.Start
		}
		if r.Results.Summary.Stop > merged.Results.Summary.Stop {
			merged.Results.Summary.Stop = r.Results.Summary.Stop
		}
		if merged.Timestamp == "" {
			merged.Timestamp = r.Timestamp
		}
		if r.Results.Environment != nil {
			merged.Results.Environment = r.Results.Environment
		}
	}

	if merged.Timestamp == "" {
		merged.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	return merged
}

// Builder accumulates test results and produces a CTRF Report.
type Builder struct {
	tool  Tool
	phase string
	env   *Environment
	tests []TestResult
	start time.Time
}

// NewBuilder creates a builder for a single phase's CTRF report.
func NewBuilder(toolName, toolVersion, phase string) *Builder {
	return &Builder{
		tool: Tool{
			Name:    toolName,
			Version: toolVersion,
		},
		phase: phase,
		tests: []TestResult{},
		start: time.Now(),
	}
}

// SetEnvironment sets the optional environment metadata for the report.
func (b *Builder) SetEnvironment(env *Environment) {
	b.env = env
}

// AddResult converts a ValidatorResult to a CTRF TestResult and appends it.
func (b *Builder) AddResult(r *ValidatorResult) {
	tr := TestResult{
		Name:     r.Name,
		Status:   r.CTRFStatus(),
		Duration: int(r.Duration.Milliseconds()),
		Suite:    []string{r.Phase},
	}

	if r.TerminationMsg != "" {
		tr.Message = r.TerminationMsg
	}

	if len(r.Stdout) > 0 {
		tr.Stdout = r.Stdout
	}

	b.tests = append(b.tests, tr)
}

// AddSkipped appends a skipped entry for a validator that was not executed.
func (b *Builder) AddSkipped(name, phase, reason string) {
	b.tests = append(b.tests, TestResult{
		Name:    name,
		Status:  StatusSkipped,
		Suite:   []string{phase},
		Message: reason,
	})
}

// Build produces the final CTRF Report with computed summary.
func (b *Builder) Build() *Report {
	now := time.Now()
	summary := Summary{
		Tests: len(b.tests),
		Start: b.start.UnixMilli(),
		Stop:  now.UnixMilli(),
	}

	for _, t := range b.tests {
		switch t.Status {
		case StatusPassed:
			summary.Passed++
		case StatusFailed:
			summary.Failed++
		case StatusSkipped:
			summary.Skipped++
		case StatusPending:
			summary.Pending++
		case StatusOther:
			summary.Other++
		}
	}

	return &Report{
		ReportFormat: ReportFormatCTRF,
		SpecVersion:  SpecVersion,
		Timestamp:    now.UTC().Format(time.RFC3339),
		GeneratedBy:  b.tool.Name,
		Results: Results{
			Tool:        b.tool,
			Summary:     summary,
			Tests:       b.tests,
			Environment: b.env,
		},
	}
}
