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
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func creDynamicListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		certificationGVR: "CertificationList",
		workflowGVR:      "WorkflowList",
		creJobGVR:        "JobList",
		trainJobGVR:      "TrainJobList",
	}
}

// creTestNamespace is the namespace the teardown fixtures share.
const creTestNamespace = "nvcre"

// creLabeled builds a CRE-managed object carrying the label that ties it to its
// parent, which is how teardown rediscovers the chain.
func creLabeled(apiVersion, kind, name, labelKey, labelValue string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: apiVersion,
			keyKind:       kind,
			keyMetadata: map[string]any{
				keyName:      name,
				keyNamespace: creTestNamespace,
				"labels":     map[string]any{labelKey: labelValue},
			},
		},
	}
}

// TestReclaimStaleCRECertifications covers the sweep that makes an
// interrupted run's Certification recoverable. The age bound is the whole
// point: without it the sweep would delete a concurrent validator's live run,
// so a young Certification must survive even though it carries the label.
func TestReclaimStaleCRECertifications(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	aged := func(name string, age time.Duration) *unstructured.Unstructured {
		obj := creLabeled(creAPI, "Certification", name, creLabelManagedBy, creManagedByAICR)
		obj.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-age)))
		return obj
	}
	unlabelled := creLabeled(creAPI, "Certification", "someone-elses", "example.com/owner", "other")
	unlabelled.SetCreationTimestamp(metav1.NewTime(time.Now().Add(-24 * time.Hour)))

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		aged("orphan", defaults.CREOrphanReclaimAge+time.Minute),
		aged("live-run", time.Minute),
		unlabelled,
	)

	if err := reclaimStaleCRECertifications(
		context.Background(), client, k8sfake.NewSimpleClientset(), ns); err != nil {
		t.Fatalf("reclaimStaleCRECertifications() error = %v", err)
	}

	for name, wantGone := range map[string]bool{
		"orphan":        true,
		"live-run":      false,
		"someone-elses": false,
	} {
		_, err := client.Resource(certificationGVR).Namespace(ns).Get(
			context.Background(), name, metav1.GetOptions{})
		if gone := apierrors.IsNotFound(err); gone != wantGone {
			t.Errorf("Certification %q gone = %v, want %v (err = %v)", name, gone, wantGone, err)
		}
	}
}

