// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package agent

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEnsureServiceAccount(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("create ServiceAccount", func(t *testing.T) {
		if err := deployer.ensureServiceAccount(ctx); err != nil {
			t.Fatalf("ensureServiceAccount() failed: %v", err)
		}

		sa, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
			Get(ctx, deployer.config.ServiceAccountName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ServiceAccount not found: %v", err)
		}

		if sa.Name != deployer.config.ServiceAccountName {
			t.Errorf("expected SA name %q, got %q", deployer.config.ServiceAccountName, sa.Name)
		}
		if sa.Namespace != deployer.config.Namespace {
			t.Errorf("expected namespace %q, got %q", deployer.config.Namespace, sa.Namespace)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		// Create twice - second call should be idempotent
		if err := deployer.ensureServiceAccount(ctx); err != nil {
			t.Fatalf("first create failed: %v", err)
		}

		if err := deployer.ensureServiceAccount(ctx); err != nil {
			t.Fatalf("second create failed (not idempotent): %v", err)
		}

		// Verify only one ServiceAccount exists
		saList, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("failed to list ServiceAccounts: %v", err)
		}
		if len(saList.Items) != 1 {
			t.Errorf("expected 1 ServiceAccount, got %d", len(saList.Items))
		}
	})
}

func TestDeleteServiceAccount(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("delete existing ServiceAccount", func(t *testing.T) {
		// Create first
		if err := deployer.ensureServiceAccount(ctx); err != nil {
			t.Fatalf("failed to create ServiceAccount: %v", err)
		}

		// Delete
		if err := deployer.deleteServiceAccount(ctx); err != nil {
			t.Fatalf("deleteServiceAccount() failed: %v", err)
		}

		// Verify it's gone
		_, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
			Get(ctx, deployer.config.ServiceAccountName, metav1.GetOptions{})
		if err == nil {
			t.Error("ServiceAccount should be deleted")
		}
	})

	t.Run("delete non-existent ServiceAccount", func(t *testing.T) {
		// Delete without creating - should not error (not found is ignored)
		if err := deployer.deleteServiceAccount(ctx); err != nil {
			t.Errorf("deleteServiceAccount() should ignore not found: %v", err)
		}
	})
}

func TestEnsureRole(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("create Role", func(t *testing.T) {
		if err := deployer.ensureRole(ctx); err != nil {
			t.Fatalf("ensureRole() failed: %v", err)
		}

		role, err := clientset.RbacV1().Roles(deployer.config.Namespace).
			Get(ctx, deployer.config.ServiceAccountName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Role not found: %v", err)
		}

		if role.Name != deployer.config.ServiceAccountName {
			t.Errorf("expected Role name %q, got %q", deployer.config.ServiceAccountName, role.Name)
		}
		if role.Namespace != deployer.config.Namespace {
			t.Errorf("expected namespace %q, got %q", deployer.config.Namespace, role.Namespace)
		}

		// Verify policy rules
		if len(role.Rules) != 5 {
			t.Errorf("expected 5 rules, got %d", len(role.Rules))
		}

		// Rule 0: namespaces, events, services, endpoints, nodes (get, list)
		rule0 := role.Rules[0]
		if len(rule0.Resources) != 5 {
			t.Errorf("expected 5 resources in first rule, got %d: %v", len(rule0.Resources), rule0.Resources)
		}
		for _, res := range []string{"namespaces", "events", "services", "endpoints", "nodes"} {
			if !containsResource(rule0.Resources, res) {
				t.Errorf("expected %s in first rule, got %v", res, rule0.Resources)
			}
		}
		if !containsVerb(rule0.Verbs, "get") || !containsVerb(rule0.Verbs, "list") {
			t.Errorf("expected get/list verbs for first rule, got %v", rule0.Verbs)
		}

		// Rule 1: configmaps (get, list, create, update, patch)
		rule1 := role.Rules[1]
		if len(rule1.Resources) != 1 || rule1.Resources[0] != "configmaps" {
			t.Errorf("expected configmaps in second rule, got %v", rule1.Resources)
		}
		if !containsVerb(rule1.Verbs, "get") || !containsVerb(rule1.Verbs, "list") ||
			!containsVerb(rule1.Verbs, "create") || !containsVerb(rule1.Verbs, "update") ||
			!containsVerb(rule1.Verbs, "patch") {

			t.Errorf("expected get/list/create/update/patch verbs for configmaps, got %v", rule1.Verbs)
		}

		// Rule 2: pods (get, list, create, update, patch, delete)
		rule2 := role.Rules[2]
		if len(rule2.Resources) != 1 || rule2.Resources[0] != "pods" {
			t.Errorf("expected pods in third rule, got %v", rule2.Resources)
		}
		if !containsVerb(rule2.Verbs, "get") || !containsVerb(rule2.Verbs, "list") ||
			!containsVerb(rule2.Verbs, "create") || !containsVerb(rule2.Verbs, "update") ||
			!containsVerb(rule2.Verbs, "patch") || !containsVerb(rule2.Verbs, "delete") {

			t.Errorf("expected get/list/create/update/patch/delete verbs for pods, got %v", rule2.Verbs)
		}

		// Rule 3: pods/log, pods/status (get, list)
		rule3 := role.Rules[3]
		if !containsResource(rule3.Resources, "pods/log") || !containsResource(rule3.Resources, "pods/status") {
			t.Errorf("expected pods/log and pods/status in fourth rule, got %v", rule3.Resources)
		}
		if !containsVerb(rule3.Verbs, "get") || !containsVerb(rule3.Verbs, "list") {
			t.Errorf("expected get/list verbs for pod subresources, got %v", rule3.Verbs)
		}

		// Rule 4: batch/jobs (get, list)
		rule4 := role.Rules[4]
		if rule4.APIGroups[0] != "batch" {
			t.Errorf("expected batch API group in fifth rule, got %v", rule4.APIGroups)
		}
		if !containsResource(rule4.Resources, "jobs") {
			t.Errorf("expected jobs in fifth rule, got %v", rule4.Resources)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		// Create twice - second call should be idempotent
		if err := deployer.ensureRole(ctx); err != nil {
			t.Fatalf("first create failed: %v", err)
		}

		if err := deployer.ensureRole(ctx); err != nil {
			t.Fatalf("second create failed (not idempotent): %v", err)
		}

		// Verify only one Role exists
		roleList, err := clientset.RbacV1().Roles(deployer.config.Namespace).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("failed to list Roles: %v", err)
		}
		if len(roleList.Items) != 1 {
			t.Errorf("expected 1 Role, got %d", len(roleList.Items))
		}
	})
}

