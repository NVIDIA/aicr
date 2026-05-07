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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnsureRBAC(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	if err := EnsureRBAC(ctx, testClientset, ns); err != nil {
		t.Fatalf("EnsureRBAC() failed: %v", err)
	}

	// Verify ServiceAccount was created
	sa, err := testClientset.CoreV1().ServiceAccounts(ns).Get(ctx, ServiceAccountName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ServiceAccount not found: %v", err)
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "aicr" {
		t.Errorf("ServiceAccount label managed-by = %q, want %q", sa.Labels["app.kubernetes.io/managed-by"], "aicr")
	}

	// Verify ClusterRoleBinding was created
	crb, err := testClientset.RbacV1().ClusterRoleBindings().Get(ctx, ClusterRoleBindingName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ClusterRoleBinding not found: %v", err)
	}
	if crb.RoleRef.Name != clusterAdminRole {
		t.Errorf("ClusterRoleBinding roleRef = %q, want %q", crb.RoleRef.Name, clusterAdminRole)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("ClusterRoleBinding subjects length = %d, want 1", len(crb.Subjects))
	}
	if crb.Subjects[0].Name != ServiceAccountName {
		t.Errorf("ClusterRoleBinding subject name = %q, want %q", crb.Subjects[0].Name, ServiceAccountName)
	}
	if crb.Subjects[0].Namespace != ns {
		t.Errorf("ClusterRoleBinding subject namespace = %q, want %q", crb.Subjects[0].Namespace, ns)
	}

	// Cleanup cluster-scoped resource
	t.Cleanup(func() {
		_ = CleanupRBAC(context.Background(), testClientset, ns)
	})
}

func TestEnsureRBACIdempotent(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	// Call twice — second call should not error
	if err := EnsureRBAC(ctx, testClientset, ns); err != nil {
		t.Fatalf("first EnsureRBAC() failed: %v", err)
	}
	if err := EnsureRBAC(ctx, testClientset, ns); err != nil {
		t.Fatalf("second EnsureRBAC() failed: %v", err)
	}

	// Verify only one ServiceAccount exists
	saList, err := testClientset.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list ServiceAccounts: %v", err)
	}
	count := 0
	for _, sa := range saList.Items {
		if sa.Name == ServiceAccountName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 ServiceAccount named %q, found %d", ServiceAccountName, count)
	}

	t.Cleanup(func() {
		_ = CleanupRBAC(context.Background(), testClientset, ns)
	})
}

func TestCleanupRBAC(t *testing.T) {
	ns := createUniqueNamespace(t)
	ctx := context.Background()

	// Create RBAC first
	if err := EnsureRBAC(ctx, testClientset, ns); err != nil {
		t.Fatalf("EnsureRBAC() failed: %v", err)
	}

	// Cleanup
	if err := CleanupRBAC(ctx, testClientset, ns); err != nil {
		t.Fatalf("CleanupRBAC() failed: %v", err)
	}

	// Verify ServiceAccount is gone
	_, err := testClientset.CoreV1().ServiceAccounts(ns).Get(ctx, ServiceAccountName, metav1.GetOptions{})
	if err == nil {
		t.Error("ServiceAccount should be deleted")
	}

	// Verify ClusterRoleBinding is gone
	_, err = testClientset.RbacV1().ClusterRoleBindings().Get(ctx, ClusterRoleBindingName, metav1.GetOptions{})
	if err == nil {
		t.Error("ClusterRoleBinding should be deleted")
	}
}

func TestCleanupRBACNotFound(t *testing.T) {
	ns := createUniqueNamespace(t)

	// Cleanup without creating — should not error
	if err := CleanupRBAC(context.Background(), testClientset, ns); err != nil {
		t.Fatalf("CleanupRBAC() on nonexistent resources should not error, got: %v", err)
	}
}
