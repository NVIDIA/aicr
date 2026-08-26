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
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestCleanupGangTestResourcesDeletesNamespace verifies the check tears down
// its own per-run namespace, so a "complete" tools/cleanup run
// (or a fresh install) is not left with residue. See issue #1672.
func TestCleanupGangTestResourcesDeletesNamespace(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}

	objs := make([]runtime.Object, 0, 1+gangMinMembers)
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: run.namespace}})
	for i := range gangMinMembers {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: run.pods[i], Namespace: run.namespace},
		})
	}
	clientset := k8sfake.NewSimpleClientset(objs...)

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podGroupGVR: "PodGroupList"},
	)

	if err := cleanupGangTestResources(context.Background(), clientset, dynClient, run); err != nil {
		t.Fatalf("cleanupGangTestResources returned error: %v", err)
	}

	if _, err := clientset.CoreV1().Namespaces().Get(
		context.Background(), run.namespace, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("namespace %s still present after cleanup: err=%v", run.namespace, err)
	}
}

// TestGangTestRunNamespaceIsPerRun verifies two concurrent invocations derive
// distinct namespaces. A shared namespace is what made the cleanup below
// destructive: names are randomized per run, but the namespace was not.
func TestGangTestRunNamespaceIsPerRun(t *testing.T) {
	runA, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun (A): %v", err)
	}
	runB, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun (B): %v", err)
	}

	if runA.namespace == runB.namespace {
		t.Fatalf("both runs share namespace %q; concurrent validate runs would collide", runA.namespace)
	}
	for _, run := range []*gangTestRun{runA, runB} {
		if want := gangTestNSPrefix + run.suffix; run.namespace != want {
			t.Errorf("namespace = %q, want %q (derived from the per-run suffix)", run.namespace, want)
		}
	}
}

// TestCleanupGangTestResourcesPreservesConcurrentRun is the regression guard
// for the cross-run cleanup race: one run finishing must not delete a
// concurrent run's namespace, pods, or PodGroup. Before the namespace was
// derived per run, cleanup deleted the single shared namespace and took the
// other run's live resources with it, failing a healthy cluster.
func TestCleanupGangTestResourcesPreservesConcurrentRun(t *testing.T) {
	runA, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun (A): %v", err)
	}
	runB, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun (B): %v", err)
	}

	objs := make([]runtime.Object, 0, 2*(1+gangMinMembers))
	for _, run := range []*gangTestRun{runA, runB} {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: run.namespace}})
		for i := range gangMinMembers {
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: run.pods[i], Namespace: run.namespace},
			})
		}
	}
	clientset := k8sfake.NewSimpleClientset(objs...)

	// Seed both PodGroups. Without them the production delete below only ever
	// sees an ignored NotFound, so the isolation assertion would pass even if
	// cleanup targeted run B.
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{podGroupGVR: "PodGroupList"},
		buildPodGroup(runA), buildPodGroup(runB),
	)

	for _, run := range []*gangTestRun{runA, runB} {
		if _, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Get(
			context.Background(), run.groupName, metav1.GetOptions{}); err != nil {
			t.Fatalf("seeded PodGroup %s/%s not retrievable: %v", run.namespace, run.groupName, err)
		}
	}

	// Run A finishes first and tears itself down while run B is still live.
	if err := cleanupGangTestResources(context.Background(), clientset, dynClient, runA); err != nil {
		t.Fatalf("cleanupGangTestResources(runA) returned error: %v", err)
	}

	if _, err := dynClient.Resource(podGroupGVR).Namespace(runA.namespace).Get(
		context.Background(), runA.groupName, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Errorf("runA PodGroup %s still present after its own cleanup: err=%v", runA.groupName, err)
	}
	if _, err := dynClient.Resource(podGroupGVR).Namespace(runB.namespace).Get(
		context.Background(), runB.groupName, metav1.GetOptions{}); err != nil {
		t.Errorf("runA cleanup destroyed concurrent runB PodGroup %s: %v", runB.groupName, err)
	}

	if _, err := clientset.CoreV1().Namespaces().Get(
		context.Background(), runA.namespace, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("runA namespace %s still present after its own cleanup: err=%v", runA.namespace, err)
	}

	if _, err := clientset.CoreV1().Namespaces().Get(
		context.Background(), runB.namespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("runA cleanup destroyed concurrent runB namespace %s: %v", runB.namespace, err)
	}
	for i := range gangMinMembers {
		if _, err := clientset.CoreV1().Pods(runB.namespace).Get(
			context.Background(), runB.pods[i], metav1.GetOptions{}); err != nil {
			t.Errorf("runA cleanup destroyed concurrent runB pod %s: %v", runB.pods[i], err)
		}
	}
}

