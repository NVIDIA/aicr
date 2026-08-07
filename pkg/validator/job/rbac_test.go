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

package job

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestEnsureRBAC(t *testing.T) {
	ns := createUniqueNamespace(t)
	runID := "test-ensure-rbac"
	saName := ServiceAccountName(runID)
	crbName := ClusterRoleBindingName(runID)
	ctx := context.Background()

	if err := EnsureRBAC(ctx, testClientset, ns, runID); err != nil {
		t.Fatalf("EnsureRBAC() failed: %v", err)
	}

	// Verify ServiceAccount was created
	sa, err := testClientset.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ServiceAccount not found: %v", err)
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "aicr" {
		t.Errorf("ServiceAccount label managed-by = %q, want %q", sa.Labels["app.kubernetes.io/managed-by"], "aicr")
	}

	// Verify ClusterRoleBinding was created
	crb, err := testClientset.RbacV1().ClusterRoleBindings().Get(ctx, crbName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ClusterRoleBinding not found: %v", err)
	}
	if crb.RoleRef.Name != clusterAdminRole {
		t.Errorf("ClusterRoleBinding roleRef = %q, want %q", crb.RoleRef.Name, clusterAdminRole)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("ClusterRoleBinding subjects length = %d, want 1", len(crb.Subjects))
	}
	if crb.Subjects[0].Name != saName {
		t.Errorf("ClusterRoleBinding subject name = %q, want %q", crb.Subjects[0].Name, saName)
	}
	if crb.Subjects[0].Namespace != ns {
		t.Errorf("ClusterRoleBinding subject namespace = %q, want %q", crb.Subjects[0].Namespace, ns)
	}

	// Cleanup cluster-scoped resource
	t.Cleanup(func() {
		_ = CleanupRBAC(context.Background(), testClientset, ns, runID)
	})
}

func TestEnsureRBACIdempotent(t *testing.T) {
	ns := createUniqueNamespace(t)
	runID := "test-rbac-idempotent"
	saName := ServiceAccountName(runID)
	ctx := context.Background()

	// Call twice — second call should not error
	if err := EnsureRBAC(ctx, testClientset, ns, runID); err != nil {
		t.Fatalf("first EnsureRBAC() failed: %v", err)
	}
	if err := EnsureRBAC(ctx, testClientset, ns, runID); err != nil {
		t.Fatalf("second EnsureRBAC() failed: %v", err)
	}

	// Verify only one ServiceAccount exists for this runID
	saList, err := testClientset.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list ServiceAccounts: %v", err)
	}
	count := 0
	for _, sa := range saList.Items {
		if sa.Name == saName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 ServiceAccount named %q, found %d", saName, count)
	}

	t.Cleanup(func() {
		_ = CleanupRBAC(context.Background(), testClientset, ns, runID)
	})
}

// TestEnsureRBACConcurrentRunsIndependent guards the fix: two overlapping runs
// against the same namespace must each get their own ServiceAccount and
// ClusterRoleBinding, and one run's cleanup must not touch the other's.
func TestEnsureRBACConcurrentRunsIndependent(t *testing.T) {
	ns := createUniqueNamespace(t)
	runA := "test-concurrent-a"
	runB := "test-concurrent-b"
	ctx := context.Background()

	if err := EnsureRBAC(ctx, testClientset, ns, runA); err != nil {
		t.Fatalf("EnsureRBAC(runA) failed: %v", err)
	}
	if err := EnsureRBAC(ctx, testClientset, ns, runB); err != nil {
		t.Fatalf("EnsureRBAC(runB) failed: %v", err)
	}

	// Run A cleans up; run B's resources must still exist.
	if err := CleanupRBAC(ctx, testClientset, ns, runA); err != nil {
		t.Fatalf("CleanupRBAC(runA) failed: %v", err)
	}

	if _, err := testClientset.CoreV1().ServiceAccounts(ns).Get(ctx, ServiceAccountName(runB), metav1.GetOptions{}); err != nil {
		t.Errorf("run B's ServiceAccount was deleted by run A's cleanup: %v", err)
	}
	if _, err := testClientset.RbacV1().ClusterRoleBindings().Get(ctx, ClusterRoleBindingName(runB), metav1.GetOptions{}); err != nil {
		t.Errorf("run B's ClusterRoleBinding was deleted by run A's cleanup: %v", err)
	}

	t.Cleanup(func() {
		_ = CleanupRBAC(context.Background(), testClientset, ns, runB)
	})
}

