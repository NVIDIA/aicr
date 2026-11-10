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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// TestAggregateSubmissionDocumented pins the manual-attestation rule: vendor
// documentation establishes a requirement the run only observed, and
// nothing else. A passed requirement needs none, a failed or untested MUST
// is not papered over, and the status itself never changes.
func TestAggregateSubmissionDocumented(t *testing.T) {
	doc := Documentation{Evidence: []string{"https://vendor.example/docs/autoscaling"}, Notes: "Node pools scale on pending accelerator pods."}
	observed := &ctrf.TestResult{Name: "cluster-autoscaling", Status: "passed", Extra: observationOnly}
	tests := []struct {
		name            string
		tests           []ctrf.TestResult
		documented      map[string]Documentation
		notApplicable   map[string]string
		requirement     string // defaults to cluster_autoscaling
		wantStatus      RequirementStatus
		wantEstablished bool
		wantDocumented  bool
		wantPassed      bool
		wantReasonHas   string
	}{
		{
			name:            "observed and documented is established",
			tests:           withResult(allMustsPassed, "cluster-autoscaling", observed),
			documented:      map[string]Documentation{"cluster_autoscaling": doc},
			wantStatus:      StatusObserved,
			wantEstablished: true,
			wantDocumented:  true,
			wantPassed:      true,
		},
		{
			name:          "observed without documentation is incomplete",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", observed),
			wantStatus:    StatusObserved,
			wantReasonHas: "observed, not demonstrated",
		},
		{
			// cluster-autoscaling can only be observed, so the passed case
			// uses a behavioral requirement and covers the observed MUST.
			name:            "passed ignores documentation",
			tests:           allMustsPassed,
			requirement:     "gang_scheduling",
			notApplicable:   map[string]string{"cluster_autoscaling": "fixed-size cluster"},
			documented:      map[string]Documentation{"gang_scheduling": doc},
			wantStatus:      StatusPassed,
			wantEstablished: true,
			wantPassed:      true,
		},
		{
			name:          "failed is not papered over",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", &ctrf.TestResult{Name: "cluster-autoscaling", Status: "failed", Message: "no autoscaler"}),
			documented:    map[string]Documentation{"cluster_autoscaling": doc},
			wantStatus:    StatusFailed,
			wantReasonHas: "documented attestation ignored",
		},
		{
			name:          "not tested is not papered over",
			tests:         withResult(allMustsPassed, "cluster-autoscaling", nil),
			documented:    map[string]Documentation{"cluster_autoscaling": doc},
			wantStatus:    StatusNotTested,
			wantReasonHas: "documented attestation ignored",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg, err := AggregateSubmission(Version137, reportOf(tt.tests...), AggregateOptions{Documented: tt.documented, NotApplicable: tt.notApplicable})
			if err != nil {
				t.Fatalf("AggregateSubmission() = %v", err)
			}
			requirement := tt.requirement
			if requirement == "" {
				requirement = "cluster_autoscaling"
			}
			r := agg.Requirement(requirement)
			if r.Status != tt.wantStatus {
				t.Errorf("Status = %s, want %s", r.Status, tt.wantStatus)
			}
			if r.Established() != tt.wantEstablished {
				t.Errorf("Established() = %v, want %v", r.Established(), tt.wantEstablished)
			}
			if (r.Documented != nil) != tt.wantDocumented {
				t.Errorf("Documented = %v, want set %v", r.Documented, tt.wantDocumented)
			}
			if tt.wantDocumented && (r.Documented.Notes != doc.Notes || r.Documented.Evidence[0] != doc.Evidence[0]) {
				t.Errorf("Documented = %+v, want %+v", *r.Documented, doc)
			}
			if agg.Passed() != tt.wantPassed {
				t.Errorf("Passed() = %v, want %v (incomplete: %v)", agg.Passed(), tt.wantPassed, agg.IncompleteMusts())
			}
			if tt.wantReasonHas != "" && !strings.Contains(r.Reason, tt.wantReasonHas) {
				t.Errorf("Reason %q lacks %q", r.Reason, tt.wantReasonHas)
			}
		})
	}
}

func TestAggregateSubmissionRefusesMalformedDocumented(t *testing.T) {
	_, err := AggregateSubmission(Version137, reportOf(allMustsPassed...), AggregateOptions{
		Documented: map[string]Documentation{"cluster_autoscaling": {Notes: "no url"}},
	})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

// A SHOULD the run only observed and the vendor documents is established
// too, so it renders Implemented; a SHOULD never fails the run either way.
func TestAggregateSubmissionDocumentedShould(t *testing.T) {
	observedDriver := &ctrf.TestResult{Name: "driver-runtime-management", Status: "passed", Extra: observationOnly}
	agg, err := AggregateSubmission(Version137, reportOf(append(allMustsPassed, *observedDriver)...), AggregateOptions{
		Documented:    map[string]Documentation{"driver_runtime_management": {Evidence: []string{"https://vendor.example/docs/drivers"}, Notes: "Drivers are reconciled by the GPU Operator."}},
		NotApplicable: map[string]string{"cluster_autoscaling": "fixed-size cluster"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := agg.Requirement("driver_runtime_management")
	if r.Status != StatusObserved || !r.Established() || r.Documented == nil {
		t.Errorf("driver_runtime_management = %+v", *r)
	}
	if !agg.Passed() {
		t.Errorf("Passed() = false: %v", agg.IncompleteMusts())
	}
}
