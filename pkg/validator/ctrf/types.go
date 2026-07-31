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

// Package ctrf provides Go types and utilities for the Common Test Report Format (CTRF).
// See https://ctrf.io/ and https://github.com/ctrf-io/ctrf/blob/main/schema/ctrf.schema.json
package ctrf

const (
	// ReportFormatCTRF is the document format identifier.
	ReportFormatCTRF = "CTRF"

	// SpecVersion is the CTRF specification version implemented by this package.
	SpecVersion = "0.0.1"

	// StatusPassed indicates the test passed.
	StatusPassed = "passed"

	// StatusFailed indicates the test failed.
	StatusFailed = "failed"

	// StatusSkipped indicates the test was skipped.
	StatusSkipped = "skipped"

	// StatusPending indicates the test is pending execution.
	StatusPending = "pending"

	// StatusOther indicates an unexpected outcome (crash, OOM, timeout, etc.).
	StatusOther = "other"

	// ExtraLinePrefix marks a single stdout line as the transport for a check's
	// structured TestResult.Extra map. An in-pod check emits one such line via
	// EmitExtra; pkg/validator/job.ExtractResult parses the last one and strips
	// all prefixed lines from the human-readable Stdout evidence.
	//
	// The prefix lives here (not in validators/ or pkg/validator/job) because it
	// is the shared contract between the in-pod producer and the orchestrator
	// consumer, and both packages import ctrf.
	ExtraLinePrefix = "##AICR-EXTRA## "
)

// IsFailingStatus reports whether a check or phase status must block progress
// — i.e., fail a readiness gate or trip cross-phase fail-fast.
//
// Both StatusFailed and StatusOther are blocking. StatusOther means the check
// could not be executed to a verdict (crash, OOM, timeout, or a Job that
// reached a terminal Failed state with no inspectable pod). For a gate, an
// inconclusive outcome must fail closed: treating it as non-blocking lets a
// not-yet-ready cluster masquerade as passing. StatusSkipped stays
// non-blocking (a legitimately inapplicable check, e.g. no GPU nodes present).
func IsFailingStatus(status string) bool {
	return status == StatusFailed || status == StatusOther
}

// Report is the top-level CTRF document.
type Report struct {
	// ReportFormat is the document format identifier. Always "CTRF".
	ReportFormat string `json:"reportFormat"`

	// SpecVersion is the CTRF specification version in SemVer format.
	SpecVersion string `json:"specVersion"`

	// Timestamp is the report generation time in RFC 3339 format.
	Timestamp string `json:"timestamp"`

	// GeneratedBy is the tool or system that produced this document.
	GeneratedBy string `json:"generatedBy"`

	// Results contains the test execution results.
	Results Results `json:"results"`
}

// Results holds the tool metadata, summary, test entries, and environment.
type Results struct {
	// Tool identifies the testing tool or framework.
	Tool Tool `json:"tool"`

	// Summary provides aggregated statistics for the test run.
	Summary Summary `json:"summary"`

	// Tests is the list of individual test case results.
	Tests []TestResult `json:"tests"`

	// Environment describes the execution environment.
	Environment *Environment `json:"environment,omitempty"`
}

// Tool identifies the testing tool that produced the results.
type Tool struct {
	// Name is the name of the testing tool.
	Name string `json:"name"`

	// Version is the version of the testing tool.
	Version string `json:"version,omitempty"`
}

// Summary provides aggregated statistics and timing for a test run.
type Summary struct {
	// Tests is the total number of tests executed.
	Tests int `json:"tests"`

	// Passed is the count of tests with status "passed".
	Passed int `json:"passed"`

	// Failed is the count of tests with status "failed".
	Failed int `json:"failed"`

	// Skipped is the count of tests with status "skipped".
	Skipped int `json:"skipped"`

	// Pending is the count of tests with status "pending".
	Pending int `json:"pending"`

	// Other is the count of tests with status "other".
	Other int `json:"other"`

	// Start is the run start time in milliseconds since Unix epoch.
	Start int64 `json:"start"`

	// Stop is the run end time in milliseconds since Unix epoch.
	Stop int64 `json:"stop"`
}

// TestResult represents an individual test case result.
type TestResult struct {
	// Name is the name or title of the test case.
	Name string `json:"name"`

	// Status is the final outcome of the test case.
	// One of: "passed", "failed", "skipped", "pending", "other".
	Status string `json:"status"`

	// Duration is the test execution time in milliseconds.
	Duration int `json:"duration"`

	// Suite is the suite hierarchy from top-level to immediate parent.
	Suite []string `json:"suite,omitempty"`

	// Message is the error or failure message (e.g., from termination log).
	Message string `json:"message,omitempty"`

	// Stdout contains standard output lines from test execution.
	Stdout []string `json:"stdout,omitempty"`

	// Extra carries structured, low-cardinality outcome data that survives the
	// default "minimal" redaction policy (unlike Stdout and Message, which are
	// free-form log text stripped by default). It mirrors the CTRF spec's
	// `extra` object.
	//
	// CONTRACT: values MUST be low-cardinality counts or enum codes only —
	// e.g. "1", "2", "no-schedulable-gpu-nodes", "no-gpu-nodes". NEVER node names, IPs,
	// hostnames, or any operator-identifying free text; those belong in Stdout,
	// which is redacted by default. The redact package enforces this at the
	// publication boundary with a fail-closed key AND value allowlist: only
	// listed keys survive, and only values matching the key's canonical shape
	// (decimal count / kebab-case enum code) — so an identifier smuggled under
	// an allowed key is dropped, not published (see pkg/evidence/redact).
	Extra map[string]string `json:"extra,omitempty"`
}

// Environment describes the execution environment and build context.
type Environment struct {
	// AppName is the name of the application under test.
	AppName string `json:"appName,omitempty"`

	// AppVersion is the version of the application under test.
	AppVersion string `json:"appVersion,omitempty"`

	// TestEnvironment is the logical test environment (e.g., "eks-h100-training").
	TestEnvironment string `json:"testEnvironment,omitempty"`
}
