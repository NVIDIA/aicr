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
	stderrors "errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCheckCRENCCLSkipsWithoutConstraint(t *testing.T) {
	ctx := &validators.Context{
		ValidationInput: &v1.ValidationInput{
			Config: v1.ValidationConfig{
				Performance: &v1.ValidationPhase{
					Constraints: []recipe.Constraint{{Name: "nccl-all-reduce-bw", Value: ">= 300"}},
				},
			},
		},
	}
	err := checkCRENCCLAllReduceBW(ctx)
	if !validators.IsSkip(err) {
		t.Fatalf("expected Skip without CRE constraint, got %v", err)
	}
}

// TestCRENCCLCleanupResult pins the deferred teardown's effect on the result.
// The case that matters is both errors together: a teardown failure used to
// be dropped once the check had already failed, which is exactly when the
// surviving Certification goes unreported.
func TestCRENCCLCleanupResult(t *testing.T) {
	primary := errors.New(errors.ErrCodeInvalidRequest, "bandwidth below threshold")
	delErr := errors.New(errors.ErrCodeTimeout, "Workflow/wf-1 still running")

	tests := []struct {
		name          string
		actual        string
		passed        bool
		primary       error
		delErr        error
		wantActual    string
		wantPassed    bool
		wantCode      errors.ErrorCode
		wantMentions  string
		wantUnwrapsTo error
	}{
		{
			name:   "clean teardown leaves a passing reading alone",
			actual: "412.00", passed: true, primary: nil, delErr: nil,
			wantActual: "412.00", wantPassed: true,
		},
		{
			name:    "clean teardown leaves the check's own failure alone",
			primary: primary, delErr: nil,
			wantCode: errors.ErrCodeInvalidRequest, wantMentions: "bandwidth below threshold",
			wantUnwrapsTo: primary,
		},
		{
			name:   "teardown failure drops a passing reading",
			actual: "412.00", passed: true, delErr: delErr,
			wantCode: errors.ErrCodeInternal, wantMentions: "wf-1 still running",
			wantUnwrapsTo: delErr,
		},
		{
			name:    "teardown failure reaches the caller even when the check failed",
			primary: primary, delErr: delErr,
			// The primary keeps the code callers switch on, but the leak has
			// to be in the message the operator reads.
			wantCode: errors.ErrCodeInvalidRequest, wantMentions: "wf-1 still running",
			wantUnwrapsTo: primary,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, passed, err := creNCCLCleanupResult(tt.actual, tt.passed, tt.primary, tt.delErr)
			if actual != tt.wantActual || passed != tt.wantPassed {
				t.Errorf("actual, passed = %q, %v, want %q, %v", actual, passed, tt.wantActual, tt.wantPassed)
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			if got := errorstest.ReportedCode(err); got != tt.wantCode {
				t.Errorf("reported code = %v, want %v", got, tt.wantCode)
			}
			if !strings.Contains(err.Error(), tt.wantMentions) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantMentions)
			}
			if !stderrors.Is(err, tt.wantUnwrapsTo) {
				t.Errorf("error = %v, want it to unwrap to %v", err, tt.wantUnwrapsTo)
			}
		})
	}
}

func TestMaxBusBandwidthGBps(t *testing.T) {
	tests := []struct {
		name    string
		results []any
		want    float64
		wantErr bool
	}{
		{
			name: "max of string busBW",
			results: []any{
				map[string]any{"sizeBytes": int64(8), "busBW": "12.5"},
				map[string]any{"sizeBytes": int64(16), "busBW": "400.25"},
				map[string]any{"sizeBytes": int64(32), "busBW": "90"},
			},
			want: 400.25,
		},
		{
			name: "float64 from JSON decoder",
			results: []any{
				map[string]any{"busBW": float64(300)},
			},
			want: 300,
		},
		{
			name:    "empty",
			results: nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := maxBusBandwidthGBps(tt.results)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildCRENCCLCertification(t *testing.T) {
	cfg := twoNodeGPUConfig()
	cfg.Nodes = append(cfg.Nodes, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-z"}})
	obj := buildCRENCCLCertification(t, "ns", cfg)
	if obj.GetAPIVersion() != "nvcre.nvidia.com/v1alpha1" {
		t.Errorf("apiVersion = %q", obj.GetAPIVersion())
	}
	if obj.GetKind() != "Certification" {
		t.Errorf("kind = %q", obj.GetKind())
	}
	spec, ok := obj.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec is not a map")
	}
	categories := spec["categories"].([]any)
	category := categories[0].(map[string]any)
	if category["domain"] != creNCCLDomain || category["variant"] != creNCCLVariant {
		t.Errorf("category = %#v", category)
	}
	target := spec["target"].(map[string]any)
	names := target["nodeNames"].([]any)
	if len(names) != 2 || names[0] != "gpu-a" || names[1] != "gpu-b" {
		t.Errorf("nodeNames = %#v, want [gpu-a gpu-b]", names)
	}
	if _, ok := target["nodeSelector"]; ok {
		t.Error("nodeSelector must not be set; it would expand past the node cap")
	}
	if spec["gpusPerNode"] != int64(8) {
		t.Errorf("gpusPerNode = %#v", spec["gpusPerNode"])
	}
	if spec["nodesPerJob"] != int64(2) {
		t.Errorf("nodesPerJob = %#v, want 2", spec["nodesPerJob"])
	}
	if spec["timeoutPerJob"] != defaults.CRECertificationTimeout.String() {
		t.Errorf("timeoutPerJob = %#v, want %s", spec["timeoutPerJob"], defaults.CRECertificationTimeout)
	}
}

func TestBuildCRENCCLCertificationCopiesSharedGPUTaints(t *testing.T) {
	taint := corev1.Taint{
		Key:    "dedicated",
		Value:  "worker-workload",
		Effect: corev1.TaintEffectNoSchedule,
	}
	cfg := &gpuConfiguration{
		WorkerCount:     2,
		GPUCountPerNode: 8,
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{taint}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{taint}}},
		},
	}
	obj := buildCRENCCLCertification(t, "ns", cfg)
	spec := obj.Object["spec"].(map[string]any)
	target := spec["target"].(map[string]any)
	sels, ok := target["taintSelectors"].([]any)
	if !ok || len(sels) != 1 {
		t.Fatalf("taintSelectors = %#v", target["taintSelectors"])
	}
	sel := sels[0].(map[string]any)
	if sel["key"] != "dedicated" || sel["value"] != "worker-workload" || sel["effect"] != string(corev1.TaintEffectNoSchedule) {
		t.Fatalf("selector = %#v", sel)
	}
}