// TestBuildCRECertificationIsDiscoverable pins the label the sweep selects on.
// Certification names carry a nonce, so this stamp is the only thing that
// makes one findable after the run that created it is gone.
func TestBuildCRECertificationIsDiscoverable(t *testing.T) {
	obj, err := buildCRECertification(creTestNamespace, "aicr-cre-nccl-abcd1234", twoNodeGPUConfig(),
		creCatalogEntry{Domain: creNCCLDomain, Variant: creNCCLVariant, MaxNodes: 2})
	if err != nil {
		t.Fatalf("buildCRECertification() error = %v", err)
	}
	if got := obj.GetLabels()[creLabelManagedBy]; got != creManagedByAICR {
		t.Errorf("label %s = %q, want %q", creLabelManagedBy, got, creManagedByAICR)
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

// buildTestCertification pins a catalog entry so the driver tests stay about
// the certification's shape and the lifecycle around it. Production callers
// resolve the entry from recipe criteria instead.
func buildTestCertification(t *testing.T, namespace, name string, gpuConfig *gpuConfiguration) *unstructured.Unstructured {
	t.Helper()
	obj, err := buildCRECertification(namespace, name, gpuConfig, testCatalogEntry)
	if err != nil {
		t.Fatalf("buildCRECertification: %v", err)
	}
	return obj
}

var testCatalogEntry = creCatalogEntry{Domain: "nccl", Variant: "all-reduce", MaxNodes: 2}

// An untargeted Certification runs against every node CRE matches, so an empty
// node list has to stop the submission rather than produce a spec without
// target.nodeNames.
func TestBuildCRECertificationRefusesEmptyNodeList(t *testing.T) {
	obj, err := buildCRECertification("nvcre", "aicr-cre-empty", &gpuConfiguration{
		WorkerCount:     2,
		GPUCountPerNode: 8,
	}, testCatalogEntry)
	if err == nil {
		t.Fatalf("buildCRECertification with no nodes = %v, want error", obj)
	}
	if obj != nil {
		t.Errorf("object = %v, want nil alongside the error", obj)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
	}
}

// Every submitted Certification carries the node list, since that is the only
// field bounding the run's total footprint.
func TestBuildCRECertificationAlwaysTargetsNodes(t *testing.T) {
	obj := buildTestCertification(t, "nvcre", "aicr-cre-target", twoNodeGPUConfig())
	names, found, err := unstructured.NestedStringSlice(obj.Object, keySpec, "target", "nodeNames")
	if err != nil || !found {
		t.Fatalf("spec.target.nodeNames: found=%v err=%v", found, err)
	}
	if len(names) != testCatalogEntry.MaxNodes {
		t.Errorf("nodeNames = %v, want %d entries", names, testCatalogEntry.MaxNodes)
	}
	perJob, found, err := unstructured.NestedInt64(obj.Object, keySpec, "nodesPerJob")
	if err != nil || !found {
		t.Fatalf("spec.nodesPerJob: found=%v err=%v", found, err)
	}
	if perJob != int64(len(names)) {
		t.Errorf("nodesPerJob = %d, want %d to match nodeNames", perJob, len(names))
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
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-watch", twoNodeGPUConfig())
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
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-done", twoNodeGPUConfig())
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

// TestWaitForCRETerminalKeepsPartialStatusOnTimeout covers #2688's criterion
// that an expired run still produces the partial report. Teardown deletes the
// Certification as soon as the wait returns, so a timeout that discarded the
// object would leave the caller with nothing to report but the timeout itself,
// even when CRE had already finished several categories.
func TestWaitForCRETerminalKeepsPartialStatusOnTimeout(t *testing.T) {
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-slow", twoNodeGPUConfig())
	obj.Object["status"] = map[string]any{
		"categoryStatuses": []any{
			map[string]any{"domain": "communication", "status": "Succeeded"},
		},
	}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(), obj)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	got, err := waitForCertificationTerminal(ctx, client, "ns", obj.GetName())
	if err == nil {
		t.Fatal("waitForCertificationTerminal() = nil error, want a timeout")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error = %v, want ErrCodeTimeout", err)
	}
	if got == nil {
		t.Fatal("timed-out wait returned no object, so the partial report is lost")
	}
	status, _, _ := unstructured.NestedSlice(got.Object, "status", "categoryStatuses")
	if len(status) != 1 {
		t.Errorf("categoryStatuses = %v, want the partial report CRE had completed", status)
	}
}

// TestCREErrorWithPartialReport covers the consuming half of #2688's criterion
// that an expired run produces a failure carrying the partial report. The
// Certification is deleted by teardown moments after the wait returns, so an
// error that did not absorb the per-category progress loses it for good.
func TestCREErrorWithPartialReport(t *testing.T) {
	withCategories := func(statuses ...map[string]any) *unstructured.Unstructured {
		raw := make([]any, 0, len(statuses))
		for _, s := range statuses {
			raw = append(raw, s)
		}
		return &unstructured.Unstructured{
			Object: map[string]any{"status": map[string]any{"categoryStatuses": raw}},
		}
	}
	timeout := errors.New(errors.ErrCodeTimeout, "timed out waiting for Certification")

	tests := []struct {
		name    string
		err     error
		obj     *unstructured.Unstructured
		wantMsg string
	}{
		{
			name: "partial progress is reported with the timeout",
			err:  timeout,
			obj: withCategories(
				map[string]any{"domain": "communication", "variant": "nccl-all-reduce", "status": "Succeeded"},
				map[string]any{"domain": "training", "variant": "nemotron5-8b", "status": "InProgress"},
			),
			wantMsg: "communication/nccl-all-reduce=Succeeded, training/nemotron5-8b=InProgress",
		},
		{
			// Nothing to add, so the error must pass through untouched rather
			// than gain an empty "completed so far:" clause.
			name:    "no categories leaves the error alone",
			err:     timeout,
			obj:     withCategories(),
			wantMsg: "timed out waiting for Certification",
		},
		{
			name:    "a run that never reported leaves the error alone",
			err:     timeout,
			obj:     nil,
			wantMsg: "timed out waiting for Certification",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := creErrorWithPartialReport(tt.err, tt.obj)
			if got == nil {
				t.Fatal("error = nil, want the wait's error preserved")
			}
			// The code must survive: ExitCodeFromError reads the outermost one.
			if errorstest.ReportedCode(got) != errors.ErrCodeTimeout {
				t.Errorf("reported code = %v, want ErrCodeTimeout", errorstest.ReportedCode(got))
			}
			if !strings.Contains(got.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", got.Error(), tt.wantMsg)
			}
		})
	}

	t.Run("a clean run stays nil", func(t *testing.T) {
		if got := creErrorWithPartialReport(nil, withCategories(
			map[string]any{"domain": "training", "variant": "nemotron5-8b", "status": "Succeeded"},
		)); got != nil {
			t.Errorf("error = %v, want nil", got)
		}
	})
}

func TestDeleteCREResourceWaitsUntilGone(t *testing.T) {
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-gone", twoNodeGPUConfig())
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
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-stuck", twoNodeGPUConfig())
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

// TestTeardownCRECertificationNamesWorkloadsWhenDeleteHangs covers the
// failure that is not the confirmation loop's: a finalizer that is never
// released holds the Certification past the budget, so teardown never reaches
// the point where it learns which workloads survived. The seed already
// recorded them, and without those names the operator is told only that a
// deletion timed out — with nothing to go delete before the GPUs are free.
func TestTeardownCRECertificationNamesWorkloadsWhenDeleteHangs(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	cert := creLabeled(creAPI, "Certification", "cert-stuck", creLabelCertification, "cert-stuck")
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		cert,
		creLabeled(creAPI, "Workflow", "wf-stuck", creLabelCertification, "cert-stuck"),
		creLabeled(creAPI, "Job", "job-stuck", creLabelWorkflow, "wf-stuck"),
	)

	held := func(k8stesting.Action) (bool, runtime.Object, error) {
		stuck := cert.DeepCopy()
		now := metav1.Now()
		stuck.SetDeletionTimestamp(&now)
		stuck.SetFinalizers([]string{"nvcre.nvidia.com/certification-finalizer"})
		return true, stuck, nil
	}
	client.PrependReactor("delete", certificationGVR.Resource, held)
	client.PrependReactor("get", certificationGVR.Resource, held)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := teardownCRECertification(ctx, client, k8sfake.NewSimpleClientset(), ns, "cert-stuck")
	if err == nil {
		t.Fatal("teardownCRECertification() = nil while the Certification was still held")
	}
	// The cause's code must survive the wrap: ExitCodeFromError reads the
	// outermost one.
	if errorstest.ReportedCode(err) != errors.ErrCodeTimeout {
		t.Errorf("reported code = %v, want ErrCodeTimeout", errorstest.ReportedCode(err))
	}
	for _, want := range []string{"cert-stuck", "job-stuck"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}
}

// TestDeleteCREResourceUsesForegroundPropagation pins the propagation policy.
// Foreground is necessary but not sufficient: CRE's Certification controller
// drops its finalizer without waiting for the child Workflows, so teardown
// still has to observe the workloads directly (waitForCREWorkloadsGone).
// Background propagation would be strictly worse, reaping dependents only
// after the parent is already gone.
func TestDeleteCREResourceUsesForegroundPropagation(t *testing.T) {
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-policy", twoNodeGPUConfig())
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

// creDrainGracePeriod mirrors podDrainGracePeriod in CRE's
// pkg/controller/pod_drain.go. CRE waits this long for workload pods to exit
// and then proceeds with cleanup regardless, so a teardown budget at or below
// it would time out while CRE is still legitimately draining.
const creDrainGracePeriod = 5 * time.Minute

func TestCRECertificationTeardownTimeoutOutlastsCREDrainGrace(t *testing.T) {
	if defaults.CRECertificationTeardownTimeout <= creDrainGracePeriod {
		t.Errorf("CRECertificationTeardownTimeout = %s, must exceed CRE's drain grace %s",
			defaults.CRECertificationTeardownTimeout, creDrainGracePeriod)
	}
	if defaults.CRECertificationTeardownTimeout < defaults.CRECertificationDeleteTimeout {
		t.Errorf("CRECertificationTeardownTimeout = %s, cannot be shorter than the delete phase it contains %s",
			defaults.CRECertificationTeardownTimeout, defaults.CRECertificationDeleteTimeout)
	}
	if defaults.CRECertificationTeardownTimeout >= defaults.CRECertificationTimeout {
		t.Errorf("CRECertificationTeardownTimeout = %s, must stay under CRECertificationTimeout %s",
			defaults.CRECertificationTeardownTimeout, defaults.CRECertificationTimeout)
	}
}

// creCascadeWorkflowDelete stands in for the garbage collector the fake
// dynamic client does not have. In a real cluster deleting a Certification
// removes the Workflows carrying its label, and teardown's shutdown barrier
// waits for exactly that, so a fixture without it never finishes.
//
// The removal goes through the object tracker rather than the client: the
// fake holds its own lock across the whole reactor chain, so a reactor that
// calls back into the client deadlocks.
func creCascadeWorkflowDelete(client *fake.FakeDynamicClient, namespace string, workflowNames ...string) {
	tracker := client.Tracker()
	client.PrependReactor("delete", certificationGVR.Resource,
		func(k8stesting.Action) (bool, runtime.Object, error) {
			for _, name := range workflowNames {
				_ = tracker.Delete(workflowGVR, namespace, name)
			}
			return false, nil, nil
		})
}

// TestTeardownCRECertificationConfirmsWorkloadsStopped enumerates the workload
// chain before the delete removes it, then confirms the workloads stopped. A
// pass here is the only outcome that lets a check report success.
func TestTeardownCRECertificationConfirmsWorkloadsStopped(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		creLabeled(creAPI, "Certification", "cert-ours", creLabelCertification, "cert-ours"),
		creLabeled(creAPI, "Workflow", "wf-ours", creLabelCertification, "cert-ours"),
		creLabeled(creAPI, "Job", "job-ours", creLabelWorkflow, "wf-ours"),
	)
	creCascadeWorkflowDelete(client, ns, "wf-ours")

	if err := teardownCRECertification(context.Background(), client, k8sfake.NewSimpleClientset(), ns, "cert-ours"); err != nil {
		t.Fatalf("teardownCRECertification() error = %v", err)
	}
	if _, err := client.Resource(certificationGVR).Namespace(ns).
		Get(context.Background(), "cert-ours", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Certification still present after teardown: %v", err)
	}
}

// A TrainJob that outlives its Certification keeps holding GPUs, so teardown
// has to report the leak rather than return the delete's success.
func TestTeardownCRECertificationFailsOnStrandedWorkload(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		creLabeled(creAPI, "Certification", "cert-ours", creLabelCertification, "cert-ours"),
		creLabeled(creAPI, "Workflow", "wf-ours", creLabelCertification, "cert-ours"),
		creLabeled(creAPI, "Job", "job-ours", creLabelWorkflow, "wf-ours"),
		creLabeled("trainer.kubeflow.org/v1alpha1", "TrainJob", "job-ours", creLabelJob, "job-ours"),
	)
	creCascadeWorkflowDelete(client, ns, "wf-ours")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := teardownCRECertification(ctx, client, k8sfake.NewSimpleClientset(), ns, "cert-ours")
	if err == nil {
		t.Fatal("teardownCRECertification() = nil, want a failure naming the stranded workload")
	}
	if !strings.Contains(err.Error(), "job-ours") {
		t.Errorf("error = %v, want it to name the stranded workload", err)
	}
}

// TestCREWorkloadTrackerObserveWalksLabelChain covers the reason the walk
// exists: CRE labels only Workflows with the Certification name, so the Job
// names that identify the TrainJobs and pods are reachable one hop at a time.
// Objects belonging to a different Certification must not be collected.
func TestCREWorkloadTrackerObserveWalksLabelChain(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		creLabeled(creAPI, "Workflow", "wf-ours", creLabelCertification, "cert-ours"),
		creLabeled(creAPI, "Workflow", "wf-theirs", creLabelCertification, "cert-theirs"),
		creLabeled(creAPI, "Job", "job-ours-b", creLabelWorkflow, "wf-ours"),
		creLabeled(creAPI, "Job", "job-ours-a", creLabelWorkflow, "wf-ours"),
		creLabeled(creAPI, "Job", "job-theirs", creLabelWorkflow, "wf-theirs"),
	)

	tracker := newCREWorkloadTracker()
	present, err := tracker.observe(context.Background(), client, ns, "cert-ours")
	if err != nil {
		t.Fatalf("observe() error = %v", err)
	}
	if strings.Join(present, ",") != "wf-ours" {
		t.Errorf("present workflows = %v, want only wf-ours", present)
	}
	want := []string{"job-ours-a", "job-ours-b"}
	if strings.Join(tracker.jobNames(), ",") != strings.Join(want, ",") {
		t.Errorf("job names = %v, want %v", tracker.jobNames(), want)
	}
}

