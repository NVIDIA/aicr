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
	"time"

	"github.com/NVIDIA/eidos/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testName = "eidos-validator"

// createDeployer is a helper function to create a test Deployer.
func createDeployer() (*Deployer, *fake.Clientset) {
	clientset := fake.NewSimpleClientset() //nolint:staticcheck // SA1019: fake.NewSimpleClientset is deprecated but still the standard for testing
	config := createConfig()
	deployer := NewDeployer(clientset, config)
	return deployer, clientset
}

// createConfig is a helper function to create a test Config.
func createConfig() Config {
	return Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		SnapshotConfigMap:  "test-snapshot",
		RecipeConfigMap:    "test-recipe",
		TestPackage:        "./pkg/validator/checks/readiness",
		TestPattern:        "TestGpuHardwareDetection",
		Timeout:            5 * time.Minute,
		ImagePullSecrets:   []string{"regcred"},
		NodeSelector: map[string]string{
			"nodeGroup": "gpu-nodes",
		},
		Tolerations: []corev1.Toleration{
			{
				Key:      "nvidia.com/gpu",
				Operator: corev1.TolerationOpEqual,
				Value:    "present",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		},
	}
}

func TestDeployer_EnsureRBAC(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	t.Run("create RBAC resources", func(t *testing.T) {
		if err := deployer.EnsureRBAC(ctx); err != nil {
			t.Fatalf("EnsureRBAC() failed: %v", err)
		}

		// Verify ServiceAccount
		sa, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ServiceAccount not found: %v", err)
		}
		if sa.Name != testName {
			t.Errorf("expected SA name %q, got %q", testName, sa.Name)
		}

		// Verify Role
		role, err := clientset.RbacV1().Roles(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Role not found: %v", err)
		}
		if len(role.Rules) != 3 {
			t.Errorf("expected 3 rules, got %d", len(role.Rules))
		}

		// Verify RoleBinding
		rb, err := clientset.RbacV1().RoleBindings(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("RoleBinding not found: %v", err)
		}
		if len(rb.Subjects) != 1 {
			t.Errorf("expected 1 subject, got %d", len(rb.Subjects))
		}
		if rb.Subjects[0].Name != testName {
			t.Errorf("expected subject name %q, got %q", testName, rb.Subjects[0].Name)
		}
	})
}

func TestDeployer_EnsureRBAC_Idempotent(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create RBAC twice - second call should be idempotent
	if err := deployer.EnsureRBAC(ctx); err != nil {
		t.Fatalf("first EnsureRBAC() failed: %v", err)
	}

	if err := deployer.EnsureRBAC(ctx); err != nil {
		t.Fatalf("second EnsureRBAC() failed (not idempotent): %v", err)
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

	// Verify only one Role exists
	roleList, err := clientset.RbacV1().Roles(deployer.config.Namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list Roles: %v", err)
	}
	if len(roleList.Items) != 1 {
		t.Errorf("expected 1 Role, got %d", len(roleList.Items))
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
}

func TestDeployer_DeployJob(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps first
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.SnapshotConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"snapshot.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Snapshot",
		},
	}
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.RecipeConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"recipe.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Recipe",
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	t.Run("create Job", func(t *testing.T) {
		if err := deployer.DeployJob(ctx); err != nil {
			t.Fatalf("DeployJob() failed: %v", err)
		}

		// Verify Job
		job, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Job not found: %v", err)
		}

		// Verify Job spec
		if job.Spec.Template.Spec.ServiceAccountName != deployer.config.ServiceAccountName {
			t.Errorf("expected ServiceAccountName %q, got %q",
				deployer.config.ServiceAccountName, job.Spec.Template.Spec.ServiceAccountName)
		}

		// Verify node selector
		if job.Spec.Template.Spec.NodeSelector["nodeGroup"] != "gpu-nodes" {
			t.Errorf("expected nodeGroup=gpu-nodes, got %v", job.Spec.Template.Spec.NodeSelector)
		}

		// Verify tolerations
		if len(job.Spec.Template.Spec.Tolerations) != 1 {
			t.Errorf("expected 1 toleration, got %d", len(job.Spec.Template.Spec.Tolerations))
		}

		// Verify container
		if len(job.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(job.Spec.Template.Spec.Containers))
		}
		container := job.Spec.Template.Spec.Containers[0]
		if container.Image != deployer.config.Image {
			t.Errorf("expected image %q, got %q", deployer.config.Image, container.Image)
		}

		// Verify volumes
		if len(job.Spec.Template.Spec.Volumes) != 2 {
			t.Errorf("expected 2 volumes, got %d", len(job.Spec.Template.Spec.Volumes))
		}

		// Verify imagePullSecrets
		if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 {
			t.Errorf("expected 1 imagePullSecret, got %d", len(job.Spec.Template.Spec.ImagePullSecrets))
		}
	})

	t.Run("recreate Job deletes old one", func(t *testing.T) {
		// Create Job first time
		if err := deployer.DeployJob(ctx); err != nil {
			t.Fatalf("first DeployJob() failed: %v", err)
		}

		// Create Job second time - should delete and recreate
		if err := deployer.DeployJob(ctx); err != nil {
			t.Fatalf("second DeployJob() failed: %v", err)
		}

		// Verify Job still exists
		_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("Job should exist after recreate: %v", err)
		}
	})
}

