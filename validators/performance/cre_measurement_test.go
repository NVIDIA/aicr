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

// A certification reruns on nodes that may still carry measurements from an
// earlier run, so listMaxBusBandwidth has to attribute by owning Workflow and
// by creation time, and report the peak of what is left.
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
				creMeasurement("BandwidthMeasurement", "m-low", creTestWorkflow, time.Minute, busBWStatus("120.5")),
				creMeasurement("BandwidthMeasurement", "m-high", creTestWorkflow, time.Minute, busBWStatus(float64(489.8))),
			},
			want: 489.8,
		},
		{
			name: "peak within a single measurement's rows",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m", creTestWorkflow, time.Minute,
					busBWStatus(int64(10), float64(300.25), "42")),
			},
			want: 300.25,
		},
		{
			name: "ignores another workflow's measurement",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-other", "some-other-workflow", time.Minute, busBWStatus("999")),
				creMeasurement("BandwidthMeasurement", "m-ours", creTestWorkflow, time.Minute, busBWStatus("300")),
			},
			want: 300,
		},
		{
			name: "ignores a measurement predating the certification",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-stale", creTestWorkflow, -time.Hour, busBWStatus("999")),
				creMeasurement("BandwidthMeasurement", "m-ours", creTestWorkflow, time.Minute, busBWStatus("300")),
			},
			want: 300,
		},
		{
			name: "no attributable measurement",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-other", "some-other-workflow", time.Minute, busBWStatus("999")),
			},
			wantErr: true,
		},
		{
			name: "measurement without results is skipped, not fatal",
			objects: []runtime.Object{
				creMeasurement("BandwidthMeasurement", "m-empty", creTestWorkflow, time.Minute, map[string]any{}),
				creMeasurement("BandwidthMeasurement", "m-ours", creTestWorkflow, time.Minute, busBWStatus("300")),
			},
			want: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(), creMeasurementListKinds(), tt.objects...)

			got, err := listMaxBusBandwidth(context.Background(), client, creTestNamespace, creTestWorkflow, createdAt)
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
					map[string]any{"result": "0.7671", "avgTFLOPSPerGPU": "40.6"}),
			},
			wantResult: "0.7671",
		},
		{
			name: "ignores another workflow's status",
			objects: []runtime.Object{
				creMeasurement("GoodputMeasurement", "g-other", "some-other-workflow", time.Minute,
					map[string]any{"result": "0.1"}),
			},
			wantErr: true,
		},
		{
			name: "ignores a status predating the certification",
			objects: []runtime.Object{
				creMeasurement("GoodputMeasurement", "g-stale", creTestWorkflow, -time.Hour,
					map[string]any{"result": "0.1"}),
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

			status, err := getGoodputStatus(context.Background(), client, creTestNamespace, creTestWorkflow, createdAt)
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
