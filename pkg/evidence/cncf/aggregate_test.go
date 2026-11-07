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
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func reportOf(tests ...ctrf.TestResult) *ctrf.Report {
	return &ctrf.Report{Results: ctrf.Results{Tests: tests}}
}

// Every v1.37 MUST passed, so the run passes; this is the baseline the
// per-requirement cases below perturb one check at a time.
var allMustsPassed = []ctrf.TestResult{
	{Name: "gang-scheduling", Status: "passed"},
	{Name: "accelerator-metrics", Status: "passed"},
	{Name: "ai-service-metrics", Status: "passed"},
	{Name: "inference-gateway", Status: "passed"},
	{Name: "pod-autoscaling", Status: "passed"},
	// cluster-autoscaling is observation-only, so no pass of it is ever a
	// demonstration; the baseline declares it not applicable where a passing
	// run is needed and passes it as an observation otherwise.
	{Name: "cluster-autoscaling", Status: "passed", Extra: observationOnly},
	{Name: "robust-controller", Status: "passed"},
	{Name: "secure-accelerator-access", Status: "passed"},
}

func withResult(base []ctrf.TestResult, name string, replacement *ctrf.TestResult) []ctrf.TestResult {
	out := make([]ctrf.TestResult, 0, len(base)+1)
	for _, t := range base {
		if t.Name != name {
			out = append(out, t)
		}
	}
	if replacement != nil {
		out = append(out, *replacement)
	}
	return out
}

func TestAggregateReportRequirementStatus(t *testing.T) {
	tests := []struct {
		name        string
		tests       []ctrf.TestResult
		requirement string
		wantStatus  RequirementStatus
		wantReason  string
		wantProof   []string
		wantPassed  bool
	}{
		{
			name:        "every check passed",
			tests:       allMustsPassed,
			requirement: "gang_scheduling",
			wantStatus:  StatusPassed,
			wantProof:   []string{"gang-scheduling"},
			wantPassed:  true,
		},
		{
			name:        "a failed MUST check fails the requirement and the run",
			tests:       withResult(allMustsPassed, "gang-scheduling", &ctrf.TestResult{Name: "gang-scheduling", Status: "failed", Message: "pods not co-scheduled"}),
			requirement: "gang_scheduling",
			wantStatus:  StatusFailed,
			wantReason:  "gang-scheduling failed",
		},
		{
			name: "a skipped MUST check is not tested, with its reason code",
			tests: withResult(allMustsPassed, "pod-autoscaling", &ctrf.TestResult{Name: "pod-autoscaling", Status: "skipped",
				Extra: map[string]string{"skipReason": "no-metrics-api"}}),
			requirement: "pod_autoscaling",
			wantStatus:  StatusNotTested,
			wantReason:  "pod-autoscaling skipped: no-metrics-api",
		},
		{
			name:        "a crashed (other) MUST check is not tested, never passed",
			tests:       withResult(allMustsPassed, "robust-controller", &ctrf.TestResult{Name: "robust-controller", Status: "other"}),
			requirement: "robust_controller",
			wantStatus:  StatusNotTested,
			wantReason:  "robust-controller other",
		},
		{
			name:        "a pending MUST check is not tested",
			tests:       withResult(allMustsPassed, "ai-service-metrics", &ctrf.TestResult{Name: "ai-service-metrics", Status: "pending"}),
			requirement: "ai_service_metrics",
			wantStatus:  StatusNotTested,
			wantReason:  "ai-service-metrics pending",
		},
		{
			name:        "a MUST check absent from the run is not tested",
			tests:       withResult(allMustsPassed, "accelerator-metrics", nil),
			requirement: "accelerator_metrics",
			wantStatus:  StatusNotTested,
			wantReason:  untestedNotRun,
		},
		{
			name:        "a SHOULD with no AICR check is not tested without failing the run",
			tests:       allMustsPassed,
			requirement: "gpu_sharing",
			wantStatus:  StatusNotTested,
			wantReason:  untestedNoCheck,
			wantPassed:  true,
		},
		{
			name:        "a passed SHOULD check passes its requirement with proof",
			tests:       append(slices.Clone(allMustsPassed), ctrf.TestResult{Name: "driver-runtime-management", Status: "passed"}),
			requirement: "driver_runtime_management",
			wantStatus:  StatusPassed,
			wantProof:   []string{"driver-runtime-management"},
			wantPassed:  true,
		},
		{
			// driver_runtime_management is SHOULD at v1.37: a disproven
			// capability renders FAILED on its own row and the requirement
			// aggregate still passes (the validate run's own per-check
			// --fail-on-error verdict is a separate matter).
			name:        "a failed SHOULD check fails the requirement but not the aggregate",
			tests:       append(slices.Clone(allMustsPassed), ctrf.TestResult{Name: "driver-runtime-management", Status: "failed", Message: "version mismatch"}),
			requirement: "driver_runtime_management",
			wantStatus:  StatusFailed,
			wantReason:  "driver-runtime-management failed",
			wantPassed:  true,
		},
		{
			name: "a skipped SHOULD check is not tested without failing the aggregate",
			tests: append(slices.Clone(allMustsPassed), ctrf.TestResult{Name: "driver-runtime-management", Status: "skipped",
				Extra: map[string]string{"skipReason": "slurm-managed-gpus"}}),
			requirement: "driver_runtime_management",
			wantStatus:  StatusNotTested,
			wantReason:  "driver-runtime-management skipped: slurm-managed-gpus",
			wantPassed:  true,
		},
		{
			name:        "a SHOULD check absent from the run is not tested without failing the aggregate",
			tests:       allMustsPassed,
			requirement: "driver_runtime_management",
			wantStatus:  StatusNotTested,
			wantReason:  untestedNotRun,
			wantPassed:  true,
		},
		{
			name:        "a passed workload_sandboxing check passes its requirement with proof",
			tests:       append(slices.Clone(allMustsPassed), ctrf.TestResult{Name: "workload-sandboxing", Status: "passed"}),
			requirement: "workload_sandboxing",
			wantStatus:  StatusPassed,
			wantProof:   []string{"workload-sandboxing"},
			wantPassed:  true,
		},
		{
			// workload_sandboxing is SHOULD at v1.37: a cluster with no
			// sandboxed RuntimeClass fails the check, renders FAILED on its
			// own row, and the requirement aggregate still passes (the
			// validate run's own per-check --fail-on-error verdict is a
			// separate matter).
			name:        "a failed workload_sandboxing check fails the requirement but not the aggregate",
			tests:       append(slices.Clone(allMustsPassed), ctrf.TestResult{Name: "workload-sandboxing", Status: "failed", Message: "unmet prerequisite: no sandboxed RuntimeClass"}),
			requirement: "workload_sandboxing",
			wantStatus:  StatusFailed,
			wantReason:  "workload-sandboxing failed",
			wantPassed:  true,
		},
		{
			name:        "a workload_sandboxing check absent from the run is not tested without failing the aggregate",
			tests:       allMustsPassed,
			requirement: "workload_sandboxing",
			wantStatus:  StatusNotTested,
			wantReason:  untestedNotRun,
			wantPassed:  true,
		},
		{
			name: "the same check reported twice passes only when both passed",
			tests: append(slices.Clone(allMustsPassed),
				ctrf.TestResult{Name: "gang-scheduling", Status: "failed", Message: "second phase"}),
			requirement: "gang_scheduling",
			wantStatus:  StatusFailed,
			wantReason:  "gang-scheduling failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg, err := AggregateReport(Version137, reportOf(tt.tests...),
				// cluster-autoscaling is observation-only, so a passing baseline
				// declares the conditional MUST not applicable, as a fixed-size
				// cluster's submission does.
				map[string]string{"cluster_autoscaling": "fixed-size cluster"})
			if err != nil {
				t.Fatalf("AggregateReport: %v", err)
			}
			r := agg.Requirement(tt.requirement)
			if r == nil {
				t.Fatalf("requirement %q missing from aggregate", tt.requirement)
			}
			if r.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", r.Status, tt.wantStatus)
			}
			if r.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", r.Reason, tt.wantReason)
			}
			if !slices.Equal(r.Proof, tt.wantProof) {
				t.Errorf("Proof = %v, want %v", r.Proof, tt.wantProof)
			}
			if agg.Passed() != tt.wantPassed {
				t.Errorf("Passed() = %v, want %v (incomplete MUSTs: %v)", agg.Passed(), tt.wantPassed, incompleteIDs(agg))
			}
		})
	}
}