// TestCREWorkloadTrackerKeepsNamesAfterGarbageCollection pins the accumulation
// rule. CRE removes a Job as soon as its Workflow goes, so a tracker that
// reported only what the latest pass found would forget the Job whose pods are
// the thing teardown is looking for.
func TestCREWorkloadTrackerKeepsNamesAfterGarbageCollection(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		creLabeled(creAPI, "Workflow", "wf-ours", creLabelCertification, "cert-ours"),
		creLabeled(creAPI, "Job", "job-ours", creLabelWorkflow, "wf-ours"),
	)

	tracker := newCREWorkloadTracker()
	if _, err := tracker.observe(context.Background(), client, ns, "cert-ours"); err != nil {
		t.Fatalf("first observe() error = %v", err)
	}

	for _, gvr := range []schema.GroupVersionResource{creJobGVR, workflowGVR} {
		name := "job-ours"
		if gvr == workflowGVR {
			name = "wf-ours"
		}
		if err := client.Resource(gvr).Namespace(ns).
			Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("deleting %s: %v", name, err)
		}
	}

	present, err := tracker.observe(context.Background(), client, ns, "cert-ours")
	if err != nil {
		t.Fatalf("second observe() error = %v", err)
	}
	if len(present) != 0 {
		t.Errorf("present workflows = %v, want none after deletion", present)
	}
	if strings.Join(tracker.jobNames(), ",") != "job-ours" {
		t.Errorf("job names = %v, want the collected Job to still be tracked", tracker.jobNames())
	}
}