// TestWaitForGangTestPodsRetriesTransientReads pins the fix for #2406: a read
// that could not land must not decide the check. client-go's own rate limiter
// returns "client rate limiter Wait returned an error: context deadline
// exceeded" on a loaded cluster; before this fix that aborted the poll and
// failed a healthy cluster, even though the test pods had been created and the
// next interval would have succeeded.
//
// This mirrors the acceptance criteria of #1513, which fixed the same shape one
// step earlier in this function (instantaneous deployment read -> bounded wait).
func TestWaitForGangTestPodsRetriesTransientReads(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}

	objs := make([]runtime.Object, 0, gangMinMembers)
	for i := range gangMinMembers {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: run.pods[i], Namespace: run.namespace},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		})
	}
	clientset := k8sfake.NewSimpleClientset(objs...)

	// Fail the first two reads the way a throttled client-go client does, then
	// let the real objects through.
	var reads atomic.Int32
	clientset.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) <= 2 {
			return true, nil, fmt.Errorf(
				"client rate limiter Wait returned an error: %w", context.DeadlineExceeded)
		}
		return false, nil, nil // fall through to the tracker
	})

	pods, err := waitForGangTestPods(context.Background(), clientset, run)
	if err != nil {
		t.Fatalf("waitForGangTestPods aborted on a transient read: %v", err)
	}
	for i := range gangMinMembers {
		if pods[i] == nil {
			t.Errorf("pod %d not collected after retry", i)
		}
	}
	if got := reads.Load(); got < 3 {
		t.Errorf("expected the poll to retry past the throttled reads, saw %d reads", got)
	}
}

// TestWaitForGangTestPodsFailsClosedOnTerminalRead is the other half: a genuine
// error (RBAC denial) must still abort rather than spin until the timeout.
func TestWaitForGangTestPodsFailsClosedOnTerminalRead(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	clientset := k8sfake.NewSimpleClientset()
	clientset.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, run.pods[0], fmt.Errorf("no access"))
	})

	_, err = waitForGangTestPods(context.Background(), clientset, run)
	if err == nil {
		t.Fatal("expected a terminal read error to fail the check, got nil")
	}
	// Assert the code, not just non-nil: a mis-coded Forbidden would otherwise
	// slip through this guard.
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("Forbidden should classify as ErrCodeInternal, got %v", err)
	}
}

// TestWaitForGangTestPodsNotFoundIsTerminal pins the one error-mapping change
// this fix makes. The pods are created by deployGangTestResources immediately
// beforehand, and a Get with unset ResourceVersion is a quorum read, so
// NotFound means the pod genuinely went away — it must abort, not retry until
// the bound expires.
func TestWaitForGangTestPodsNotFoundIsTerminal(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}
	clientset := k8sfake.NewSimpleClientset() // no pods seeded

	start := time.Now()
	_, err = waitForGangTestPods(context.Background(), clientset, run)
	if err == nil {
		t.Fatal("expected NotFound to fail the check, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("want ErrCodeNotFound, got %v", err)
	}
	// Terminal means immediate: it must not have burned the poll budget.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("NotFound should abort immediately, took %s", elapsed)
	}
}

// TestWaitForDeploymentAvailableRetriesTransientReads covers the same defect at
// the site that runs FIRST in the gang check (step 1, via
// gang_scheduling_check.go's KAI deployment readiness loop). Fixing only the
// pod poll would have left the check flaking a few lines earlier under the same
// throttling. See #2406; this function is the one #1514 introduced for #1513.
func TestWaitForDeploymentAvailableRetriesTransientReads(t *testing.T) {
	const ns, name = "kai-scheduler", "podgroup-controller"
	ready := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	clientset := k8sfake.NewSimpleClientset(ready)

	var reads atomic.Int32
	clientset.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if reads.Add(1) <= 2 {
			return true, nil, fmt.Errorf(
				"client rate limiter Wait returned an error: %w", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	vctx := &validators.Context{Ctx: context.Background(), Clientset: clientset}
	deploy, err := waitForDeploymentAvailable(vctx, ns, name, 30*time.Second)
	if err != nil {
		t.Fatalf("aborted on a transient read: %v", err)
	}
	if deploy == nil || deploy.Status.AvailableReplicas != 1 {
		t.Errorf("expected the ready deployment after retry, got %v", deploy)
	}
	if got := reads.Load(); got < 3 {
		t.Errorf("expected retries past the throttled reads, saw %d", got)
	}
}
