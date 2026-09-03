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
	"testing"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// trainerListKinds covers every GVR the trainer lifecycle code Lists or Watches,
// so the dynamic fake client can serve them without a real REST mapper.
var trainerListKinds = map[schema.GroupVersionResource]string{
	trainerCRDGVR:               "CustomResourceDefinitionList",
	trainerDeploymentGVR:        "DeploymentList",
	trainerServiceGVR:           "ServiceList",
	trainerValidatingWebhookGVR: "ValidatingWebhookConfigurationList",
	trainerMutatingWebhookGVR:   "MutatingWebhookConfigurationList",
	{Group: "", Version: "v1", Resource: "configmaps"}:     "ConfigMapList",
	{Group: "", Version: "v1", Resource: "secrets"}:        "SecretList",
	{Group: "apps", Version: "v1", Resource: "daemonsets"}: "DaemonSetList",
}

func newTrainerFakeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), trainerListKinds, objs...)
}

// newTrainerTestMapper builds a REST mapper covering the kinds the rollback
// tests apply, so applyTrainerResources can resolve GVK -> GVR without live
// discovery.
func newTrainerTestMapper() apimeta.RESTMapper {
	m := apimeta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}, apimeta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}, apimeta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}, apimeta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{
		Group: apiGroupAPIExtensions, Version: "v1", Kind: "CustomResourceDefinition",
	}, apimeta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{
		Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration",
	}, apimeta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{
		Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfiguration",
	}, apimeta.RESTScopeRoot)
	return m
}

// errBoom is the injected failure used by the reactors below.
var errBoom = stderrors.New("boom")

// establishedCRD builds a CRD object whose Established condition is True, as the
// installed-state probe and the post-install wait both require.
func establishedCRD(name string) *unstructured.Unstructured {
	obj := newTestObject(apiGroupAPIExtensions+"/v1", "CustomResourceDefinition", "", name)
	if err := unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Established", "status": "True"},
	}, "status", "conditions"); err != nil {
		panic(err)
	}
	return obj
}

// readyTrainerDeployment builds the Trainer controller-manager Deployment with a
// ready replica, satisfying waitForTrainerControllerReady.
func readyTrainerDeployment() *unstructured.Unstructured {
	obj := newTestObject("apps/v1", "Deployment", trainerNamespace, trainerControllerDeployment)
	if err := unstructured.SetNestedField(obj.Object, int64(1), "status", "readyReplicas"); err != nil {
		panic(err)
	}
	return obj
}

// notReadyTrainerDeployment builds the Trainer controller-manager Deployment with
// no ready replica, as during a rollout or an ImagePullBackOff.
func notReadyTrainerDeployment() *unstructured.Unstructured {
	return trainerDeploymentNamed(trainerNamespace, trainerControllerDeployment, 0)
}

// readyTrainerDeploymentNamed builds a controller Deployment reporting one ready
// replica under an arbitrary name, covering an externally managed chart
// installation with a custom name.
func readyTrainerDeploymentNamed(namespace, name string) *unstructured.Unstructured {
	return trainerDeploymentNamed(namespace, name, 1)
}

// trainerDeploymentNamed builds a controller Deployment carrying the labels both
// deployment paths set, which is how the probe locates it.
func trainerDeploymentNamed(namespace, name string, readyReplicas int64) *unstructured.Unstructured {
	obj := newTestObject("apps/v1", "Deployment", namespace, name)
	obj.SetLabels(map[string]string{
		trainerComponentLabel: trainerComponentValue,
		trainerPartOfLabel:    trainerPartOfValue,
	})
	if err := unstructured.SetNestedField(obj.Object, readyReplicas, "status", "readyReplicas"); err != nil {
		panic(err)
	}
	return obj
}

func newTestObject(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
	}}
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	return obj
}

// trainerConfigMapName is the ConfigMap the rollback tests apply first, so it is
// the resource whose presence proves whether rollback ran.
const trainerConfigMapName = "kubeflow-trainer-config"

