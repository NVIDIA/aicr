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

	"github.com/NVIDIA/aicr/pkg/errors"
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
		workloadRunGVR:   "WorkloadRunList",
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
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-watch", cfg, nil)
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
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-done", cfg, nil)
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
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-gone", cfg, nil)
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
	cfg := &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-stuck", cfg, nil)
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

func TestDeleteCREResourceIgnoresAlreadyGone(t *testing.T) {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
	if err := deleteCRECertification(context.Background(), client, "ns", "missing"); err != nil {
		t.Fatalf("delete of missing Certification = %v", err)
	}
}

func TestBuildCRENCCLCertificationUsesCallerName(t *testing.T) {
	obj := buildCRENCCLCertification("ns", "aicr-cre-nccl-abcd1234", &gpuConfiguration{WorkerCount: 2, GPUCountPerNode: 8}, nil)
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
