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
	"sync/atomic"
	"testing"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

func newFakeDynamicClient(objs ...runtime.Object) dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), ncclGVRListKinds, objs...)
}

// roceClaimCount walks the RoCE ResourceClaimTemplate to the templated device
// count (spec.spec.devices.requests[0].exactly.count). Fails the test on any
// shape mismatch so a future template restructure is caught here.
func roceClaimCount(t *testing.T, claim *unstructured.Unstructured) int64 {
	t.Helper()
	requests, found, err := unstructured.NestedSlice(claim.Object, "spec", "spec", "devices", "requests")
	if err != nil || !found || len(requests) == 0 {
		t.Fatalf("claim has no devices.requests (found=%v err=%v)", found, err)
	}
	req0, ok := requests[0].(map[string]any)
	if !ok {
		t.Fatalf("requests[0] is %T, want map", requests[0])
	}
	exactly, ok := req0["exactly"].(map[string]any)
	if !ok {
		t.Fatalf("requests[0].exactly is %T, want map", req0["exactly"])
	}
	count, ok := exactly["count"].(int64)
	if !ok {
		t.Fatalf("requests[0].exactly.count is %T, want int64", exactly["count"])
	}
	return count
}

// TestNCCLFabricEnvNameLocked pins the validator-pod (reading) end of the fabric
// env name. The orchestrator (forwarding) end in pkg/validator/v1 defines the
// same literal independently; a fat-finger in either redeclaration would silently
// no-op RoCE forwarding (the pod would never see the value and default to EFA).
// Both ends pin to this canonical string so a typo fails its own package's test.
func TestNCCLFabricEnvNameLocked(t *testing.T) {
	if ncclFabricEnv != "AICR_NCCL_FABRIC" {
		t.Errorf("ncclFabricEnv = %q, want AICR_NCCL_FABRIC (keep in sync with pkg/validator/v1)", ncclFabricEnv)
	}
}

// TestCreateOrUpdateFromTemplate_RoCEClaimIdempotent is the regression guard for
// the create-or-update fix: applying the RoCE ResourceClaimTemplate twice (as a
// reused, persistent validation namespace would) must not fail with
// AlreadyExists on the second apply, and the second apply must reflect the new
// templated device count rather than erroring out.
func TestCreateOrUpdateFromTemplate_RoCEClaimIdempotent(t *testing.T) {
	const ns = "aicr-validation"
	claimPath := filepath.Join("testdata", "roce", "eks", "roce-claim.yaml")

	fakeClient := newFakeDynamicClient()
	ctx := &validators.Context{Ctx: context.Background(), DynamicClient: fakeClient}

	// First apply: claim does not exist → plain create.
	if err := createOrUpdateFromTemplate(ctx, resourceClaimTemplateGVR, ns, claimPath,
		map[string]string{"NAMESPACE": ns, "ROCE_DEVICE_COUNT": "8"}, nil); err != nil {
		t.Fatalf("first apply (create) failed: %v", err)
	}

	// Second apply with a different count: claim already exists → must
	// create-or-update (Get + Update), NOT fail with AlreadyExists.
	if err := createOrUpdateFromTemplate(ctx, resourceClaimTemplateGVR, ns, claimPath,
		map[string]string{"NAMESPACE": ns, "ROCE_DEVICE_COUNT": "4"}, nil); err != nil {
		t.Fatalf("second apply (update) failed — create-or-update regressed to plain create: %v", err)
	}

	got, err := fakeClient.Resource(resourceClaimTemplateGVR).Namespace(ns).
		Get(context.Background(), ncclRoceClaimName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("claim not found after idempotent re-apply: %v", err)
	}
	if c := roceClaimCount(t, got); c != 4 {
		t.Errorf("device count = %d after second apply, want 4 (update did not take effect)", c)
	}
}

// TestCleanupNCCLResources_ToleratesMissing verifies the deferred cleanup is
// safe to run after an early/partial-apply failure: with no namespace ever
// created, deleting it must be treated as success (NotFound-tolerant), not
// an error.
func TestCleanupNCCLResources_ToleratesMissing(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset()
	if err := cleanupNCCLResources(fakeClient, ns, ""); err != nil {
		t.Fatalf("cleanup of a namespace that was never created should not error, got: %v", err)
	}
}

