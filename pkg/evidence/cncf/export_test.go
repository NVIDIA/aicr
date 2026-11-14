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
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// TestExportTests pins the export rule (ADR-007 amendment, decision 6): the
// disposition of a non-passing result follows the requirement behind the
// check, and a passing or skipped result is exported as it is.
func TestExportTests(t *testing.T) {
	tests := []struct {
		name          string
		result        ctrf.TestResult
		notApplicable map[string]string
		wantStatus    exportedStatus
		wantRewritten bool
		wantMessage   []string
	}{
		{
			name:       "passed stays passed",
			result:     ctrf.TestResult{Name: "gang-scheduling", Status: "passed", Duration: 1500},
			wantStatus: exportPassed,
		},
		{
			name:        "skipped keeps its reason code",
			result:      ctrf.TestResult{Name: "secure-accelerator-access", Status: "skipped", Message: "Slurm holds the GPUs", Extra: map[string]string{"skipReason": "slurm-managed-gpus"}},
			wantStatus:  exportSkipped,
			wantMessage: []string{"skipped: slurm-managed-gpus"},
		},
		{
			name:        "skipped without a code keeps its message",
			result:      ctrf.TestResult{Name: "secure-accelerator-access", Status: "skipped", Message: "no GPU node"},
			wantStatus:  exportSkipped,
			wantMessage: []string{"skipped: no GPU node"},
		},
		{
			name:          "failed check behind a SHOULD is exported skipped",
			result:        ctrf.TestResult{Name: "driver-runtime-management", Status: "failed", Message: "driver version mismatch"},
			wantStatus:    exportSkipped,
			wantRewritten: true,
			wantMessage:   []string{"not implemented: driver_runtime_management is a SHOULD at v1.37", "original verdict failed: driver version mismatch"},
		},
		{
			name:          "crashed check behind a SHOULD is exported skipped",
			result:        ctrf.TestResult{Name: "driver-runtime-management", Status: "other"},
			wantStatus:    exportSkipped,
			wantRewritten: true,
			wantMessage:   []string{"original verdict other"},
		},
		{
			name:          "failed check behind an N/A MUST is exported skipped with the justification",
			result:        ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Message: "no autoscaler confirmed"},
			notApplicable: map[string]string{"cluster_autoscaling": "fixed-size pools"},
			wantStatus:    exportSkipped,
			wantRewritten: true,
			wantMessage:   []string{"not applicable: fixed-size pools", "original verdict failed: no autoscaler confirmed"},
		},
		{
			name:        "failed check behind an applicable MUST stays failed",
			result:      ctrf.TestResult{Name: "gang-scheduling", Status: "failed", Message: "pods not co-scheduled"},
			wantStatus:  exportFailed,
			wantMessage: []string{"failed: pods not co-scheduled"},
		},
		{
			name:        "failed check that evidences no requirement stays failed",
			result:      ctrf.TestResult{Name: "platform-health", Status: "failed", Message: "pods pending"},
			wantStatus:  exportFailed,
			wantMessage: []string{"failed: pods pending"},
		},
		{
			name:          "failed supporting check is exported skipped",
			result:        ctrf.TestResult{Name: "dra-support", Status: "failed", Message: "no ResourceSlice"},
			wantStatus:    exportSkipped,
			wantRewritten: true,
			wantMessage:   []string{"supporting evidence: dra_support is not a requirement of the v1.37 checklist"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := reportOf(append(withResult(allMustsPassed, tt.result.Name, nil), tt.result)...)
			agg, err := AggregateReport(Version137, report, tt.notApplicable)
			if err != nil {
				t.Fatal(err)
			}
			var got *exportedTest
			for _, e := range exportTests(report, agg) {
				if e.Name == tt.result.Name {
					e := e
					got = &e
				}
			}
			if got == nil {
				t.Fatalf("%s not exported", tt.result.Name)
			}
			if got.Status != tt.wantStatus || got.Rewritten != tt.wantRewritten || got.Original != tt.result.Status {
				t.Errorf("exported = %+v, want status %s rewritten %v", *got, tt.wantStatus, tt.wantRewritten)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(got.Message, want) {
					t.Errorf("message %q lacks %q", got.Message, want)
				}
			}
			if got.Duration != time.Duration(tt.result.Duration)*time.Millisecond {
				t.Errorf("Duration = %s", got.Duration)
			}
		})
	}
}