func TestUnstructuredConditionTrue(t *testing.T) {
	obj := buildCRENCCLCertification(t, "ns", twoNodeGPUConfig())
	obj.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Succeeded", "status": "True"},
		},
	}
	if !unstructuredConditionTrue(obj, "Succeeded") {
		t.Fatal("expected Succeeded=True")
	}
	if unstructuredConditionTrue(obj, "Failed") {
		t.Fatal("did not expect Failed")
	}
}

func TestYoungestLivePodSince(t *testing.T) {
	cutoff := metav1.NewTime(time.Unix(50, 0))
	older := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "old",
			CreationTimestamp: metav1.NewTime(time.Unix(1, 0)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	newer := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "new",
			CreationTimestamp: metav1.NewTime(time.Unix(100, 0)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	failed := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "failed",
			CreationTimestamp: metav1.NewTime(time.Unix(200, 0)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	got := youngestLivePodSince([]corev1.Pod{older, failed, newer}, cutoff)
	if got == nil || got.Name != "new" {
		t.Fatalf("got %#v, want new", got)
	}
}

func TestMeasurementBelongsToRun(t *testing.T) {
	createdAt := metav1.NewTime(time.Unix(100, 0))
	obj := buildCRENCCLCertification(t, "ns", twoNodeGPUConfig())
	obj.SetCreationTimestamp(metav1.NewTime(time.Unix(101, 0)))
	obj.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Workflow", Name: creNCCLRunName}})
	if !measurementBelongsToRun(obj, creNCCLRunName, createdAt) {
		t.Fatal("expected matching measurement")
	}
	if measurementBelongsToRun(obj, "aicr-cre-nemo", createdAt) {
		t.Fatal("measurement from another Certification must not match")
	}
	obj.SetCreationTimestamp(metav1.NewTime(time.Unix(99, 0)))
	if measurementBelongsToRun(obj, creNCCLRunName, createdAt) {
		t.Fatal("stale measurement must not match")
	}
}

// One threshold has to mean the same under both backends: the CRE verdict is
// the TrainJob verdict, so the floor sits at ncclBandwidthFloorFraction of the
// recipe value and nothing below it passes.
func TestCRENCCLVerdictMatchesTrainJob(t *testing.T) {
	tests := []struct {
		name      string
		bandwidth float64
		threshold float64
		want      bool
	}{
		{"at threshold", 300, 300, true},
		{"inside the floor band", 285, 300, true},
		{"exactly at the floor", 270, 300, true},
		{"just below the floor", 269.99, 300, false},
		{"zero bandwidth", 0, 300, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creActual, creOK := creNCCLVerdict(tt.bandwidth, tt.threshold)
			tjActual, tjOK := ncclBandwidthVerdict(tt.bandwidth, tt.threshold, "")
			if creOK != tt.want {
				t.Errorf("CRE verdict for %.2f against %.2f = %v, want %v", tt.bandwidth, tt.threshold, creOK, tt.want)
			}
			if creOK != tjOK || creActual != tjActual {
				t.Errorf("CRE verdict (%q, %v) differs from TrainJob (%q, %v)", creActual, creOK, tjActual, tjOK)
			}
		})
	}
}

// The extras are half of what one threshold meaning the same under both
// backends asks for, so a refactor that drops or renames one has to fail here.
func TestEmitCREBandwidthExtras(t *testing.T) {
	out := captureStdout(t, func() { emitCREBandwidthExtras(270, 300) })
	got := extraFromStdout(t, out)
	want := map[string]string{
		validators.ExtraKeyAchievedGbps:  "270",
		validators.ExtraKeyThresholdGbps: "300",
		validators.ExtraKeyTolerancePct:  "10",
		validators.ExtraKeyPassMargin:    "-10",
	}
	if !maps.Equal(got, want) {
		t.Errorf("extras = %v, want %v", got, want)
	}
}