// observationOnly marks a passed result observation-only, the marker a check
// publishes through validators.ObservationOnlyExtra.
var observationOnly = map[string]string{"observationOnly": "true"}

// stamped is the result-contract stamp alone: an image that speaks the
// contract and marked nothing.
var stamped = map[string]string{"resultSchema": "1"}

// unstamped is a pass from an image that predates the contract.
var unstamped = &ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed"}

// TestAggregateReportObservationOnly pins #3681: a passed result carrying
// the observation-only marker renders its requirement OBSERVED, which is
// not PASSED. A MUST so observed is not established (the verdict names it)
// unless notApplicable covers it; a SHOULD so observed never fails the run;
// a behavioral check's pass is untouched.
func TestAggregateReportObservationOnly(t *testing.T) {
	observed := &ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Extra: observationOnly}
	tests := []struct {
		name          string
		tests         []ctrf.TestResult
		notApplicable map[string]string
		requirement   string
		wantStatus    RequirementStatus
		wantReason    string
		wantProof     []string
		wantPassed    bool
		wantObserved  bool // CheckResult.ObservationOnly of the requirement's first check
		wantInVerdict []string
	}{
		{
			name:          "an observation-only pass on a MUST is observed, not passed, and fails the run",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", observed),
			requirement:   "cluster_autoscaling",
			wantStatus:    StatusObserved,
			wantReason:    "cluster-autoscaling observed, not demonstrated",
			wantObserved:  true,
			wantInVerdict: []string{"1 applicable MUST", "cluster_autoscaling observed (cluster-autoscaling observed, not demonstrated)"},
		},
		{
			name:          "notApplicable covers the observed MUST",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", observed),
			notApplicable: map[string]string{"cluster_autoscaling": "fixed-size cluster"},
			requirement:   "cluster_autoscaling",
			wantStatus:    StatusNotApplicable,
			wantReason:    "fixed-size cluster",
			wantPassed:    true,
			wantObserved:  true,
		},
		{
			// A stamped pass with no marker from a check every image marks on
			// every passing path means the marker line was lost in transit;
			// it is not read as a demonstration.
			name:          "a stamped pass without the marker is observed, naming the dropped marker",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Extra: stamped}),
			requirement:   "cluster_autoscaling",
			wantStatus:    StatusObserved,
			wantReason:    "cluster-autoscaling observed, not demonstrated: " + reasonMarkerDroppedPrefix + "1",
			wantObserved:  true,
			wantInVerdict: []string{"cluster_autoscaling observed"},
		},
		{
			name:         "the marker set to false on a stamped pass is no marker, so the pass is observed",
			tests:        withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Extra: map[string]string{"observationOnly": "false", "resultSchema": "1"}}),
			requirement:  "cluster_autoscaling",
			wantStatus:   StatusObserved,
			wantReason:   "cluster-autoscaling observed, not demonstrated: " + reasonMarkerDroppedPrefix + "1",
			wantObserved: true,
		},
		{
			// Fail closed on a contract version this reader does not know:
			// its markers may differ, so its pass is not a demonstration.
			name:         "a pass stamped with an unknown contract version is observed",
			tests:        withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Extra: map[string]string{"resultSchema": "2"}}),
			requirement:  "cluster_autoscaling",
			wantStatus:   StatusObserved,
			wantReason:   "cluster-autoscaling observed, not demonstrated: " + reasonSchemaUnknownPrefix + "2",
			wantObserved: true,
		},
		{
			// An earlier skipped result sets the fold's status and reason; a
			// later unstamped pass must not overwrite the skip reason.
			name:        "an unstamped pass after a skipped result keeps the skip reason",
			tests:       append(withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "skipped", Extra: map[string]string{"skipReason": "unrecognized-platform"}}), *unstamped),
			requirement: "cluster_autoscaling",
			wantStatus:  StatusNotTested,
			wantReason:  "cluster-autoscaling skipped: unrecognized-platform",
		},
		{
			// #3703: an image that predates the marker cannot have set it,
			// so its pass of an observation-only check is not a demonstration.
			name:          "an unstamped pass of an observation-only check is observed with the mismatch recorded",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", unstamped),
			requirement:   "cluster_autoscaling",
			wantStatus:    StatusObserved,
			wantReason:    "cluster-autoscaling observed, not demonstrated: " + reasonResultSchemaAbsent,
			wantObserved:  true,
			wantInVerdict: []string{"cluster_autoscaling observed", reasonResultSchemaAbsent},
		},
		{
			name:          "notApplicable covers the unstamped observation-only pass",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", unstamped),
			notApplicable: map[string]string{"cluster_autoscaling": "fixed-size cluster"},
			requirement:   "cluster_autoscaling",
			wantStatus:    StatusNotApplicable,
			wantReason:    "fixed-size cluster",
			wantPassed:    true,
			wantObserved:  true,
		},
		{
			name:          "an unstamped pass of a behavioral check is credited",
			tests:         withResult(allMustsPassed, "gang-scheduling", &ctrf.TestResult{Name: "gang-scheduling", Status: "passed"}),
			notApplicable: map[string]string{"cluster_autoscaling": "fixed-size cluster"},
			requirement:   "gang_scheduling",
			wantStatus:    StatusPassed,
			wantProof:     []string{"gang-scheduling"},
			wantPassed:    true,
		},
		{
			name:         "an unstamped pass folded with a stamped pass is observed",
			tests:        append(slices.Clone(allMustsPassed), *unstamped),
			requirement:  "cluster_autoscaling",
			wantStatus:   StatusObserved,
			wantReason:   "cluster-autoscaling observed, not demonstrated: " + reasonResultSchemaAbsent,
			wantObserved: true,
		},
		{
			name:        "an unstamped failed result of an observation-only check stays failed",
			tests:       withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Message: "no node group"}),
			requirement: "cluster_autoscaling",
			wantStatus:  StatusFailed,
			wantReason:  "cluster-autoscaling failed",
		},
		{
			name: "the same check reported as a demonstration and as an observation is observed",
			tests: append(slices.Clone(allMustsPassed),
				ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Extra: observationOnly}),
			requirement:  "cluster_autoscaling",
			wantStatus:   StatusObserved,
			wantReason:   "cluster-autoscaling observed, not demonstrated",
			wantObserved: true,
		},
		{
			name:        "the marker on a failed result changes nothing: failed wins",
			tests:       withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Message: "no node group", Extra: observationOnly}),
			requirement: "cluster_autoscaling",
			wantStatus:  StatusFailed,
			wantReason:  "cluster-autoscaling failed",
		},
		{
			name:        "the marker on a skipped result changes nothing: not tested",
			tests:       withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "skipped", Extra: map[string]string{"observationOnly": "true", "skipReason": "unrecognized-platform"}}),
			requirement: "cluster_autoscaling",
			wantStatus:  StatusNotTested,
			wantReason:  "cluster-autoscaling skipped: unrecognized-platform",
		},
		{
			name:         "an observed alternative does not establish a requirement whose primary was skipped",
			tests:        withResult(withResult(allMustsPassed, "secure-accelerator-access", &ctrf.TestResult{Name: "secure-accelerator-access", Status: "skipped", Extra: map[string]string{"skipReason": "slurm-managed-gpus"}}), "slinky-slurm-gpu-access", &ctrf.TestResult{Name: "slinky-slurm-gpu-access", Status: "passed", Extra: observationOnly}),
			requirement:  "secure_accelerator_access",
			wantStatus:   StatusObserved,
			wantReason:   "slinky-slurm-gpu-access observed, not demonstrated; secure-accelerator-access skipped: slurm-managed-gpus",
			wantObserved: false,
		},
		{
			name:          "a demonstrated alternative establishes the requirement beside an observed one",
			tests:         append(slices.Clone(allMustsPassed), ctrf.TestResult{Name: "slinky-slurm-gpu-access", Status: "passed", Extra: observationOnly}),
			notApplicable: map[string]string{"cluster_autoscaling": "fixed-size cluster"},
			requirement:   "secure_accelerator_access",
			wantStatus:    StatusPassed,
			wantProof:     []string{"secure-accelerator-access"},
			wantPassed:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg, err := AggregateReport(Version137, reportOf(tt.tests...), tt.notApplicable)
			if err != nil {
				t.Fatalf("AggregateReport: %v", err)
			}
			r := agg.Requirement(tt.requirement)
			if r == nil {
				t.Fatalf("requirement %q missing from aggregate", tt.requirement)
			}
			if r.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", r.Status, tt.wantStatus)
			}
			if r.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", r.Reason, tt.wantReason)
			}
			if !slices.Equal(r.Proof, tt.wantProof) {
				t.Errorf("Proof = %v, want %v", r.Proof, tt.wantProof)
			}
			if r.Checks[0].ObservationOnly != tt.wantObserved {
				t.Errorf("Checks[0].ObservationOnly = %v, want %v", r.Checks[0].ObservationOnly, tt.wantObserved)
			}
			if agg.Passed() != tt.wantPassed {
				t.Errorf("Passed() = %v, want %v (incomplete MUSTs: %v)", agg.Passed(), tt.wantPassed, incompleteIDs(agg))
			}
			verdict := agg.Verdict()
			if tt.wantPassed != (verdict == nil) {
				t.Errorf("Verdict() = %v, want nil: %v", verdict, tt.wantPassed)
			}
			for _, want := range tt.wantInVerdict {
				if verdict == nil || !strings.Contains(verdict.Error(), want) {
					t.Errorf("Verdict() = %v does not mention %q", verdict, want)
				}
			}
		})
	}

	t.Run("an observed SHOULD renders observed and never fails the run", func(t *testing.T) {
		// dra_support is a MUST at v1.35; the SHOULDs of later checklists
		// have no AICR check, so an observed SHOULD is exercised through a
		// synthetic aggregate rather than a report.
		agg := &Aggregate{Version: LatestVersion, Requirements: []RequirementResult{
			{ID: "gang_scheduling", Level: LevelMust, Status: StatusPassed},
			{ID: "workload_sandboxing", Level: LevelShould, Status: StatusObserved, Reason: "sandbox observed, not demonstrated"},
		}}
		if !agg.Passed() || agg.Verdict() != nil || len(agg.IncompleteMusts()) != 0 {
			t.Errorf("an observed SHOULD failed the run: Passed=%v Verdict=%v incomplete=%v", agg.Passed(), agg.Verdict(), incompleteIDs(agg))
		}
		if err := agg.Outcome(reportOf(ctrf.TestResult{Name: "gang-scheduling", Status: "passed"}), true); err != nil {
			t.Errorf("Outcome() = %v, want nil", err)
		}
	})
}