// configMapExists reports whether the applied ConfigMap is still present.
func configMapExists(t *testing.T, client *dynamicfake.FakeDynamicClient) bool {
	t.Helper()
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	_, err := client.Resource(gvr).Namespace(trainerNamespace).
		Get(context.Background(), trainerConfigMapName, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if apierrors.IsNotFound(err) {
		return false
	}
	t.Fatalf("unexpected error getting ConfigMap %s/%s: %v", trainerNamespace, trainerConfigMapName, err)
	return false
}

// TestInstallTrainerResources_RollsBackOnApplyFailure is the core regression guard
// for issue #2123: when a later resource fails to apply, every resource this
// installation already created must be deleted before the error is returned.
func TestInstallTrainerResources_RollsBackOnApplyFailure(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
		newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert"),
	}

	refs, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected apply failure, got nil error")
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0 (rollback owns cleanup, caller must not re-delete)", len(refs))
	}
	if configMapExists(t, client) {
		t.Error("ConfigMap created before the failure was not rolled back")
	}
}

// TestApplyTrainerResources_PersistsEachCreateBeforeReturning checks that
// each created resource lands in the manifest immediately, not only once
// the whole install returns. A process killed right after this call must
// not lose track of what it already created.
func TestApplyTrainerResources_PersistsEachCreateBeforeReturning(t *testing.T) {
	dynamicClient := newTrainerFakeClient()
	dynamicClient.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})
	clientset := fake.NewClientset()

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
		newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert"),
	}

	created, err := applyTrainerResources(context.Background(), dynamicClient, clientset, newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected the Secret create to fail")
	}
	if len(created) != 1 {
		t.Fatalf("created = %d, want 1 (the ConfigMap, before the Secret failed)", len(created))
	}

	_, recorded, loadErr := loadTrainerInstallManifest(context.Background(), clientset)
	if loadErr != nil {
		t.Fatalf("loadTrainerInstallManifest() error = %v", loadErr)
	}
	if len(recorded) != 1 || recorded[0].Name != trainerConfigMapName {
		t.Fatalf("recorded manifest = %v, want the ConfigMap recorded before the call returned", recorded)
	}
}

// TestInstallTrainerResources_LeavesPreexistingResources verifies rollback deletes
// only what this installation created. A resource that already existed (and was
// therefore updated, not created) must survive the rollback.
func TestInstallTrainerResources_LeavesPreexistingResources(t *testing.T) {
	existing := newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName)
	client := newTrainerFakeClient(existing)
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
		newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert"),
	}

	if _, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs); err == nil {
		t.Fatal("expected apply failure, got nil error")
	}
	if !configMapExists(t, client) {
		t.Error("pre-existing ConfigMap was deleted; rollback must only remove resources it created")
	}
}

// TestInstallTrainerResources_UpdateFailureStopsInstall pins the fix for the
// warning-only update path: a failed update of an existing resource must abort
// the installation rather than continue against a half-configured Trainer.
func TestInstallTrainerResources_UpdateFailureStopsInstall(t *testing.T) {
	existing := newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert")
	client := newTrainerFakeClient(existing)
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
		newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert"),
	}

	_, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected update failure to abort installation, got nil error")
	}
	if configMapExists(t, client) {
		t.Error("ConfigMap created before the update failure was not rolled back")
	}
}

// TestInstallTrainerResources_RollsBackOnCRDEstablishTimeout covers the readiness
// half of the fix: resources apply cleanly but the CRDs never reach Established,
// so the installation must not leak them.
func TestInstallTrainerResources_RollsBackOnCRDEstablishTimeout(t *testing.T) {
	client := newTrainerFakeClient()
	// Fail the establishment watch outright rather than waiting out a deadline:
	// deterministic and timer-free, so the test cannot flake under load.
	client.PrependWatchReactor(resourceCRDs, func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
	}

	refs, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected CRD establishment timeout, got nil error")
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0", len(refs))
	}
	if configMapExists(t, client) {
		t.Error("applied ConfigMap was not rolled back after CRD establishment timeout")
	}
}