// TestWaitForCREWorkloadsGoneWaitsOutASurvivingWorkflow is the shutdown
// barrier. A Workflow that outlives the Certification can still create another
// Job, so an empty workload list while one is present proves nothing: teardown
// must keep waiting and, if the budget runs out, name the Workflow rather than
// report a clean sweep over work that had not started yet.
func TestWaitForCREWorkloadsGoneWaitsOutASurvivingWorkflow(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		creLabeled(creAPI, "Workflow", "wf-ours", creLabelCertification, "cert-ours"),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := waitForCREWorkloadsGone(
		ctx, client, k8sfake.NewSimpleClientset(), ns, "cert-ours", newCREWorkloadTracker())
	if err == nil {
		t.Fatal("teardown reported success while a Workflow could still create work")
	}
	if !strings.Contains(err.Error(), "Workflow/wf-ours") {
		t.Errorf("error = %q, want it to name the surviving Workflow", err)
	}
}

// TestWaitForCREWorkloadsGoneCatchesAJobCreatedAfterTheSeed is the race the
// barrier exists for. CRE can create a Job between the seed snapshot and the
// moment its Workflow controller observes the deletion, including from a
// reconcile already in flight. A teardown that checked only the seeded names
// would return nil while that Job's pod kept holding GPUs.
func TestWaitForCREWorkloadsGoneCatchesAJobCreatedAfterTheSeed(t *testing.T) {
	const ns = creTestNamespace
	creAPI := creAPIGroup + "/" + versionV1alpha1

	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
		creLabeled(creAPI, "Workflow", "wf-ours", creLabelCertification, "cert-ours"),
	)
	pods := k8sfake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "late-worker-0",
			Namespace: ns,
			Labels:    map[string]string{creLabelJob: "job-late"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	// The seed saw no Jobs at all, which is what makes this the dangerous case.
	tracker := newCREWorkloadTracker()
	if _, err := tracker.observe(context.Background(), client, ns, "cert-ours"); err != nil {
		t.Fatalf("seed observe() error = %v", err)
	}
	if len(tracker.jobNames()) != 0 {
		t.Fatalf("seed recorded %v, want no Jobs", tracker.jobNames())
	}

	// The in-flight reconcile lands after the seed.
	if _, err := client.Resource(creJobGVR).Namespace(ns).Create(context.Background(),
		creLabeled(creAPI, "Job", "job-late", creLabelWorkflow, "wf-ours"),
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the late Job: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := waitForCREWorkloadsGone(ctx, client, pods, ns, "cert-ours", tracker)
	if err == nil {
		t.Fatal("teardown reported success while a pod from a late Job was running")
	}
	if !strings.Contains(err.Error(), "late-worker-0") {
		t.Errorf("error = %q, want it to name the stranded pod from the late Job", err)
	}
}

// TestCREActiveWorkloads pins what counts as still holding a GPU. A Succeeded
// or Failed pod has exited and holds no CUDA context, matching CRE's own drain
// barrier; a Running or Pending one does, and so does any surviving TrainJob.
func TestCREActiveWorkloads(t *testing.T) {
	const ns = creTestNamespace

	tests := []struct {
		name      string
		phase     corev1.PodPhase
		wantEmpty bool
	}{
		{"running pod blocks teardown", corev1.PodRunning, false},
		{"pending pod blocks teardown", corev1.PodPending, false},
		{"succeeded pod has exited", corev1.PodSucceeded, true},
		{"failed pod has exited", corev1.PodFailed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
			pods := k8sfake.NewSimpleClientset(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "worker-0",
					Namespace: ns,
					Labels:    map[string]string{creLabelJob: "job-a"},
				},
				Status: corev1.PodStatus{Phase: tt.phase},
			})

			active, err := creActiveWorkloads(context.Background(), dyn, pods, ns, []string{"job-a"})
			if err != nil {
				t.Fatalf("creActiveWorkloads() error = %v", err)
			}
			if gotEmpty := len(active) == 0; gotEmpty != tt.wantEmpty {
				t.Errorf("active = %v, wantEmpty %v", active, tt.wantEmpty)
			}
		})
	}

	t.Run("surviving TrainJob blocks teardown", func(t *testing.T) {
		dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds(),
			creLabeled("trainer.kubeflow.org/"+versionV1alpha1, "TrainJob", "tj-a", creLabelJob, "job-a"),
		)
		active, err := creActiveWorkloads(context.Background(), dyn, k8sfake.NewSimpleClientset(), ns, []string{"job-a"})
		if err != nil {
			t.Fatalf("creActiveWorkloads() error = %v", err)
		}
		if len(active) != 1 || !strings.Contains(active[0], "TrainJob/tj-a") {
			t.Errorf("active = %v, want the surviving TrainJob", active)
		}
	})
}

