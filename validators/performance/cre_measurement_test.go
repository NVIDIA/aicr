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

package main

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

const creTestWorkflow = "aicr-cre-nccl-abcd1234-workflow"

func creMeasurementListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		bandwidthMeasurementGVR: "BandwidthMeasurementList",
		goodputMeasurementGVR:   "GoodputMeasurementList",
	}
}

// creMeasurement builds a measurement owned by workflowName, offset from the
// certification's creation time so the run-attribution filter can be exercised.
func creMeasurement(kind, name, workflowName string, age time.Duration, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: creAPIGroup + "/" + versionV1alpha1,
			keyKind:       kind,
			keyMetadata: map[string]any{
				keyName:             name,
				keyNamespace:        creTestNamespace,
				"creationTimestamp": metav1.NewTime(time.Now().Add(age)).Format(time.RFC3339),
				"ownerReferences": []any{
					map[string]any{
						keyAPIVersion: creAPIGroup + "/" + versionV1alpha1,
						keyKind:       "Workflow",
						keyName:       workflowName,
						"uid":         "00000000-0000-0000-0000-000000000000",
					},
				},
			},
			"status": status,
		},
	}
}

func busBWStatus(values ...any) map[string]any {
	rows := make([]any, 0, len(values))
	for _, v := range values {
		rows = append(rows, map[string]any{"busBW": v})
	}
	return map[string]any{"results": rows}
}

// completedWith stamps the Complete condition CRE writes when a measurement
// stops changing. Results are provisional until it appears, so a fixture
// without one is not readable by the checks.
func completedWith(status map[string]any, reason string) map[string]any {
	out := make(map[string]any, len(status)+1)
	for k, v := range status {
		out[k] = v
	}
	out["conditions"] = []any{
		map[string]any{
			"type":    measurementConditionComplete,
			"status":  conditionStatusTrue,
			"reason":  reason,
			"message": "fixture",
		},
	}
	return out
}

// final is the only completion whose results a check may read.
func final(status map[string]any) map[string]any {
	return completedWith(status, measurementReasonJobSucceeded)
}