func incompleteIDs(agg *Aggregate) []string {
	incomplete := agg.IncompleteMusts()
	ids := make([]string, 0, len(incomplete))
	for _, r := range incomplete {
		ids = append(ids, r.ID)
	}
	return ids
}

// secure_accelerator_access has two proof paths; one complete path passes it
// while the other is absent or skipped, and a failed path fails it whatever
// the other did.
func TestAggregateReportAlternativeProofs(t *testing.T) {
	tests := []struct {
		name       string
		tests      []ctrf.TestResult
		wantStatus RequirementStatus
		wantReason string
		wantProof  []string
	}{
		{
			name: "Slinky: ordinary check skipped, Slurm check passed",
			tests: []ctrf.TestResult{
				{Name: "secure-accelerator-access", Status: "skipped", Extra: map[string]string{"skipReason": "slinky-slurm-recipe"}},
				{Name: "slinky-slurm-gpu-access", Status: "passed"},
			},
			wantStatus: StatusPassed,
			wantProof:  []string{"slinky-slurm-gpu-access"},
		},
		{
			name:       "ordinary: only the Kubernetes check ran and passed",
			tests:      []ctrf.TestResult{{Name: "secure-accelerator-access", Status: "passed"}},
			wantStatus: StatusPassed,
			wantProof:  []string{"secure-accelerator-access"},
		},
		{
			name:       "both absent is not run",
			tests:      []ctrf.TestResult{{Name: "gang-scheduling", Status: "passed"}},
			wantStatus: StatusNotTested,
			wantReason: untestedNotRun,
		},
		{
			name: "one skipped and the other absent is not tested, naming both",
			tests: []ctrf.TestResult{
				{Name: "secure-accelerator-access", Status: "skipped", Extra: map[string]string{"skipReason": "no-gpu-nodes"}},
			},
			wantStatus: StatusNotTested,
			wantReason: "secure-accelerator-access skipped: no-gpu-nodes; slinky-slurm-gpu-access not run",
		},
		{
			name: "a failed path with the other absent fails",
			tests: []ctrf.TestResult{
				{Name: "secure-accelerator-access", Status: "failed", Message: "hostPath mount found"},
			},
			wantStatus: StatusFailed,
			wantReason: "secure-accelerator-access failed",
		},
		{
			name: "a failed alternative fails the requirement even when the other passed",
			tests: []ctrf.TestResult{
				{Name: "secure-accelerator-access", Status: "failed", Message: "hostPath mount found"},
				{Name: "slinky-slurm-gpu-access", Status: "passed"},
			},
			wantStatus: StatusFailed,
			wantReason: "secure-accelerator-access failed",
		},
		{
			name: "a failed alternative outranks a skipped one",
			tests: []ctrf.TestResult{
				{Name: "secure-accelerator-access", Status: "skipped", Extra: map[string]string{"skipReason": "slurm-managed-gpus"}},
				{Name: "slinky-slurm-gpu-access", Status: "failed", Message: "device cgroup not confined"},
			},
			wantStatus: StatusFailed,
			wantReason: "slinky-slurm-gpu-access failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg, err := AggregateReport(Version137, reportOf(tt.tests...), nil)
			if err != nil {
				t.Fatal(err)
			}
			r := agg.Requirement("secure_accelerator_access")
			if r.Status != tt.wantStatus || r.Reason != tt.wantReason || !slices.Equal(r.Proof, tt.wantProof) {
				t.Errorf("got status=%q reason=%q proof=%v, want %q/%q/%v", r.Status, r.Reason, r.Proof, tt.wantStatus, tt.wantReason, tt.wantProof)
			}
			if len(r.Checks) != 2 || r.Checks[0].Name != checkSecureAccess || r.Checks[1].Name != checkSlinkySlurmGPUAccess {
				t.Errorf("Checks = %+v, want both alternatives in page order", r.Checks)
			}
		})
	}

	// The failed alternative is kept as a check result, so packaging can see
	// what established the verdict.
	agg, _ := AggregateReport(Version137, reportOf(
		ctrf.TestResult{Name: "secure-accelerator-access", Status: "failed", Message: "hostPath mount found"},
		ctrf.TestResult{Name: "slinky-slurm-gpu-access", Status: "passed"},
	), nil)
	c := agg.Requirement("secure_accelerator_access").Checks[0]
	if !c.Present || c.Status != ctrf.StatusFailed || c.Reason != "hostPath mount found" {
		t.Errorf("failed alternative not recorded: %+v", c)
	}
}

