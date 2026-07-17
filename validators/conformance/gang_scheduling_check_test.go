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
// its own gang-scheduling-test namespace, so a "complete" tools/cleanup run
// (or a fresh install) is not left with residue. See issue #1672.
func TestCleanupGangTestResourcesDeletesNamespace(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun: %v", err)
	}

	objs := make([]runtime.Object, 0, 1+gangMinMembers)
	objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: gangTestNamespace}})
	for i := range gangMinMembers {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: run.pods[i], Namespace: gangTestNamespace},
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
		context.Background(), gangTestNamespace, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("namespace %s still present after cleanup: err=%v", gangTestNamespace, err)
	}
}