// creMeasurementCtx bounds the readers' finalize wait so a fixture that never
// finalizes fails the case instead of burning CREMeasurementFinalizeTimeout.
func creMeasurementCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A certification reruns on nodes that may still carry measurements from an
// earlier run, so listMaxBusBandwidth has to attribute by owning Workflow and
// by creation time, and report the peak of what is left.
// The workflow name is the key every later lookup is scoped by, so a status
// that does not carry one has to fail here. A missing or null name must not
// travel on as a name: the measurement and pod lookups would then fail against
// a workflow that never existed, reporting no results rather than no workflow.
func TestCertificationWorkflowName(t *testing.T) {
	certification := func(ref any) *unstructured.Unstructured {
		status := map[string]any{"domain": "communication", "variant": "nccl-all-reduce"}
		if ref != nil {
			status["workflowRef"] = ref
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"categoryStatuses": []any{status}},
		}}
	}

	tests := []struct {
		name    string
		ref     any
		want    string
		wantErr bool
	}{
		{"name present", map[string]any{"name": creTestWorkflow}, creTestWorkflow, false},
		{"name empty", map[string]any{"name": ""}, "", true},
		{"name null", map[string]any{"name": nil}, "", true},
		{"name absent", map[string]any{}, "", true},
		{"name not a string", map[string]any{"name": int64(7)}, "", true},
		{"workflowRef absent", nil, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := certificationWorkflowName(certification(tt.ref), "communication", "nccl-all-reduce")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("workflow name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListMaxBusBandwidth(t *testing.T) {
	createdAt := metav1.NewTime(time.Now())

	tests := []struct {
		name    string
		objects []runtime.Object
		want    float64
		wantErr bool
	}{
		{
			name: "peak across this run's measurements",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-low", creTestWorkflow, time.Minute, final(busBWStatus("120.5"))),
				creMeasurement("BandwidthMeasurement", "m-high", creTestWorkflow, time.Minute, final(busBWStatus(float64(489.8)))),
			},
			want: 489.8,
		},
		{
			name: "peak within a single measurement's rows",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m", creTestWorkflow, time.Minute,
					final(busBWStatus(int64(10), float64(300.25), "42"))),
			},
			want: 300.25,
		},
		{
			name: "ignores another workflow's measurement",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-other", "some-other-workflow", time.Minute, final(busBWStatus("999"))),
				creMeasurement("BandwidthMeasurement", "m-ours", creTestWorkflow, time.Minute, final(busBWStatus("300"))),
			},
			want: 300,
		},
		{
			name: "ignores a measurement predating the certification",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-stale", creTestWorkflow, -time.Hour, final(busBWStatus("999"))),
				creMeasurement("BandwidthMeasurement", "m-ours", creTestWorkflow, time.Minute, final(busBWStatus("300"))),
			},
			want: 300,
		},
		{
			name: "no attributable measurement",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-other", "some-other-workflow", time.Minute, final(busBWStatus("999"))),
			},
			wantErr: true,
		},
		{
			name: "measurement without results is skipped, not fatal",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-empty", creTestWorkflow, time.Minute, final(map[string]any{})),
				creMeasurement("BandwidthMeasurement", "m-ours", creTestWorkflow, time.Minute, final(busBWStatus("300"))),
			},
			want: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(), creMeasurementListKinds(), tt.objects...)

			got, err := listMaxBusBandwidth(creMeasurementCtx(t), client, creTestNamespace, creTestWorkflow, createdAt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("busBW = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetGoodputStatus(t *testing.T) {
	createdAt := metav1.NewTime(time.Now())

	tests := []struct {
		name       string
		objects    []runtime.Object
		wantResult any
		wantErr    bool
	}{
		{
			name: "this run's status",
			objects: []runtime.Object{
				creMeasurement("GoodputMeasurement", "g", creTestWorkflow, time.Minute,
					final(map[string]any{"result": "0.7671", "avgTFLOPSPerGPU": "40.6"})),
			},
			wantResult: "0.7671",
		},
		{
			name: "ignores another workflow's status",
			objects: []runtime.Object{
				creMeasurement("GoodputMeasurement", "g-other", "some-other-workflow", time.Minute,
					final(map[string]any{"result": "0.1"})),
			},
			wantErr: true,
		},
		{
			name: "ignores a status predating the certification",
			objects: []runtime.Object{
				creMeasurement("GoodputMeasurement", "g-stale", creTestWorkflow, -time.Hour,
					final(map[string]any{"result": "0.1"})),
			},
			wantErr: true,
		},
		{
			name:    "no measurement at all",
			objects: nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(), creMeasurementListKinds(), tt.objects...)

			status, err := getGoodputStatus(creMeasurementCtx(t), client, creTestNamespace, creTestWorkflow, createdAt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if status["result"] != tt.wantResult {
				t.Errorf("result = %#v, want %#v", status["result"], tt.wantResult)
			}
		})
	}
}

// CRE marks a Job Succeeded before it performs the final log read, and the
// Certifications AICR builds set no thresholds, so nothing upstream holds the
// Workflow until the measurement is final. Provisional results are present and
// readable the whole time, and they "can stop short of the end of the sweep,
// where the largest message sizes and so the peak bandwidth are printed" — so
// reading them judges a cluster on a partial sweep. Only Complete=True with
// reason JobSucceeded means final; LogsUnavailable completes while leaving the
// provisional results in place, and NoDataCollected has none at all.
func TestListMaxBusBandwidthRequiresFinalResults(t *testing.T) {
	createdAt := metav1.NewTime(time.Now())

	tests := []struct {
		name       string
		status     map[string]any
		want       float64
		wantErr    bool
		wantReason string
	}{
		{
			name:   "final results are read",
			status: final(busBWStatus("489.80")),
			want:   489.80,
		},
		{
			name:    "provisional results are refused",
			status:  busBWStatus("3.1"),
			wantErr: true,
		},
		{
			name:       "LogsUnavailable leaves provisional results behind",
			status:     completedWith(busBWStatus("3.1"), "LogsUnavailable"),
			wantErr:    true,
			wantReason: "LogsUnavailable",
		},
		{
			name:       "NoDataCollected never matched the pattern",
			status:     completedWith(map[string]any{}, "NoDataCollected"),
			wantErr:    true,
			wantReason: "NoDataCollected",
		},
		{
			name:       "JobFailed is not a measurement",
			status:     completedWith(busBWStatus("489.80"), "JobFailed"),
			wantErr:    true,
			wantReason: "JobFailed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(), creMeasurementListKinds(),
				creMeasurement("BandwidthMeasurement", "m", creTestWorkflow, time.Minute, tt.status))

			got, err := listMaxBusBandwidth(creMeasurementCtx(t), client, creTestNamespace, creTestWorkflow, createdAt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantReason != "" && !strings.Contains(err.Error(), tt.wantReason) {
					t.Errorf("error %q does not name reason %q", err, tt.wantReason)
				}
				return
			}
			if got != tt.want {
				t.Errorf("busBW = %v, want %v", got, tt.want)
			}
		})
	}
}