// TestWaitForCREWorkloadsGoneFailsClosedOnTimeout is the core of the guarantee:
// when the deadline expires with GPU work still running, teardown must return
// an error naming what survived rather than reporting a clean cleanup.
func TestWaitForCREWorkloadsGoneFailsClosedOnTimeout(t *testing.T) {
	const ns = creTestNamespace
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
	pods := k8sfake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: ns,
			Labels:    map[string]string{creLabelJob: "job-a"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := waitForCREWorkloadsGone(ctx, dyn, pods, ns, "cert-a", newCREWorkloadTracker("job-a"))
	if err == nil {
		t.Fatal("teardown reported success while a pod was still running")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout", err)
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Errorf("error = %q, want it to say workloads are still running", err)
	}
}

func TestWaitForCREWorkloadsGoneReturnsWhenClear(t *testing.T) {
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())

	// No Workflows and no Jobs means the Certification is fully torn down and
	// nothing remains that could start more work.
	if err := waitForCREWorkloadsGone(
		context.Background(), dyn, k8sfake.NewSimpleClientset(), "nvcre", "cert-a", newCREWorkloadTracker(),
	); err != nil {
		t.Errorf("waitForCREWorkloadsGone() with no jobs error = %v", err)
	}

	if err := waitForCREWorkloadsGone(
		context.Background(), dyn, k8sfake.NewSimpleClientset(), "nvcre", "cert-a", newCREWorkloadTracker("job-a"),
	); err != nil {
		t.Errorf("waitForCREWorkloadsGone() with drained job error = %v", err)
	}
}