// TestCleanupNCCLResources_DeletesNamespace verifies the happy path: the
// per-run namespace this run created is deleted, cascading away everything
// created in it (TrainJob, TrainingRuntime, ComputeDomain, RoCE claim) via
// ordinary Kubernetes namespace garbage collection, with no per-resource
// tracking required since nothing else ever shares this namespace.
func TestCleanupNCCLResources_DeletesNamespace(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})

	if err := cleanupNCCLResources(fakeClient, ns, ""); err != nil {
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
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	fakeClient.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	err := cleanupNCCLResources(fakeClient, ns, "")
	if err == nil {
		t.Fatal("expected an error from a non-NotFound namespace delete failure, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeInternal, "")) {
		t.Errorf("got %v, want an ErrCodeInternal-wrapped delete failure", err)
	}
}

// TestCleanupNCCLResources_WaitsForFinalizerHeldNamespace is the regression
// guard for the wait-for-termination fix: a namespace whose first delete
// only stamps a DeletionTimestamp (finalizers still cascading, as the
// ComputeDomain/ResourceClaimTemplate CRs this namespace can hold commonly
// do) must not be reported as cleaned up until it actually disappears.
// Before this fix, cleanupNCCLResources returned nil as soon as the first
// Delete call was accepted, even though the namespace, and everything
// still finalizing inside it, was still there.
func TestCleanupNCCLResources_WaitsForFinalizerHeldNamespace(t *testing.T) {
	const ns = "aicr-nccl-perf-deadbeef"
	const holdFinalizer = 200 * time.Millisecond
	fakeClient := fake.NewClientset(&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	nsGVR := v1.SchemeGroupVersion.WithResource("namespaces")

	var deleteCalls int32
	fakeClient.PrependReactor("delete", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.AddInt32(&deleteCalls, 1) == 1 {
			// First delete: simulate a still-cascading finalizer by stamping
			// DeletionTimestamp via the tracker directly instead of actually
			// removing the object, then tell the caller the delete request
			// was accepted (handled=true, err=nil), matching what a real
			// apiserver does for a namespace with finalizers.
			now := metav1.Now()
			held := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name:              ns,
				Finalizers:        []string{"kubernetes"},
				DeletionTimestamp: &now,
			}}
			if err := fakeClient.Tracker().Update(nsGVR, held, ""); err != nil {
				return true, nil, err
			}
			return true, nil, nil
		}
		// Second delete (fired by the goroutine below once the "finalizer"
		// clears): let the default reactor perform the real delete, which
		// also emits the watch.Deleted event waitForNamespaceGone is
		// blocked on.
		return false, nil, nil
	})

	go func() {
		time.Sleep(holdFinalizer)
		_ = fakeClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	}()

	start := time.Now()
	if err := cleanupNCCLResources(fakeClient, ns, ""); err != nil {
		t.Fatalf("cleanup should succeed once the finalizer clears, got: %v", err)
	}
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&deleteCalls); got < 2 {
		t.Fatalf("expected cleanup to observe the namespace still present and wait for a second delete, got %d delete call(s)", got)
	}
	if elapsed < holdFinalizer {
		t.Errorf("cleanup returned after %v, want it to have blocked at least %v for the finalizer to clear (it returned before the namespace actually disappeared)", elapsed, holdFinalizer)
	}
}

// TestWaitForNamespaceGone_TimesOutWhenNeverDeleted is the regression guard
// for the "wait boundedly, then fail" half of the fix: if a namespace's
// finalizers never clear within the caller's deadline, the wait must return
// a timeout error rather than either hang indefinitely or (the pre-fix
// cleanupNCCLResources behavior this mirrors) silently report success while
// the namespace is still there. Calls waitForNamespaceGone directly with a
// short local context so the test doesn't have to wait out
// cleanupNCCLResources's real 5-minute production timeout.
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
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeTimeout, "")) {
		t.Errorf("got %v, want an ErrCodeTimeout-wrapped wait failure", err)
	}
}