// A MUST failed at one version can be a SHOULD at another; the export rule
// follows the aggregated version. dra_support is a MUST at v1.35 and
// supporting evidence from v1.36.
func TestExportTestsFollowsVersion(t *testing.T) {
	failed := ctrf.TestResult{Name: "dra-support", Status: "failed", Message: "no driver"}
	for _, tc := range []struct {
		version Version
		want    exportedStatus
	}{{Version135, exportFailed}, {Version136, exportSkipped}} {
		agg, err := AggregateReport(tc.version, reportOf(failed), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := exportTests(reportOf(failed), agg)[0].Status; got != tc.want {
			t.Errorf("%s: dra-support exported %s, want %s", tc.version, got, tc.want)
		}
	}
}

// A multi-phase report's deployment and performance results are not
// exported; a result with no recorded phase is.
func TestExportTestsConformancePhaseOnly(t *testing.T) {
	report := reportOf(
		ctrf.TestResult{Name: "gang-scheduling", Status: "passed", Suite: []string{"conformance"}},
		ctrf.TestResult{Name: "operator-health", Status: "failed", Suite: []string{"deployment"}},
		ctrf.TestResult{Name: "nccl-all-reduce-bw", Status: "failed", Suite: []string{"performance"}},
		ctrf.TestResult{Name: "platform-health", Status: "passed"},
	)
	agg, err := AggregateReport(Version137, report, nil)
	if err != nil {
		t.Fatal(err)
	}
	exported := exportTests(report, agg)
	names := make([]string, 0, len(exported))
	for _, e := range exported {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "gang-scheduling,platform-health" {
		t.Errorf("exported %v", names)
	}
}

func TestExportTestsNilReport(t *testing.T) {
	if got := exportTests(nil, &Aggregate{}); got != nil {
		t.Errorf("exportTests(nil) = %v", got)
	}
}

// The artifacts must parse the way upstream parses them: junit.xml as
// <testsuites><testsuite><testcase> with failure/skipped children, e2e.log
// with one "--- PASS|FAIL|SKIP: <name>" line per check.
func TestRenderArtifactsShape(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tests := []exportedTest{
		{Name: "gang-scheduling", Status: exportPassed, Duration: 1500 * time.Millisecond, Original: "passed"},
		{Name: "driver-runtime-management", Status: exportSkipped, Message: "not implemented: x; original verdict failed: y", Duration: 2 * time.Second, Original: "failed", Rewritten: true},
		{Name: "platform-health", Status: exportFailed, Message: "failed: pods pending\nsecond line", Duration: 500 * time.Millisecond, Original: "failed"},
	}
	junit, err := renderJUnit(tests, now)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		XMLName  xml.Name `xml:"testsuites"`
		Tests    int      `xml:"tests,attr"`
		Failures int      `xml:"failures,attr"`
		Skipped  int      `xml:"skipped,attr"`
		Suites   []struct {
			Name  string `xml:"name,attr"`
			Cases []struct {
				Name    string `xml:"name,attr"`
				Time    string `xml:"time,attr"`
				Failure *struct {
					Message string `xml:"message,attr"`
					Body    string `xml:",chardata"`
				} `xml:"failure"`
				Skipped *struct {
					Message string `xml:"message,attr"`
				} `xml:"skipped"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(junit, &root); err != nil {
		t.Fatalf("junit.xml does not parse: %v\n%s", err, junit)
	}
	if root.Tests != 3 || root.Failures != 1 || root.Skipped != 1 || len(root.Suites) != 1 || root.Suites[0].Name != junitSuiteName {
		t.Errorf("junit counts = %+v", root)
	}
	cases := root.Suites[0].Cases
	if cases[0].Failure != nil || cases[0].Skipped != nil || cases[0].Time != "1.500" {
		t.Errorf("passed case = %+v", cases[0])
	}
	if cases[1].Skipped == nil || !strings.Contains(cases[1].Skipped.Message, "original verdict failed") || cases[1].Failure != nil {
		t.Errorf("skipped case = %+v", cases[1])
	}
	if cases[2].Failure == nil || !strings.Contains(cases[2].Failure.Body, "second line") {
		t.Errorf("failed case = %+v", cases[2])
	}

	e2e := string(renderE2ELog(tests, now))
	for _, want := range []string{
		"=== RUN   gang-scheduling\n--- PASS: gang-scheduling (1.500s)\n",
		"--- SKIP: driver-runtime-management (2.000s)\n    not implemented: x; original verdict failed: y\n",
		"--- FAIL: platform-health (0.500s)\n    failed: pods pending\n    second line\n",
		"FAIL\nFAIL\taicr/conformance\t4.000s\n",
	} {
		if !strings.Contains(e2e, want) {
			t.Errorf("e2e.log lacks %q:\n%s", want, e2e)
		}
	}
	passing := string(renderE2ELog(tests[:1], now))
	if !strings.HasSuffix(passing, "PASS\nok  \taicr/conformance\t1.500s\n") {
		t.Errorf("passing e2e.log tail:\n%s", passing)
	}
}

func TestSkipMessageBare(t *testing.T) {
	if got := skipMessage(ctrf.TestResult{Name: "x", Status: "skipped"}); got != "skipped" {
		t.Errorf("skipMessage() = %q", got)
	}
}