// TestInstallTrainerResources_RollsBackOnControllerReadyTimeout covers the case
// where the CRDs establish but the controller-manager never becomes ready.
func TestInstallTrainerResources_RollsBackOnControllerReadyTimeout(t *testing.T) {
	client := newTrainerFakeClient(establishedCRD(trainerCRDTrainJobs),
		establishedCRD(trainerCRDTrainingRuntimes), establishedCRD(jobSetCRDName))
	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
	}

	// Let the readiness poll observe the absent controller once, then cancel from
	// the reactor: deterministic, and not coupled to the poll interval.
	ctx, cancel := context.WithCancel(context.Background())
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})
	defer cancel()

	refs, err := installTrainerResources(ctx, client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected controller readiness timeout, got nil error")
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0", len(refs))
	}
	if configMapExists(t, client) {
		t.Error("applied ConfigMap was not rolled back after controller readiness timeout")
	}
}

// TestInstallTrainerResources_SucceedsWhenReady is the positive control: with the
// CRDs established and the controller ready, the created resources are returned
// to the caller for later cleanup rather than rolled back.
func TestInstallTrainerResources_SucceedsWhenReady(t *testing.T) {
	client := newTrainerFakeClient(
		establishedCRD(trainerCRDTrainJobs),
		establishedCRD(trainerCRDTrainingRuntimes),
		establishedCRD(jobSetCRDName),
		readyTrainerDeployment(),
	)
	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
	}

	refs, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %d, want 1", len(refs))
	}
	if !configMapExists(t, client) {
		t.Error("successful install must leave its resources in place")
	}
}

// TestInstallTrainerResources_CleanupFailureNamesResource pins the requirement
// that a rollback which cannot delete a resource surfaces that resource's
// identity, so an operator can find the leak.
func TestInstallTrainerResources_CleanupFailureNamesResource(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
		newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert"),
	}

	_, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), trainerConfigMapName) {
		t.Errorf("error does not name the leaked resource: %v", err)
	}
}

// TestInstallTrainerResources_StopsApplyingOnCanceledContext verifies the apply
// loop honors cancellation. Without the check it would keep issuing Create calls
// for every remaining object, relying on each API call to fail on its own.
func TestInstallTrainerResources_StopsApplyingOnCanceledContext(t *testing.T) {
	client := newTrainerFakeClient()
	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
		newTestObject("v1", "Secret", trainerNamespace, "kubeflow-trainer-webhook-cert"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	refs, err := installTrainerResources(ctx, client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected canceled context to abort the install, got nil error")
	}
	if len(refs) != 0 {
		t.Errorf("refs = %d, want 0", len(refs))
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			t.Errorf("applied %q after context cancellation; the loop must abort first",
				action.GetResource().Resource)
		}
	}
}

// TestDeleteTrainer_RetriesTransientFailures verifies a momentary control-plane
// blip during teardown does not fail the check. Because a cleanup failure now
// fails an otherwise-passing benchmark, an unretried 503 would turn a good NCCL
// measurement into a false red on nightly UAT.
func TestDeleteTrainer_RetriesTransientFailures(t *testing.T) {
	client := newTrainerFakeClient()
	var attempts int
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts == 1 {
			return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
		}
		return true, nil, nil
	})

	refs := []trainerResourceRef{{
		GVR:       schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Namespace: trainerNamespace, Name: trainerConfigMapName,
	}}

	if err := deleteTrainer(client, refs); err != nil {
		t.Fatalf("transient delete failure should have been retried, got: %v", err)
	}
	if attempts != 2 {
		t.Errorf("delete attempts = %d, want 2 (one transient failure, one success)", attempts)
	}
}

