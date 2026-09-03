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
	stderrors "errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func creDynamicListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		certificationGVR: "CertificationList",
	}
}

func twoNodeGPUConfig() *gpuConfiguration {
	return &gpuConfiguration{
		WorkerCount:     2,
		GPUCountPerNode: 8,
		Nodes: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "gpu-b"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "gpu-a"}},
		},
	}
}

func TestUniqueCREResourceName(t *testing.T) {
	a, err := uniqueCREResourceName(creNCCLRunName)
	if err != nil {
		t.Fatal(err)
	}
	b, err := uniqueCREResourceName(creNCCLRunName)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("expected distinct names, both %q", a)
	}
	prefix := creNCCLRunName + "-"
	if !strings.HasPrefix(a, prefix) || !strings.HasPrefix(b, prefix) {
		t.Fatalf("names %q %q missing prefix %q", a, b, prefix)
	}
}

func TestWaitForCRETerminalSeedsWatchResourceVersion(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-watch", twoNodeGPUConfig())
	obj.SetResourceVersion("10")

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(), obj)

	fw := watch.NewFake()
	var watchRV atomic.Value
	client.PrependWatchReactor("certifications", func(action k8stesting.Action) (bool, watch.Interface, error) {
		wa, ok := action.(k8stesting.WatchAction)
		if !ok {
			t.Errorf("unexpected watch action %T", action)
			return false, nil, nil
		}
		watchRV.Store(wa.GetWatchRestrictions().ResourceVersion)
		go func() {
			terminal := obj.DeepCopy()
			terminal.Object["status"] = map[string]any{
				"conditions": []any{
					map[string]any{"type": "Succeeded", "status": "True"},
				},
			}
			fw.Modify(terminal)
		}()
		return true, fw, nil
	})

	got, err := waitForCertificationTerminal(context.Background(), client, "ns", obj.GetName())
	if err != nil {
		t.Fatalf("waitForCertificationTerminal() error = %v", err)
	}
	if !unstructuredConditionTrue(got, "Succeeded") {
		t.Fatal("expected Succeeded=True")
	}
	rv, _ := watchRV.Load().(string)
	if rv != "10" {
		t.Fatalf("Watch ResourceVersion = %q, want 10", rv)
	}
}

func TestWaitForCRETerminalReturnsInitialGetWhenAlreadyTerminal(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-done", twoNodeGPUConfig())
	obj.Object["status"] = map[string]any{
		"conditions": []any{
			map[string]any{"type": "Failed", "status": "True"},
		},
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(), obj)
	client.PrependWatchReactor("certifications", func(k8stesting.Action) (bool, watch.Interface, error) {
		t.Error("Watch must not run when Get is already terminal")
		return true, watch.NewFake(), nil
	})

	got, err := waitForCertificationTerminal(context.Background(), client, "ns", obj.GetName())
	if err != nil {
		t.Fatalf("waitForCertificationTerminal() error = %v", err)
	}
	if !unstructuredConditionTrue(got, "Failed") {
		t.Fatal("expected Failed=True")
	}
}

func TestDeleteCREResourceWaitsUntilGone(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-gone", twoNodeGPUConfig())
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(), obj)

	if err := deleteCRECertification(context.Background(), client, "ns", obj.GetName()); err != nil {
		t.Fatalf("deleteCRECertification() error = %v", err)
	}
	_, err := client.Resource(certificationGVR).Namespace("ns").Get(context.Background(), obj.GetName(), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Get after delete = %v, want NotFound", err)
	}
}

func TestDeleteCREResourceTimesOutWhenFinalizerHolds(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-stuck", twoNodeGPUConfig())
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(), obj)

	client.PrependReactor("delete", "certifications", func(k8stesting.Action) (bool, runtime.Object, error) {
		stuck := obj.DeepCopy()
		now := metav1.Now()
		stuck.SetDeletionTimestamp(&now)
		stuck.SetFinalizers([]string{"nvcre.nvidia.com/cleanup"})
		return true, stuck, nil
	})
	client.PrependReactor("get", "certifications", func(k8stesting.Action) (bool, runtime.Object, error) {
		stuck := obj.DeepCopy()
		now := metav1.Now()
		stuck.SetDeletionTimestamp(&now)
		stuck.SetFinalizers([]string{"nvcre.nvidia.com/cleanup"})
		return true, stuck, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := deleteCRECertification(ctx, client, "ns", obj.GetName())
	if err == nil {
		t.Fatal("expected timeout while Certification remains")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("error = %v, want ErrCodeTimeout", err)
	}
}

// TestDeleteCREResourceUsesForegroundPropagation pins the propagation policy.
// Under background propagation the API server removes the Certification first
// and reaps CRE's Workflows, TrainJobs, and pods afterwards, so the parent
// going away is no evidence the GPU work stopped — waitForCREResourceGone
// would report a clean teardown over jobs still holding the GPUs.
func TestDeleteCREResourceUsesForegroundPropagation(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-policy", twoNodeGPUConfig())
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(), obj)

	var got *metav1.DeletionPropagation
	var sawDelete bool
	client.PrependReactor("delete", "certifications", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(k8stesting.DeleteActionImpl)
		if !ok {
			t.Errorf("delete action has type %T, want DeleteActionImpl", action)
			return false, nil, nil
		}
		sawDelete = true
		got = del.DeleteOptions.PropagationPolicy
		return false, nil, nil
	})

	if err := deleteCRECertification(context.Background(), client, "ns", obj.GetName()); err != nil {
		t.Fatalf("deleteCRECertification() error = %v", err)
	}
	if !sawDelete {
		t.Fatal("no delete reached the API server")
	}
	if got == nil {
		t.Fatal("delete sent no PropagationPolicy, so dependents would be reaped in the background")
	}
	if *got != metav1.DeletePropagationForeground {
		t.Errorf("PropagationPolicy = %q, want %q", *got, metav1.DeletePropagationForeground)
	}
}

