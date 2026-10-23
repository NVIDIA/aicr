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
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/aicr/validators"
)

// ncclGVRListKinds maps every GVR applyNCCLResources touches to a fake list
// kind, so the dynamic fake client can serve Create/Get/Update for these CRDs
// without a real REST mapper.
var ncclGVRListKinds = map[schema.GroupVersionResource]string{
	resourceClaimTemplateGVR: "ResourceClaimTemplateList",
	trainJobGVR:              "TrainJobList",
	trainingRuntimeGVR:       "TrainingRuntimeList",
	computeDomainGVR:         "ComputeDomainList",
}

func newFakeDynamicClient() dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), ncclGVRListKinds)
}

// testNamespaceUID is the owning UID used across cleanupNCCLResources tests
// that seed a namespace. A real namespace always has one, and
// cleanupNCCLResources now refuses to delete without one (see
// TestCleanupNCCLResources_RejectsEmptyUID).
const testNamespaceUID = types.UID("test-owner-uid")

// testHolderID is the execution lock holder ID used across cleanupNCCLRun
// tests. Pair it with testHeldLease so ncclExecutionLockHeldBy finds a live
// Lease naming this holder, rather than failing closed on one that's missing.
const testHolderID = "test-holder-id"

// testHeldLease returns a Lease naming testHolderID as the current holder
// of namespace's execution lock, for tests that exercise cleanupNCCLRun
// without covering the lock check itself.
func testHeldLease(namespace string) *coordinationv1.Lease {
	holder := testHolderID
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: namespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

// TestCleanupNCCLResources_ToleratesMissing verifies the deferred cleanup is
// safe to run after an early/partial-apply failure. With no namespace ever
// created, deleting it must be treated as success (NotFound-tolerant), not
// an error.
func TestCleanupNCCLResources_ToleratesMissing(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset()
	if err := cleanupNCCLResources(fakeClient, ns, testNamespaceUID, defaults.InferenceNamespaceTerminationWait); err != nil {
		t.Fatalf("cleanup of a namespace that was never created should not error, got: %v", err)
	}
}

// TestCleanupNCCLResources_DeletesNamespace verifies the happy path. The
// per-run namespace this run created is deleted, cascading away everything
// created in it (TrainJob, TrainingRuntime, ComputeDomain) via
// ordinary Kubernetes namespace garbage collection, with no per-resource
// tracking required since nothing else ever shares this namespace.
func TestCleanupNCCLResources_DeletesNamespace(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})

	if err := cleanupNCCLResources(fakeClient, ns, testNamespaceUID, defaults.InferenceNamespaceTerminationWait); err != nil {
		t.Fatalf("cleanup should not error, got: %v", err)
	}

	if _, err := fakeClient.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("namespace should be deleted after cleanup, got err=%v", err)
	}
}

// TestCleanupNCCLResources_ReturnsErrorOnDeleteFailure verifies a delete
// failure that is not NotFound (e.g. a transient apiserver error) is
// returned to the caller instead of only logged, so foldCleanupError can
// still fail an otherwise-passing check on it.
func TestCleanupNCCLResources_ReturnsErrorOnDeleteFailure(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	fakeClient.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	err := cleanupNCCLResources(fakeClient, ns, testNamespaceUID, defaults.InferenceNamespaceTerminationWait)
	if err == nil {
		t.Fatal("expected an error from a non-NotFound namespace delete failure, got nil")
	}
	if errorstest.ReportedCode(err) != aicrErrors.ErrCodeInternal {
		t.Errorf("got %v, want an ErrCodeInternal-wrapped delete failure", err)
	}
}

// TestCleanupNCCLResources_RejectsEmptyUID is the regression guard for the
// hardening finding that an empty owning UID must be rejected outright,
// since the fake client used in the other tests here ignores Delete
// preconditions entirely and would silently proceed on a caller bug that
// drops the UID.
func TestCleanupNCCLResources_RejectsEmptyUID(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})

	err := cleanupNCCLResources(fakeClient, ns, "", defaults.InferenceNamespaceTerminationWait)
	if err == nil {
		t.Fatal("expected an error for an empty owning UID, got nil")
	}
	if errorstest.ReportedCode(err) != aicrErrors.ErrCodeInternal {
		t.Errorf("got %v, want an ErrCodeInternal error", err)
	}
	if _, getErr := fakeClient.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); getErr != nil {
		t.Errorf("namespace should be left alone when the UID check is rejected, got err=%v", getErr)
	}
}