// TestDeleteTrainer_PersistentTransientFailureIsUnavailable verifies a teardown
// that keeps failing transiently reports a retryable code, so triage can tell a
// cluster outage from a product defect.
func TestDeleteTrainer_PersistentTransientFailureIsUnavailable(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	refs := []trainerResourceRef{{
		GVR:       schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Namespace: trainerNamespace, Name: trainerConfigMapName,
	}}

	err := deleteTrainer(client, refs)
	if err == nil {
		t.Fatal("expected persistent transient failure to surface, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeUnavailable, "")) {
		t.Errorf("error code is not Unavailable: %v", err)
	}
}

// TestDeleteTrainer_DeterministicFailureIsInternal verifies a deterministic
// failure is neither retried nor laundered into a retryable-looking outage.
func TestDeleteTrainer_DeterministicFailureIsInternal(t *testing.T) {
	client := newTrainerFakeClient()
	var attempts int
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "configmaps"}, trainerConfigMapName, errBoom)
	})

	refs := []trainerResourceRef{{
		GVR:       schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Namespace: trainerNamespace, Name: trainerConfigMapName,
	}}

	err := deleteTrainer(client, refs)
	if err == nil {
		t.Fatal("expected forbidden delete to surface, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeInternal, "")) {
		t.Errorf("error code is not Internal: %v", err)
	}
	if attempts != 1 {
		t.Errorf("delete attempts = %d, want 1 (deterministic failures must not retry)", attempts)
	}
}

// TestApplyTrainerResources_ClassifiesTransientCreateFailure verifies the write
// path classifies like the read path: a transient apiserver failure during Create
// must not be reported as a product defect.
func TestApplyTrainerResources_ClassifiesTransientCreateFailure(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver is down")
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
	}

	_, err := installTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected create failure, got nil")
	}
	if !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeUnavailable, "")) {
		t.Errorf("error code is not Unavailable: %v", err)
	}
}

// TestDeleteTrainer_LeavesReplacedObjectAlone verifies cleanup never deletes a
// same-named object that another owner recreated while the benchmark ran. The UID
// precondition is what makes "never delete what we didn't create" hold over time,
// not just at apply time.
func TestDeleteTrainer_LeavesReplacedObjectAlone(t *testing.T) {
	client := newTrainerFakeClient()
	var sawPrecondition bool
	client.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(k8stesting.DeleteActionImpl)
		if ok && del.DeleteOptions.Preconditions != nil && del.DeleteOptions.Preconditions.UID != nil {
			sawPrecondition = true
		}
		// The apiserver rejects a UID precondition that no longer matches.
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Resource: "configmaps"}, trainerConfigMapName, errBoom)
	})

	refs := []trainerResourceRef{{
		GVR:       schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Namespace: trainerNamespace, Name: trainerConfigMapName, UID: "original-uid",
	}}

	if err := deleteTrainer(client, refs); err != nil {
		t.Errorf("a replaced object is not ours to delete and must not fail cleanup, got: %v", err)
	}
	if !sawPrecondition {
		t.Error("delete was issued without a UID precondition")
	}
}

// TestApplyTrainerResources_ClaimsAmbiguousCreate verifies a Create that fails
// inconclusively but persisted is still claimed for rollback. Without this the
// object is absent from the rollback set and leaks, which is the exact failure
// this change exists to close.
func TestApplyTrainerResources_ClaimsAmbiguousCreate(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		// Persist the object exactly as submitted (carrying this attempt's marker),
		// then report a transport failure, as an apiserver timeout after a
		// successful write does.
		if err := client.Tracker().Create(action.GetResource(), create.Object, action.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewTimeoutError("request timed out", 1)
	})

	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
	}

	created, err := applyTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected the ambiguous create to surface as an error")
	}
	if len(created) != 1 {
		t.Fatalf("created = %d, want 1 (a persisted object must be claimed for rollback)", len(created))
	}
	if created[0].Name != trainerConfigMapName {
		t.Errorf("claimed %q, want %q", created[0].Name, trainerConfigMapName)
	}
}