func TestDeployer_DeployJob_MissingSnapshotConfigMap(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Only create recipe ConfigMap, not snapshot
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.RecipeConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"recipe.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Recipe",
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	// DeployJob should fail
	err := deployer.DeployJob(ctx)
	if err == nil {
		t.Error("DeployJob() should fail when snapshot ConfigMap is missing")
	}
}

func TestDeployer_DeployJob_MissingRecipeConfigMap(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Only create snapshot ConfigMap, not recipe
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.SnapshotConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"snapshot.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Snapshot",
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}

	// DeployJob should fail
	err := deployer.DeployJob(ctx)
	if err == nil {
		t.Error("DeployJob() should fail when recipe ConfigMap is missing")
	}
}

func TestDeployer_Deploy(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.SnapshotConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"snapshot.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Snapshot",
		},
	}
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.RecipeConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"recipe.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Recipe",
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	// Deploy should create all resources
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Verify ServiceAccount
	_, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}

	// Verify Role
	_, err = clientset.RbacV1().Roles(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Role not created: %v", err)
	}

	// Verify RoleBinding
	_, err = clientset.RbacV1().RoleBindings(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("RoleBinding not created: %v", err)
	}

	// Verify Job
	_, err = clientset.BatchV1().Jobs(deployer.config.Namespace).
		Get(ctx, deployer.config.JobName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Job not created: %v", err)
	}
}

func TestDeployer_CleanupRBAC(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create RBAC first
	if err := deployer.EnsureRBAC(ctx); err != nil {
		t.Fatalf("EnsureRBAC() failed: %v", err)
	}

	// Cleanup RBAC
	if err := deployer.CleanupRBAC(ctx); err != nil {
		t.Fatalf("CleanupRBAC() failed: %v", err)
	}

	// Verify ServiceAccount is deleted
	_, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("ServiceAccount should be deleted")
	}

	// Verify Role is deleted
	_, err = clientset.RbacV1().Roles(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("Role should be deleted")
	}

	// Verify RoleBinding is deleted
	_, err = clientset.RbacV1().RoleBindings(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("RoleBinding should be deleted")
	}
}

func TestDeployer_CleanupRBAC_NotFound(t *testing.T) {
	deployer, _ := createDeployer()
	ctx := context.Background()

	// Cleanup without creating resources first should succeed (not found errors are ignored)
	if err := deployer.CleanupRBAC(ctx); err != nil {
		t.Fatalf("CleanupRBAC() should succeed when resources don't exist: %v", err)
	}
}

func TestDeployer_CleanupJob(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.SnapshotConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"snapshot.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Snapshot",
		},
	}
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.RecipeConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"recipe.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Recipe",
		},
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	// Deploy Job
	if err := deployer.DeployJob(ctx); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}

	// Cleanup Job
	if err := deployer.CleanupJob(ctx); err != nil {
		t.Fatalf("CleanupJob() failed: %v", err)
	}

	// Verify Job is deleted
	_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
		Get(ctx, deployer.config.JobName, metav1.GetOptions{})
	if err == nil {
		t.Error("Job should be deleted")
	}

	// Note: Result ConfigMaps are no longer used (results read from Job logs)
}