// TestCleanupNCCLResources_ReturnsErrorOnTerminationTimeout verifies that a
// namespace stuck in Terminating (e.g. a stuck DRA/IMEX finalizer) after a
// successful Delete call is surfaced as a returned ErrCodeTimeout, not
// silently swallowed — a finalizer-stuck teardown must not report a clean
// "Deleted" while the ComputeDomain/ResourceClaimTemplate leaks forever.
// terminationWait is injected as a tiny bound so the test doesn't wait out
// the real production timeout.
func TestCleanupNCCLResources_ReturnsErrorOnTerminationTimeout(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	nsGVR := v1.SchemeGroupVersion.WithResource("namespaces")

	// Unlike TestCleanupNCCLResources_WaitsForFinalizerHeldNamespace, this
	// finalizer never clears: every Delete re-stamps DeletionTimestamp
	// instead of ever letting the object actually disappear, so
	// waitForNamespaceGone never observes NotFound within the bound. The
	// fake client's default Delete otherwise ignores Finalizers entirely
	// and removes the object outright, which is why a pre-seeded
	// already-Terminating object alone (without this reactor) does not
	// reproduce a stuck finalizer.
	fakeClient.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		existing, getErr := fakeClient.Tracker().Get(nsGVR, "", ns)
		obj, ok := existing.(*v1.Namespace)
		if getErr != nil || !ok {
			return false, nil, nil
		}
		held := obj.DeepCopy()
		held.Finalizers = []string{"kubernetes"}
		now := metav1.Now()
		held.DeletionTimestamp = &now
		if err := fakeClient.Tracker().Update(nsGVR, held, ""); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})

	err := cleanupNCCLResources(fakeClient, ns, testNamespaceUID, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error when the namespace never finishes terminating, got nil")
	}
	if errorstest.ReportedCode(err) != aicrErrors.ErrCodeTimeout {
		t.Errorf("got %v, want an ErrCodeTimeout-wrapped termination-wait failure", err)
	}
}

// TestCleanupNCCLResources_UIDMismatchPreventsDelete verifies a UID
// mismatch is treated like NotFound, not a cleanup failure. client-go's
// fake ObjectTracker ignores Delete preconditions, so this reactor emulates
// the real apiserver's check.
func TestCleanupNCCLResources_UIDMismatchPreventsDelete(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	const actualUID = types.UID("actual-owner-uid")
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: actualUID}})
	fakeClient.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deleteAction, ok := action.(k8stesting.DeleteAction)
		if !ok {
			return false, nil, nil
		}
		preconditions := deleteAction.GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil || *preconditions.UID == actualUID {
			return false, nil, nil
		}
		return true, nil, apierrors.NewConflict(v1.Resource("namespaces"), ns,
			stderrors.New("uid in precondition does not match uid in record"))
	})

	if err := cleanupNCCLResources(fakeClient, ns, "wrong-uid", defaults.InferenceNamespaceTerminationWait); err != nil {
		t.Fatalf("expected a UID mismatch to be treated as already-replaced, got err=%v", err)
	}
	if _, getErr := fakeClient.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); getErr != nil {
		t.Errorf("namespace should be left alone on a UID mismatch, got err=%v", getErr)
	}
}