func TestCleanupRBAC(t *testing.T) {
	ns := createUniqueNamespace(t)
	runID := "test-cleanup-rbac"
	ctx := context.Background()

	// Create RBAC first
	if err := EnsureRBAC(ctx, testClientset, ns, runID); err != nil {
		t.Fatalf("EnsureRBAC() failed: %v", err)
	}

	// Cleanup
	if err := CleanupRBAC(ctx, testClientset, ns, runID); err != nil {
		t.Fatalf("CleanupRBAC() failed: %v", err)
	}

	// Verify ServiceAccount is gone
	_, err := testClientset.CoreV1().ServiceAccounts(ns).Get(ctx, ServiceAccountName(runID), metav1.GetOptions{})
	if err == nil {
		t.Error("ServiceAccount should be deleted")
	}

	// Verify ClusterRoleBinding is gone
	_, err = testClientset.RbacV1().ClusterRoleBindings().Get(ctx, ClusterRoleBindingName(runID), metav1.GetOptions{})
	if err == nil {
		t.Error("ClusterRoleBinding should be deleted")
	}
}

func TestCleanupRBACNotFound(t *testing.T) {
	ns := createUniqueNamespace(t)

	// Cleanup without creating — should not error
	if err := CleanupRBAC(context.Background(), testClientset, ns, "test-cleanup-notfound"); err != nil {
		t.Fatalf("CleanupRBAC() on nonexistent resources should not error, got: %v", err)
	}
}

// TestCleanupRBACDeletesClusterRoleBindingBeforeServiceAccount guards the
// ordering fix: the cluster-admin ClusterRoleBinding must be revoked BEFORE the
// ServiceAccount is deleted, so the privilege-escalation window closes first. A
// reactor records delete order without handling the delete (returns handled =
// false to fall through to the tracker). Reverting the reorder in CleanupRBAC
// flips the recorded order and fails this test.
func TestCleanupRBACDeletesClusterRoleBindingBeforeServiceAccount(t *testing.T) {
	cs := k8sfake.NewSimpleClientset()

	var order []string
	cs.PrependReactor("delete", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		order = append(order, action.GetResource().Resource)
		return false, nil, nil // fall through to the default tracker
	})

	if err := CleanupRBAC(context.Background(), cs, "ns", "run"); err != nil {
		t.Fatalf("CleanupRBAC() error = %v, want nil", err)
	}

	want := []string{"clusterrolebindings", "serviceaccounts"}
	if len(order) != len(want) {
		t.Fatalf("delete order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("delete order = %v, want %v", order, want)
		}
	}
}

// TestCleanupRBACClusterRoleBindingDeleteFailureSurfaced proves CleanupRBAC
// fails closed AND does not short-circuit: a ClusterRoleBinding delete error
// (anything other than NotFound) is surfaced as an ErrCodeInternal error rather
// than swallowed, so a leaked cluster-admin binding cannot masquerade as a
// clean teardown — and cleanup still proceeds to delete the ServiceAccount so a
// binding failure does not strand the SA behind an early return.
func TestCleanupRBACClusterRoleBindingDeleteFailureSurfaced(t *testing.T) {
	const ns, runID = "ns", "run"
	saName := ServiceAccountName(runID)

	// Seed the ServiceAccount so its post-cleanup absence proves the delete ran.
	cs := k8sfake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: ns},
	})

	wantCause := stderrors.New("apiserver unavailable")
	cs.PrependReactor("delete", "clusterrolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, wantCause
	})

	err := CleanupRBAC(context.Background(), cs, ns, runID)
	if err == nil {
		t.Fatal("CleanupRBAC() error = nil, want error when ClusterRoleBinding delete fails")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("CleanupRBAC() error = %v, want ErrCodeInternal", err)
	}
	if !stderrors.Is(err, wantCause) {
		t.Errorf("CleanupRBAC() error = %v, want wrapped underlying cause", err)
	}

	// Despite the binding-delete failure, the ServiceAccount delete must still
	// have run — proving CleanupRBAC does not return early on the first error.
	if _, getErr := cs.CoreV1().ServiceAccounts(ns).Get(context.Background(), saName, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("ServiceAccount %q still present after cleanup (Get err = %v); cleanup returned early on binding failure", saName, getErr)
	}
}