func TestAggregateReportNotApplicable(t *testing.T) {
	justification := "fixed-size cluster; the provider exposes no node autoscaler"

	t.Run("overrides a check that ran and keeps its result", func(t *testing.T) {
		agg, err := AggregateReport(Version137,
			reportOf(withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Message: "no scale event"})...),
			map[string]string{"cluster_autoscaling": " " + justification + " "})
		if err != nil {
			t.Fatal(err)
		}
		r := agg.Requirement("cluster_autoscaling")
		if r.Status != StatusNotApplicable || r.Reason != justification || r.Proof != nil || r.Applicable() {
			t.Errorf("cluster_autoscaling = %+v", *r)
		}
		if !r.Checks[0].Present || r.Checks[0].Status != ctrf.StatusFailed {
			t.Errorf("the observation was dropped: %+v", r.Checks)
		}
		if !agg.Passed() {
			t.Errorf("Passed() = false; a not-applicable MUST must not count: %v", incompleteIDs(agg))
		}
	})

	t.Run("applies when the check did not run", func(t *testing.T) {
		agg, err := AggregateReport(Version137, reportOf(withResult(allMustsPassed, "cluster-autoscaling", nil)...),
			map[string]string{"cluster_autoscaling": justification})
		if err != nil {
			t.Fatal(err)
		}
		if r := agg.Requirement("cluster_autoscaling"); r.Status != StatusNotApplicable || r.Checks[0].Present {
			t.Errorf("cluster_autoscaling = %+v", *r)
		}
		if !agg.Passed() {
			t.Error("Passed() = false")
		}
	})

	rejected := []struct {
		name string
		na   map[string]string
	}{
		{"unconditional MUST", map[string]string{"gang_scheduling": "we do not gang"}},
		{"SHOULD", map[string]string{"workload_sandboxing": "no sandbox runtime"}},
		{"unknown requirement", map[string]string{"dra_support": "retired"}},
		{"blank justification", map[string]string{"cluster_autoscaling": "  "}},
	}
	for _, tt := range rejected {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := AggregateReport(Version137, reportOf(allMustsPassed...), tt.na)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			errorstest.WantReportedCode(t, ValidateNotApplicable(Version137, tt.na), errors.ErrCodeInvalidRequest)
		})
	}
}