// TestWaitForCREWorkloadsGoneFailsClosedWithoutClientset guards the nil path:
// without a pod client the teardown cannot see GPU pods at all, and must say so
// rather than pass vacuously.
func TestWaitForCREWorkloadsGoneFailsClosedWithoutClientset(t *testing.T) {
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
	err := waitForCREWorkloadsGone(
		context.Background(), dyn, nil, "nvcre", "cert-a", newCREWorkloadTracker("job-a"))
	if err == nil {
		t.Fatal("teardown passed without any way to observe pods")
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
		wantAlso  string
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
			// The primary error keeps the code, but the operator still has to
			// learn the GPUs may be held: a timed-out run that reported only
			// the timeout left the leak invisible.
			name:      "primary error keeps its code and carries the leak",
			primary:   primary,
			deleteErr: deleteErr,
			wantCode:  errors.New(errors.ErrCodeNotFound, ""),
			wantMsg:   "finalizer still held",
			wantAlso:  "no GoodputMeasurement",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := creCleanupFailure("cre-training-goodput", tt.primary, tt.deleteErr)
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
			if tt.wantAlso != "" && !strings.Contains(got.Error(), tt.wantAlso) {
				t.Errorf("error = %q, want it to also contain %q", got.Error(), tt.wantAlso)
			}
		})
	}
}

func TestDeleteCREResourceIgnoresAlreadyGone(t *testing.T) {
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), creDynamicListKinds())
	if err := deleteCRECertification(context.Background(), client, creTestNamespace, "missing"); err != nil {
		t.Fatalf("delete of missing Certification = %v", err)
	}
}

func TestBuildCRECertificationUsesCallerName(t *testing.T) {
	obj := buildTestCertification(t, "ns", "aicr-cre-nccl-abcd1234", twoNodeGPUConfig())
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
	tests := []struct {
		name     string
		maxNodes int
		want     []string
	}{
		{"caps to the qualified footprint", 2, []string{"a", "m"}},
		{"cap above the pool keeps every node", 5, []string{"a", "m", "z"}},
		{"missing cap clamps to one rather than the whole pool", 0, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := capCRECertificationNodes(nodes, tt.maxNodes)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i].Name != tt.want[i] {
					t.Errorf("node[%d] = %q, want %q", i, got[i].Name, tt.want[i])
				}
			}
		})
	}
}