// A run whose measurements have not all settled is not yet readable: taking
// the peak over the finalized subset would report a number that the pending
// one could still beat.
func TestListMaxBusBandwidthWaitsForEveryMeasurement(t *testing.T) {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), creMeasurementListKinds(),
		creMeasurement("BandwidthMeasurement", "m-final", creTestWorkflow, time.Minute, final(busBWStatus("300"))),
		creMeasurement("BandwidthMeasurement", "m-pending", creTestWorkflow, time.Minute, busBWStatus("489.80")))

	if _, err := listMaxBusBandwidth(
		creMeasurementCtx(t), client, creTestNamespace, creTestWorkflow, metav1.NewTime(time.Now()),
	); err == nil {
		t.Fatal("expected an error while one of the run's measurements is still provisional")
	}
}

// A measurement disappearing mid-wait is the Workflow being collected under
// the check, since CRE never deletes one itself. The wait has to end there
// rather than ride out the finalize budget and then read whatever survived.
func TestMeasurementDeleted(t *testing.T) {
	tests := []struct {
		name      string
		tracked   map[string]any
		deleteKey string
		wantErr   bool
		wantKept  bool
	}{
		{
			name:      "untracked name belongs to another run",
			tracked:   final(busBWStatus("300")),
			deleteKey: "other",
			wantKept:  true,
		},
		{
			name:      "already final, the reading stands",
			tracked:   final(busBWStatus("300")),
			deleteKey: "m-1",
			wantKept:  true,
		},
		{
			name:      "still provisional",
			tracked:   busBWStatus("300"),
			deleteKey: "m-1",
			wantErr:   true,
		},
		{
			name:      "completed without usable results",
			tracked:   completedWith(busBWStatus("300"), "LogsUnavailable"),
			deleteKey: "m-1",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byName := map[string]*unstructured.Unstructured{
				"m-1": creMeasurement("BandwidthMeasurement", "m-1", creTestWorkflow, time.Minute, tt.tracked),
			}

			err := measurementDeleted(byName, tt.deleteKey, "BandwidthMeasurement")
			if (err != nil) != tt.wantErr {
				t.Fatalf("measurementDeleted() error = %v, wantErr %v", err, tt.wantErr)
			}
			if _, kept := byName["m-1"]; kept != tt.wantKept {
				t.Errorf("m-1 retained = %v, want %v", kept, tt.wantKept)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "deleted before CRE finalized it") {
				t.Errorf("error does not name the cause: %v", err)
			}
		})
	}
}

func TestMeasurementCompletion(t *testing.T) {
	tests := []struct {
		name         string
		conditions   any
		wantReason   string
		wantComplete bool
	}{
		{
			name: "complete with reason",
			conditions: []any{map[string]any{
				"type": "Complete", "status": "True", "reason": "JobSucceeded",
			}},
			wantReason:   "JobSucceeded",
			wantComplete: true,
		},
		{
			name: "complete is False while measuring",
			conditions: []any{map[string]any{
				"type": "Complete", "status": "False", "reason": "JobRunning",
			}},
		},
		{
			name: "another condition type does not complete it",
			conditions: []any{map[string]any{
				"type": "Measuring", "status": "True", "reason": "JobRunning",
			}},
		},
		{
			name:       "no conditions at all",
			conditions: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := map[string]any{}
			if tt.conditions != nil {
				status["conditions"] = tt.conditions
			}
			obj := &unstructured.Unstructured{Object: map[string]any{"status": status}}

			reason, _, complete := measurementCompletion(obj)
			if complete != tt.wantComplete {
				t.Fatalf("complete = %v, want %v", complete, tt.wantComplete)
			}
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// Both checks fail only on "measured < threshold", so a non-finite reading
// would pass: NaN compares false against every threshold and +Inf is above
// all of them.
func TestParseBusBWFieldRejectsNonFinite(t *testing.T) {
	for _, value := range []any{"NaN", "+Inf", "-Inf", "inf", math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := parseBusBWField(value); err == nil {
			t.Errorf("parseBusBWField(%v) = nil error, want rejection", value)
		}
	}
}