// TestCRECertificationDeleteTimeoutIsSizedForTeardown pins why teardown owns a
// budget separate from the diagnostics one it used to borrow: foreground
// propagation waits on multi-node GPU pods terminating, which the shorter
// diagnostics budget would cut short and leak a running job. It still has to
// stay under the run budget so cleanup cannot outlast the check it belongs to.
func TestCRECertificationDeleteTimeoutIsSizedForTeardown(t *testing.T) {
	if defaults.CRECertificationDeleteTimeout <= defaults.DiagnosticTimeout {
		t.Errorf("CRECertificationDeleteTimeout = %s, must exceed DiagnosticTimeout %s",
			defaults.CRECertificationDeleteTimeout, defaults.DiagnosticTimeout)
	}
	if defaults.CRECertificationDeleteTimeout >= defaults.CRECertificationTimeout {
		t.Errorf("CRECertificationDeleteTimeout = %s, must stay under CRECertificationTimeout %s",
			defaults.CRECertificationDeleteTimeout, defaults.CRECertificationTimeout)
	}
}

// TestCreCleanupFailure covers the precedence rule both CRE checks rely on: a
// leaked Certification must fail a check that would otherwise have passed, and
// must never overwrite the error that actually explains the run.
func TestCreCleanupFailure(t *testing.T) {
	primary := errors.New(errors.ErrCodeNotFound, "no GoodputMeasurement")
	deleteErr := errors.New(errors.ErrCodeTimeout, "finalizer still held")

	tests := []struct {
		name      string
		primary   error
		deleteErr error
		wantCode  error
		wantMsg   string
	}{
		{
			name: "clean run and clean teardown pass",
		},
		{
			name:      "teardown failure fails an otherwise passing check",
			deleteErr: deleteErr,
			wantCode:  errors.New(errors.ErrCodeInternal, ""),
			wantMsg:   "may still be running",
		},
		{
			name:     "primary error survives a clean teardown",
			primary:  primary,
			wantCode: errors.New(errors.ErrCodeNotFound, ""),
			wantMsg:  "no GoodputMeasurement",
		},
		{
			name:      "primary error wins over a teardown failure",
			primary:   primary,
			deleteErr: deleteErr,
			wantCode:  errors.New(errors.ErrCodeNotFound, ""),
			wantMsg:   "no GoodputMeasurement",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := creCleanupFailure(checkNameCRETrainingGoodput, tt.primary, tt.deleteErr)
			if tt.wantCode == nil {
				if got != nil {
					t.Fatalf("error = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("error = nil, want %v", tt.wantMsg)
			}
			if !stderrors.Is(got, tt.wantCode) {
				t.Errorf("error = %v, wrong code", got)
			}
			if !strings.Contains(got.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", got.Error(), tt.wantMsg)
			}
		})
	}
}

func TestDeleteCREResourceIgnoresAlreadyGone(t *testing.T) {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
	if err := deleteCRECertification(context.Background(), client, "ns", "missing"); err != nil {
		t.Fatalf("delete of missing Certification = %v", err)
	}
}

func TestBuildCRENCCLCertificationUsesCallerName(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-abcd1234", twoNodeGPUConfig())
	if obj.GetName() != "aicr-cre-nccl-abcd1234" {
		t.Fatalf("name = %q", obj.GetName())
	}
	if _, ok := obj.Object["spec"].(map[string]any); !ok {
		t.Fatal("missing spec")
	}
}

func TestWaitForCREResourceGoneNotFound(t *testing.T) {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
	res := client.Resource(certificationGVR).Namespace("ns")
	if err := waitForCREResourceGone(context.Background(), res, "Certification", "gone"); err != nil {
		t.Fatal(err)
	}
}

func TestCapCRECertificationNodes(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "z"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "m"}},
	}
	got := capCRECertificationNodes(nodes)
	if len(got) != creMaxNodesPerCertification {
		t.Fatalf("len = %d, want %d", len(got), creMaxNodesPerCertification)
	}
	if got[0].Name != "a" || got[1].Name != "m" {
		t.Fatalf("capped names = %q %q, want a m", got[0].Name, got[1].Name)
	}
}