// TestApplyTrainerResources_DoesNotClaimForeignObjects is the ownership half of
// the ambiguous-create recovery. Presence is not ownership: an object that this
// attempt did not create must never enter the rollback set, or cleanup deletes a
// resource the validator never installed.
func TestApplyTrainerResources_DoesNotClaimForeignObjects(t *testing.T) {
	tests := []struct {
		name      string
		preexist  map[string]string // annotations on the object already in the cluster
		createErr error
	}{
		{
			name:      "object with no attempt marker",
			preexist:  nil,
			createErr: apierrors.NewTimeoutError("request timed out", 1),
		},
		{
			name:      "object marked by a different attempt",
			preexist:  map[string]string{installAttemptAnnotation: "some-other-attempt"},
			createErr: apierrors.NewTimeoutError("request timed out", 1),
		},
		{
			name:      "deterministic rejection is never probed",
			preexist:  nil,
			createErr: apierrors.NewTooManyRequests("slow down", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName)
			if tt.preexist != nil {
				existing.SetAnnotations(tt.preexist)
			}
			client := newTrainerFakeClient(existing)
			client.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.createErr
			})

			objs := []*unstructured.Unstructured{
				newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
			}

			created, err := applyTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
			if err == nil {
				t.Fatal("expected the create failure to surface")
			}
			if len(created) != 0 {
				t.Errorf("created = %d, want 0 (a foreign object must not be claimed for rollback)", len(created))
			}
		})
	}
}

// TestApplyTrainerResources_StampsCreatedObjects verifies created resources carry
// the attempt marker, which is what lets an ambiguous retry prove ownership.
func TestApplyTrainerResources_StampsCreatedObjects(t *testing.T) {
	client := newTrainerFakeClient()
	objs := []*unstructured.Unstructured{
		newTestObject("v1", "ConfigMap", trainerNamespace, trainerConfigMapName),
	}

	if _, err := applyTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := client.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}).
		Namespace(trainerNamespace).Get(context.Background(), trainerConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.GetAnnotations()[installAttemptAnnotation] == "" {
		t.Error("created resource carries no install-attempt marker")
	}
}

// TestApplyTrainerResources_RefusesToRepointForeignInstall verifies the installer
// will not steal a Trainer installation deployed another way. The Helm chart the
// recipes deploy lands in kubeflow; repointing its admission configuration at this
// installer's namespace would break its admission path, and since updates are
// excluded from the rollback set nothing would put it back.
func TestApplyTrainerResources_RefusesToRepointForeignInstall(t *testing.T) {
	chartInstalled := webhookConfigIn("ValidatingWebhookConfiguration",
		trainerValidatingWebhookConfig, trainerValidatingWebhookName, "kubeflow")
	client := newTrainerFakeClient(chartInstalled)

	objs := []*unstructured.Unstructured{
		webhookConfigIn("ValidatingWebhookConfiguration", trainerValidatingWebhookConfig,
			trainerValidatingWebhookName, trainerNamespace),
	}

	_, err := applyTrainerResources(context.Background(), client, fake.NewClientset(), newTrainerTestMapper(), objs)
	if err == nil {
		t.Fatal("expected the installer to refuse repointing another installation's webhooks")
	}

	got, getErr := client.Resource(trainerValidatingWebhookGVR).
		Get(context.Background(), trainerValidatingWebhookConfig, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("unexpected error: %v", getErr)
	}
	if ns := webhookServiceNamespace(got); ns != "kubeflow" {
		t.Errorf("live installation was repointed to %q, want it left at kubeflow", ns)
	}
}

// TestDeleteTrainer_ToleratesNotFound verifies cleanup treats an already-deleted
// resource as success, so a partially garbage-collected install still reports clean.
func TestDeleteTrainer_ToleratesNotFound(t *testing.T) {
	client := newTrainerFakeClient()
	refs := []trainerResourceRef{
		{GVR: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
			Namespace: trainerNamespace, Name: "gone"},
	}
	if err := deleteTrainer(client, refs); err != nil {
		t.Errorf("deleteTrainer on missing resource = %v, want nil", err)
	}
}

// TestDeleteTrainer_AggregatesFailures verifies every failed delete is reported,
// not just the first, and that each carries its resource identity.
func TestDeleteTrainer_AggregatesFailures(t *testing.T) {
	client := newTrainerFakeClient()
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errBoom)
	})

	cmGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	refs := []trainerResourceRef{
		{GVR: cmGVR, Namespace: trainerNamespace, Name: "first"},
		{GVR: cmGVR, Namespace: trainerNamespace, Name: "second"},
	}

	err := deleteTrainer(client, refs)
	if err == nil {
		t.Fatal("expected aggregated delete error, got nil")
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing resource %q: %v", want, err)
		}
	}
}