func TestAggregateReportVersions(t *testing.T) {
	report := reportOf(ctrf.TestResult{Name: "dra-support", Status: "passed"}, ctrf.TestResult{Name: "gang-scheduling", Status: "passed"})

	t.Run("unsupported version", func(t *testing.T) {
		_, err := AggregateReport("v1.38", report, nil)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
	t.Run("dra_support is a requirement at v1.35", func(t *testing.T) {
		agg, err := AggregateReport(Version135, report, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r := agg.Requirement("dra_support"); r == nil || r.Status != StatusPassed || r.Level != LevelMust {
			t.Errorf("dra_support = %+v", r)
		}
		if len(agg.Supporting) != 0 {
			t.Errorf("Supporting = %+v, want none", agg.Supporting)
		}
	})
	t.Run("dra_support is supporting evidence at v1.36", func(t *testing.T) {
		agg, err := AggregateReport(Version136, report, nil)
		if err != nil {
			t.Fatal(err)
		}
		if agg.Requirement("dra_support") != nil {
			t.Error("dra_support listed as a v1.36 requirement")
		}
		if len(agg.Supporting) != 1 || agg.Supporting[0].ID != "dra_support" || agg.Supporting[0].Status != StatusPassed || agg.Supporting[0].Level != "" {
			t.Errorf("Supporting = %+v", agg.Supporting)
		}
	})
	t.Run("supporting evidence is recorded only when its check ran", func(t *testing.T) {
		agg, err := AggregateReport(Version136, reportOf(ctrf.TestResult{Name: "gang-scheduling", Status: "passed"}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(agg.Supporting) != 0 {
			t.Errorf("Supporting = %+v, want none", agg.Supporting)
		}
	})
	t.Run("nil report aggregates every requirement not tested in checklist order", func(t *testing.T) {
		agg, err := AggregateReport(Version137, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		cl, _ := ChecklistFor(Version137)
		if len(agg.Requirements) != len(cl.Requirements) {
			t.Fatalf("Requirements = %d, want %d", len(agg.Requirements), len(cl.Requirements))
		}
		for i, r := range agg.Requirements {
			if r.ID != cl.Requirements[i].ID || r.Level != cl.Requirements[i].Level || r.Status != StatusNotTested {
				t.Errorf("row %d = %+v, want %s/%s not tested", i, r, cl.Requirements[i].ID, cl.Requirements[i].Level)
			}
			want := untestedNoCheck
			if expected, _ := ExpectedChecks(Version137, r.ID); len(expected) > 0 {
				want = untestedNotRun
				if r.Page == "" || len(r.Checks) != len(expected) {
					t.Errorf("%s: Page=%q Checks=%d, want a page and %d checks", r.ID, r.Page, len(r.Checks), len(expected))
				}
			}
			if r.Reason != want {
				t.Errorf("%s reason = %q, want %q", r.ID, r.Reason, want)
			}
		}
		if agg.Passed() {
			t.Error("Passed() = true with nothing run")
		}
		if got := len(agg.IncompleteMusts()); got != len(cl.MustRequirements()) {
			t.Errorf("IncompleteMusts = %d, want every MUST (%d)", got, len(cl.MustRequirements()))
		}
	})
}

func TestTableCell(t *testing.T) {
	tests := []struct{ in, want string }{
		{"fixed-size cluster", "fixed-size cluster"},
		{"fixed-size | no autoscaler", `fixed-size \| no autoscaler`},
		{"line one\nline two\n", "line one line two"},
		{"  padded\t text ", "padded text"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := tableCell(tt.in); got != tt.want {
			t.Errorf("tableCell(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestFormatResult(t *testing.T) {
	tests := []struct {
		status RequirementStatus
		reason string
		want   string
	}{
		{StatusPassed, "", "PASSED"},
		{StatusFailed, "gang-scheduling failed", "FAILED (gang-scheduling failed)"},
		{StatusNotTested, untestedNoCheck, "NOT TESTED (no AICR check)"},
		{StatusNotApplicable, "fixed-size cluster", "NOT APPLICABLE (fixed-size cluster)"},
	}
	for _, tt := range tests {
		if got := formatResult(tt.status, tt.reason); got != tt.want {
			t.Errorf("formatResult(%q, %q) = %q, want %q", tt.status, tt.reason, got, tt.want)
		}
	}
}

// TestAlternativeProofsCoverPageChecks pins alternativeProofs to the
// evidence pages: every requirement with alternatives has a page, every
// check on a path is one the page expects, and the paths together cover
// exactly the page's checks. A check added to a page without a path would
// otherwise be silently unrequired, because proofPaths replaces the page's
// single all-checks path with the alternatives.
func TestAlternativeProofsCoverPageChecks(t *testing.T) {
	for id, paths := range alternativeProofs {
		page := pageForRequirement(id)
		if page == nil {
			t.Errorf("alternativeProofs[%q]: no evidence page carries that requirement", id)
			continue
		}
		if len(paths) < 2 {
			t.Errorf("alternativeProofs[%q]: %d path(s); a single path is the default and needs no entry", id, len(paths))
		}
		covered := make(map[string]bool)
		for _, path := range paths {
			if len(path) == 0 {
				t.Errorf("alternativeProofs[%q]: empty proof path would pass with no check", id)
			}
			for _, check := range path {
				if !slices.Contains(page.Checks, check) {
					t.Errorf("alternativeProofs[%q]: path names %q, which page %s does not expect", id, check, page.File)
				}
				covered[check] = true
			}
		}
		for _, check := range page.Checks {
			if !covered[check] {
				t.Errorf("alternativeProofs[%q]: page %s expects %q but no proof path requires it", id, page.File, check)
			}
		}
	}
}

// TestAggregateVerdict pins the exit rule of a submission run: the
// aggregation decides, scoped by level. A SHOULD that did not pass never
// fails it; an applicable MUST that did always does, and the error names each
// one with status and reason; a MUST declared not applicable is not counted.
func TestAggregateVerdict(t *testing.T) {
	tests := []struct {
		name    string
		agg     *Aggregate
		wantErr []string
		absent  []string
	}{
		{name: "every MUST passed", agg: &Aggregate{Version: LatestVersion, Requirements: []RequirementResult{
			{ID: "gang_scheduling", Level: LevelMust, Status: StatusPassed},
			{ID: "workload_sandboxing", Level: LevelShould, Status: StatusNotTested, Reason: untestedNoCheck},
		}}},
		{name: "not applicable MUST", agg: &Aggregate{Version: LatestVersion, Requirements: []RequirementResult{
			{ID: "cluster_autoscaling", Level: LevelMust, Status: StatusNotApplicable, Reason: "fixed-size cluster"},
		}}},
		{name: "incomplete MUSTs", agg: &Aggregate{Version: LatestVersion, Requirements: []RequirementResult{
			{ID: "gang_scheduling", Level: LevelMust, Status: StatusFailed, Reason: "gang-scheduling failed"},
			{ID: "pod_autoscaling", Level: LevelMust, Status: StatusNotTested, Reason: "pod-autoscaling not run"},
			{ID: "cluster_autoscaling", Level: LevelMust, Status: StatusObserved, Reason: "cluster-autoscaling observed, not demonstrated"},
			{ID: "ai_inference", Level: LevelMust, Status: StatusPassed},
			{ID: "workload_sandboxing", Level: LevelShould, Status: StatusFailed, Reason: "sandbox failed"},
		}},
			wantErr: []string{"3 applicable MUST", "gang_scheduling failed (gang-scheduling failed)", "pod_autoscaling not tested (pod-autoscaling not run)",
				"cluster_autoscaling observed (cluster-autoscaling observed, not demonstrated)"},
			absent: []string{"ai_inference", "workload_sandboxing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.agg.Verdict()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Verdict() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Verdict() = nil, want the incomplete MUSTs to fail")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not mention %q", err, want)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %v names %q, which did not fail the run", err, bad)
				}
			}
		})
	}
}

// TestAggregateOutcome pins the shared exit rule of a submission run: the
// per-check rule (any failing CTRF status, mapped to a requirement or not)
// and the requirement verdict are both applied, the per-check rule can be
// switched off by the caller (--fail-on-error=false), and the verdict
// cannot.
func TestAggregateOutcome(t *testing.T) {
	passed := &Aggregate{Version: LatestVersion, Requirements: []RequirementResult{
		{ID: "gang_scheduling", Level: LevelMust, Status: StatusPassed},
	}}
	incomplete := &Aggregate{Version: LatestVersion, Requirements: []RequirementResult{
		{ID: "gang_scheduling", Level: LevelMust, Status: StatusNotTested, Reason: "gang-scheduling not run"},
	}}
	clean := reportOf(ctrf.TestResult{Name: "gang-scheduling", Status: ctrf.StatusPassed})
	crashed := reportOf(
		ctrf.TestResult{Name: "gang-scheduling", Status: ctrf.StatusPassed},
		ctrf.TestResult{Name: "platform-health", Status: ctrf.StatusOther, Message: "OOMKilled"})
	skippedOnly := reportOf(ctrf.TestResult{Name: "gang-scheduling", Status: ctrf.StatusSkipped})

	tests := []struct {
		name     string
		agg      *Aggregate
		report   *ctrf.Report
		perCheck bool
		want     []string
		absent   []string
	}{
		{name: "all passed", agg: passed, report: clean, perCheck: true},
		{name: "unmapped check crashed, every MUST established", agg: passed, report: crashed, perCheck: true,
			want: []string{"one or more checks did not pass"}, absent: []string{"not established"}},
		{name: "unmapped crash ignored without the per-check rule", agg: passed, report: crashed},
		{name: "MUST not tested, nothing failed", agg: incomplete, report: skippedOnly, perCheck: true,
			want: []string{"not established", "gang_scheduling not tested"}, absent: []string{"checks did not pass"}},
		{name: "verdict applies without the per-check rule", agg: incomplete, report: skippedOnly,
			want: []string{"not established"}},
		{name: "both", agg: incomplete, report: crashed, perCheck: true,
			want: []string{"one or more checks did not pass", "not established", "gang_scheduling not tested"}},
		{name: "nil aggregate, failed check", agg: nil, report: crashed, perCheck: true,
			want: []string{"one or more checks did not pass"}},
		{name: "nil report", agg: passed, report: nil, perCheck: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.agg.Outcome(tt.report, tt.perCheck)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Outcome() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Outcome() = nil, want an error")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %v does not mention %q", err, want)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("error %v mentions %q", err, bad)
				}
			}
		})
	}
}

// driver_runtime_management graduates to MUST at v1.38 (KAR-0001). No v1.38
// checklist is carried yet, so the level flip is exercised on the
// requirement aggregation directly: the same failed or skipped result that
// leaves the aggregate passing at SHOULD blocks it once the level is MUST,
// and a passed result establishes the MUST.
func TestDriverRuntimeManagementLevelDecidesRunVerdict(t *testing.T) {
	page := pageForRequirement("driver_runtime_management")
	if page == nil {
		t.Fatal("driver_runtime_management has no evidence page")
	}
	results := func(status string) map[string][]ctrf.TestResult {
		return map[string][]ctrf.TestResult{"driver-runtime-management": {{Name: "driver-runtime-management", Status: status}}}
	}
	tests := []struct {
		name       string
		level      Level
		status     string
		wantStatus RequirementStatus
		wantPassed bool
	}{
		{"SHOULD failed does not block", LevelShould, "failed", StatusFailed, true},
		{"SHOULD skipped does not block", LevelShould, "skipped", StatusNotTested, true},
		{"MUST failed blocks", LevelMust, "failed", StatusFailed, false},
		{"MUST skipped blocks", LevelMust, "skipped", StatusNotTested, false},
		{"MUST passed establishes", LevelMust, "passed", StatusPassed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := aggregateRequirement("driver_runtime_management", tt.level, page, results(tt.status))
			if r.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", r.Status, tt.wantStatus)
			}
			agg := &Aggregate{Version: Version137, Requirements: []RequirementResult{r}}
			if agg.Passed() != tt.wantPassed {
				t.Errorf("Passed() = %v, want %v (incomplete: %v)", agg.Passed(), tt.wantPassed, incompleteIDs(agg))
			}
		})
	}
	// Every carried checklist holds driver_runtime_management at SHOULD, the
	// level the SHOULD cases above assume; the MUST cases model v1.38.
	for _, v := range SupportedVersions() {
		cl, err := ChecklistFor(v)
		if err != nil {
			t.Fatal(err)
		}
		if r := cl.Requirement("driver_runtime_management"); r == nil || r.Level != LevelShould {
			t.Errorf("%s: driver_runtime_management = %+v, want SHOULD", v, r)
		}
	}
}