// TestCleanupNCCLResources_WaitsForFinalizerHeldNamespace is the regression
// guard for the wait-for-termination fix. A namespace whose first delete
// only stamps a DeletionTimestamp (finalizers still cascading) must not be
// reported as cleaned up until it actually disappears. Before this fix,
// cleanupNCCLResources returned nil as soon as the first Delete call was
// accepted.
func TestCleanupNCCLResources_WaitsForFinalizerHeldNamespace(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	const holdFinalizer = 200 * time.Millisecond
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, UID: testNamespaceUID}})
	nsGVR := v1.SchemeGroupVersion.WithResource("namespaces")

	fakeClient.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		// Branch on the DeletionTimestamp, not a counter, matching how a
		// real apiserver decides.
		existing, getErr := fakeClient.Tracker().Get(nsGVR, "", ns)
		obj, ok := existing.(*v1.Namespace)
		if getErr == nil && ok && obj.DeletionTimestamp == nil {
			// On the first delete, simulate a still-cascading finalizer by
			// stamping DeletionTimestamp instead of removing the object,
			// then accept the request as a real apiserver would.
			held := obj.DeepCopy()
			held.Finalizers = []string{"kubernetes"}
			now := metav1.Now()
			held.DeletionTimestamp = &now
			if err := fakeClient.Tracker().Update(nsGVR, held, ""); err != nil {
				return true, nil, err
			}
			return true, nil, nil
		}
		// Already marked for deletion. The goroutine below fires this once
		// the "finalizer" clears, so let the default reactor delete it for
		// real, emitting the watch.Deleted event waitForNamespaceGone is
		// blocked on.
		return false, nil, nil
	})

	// Captured before the goroutine launches so elapsed can't dip under
	// holdFinalizer from a head start.
	start := time.Now()
	go func() {
		time.Sleep(holdFinalizer)
		_ = fakeClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	}()

	if err := cleanupNCCLResources(fakeClient, ns, testNamespaceUID, defaults.InferenceNamespaceTerminationWait); err != nil {
		t.Fatalf("cleanup should succeed once the finalizer clears, got: %v", err)
	}
	elapsed := time.Since(start)

	// A delete count of 2 alone doesn't prove cleanup blocked. The
	// goroutine fires the second delete unconditionally at t=200ms. The
	// elapsed-time check below is the real guard. A pre-fix early return
	// would hit it at ~0ms with the count still at 1.
	if got := countDeleteActions(fakeClient.Actions(), "namespaces"); got < 2 {
		t.Fatalf("expected cleanup to observe the namespace still present and wait for a second delete, got %d delete call(s)", got)
	}
	if elapsed < holdFinalizer {
		t.Errorf("cleanup returned after %v, want it to have blocked at least %v for the finalizer to clear (it returned before the namespace actually disappeared)", elapsed, holdFinalizer)
	}
}

// TestWaitForNamespaceGone_TimesOutWhenNeverDeleted guards the bounded-wait
// contract of waitForNamespaceGone itself. If finalizers never clear within
// the deadline, it must return ErrCodeTimeout rather than hang indefinitely
// (cleanupNCCLResources surfaces this as a returned ErrCodeTimeout). Calls it
// directly with a short local context to avoid the real 5-minute
// production bound.
func TestWaitForNamespaceGone_TimesOutWhenNeverDeleted(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	now := metav1.Now()
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              ns,
		Finalizers:        []string{"kubernetes"},
		DeletionTimestamp: &now,
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := waitForNamespaceGone(ctx, fakeClient.CoreV1().Namespaces(), ns)
	if err == nil {
		t.Fatal("expected a timeout error waiting for a namespace that never finishes terminating, got nil")
	}
	if errorstest.ReportedCode(err) != aicrErrors.ErrCodeTimeout {
		t.Errorf("got %v, want an ErrCodeTimeout-wrapped wait failure", err)
	}
}

// TestCreateOrUpdateFromTemplate_Idempotent covers the AlreadyExists branch:
// a re-apply must Get + Update the existing object, not fail.
func TestCreateOrUpdateFromTemplate_Idempotent(t *testing.T) {
	const ns = "aicr-validation"
	const name = "test-claim"
	claimPath := filepath.Join("testdata", "inference", "resource-claim-template.yaml")
	data := map[string]string{"CLAIM_API_VERSION": "v1", "CLAIM_TEMPLATE_NAME": name, "NAMESPACE": ns}

	fakeClient := newFakeDynamicClient()
	ctx := &validators.Context{Ctx: context.Background(), DynamicClient: fakeClient}

	if err := createOrUpdateFromTemplate(ctx, resourceClaimTemplateGVR, ns, claimPath, data, nil); err != nil {
		t.Fatalf("first apply (create) failed: %v", err)
	}
	relabel := func(u *unstructured.Unstructured) error {
		u.SetLabels(map[string]string{"apply": "second"})
		return nil
	}
	if err := createOrUpdateFromTemplate(ctx, resourceClaimTemplateGVR, ns, claimPath, data, relabel); err != nil {
		t.Fatalf("second apply (update) failed: %v", err)
	}

	got, err := fakeClient.Resource(resourceClaimTemplateGVR).Namespace(ns).
		Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("object not found after re-apply: %v", err)
	}
	if got.GetLabels()["apply"] != "second" {
		t.Errorf("labels = %v after second apply, want apply=second (update did not take effect)", got.GetLabels())
	}
}
