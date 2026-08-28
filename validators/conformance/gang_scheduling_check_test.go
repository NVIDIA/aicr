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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
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