func TestDeployer_Cleanup(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.SnapshotConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"snapshot.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Snapshot",
		},
	}
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.RecipeConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"recipe.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Recipe",
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	// Deploy first
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	t.Run("cleanup disabled", func(t *testing.T) {
		// Cleanup without enabled flag should keep everything
		if err := deployer.Cleanup(ctx, CleanupOptions{Enabled: false}); err != nil {
			t.Fatalf("Cleanup() failed: %v", err)
		}

		// Job should still exist
		_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("Job should still exist when cleanup disabled: %v", err)
		}

		// ServiceAccount should still exist
		_, err = clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("ServiceAccount should still exist: %v", err)
		}
	})

	t.Run("cleanup enabled", func(t *testing.T) {
		// Cleanup with enabled flag
		if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
			t.Fatalf("Cleanup() with Enabled failed: %v", cleanupErr)
		}

		// Job should be deleted
		_, err := clientset.BatchV1().Jobs(deployer.config.Namespace).
			Get(ctx, deployer.config.JobName, metav1.GetOptions{})
		if err == nil {
			t.Errorf("Job should be deleted")
		}

		// ServiceAccount should be deleted
		_, err = clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err == nil {
			t.Error("ServiceAccount should be deleted")
		}

		// Role should be deleted
		_, err = clientset.RbacV1().Roles(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err == nil {
			t.Error("Role should be deleted")
		}

		// RoleBinding should be deleted
		_, err = clientset.RbacV1().RoleBindings(deployer.config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err == nil {
			t.Error("RoleBinding should be deleted")
		}
	})
}

func TestDeployer_Cleanup_AttemptsAllDeletions(t *testing.T) {
	deployer, clientset := createDeployer()
	ctx := context.Background()

	// Create required ConfigMaps
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.SnapshotConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"snapshot.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Snapshot",
		},
	}
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployer.config.RecipeConfigMap,
			Namespace: deployer.config.Namespace,
		},
		Data: map[string]string{
			"recipe.yaml": "apiVersion: eidos.nvidia.com/v1alpha1\nkind: Recipe",
		},
	}

	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create snapshot ConfigMap: %v", err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(deployer.config.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create recipe ConfigMap: %v", err)
	}

	// Deploy first
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Manually delete the Job to simulate it already being cleaned up
	if err := clientset.BatchV1().Jobs(deployer.config.Namespace).Delete(ctx, deployer.config.JobName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Failed to pre-delete Job: %v", err)
	}

	// Cleanup should still succeed (Job not found is ignored)
	// and should delete all RBAC resources
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() should succeed even when Job already deleted: %v", cleanupErr)
	}

	// Verify all RBAC resources were deleted
	_, err := clientset.CoreV1().ServiceAccounts(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("ServiceAccount should be deleted")
	}

	_, err = clientset.RbacV1().Roles(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("Role should be deleted")
	}

	_, err = clientset.RbacV1().RoleBindings(deployer.config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("RoleBinding should be deleted")
	}
}

func TestIgnoreAlreadyExists(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{
			name:    "nil error",
			err:     nil,
			wantErr: false,
		},
		{
			name:    "already exists error",
			err:     &fakeAlreadyExistsError{},
			wantErr: false,
		},
		{
			name:    "other error",
			err:     &fakeNotFoundError{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := k8s.IgnoreAlreadyExists(tt.err)
			if (err != nil) != tt.wantErr {
				t.Errorf("k8s.IgnoreAlreadyExists() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestIgnoreNotFound(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{
			name:    "nil error",
			err:     nil,
			wantErr: false,
		},
		{
			name:    "not found error",
			err:     &fakeNotFoundError{},
			wantErr: false,
		},
		{
			name:    "other error",
			err:     &fakeAlreadyExistsError{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := k8s.IgnoreNotFound(tt.err)
			if (err != nil) != tt.wantErr {
				t.Errorf("k8s.IgnoreNotFound() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeployer_GetResult_NoJob(t *testing.T) {
	deployer, _ := createDeployer()
	ctx := context.Background()

	// GetResult should fail when there's no Job/pod
	_, err := deployer.GetResult(ctx)
	if err == nil {
		t.Error("GetResult() expected error when no Job exists, got nil")
	}
}

func TestDeployer_GetPodLogs_NoJob(t *testing.T) {
	deployer, _ := createDeployer()
	ctx := context.Background()

	// GetPodLogs should fail when there's no Job/pod
	_, err := deployer.GetPodLogs(ctx)
	if err == nil {
		t.Error("GetPodLogs() expected error when no Job exists, got nil")
	}
}

// Fake error types for testing
type fakeNotFoundError struct{}

func (e *fakeNotFoundError) Error() string { return "not found" }
func (e *fakeNotFoundError) Status() metav1.Status {
	return metav1.Status{
		Reason: metav1.StatusReasonNotFound,
	}
}

type fakeAlreadyExistsError struct{}

func (e *fakeAlreadyExistsError) Error() string { return "already exists" }
func (e *fakeAlreadyExistsError) Status() metav1.Status {
	return metav1.Status{
		Reason: metav1.StatusReasonAlreadyExists,
	}
}