// workload_sandboxing graduates to MUST at v1.38 (KAR-0020). No v1.38
// checklist is carried yet, so the level flip is exercised on the
// requirement aggregation directly: the same failed result (a cluster with no
// sandboxed RuntimeClass fails the check, it never skips) that leaves the
// aggregate passing at SHOULD blocks it once the level is MUST, and a passed
// result establishes the MUST.
func TestWorkloadSandboxingLevelDecidesRunVerdict(t *testing.T) {
	page := pageForRequirement("workload_sandboxing")
	if page == nil {
		t.Fatal("workload_sandboxing has no evidence page")
	}
	results := func(status string) map[string][]ctrf.TestResult {
		return map[string][]ctrf.TestResult{"workload-sandboxing": {{Name: "workload-sandboxing", Status: status, Message: "unmet prerequisite: no sandboxed RuntimeClass"}}}
	}
	tests := []struct {
		name       string
		level      Level
		status     string
		wantStatus RequirementStatus
		wantPassed bool
	}{
		{"SHOULD failed does not block", LevelShould, "failed", StatusFailed, true},
		{"SHOULD not run does not block", LevelShould, "pending", StatusNotTested, true},
		{"MUST failed blocks", LevelMust, "failed", StatusFailed, false},
		{"MUST not run blocks", LevelMust, "pending", StatusNotTested, false},
		{"MUST passed establishes", LevelMust, "passed", StatusPassed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := aggregateRequirement("workload_sandboxing", tt.level, page, results(tt.status))
			if r.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", r.Status, tt.wantStatus)
			}
			agg := &Aggregate{Version: Version137, Requirements: []RequirementResult{r}}
			if agg.Passed() != tt.wantPassed {
				t.Errorf("Passed() = %v, want %v (incomplete: %v)", agg.Passed(), tt.wantPassed, incompleteIDs(agg))
			}
		})
	}
	// Every carried checklist that carries workload_sandboxing holds it at
	// SHOULD, the level the SHOULD cases above assume; the MUST cases model
	// v1.38. Checklists before v1.37 do not carry it at all.
	var carried int
	for _, v := range SupportedVersions() {
		cl, err := ChecklistFor(v)
		if err != nil {
			t.Fatal(err)
		}
		r := cl.Requirement("workload_sandboxing")
		if r == nil {
			continue
		}
		carried++
		if r.Level != LevelShould {
			t.Errorf("%s: workload_sandboxing = %+v, want SHOULD", v, r)
		}
	}
	if carried == 0 {
		t.Error("no carried checklist holds workload_sandboxing; the SHOULD cases above assert against nothing")
	}
}
