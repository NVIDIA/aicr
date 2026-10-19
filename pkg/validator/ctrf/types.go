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

	// ExtraLinePrefix marks a stdout line carrying one JSON object for
	// TestResult.Extra. pkg/validator/job.ExtractResult merges every such line,
	// a later value winning per key, and strips them from Stdout.
	//
	// The prefix lives here (not in validators/ or pkg/validator/job) because it
	// is the shared contract between the in-pod producer and the orchestrator
	// consumer, and both packages import ctrf.
	ExtraLinePrefix = "##AICR-EXTRA## "

	// ProvenanceLinePrefix marks a stdout line carrying one JSON
	// RuntimeProvenance record. Every such line is stripped from Stdout, and
	// the last valid one wins.
	ProvenanceLinePrefix = "##AICR-PROVENANCE## "
)

// RuntimeProvenance is the bounded, digest-bound evidence carrier for a
// benchmark runtime DERIVED from an artifact the recipe ships (#2297). It binds
// the result to the exact templates compared without shipping them: content
// identities of the normalized shipped and applied worker PodTemplateSpecs,
// the paths at which they differ, and the inventory of shipped paths carried
// into the applied runtime unchanged — an inherited field is equal on both
// sides and therefore invisible to a diff, so the inventory is what proves the
// wiring was measured rather than re-injected.
//
// Paths are template KEYS (dotted, named-list elements addressed as [name]),
// never values: no network name, node name, or env value appears. Names the
// operator authors — list element names, and keys of user-keyed maps such as
// labels, annotations, nodeSelector and resource limits — are collapsed by
// pkg/evidence/redact in minimal evidence unless they are on its exact
// allowlists (the shipped runtime's fabric env names and vendor keys). This
// is an AICR extension alongside the CTRF `extra` object; it is absent for
// every result that did not derive a runtime.
type RuntimeProvenance struct {
	// ShippedDigest is sha256 (lowercase hex) of the normalized worker template
	// read from the deployed artifact.
	ShippedDigest string `json:"shippedDigest"`
	// DerivedDigest is sha256 (lowercase hex) of the normalized worker template
	// as applied for the measurement — scheduling stamped.
	DerivedDigest string `json:"derivedDigest"`
	// OverriddenPaths are the paths at which the applied template differs from
	// the shipped one (benchmark-owned overrides plus stamped scheduling).
	OverriddenPaths []string `json:"overriddenPaths,omitempty"`
	// InheritedPaths are the shipped template's leaf paths carried unchanged.
	InheritedPaths []string `json:"inheritedPaths,omitempty"`
}

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

// TestResult represents an individual test case result. Message and Stdout
// are free-form text that minimal evidence redaction strips, while Extra is
// the low-cardinality outcome data it keeps.
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

	// Extra holds low-cardinality outcome data, mirroring the CTRF spec's
	// extra object. Each value must be one of:
	//   - a count ("2")
	//   - a number rounded to one decimal place ("305.4", "+1.8")
	//   - "true" or "false"
	//   - a "v"-prefixed numeric version ("v25.3.0", "v25.3.0-rc.1")
	//   - a closed-set code ("no-gpu-nodes")
	//
	// Never node names, IPs, hostnames, digests of cluster-specific content,
	// or other operator-identifying text. Keys are the validators.ExtraKey*
	// constants.
	Extra map[string]string `json:"extra,omitempty"`

	// RuntimeProvenance binds a derived-runtime measurement to the templates it
	// compared (see RuntimeProvenance). It survives minimal redaction after the
	// shape and key-collapsing rules in pkg/evidence/redact; nil otherwise.
	RuntimeProvenance *RuntimeProvenance `json:"runtimeProvenance,omitempty"`
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