func TestDeleteRole(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("delete existing Role", func(t *testing.T) {
		// Create first
		if err := deployer.ensureRole(ctx); err != nil {
			t.Fatalf("failed to create Role: %v", err)
		}

		// Delete
		if err := deployer.deleteRole(ctx); err != nil {
			t.Fatalf("deleteRole() failed: %v", err)
		}

		// Verify it's gone
		_, err := clientset.RbacV1().Roles(deployer.config.Namespace).
			Get(ctx, deployer.config.ServiceAccountName, metav1.GetOptions{})
		if err == nil {
			t.Error("Role should be deleted")
		}
	})

	t.Run("delete non-existent Role", func(t *testing.T) {
		// Delete without creating - should not error (not found is ignored)
		if err := deployer.deleteRole(ctx); err != nil {
			t.Errorf("deleteRole() should ignore not found: %v", err)
		}
	})
}

func TestEnsureRoleBinding(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("create RoleBinding", func(t *testing.T) {
		if err := deployer.ensureRoleBinding(ctx); err != nil {
			t.Fatalf("ensureRoleBinding() failed: %v", err)
		}

		rb, err := clientset.RbacV1().RoleBindings(deployer.config.Namespace).
			Get(ctx, deployer.config.ServiceAccountName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("RoleBinding not found: %v", err)
		}

		if rb.Name != deployer.config.ServiceAccountName {
			t.Errorf("expected RoleBinding name %q, got %q", deployer.config.ServiceAccountName, rb.Name)
		}
		if rb.Namespace != deployer.config.Namespace {
			t.Errorf("expected namespace %q, got %q", deployer.config.Namespace, rb.Namespace)
		}

		// Verify subjects
		if len(rb.Subjects) != 1 {
			t.Errorf("expected 1 subject, got %d", len(rb.Subjects))
		}
		if rb.Subjects[0].Kind != "ServiceAccount" {
			t.Errorf("expected subject kind ServiceAccount, got %q", rb.Subjects[0].Kind)
		}
		if rb.Subjects[0].Name != deployer.config.ServiceAccountName {
			t.Errorf("expected subject name %q, got %q", deployer.config.ServiceAccountName, rb.Subjects[0].Name)
		}
		if rb.Subjects[0].Namespace != deployer.config.Namespace {
			t.Errorf("expected subject namespace %q, got %q", deployer.config.Namespace, rb.Subjects[0].Namespace)
		}

		// Verify roleRef
		if rb.RoleRef.APIGroup != "rbac.authorization.k8s.io" {
			t.Errorf("expected roleRef APIGroup rbac.authorization.k8s.io, got %q", rb.RoleRef.APIGroup)
		}
		if rb.RoleRef.Kind != "Role" {
			t.Errorf("expected roleRef kind Role, got %q", rb.RoleRef.Kind)
		}
		if rb.RoleRef.Name != deployer.config.ServiceAccountName {
			t.Errorf("expected roleRef name %q, got %q", deployer.config.ServiceAccountName, rb.RoleRef.Name)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		// Create twice - second call should be idempotent
		if err := deployer.ensureRoleBinding(ctx); err != nil {
			t.Fatalf("first create failed: %v", err)
		}

		if err := deployer.ensureRoleBinding(ctx); err != nil {
			t.Fatalf("second create failed (not idempotent): %v", err)
		}

		// Verify only one RoleBinding exists
		rbList, err := clientset.RbacV1().RoleBindings(deployer.config.Namespace).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("failed to list RoleBindings: %v", err)
		}
		if len(rbList.Items) != 1 {
			t.Errorf("expected 1 RoleBinding, got %d", len(rbList.Items))
		}
	})
}

