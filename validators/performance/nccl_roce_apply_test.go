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
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/validators"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// ncclGVRListKinds maps every GVR cleanupNCCLResources / applyNCCLResources
// touch to a fake list kind, so the dynamic fake client can serve Create/Get/
// Update/Delete for these CRDs without a real REST mapper.
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
	req0, ok := requests[0].(map[string]interface{})
	if !ok {
		t.Fatalf("requests[0] is %T, want map", requests[0])
	}
	exactly, ok := req0["exactly"].(map[string]interface{})
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
// safe to run after an early/partial-apply failure: with no resources present,
// every Delete hits NotFound and the function must complete without panicking.
func TestCleanupNCCLResources_ToleratesMissing(t *testing.T) {
	const ns = "aicr-validation"
	// No objects seeded — every Delete returns NotFound.
	fakeClient := newFakeDynamicClient()
	cleanupNCCLResources(fakeClient, ns)

	// Cleanup must tolerate the absence, not resurrect anything.
	_, err := fakeClient.Resource(resourceClaimTemplateGVR).Namespace(ns).
		Get(context.Background(), ncclRoceClaimName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("claim should remain absent after cleanup of empty namespace, got err=%v", err)
	}
}

// TestCleanupNCCLResources_DeletesRoCEClaim verifies the happy path: a RoCE
// claim left in the persistent namespace is deleted by cleanup, so the next run
// does not collide with it.
func TestCleanupNCCLResources_DeletesRoCEClaim(t *testing.T) {
	const ns = "aicr-validation"

	claim := &unstructured.Unstructured{}
	claim.SetAPIVersion("resource.k8s.io/v1")
	claim.SetKind("ResourceClaimTemplate")
	claim.SetName(ncclRoceClaimName)
	claim.SetNamespace(ns)

	fakeClient := newFakeDynamicClient(claim)

	// Sanity: the claim exists before cleanup.
	if _, err := fakeClient.Resource(resourceClaimTemplateGVR).Namespace(ns).
		Get(context.Background(), ncclRoceClaimName, metav1.GetOptions{}); err != nil {
		t.Fatalf("precondition: claim should exist before cleanup: %v", err)
	}

	cleanupNCCLResources(fakeClient, ns)

	_, err := fakeClient.Resource(resourceClaimTemplateGVR).Namespace(ns).
		Get(context.Background(), ncclRoceClaimName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("claim should be deleted after cleanup, got err=%v", err)
	}
}