func TestDeleteRoleBinding(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("delete existing RoleBinding", func(t *testing.T) {
		// Create first
		if err := deployer.ensureRoleBinding(ctx); err != nil {
			t.Fatalf("failed to create RoleBinding: %v", err)
		}

		// Delete
		if err := deployer.deleteRoleBinding(ctx); err != nil {
			t.Fatalf("deleteRoleBinding() failed: %v", err)
		}

		// Verify it's gone
		_, err := clientset.RbacV1().RoleBindings(deployer.config.Namespace).
			Get(ctx, deployer.config.ServiceAccountName, metav1.GetOptions{})
		if err == nil {
			t.Error("RoleBinding should be deleted")
		}
	})

	t.Run("delete non-existent RoleBinding", func(t *testing.T) {
		// Delete without creating - should not error (not found is ignored)
		if err := deployer.deleteRoleBinding(ctx); err != nil {
			t.Errorf("deleteRoleBinding() should ignore not found: %v", err)
		}
	})
}

func TestEnsureInputConfigMaps(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                    string
		createSnapshot          bool
		createRecipe            bool
		expectSnapshotConfigMap string
		expectRecipeConfigMap   string
		wantErr                 bool
	}{
		{
			name:                    "both ConfigMaps exist",
			createSnapshot:          true,
			createRecipe:            true,
			expectSnapshotConfigMap: "test-snapshot",
			expectRecipeConfigMap:   "test-recipe",
			wantErr:                 false,
		},
		{
			name:                    "snapshot ConfigMap missing",
			createSnapshot:          false,
			createRecipe:            true,
			expectSnapshotConfigMap: "test-snapshot",
			expectRecipeConfigMap:   "test-recipe",
			wantErr:                 true,
		},
		{
			name:                    "recipe ConfigMap missing",
			createSnapshot:          true,
			createRecipe:            false,
			expectSnapshotConfigMap: "test-snapshot",
			expectRecipeConfigMap:   "test-recipe",
			wantErr:                 true,
		},
		{
			name:                    "empty ConfigMap names",
			createSnapshot:          false,
			createRecipe:            false,
			expectSnapshotConfigMap: "",
			expectRecipeConfigMap:   "",
			wantErr:                 false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployer, clientset := createDeployer()
			deployer.config.SnapshotConfigMap = tt.expectSnapshotConfigMap
			deployer.config.RecipeConfigMap = tt.expectRecipeConfigMap

			if tt.createSnapshot && tt.expectSnapshotConfigMap != "" {
				snapshotCM := createTestConfigMap(tt.expectSnapshotConfigMap, deployer.config.Namespace, "snapshot.yaml", "test data")
				if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create snapshot ConfigMap: %v", err)
				}
			}

			if tt.createRecipe && tt.expectRecipeConfigMap != "" {
				recipeCM := createTestConfigMap(tt.expectRecipeConfigMap, deployer.config.Namespace, "recipe.yaml", "test data")
				if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create recipe ConfigMap: %v", err)
				}
			}

			err := deployer.ensureInputConfigMaps(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("ensureInputConfigMaps() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeleteResultConfigMap(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()
	resultName := deployer.config.JobName + "-result"

	t.Run("delete existing result ConfigMap", func(t *testing.T) {
		// Create result ConfigMap first
		resultCM := createTestConfigMap(resultName, deployer.config.Namespace, "result.json", `{"Status":"pass"}`)
		if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, resultCM, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create result ConfigMap: %v", err)
		}

		// Delete
		if err := deployer.deleteResultConfigMap(ctx); err != nil {
			t.Fatalf("deleteResultConfigMap() failed: %v", err)
		}

		// Verify it's gone
		_, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).
			Get(ctx, resultName, metav1.GetOptions{})
		if err == nil {
			t.Error("Result ConfigMap should be deleted")
		}
	})

	t.Run("delete non-existent result ConfigMap", func(t *testing.T) {
		// Delete without creating - should not error (not found is ignored)
		if err := deployer.deleteResultConfigMap(ctx); err != nil {
			t.Errorf("deleteResultConfigMap() should ignore not found: %v", err)
		}
	})
}

// Helper function
func containsVerb(verbs []string, verb string) bool {
	for _, v := range verbs {
		if v == verb {
			return true
		}
	}
	return false
}

func containsResource(resources []string, resource string) bool {
	for _, r := range resources {
		if r == resource {
			return true
		}
	}
	return false
}
