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
	"bytes"
	"context"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testName = "eidos"

func TestDeployer_EnsureRBAC(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Test ServiceAccount creation
	t.Run("create ServiceAccount", func(t *testing.T) {
		if err := deployer.ensureServiceAccount(ctx); err != nil {
			t.Fatalf("failed to create ServiceAccount: %v", err)
		}

		sa, err := clientset.CoreV1().ServiceAccounts(config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ServiceAccount not found: %v", err)
		}
		if sa.Name != testName {
			t.Errorf("expected SA name %q, got %q", testName, sa.Name)
		}
	})

	// Test Role creation
	t.Run("create Role", func(t *testing.T) {
		if err := deployer.ensureRole(ctx); err != nil {
			t.Fatalf("failed to create Role: %v", err)
		}

		role, err := clientset.RbacV1().Roles(config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Role not found: %v", err)
		}

		// Verify policy rules
		if len(role.Rules) != 2 {
			t.Errorf("expected 2 rules, got %d", len(role.Rules))
		}

		// Check ConfigMap rule
		rule0 := role.Rules[0]
		if len(rule0.Resources) != 1 || rule0.Resources[0] != "configmaps" {
			t.Errorf("expected configmaps in first rule, got %v", rule0.Resources)
		}
		if !containsVerb(rule0.Verbs, "create") || !containsVerb(rule0.Verbs, "update") {
			t.Errorf("expected create/update verbs, got %v", rule0.Verbs)
		}
	})

	// Test RoleBinding creation
	t.Run("create RoleBinding", func(t *testing.T) {
		if err := deployer.ensureRoleBinding(ctx); err != nil {
			t.Fatalf("failed to create RoleBinding: %v", err)
		}

		rb, err := clientset.RbacV1().RoleBindings(config.Namespace).
			Get(ctx, testName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("RoleBinding not found: %v", err)
		}

		// Verify subjects
		if len(rb.Subjects) != 1 {
			t.Errorf("expected 1 subject, got %d", len(rb.Subjects))
		}
		if rb.Subjects[0].Name != testName {
			t.Errorf("expected subject name 'eidos', got %q", rb.Subjects[0].Name)
		}

		// Verify roleRef
		if rb.RoleRef.Name != testName {
			t.Errorf("expected roleRef name 'eidos', got %q", rb.RoleRef.Name)
		}
	})

	// Test ClusterRole creation
	t.Run("create ClusterRole", func(t *testing.T) {
		if err := deployer.ensureClusterRole(ctx); err != nil {
			t.Fatalf("failed to create ClusterRole: %v", err)
		}

		cr, err := clientset.RbacV1().ClusterRoles().
			Get(ctx, "eidos-node-reader", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ClusterRole not found: %v", err)
		}

		// Verify policy rules
		if len(cr.Rules) != 4 {
			t.Errorf("expected 4 rules, got %d", len(cr.Rules))
		}
	})

	// Test ClusterRoleBinding creation
	t.Run("create ClusterRoleBinding", func(t *testing.T) {
		if err := deployer.ensureClusterRoleBinding(ctx); err != nil {
			t.Fatalf("failed to create ClusterRoleBinding: %v", err)
		}

		crb, err := clientset.RbacV1().ClusterRoleBindings().
			Get(ctx, "eidos-node-reader", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ClusterRoleBinding not found: %v", err)
		}

		// Verify subjects
		if len(crb.Subjects) != 1 {
			t.Errorf("expected 1 subject, got %d", len(crb.Subjects))
		}

		// Verify roleRef
		if crb.RoleRef.Name != "eidos-node-reader" {
			t.Errorf("expected roleRef name 'eidos-node-reader', got %q", crb.RoleRef.Name)
		}
	})
}

func TestDeployer_EnsureRBAC_Idempotent(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Create resources twice - second call should be idempotent
	if err := deployer.ensureServiceAccount(ctx); err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	if err := deployer.ensureServiceAccount(ctx); err != nil {
		t.Fatalf("second create failed (not idempotent): %v", err)
	}

	// Verify only one ServiceAccount exists
	saList, err := clientset.CoreV1().ServiceAccounts(config.Namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list ServiceAccounts: %v", err)
	}
	if len(saList.Items) != 1 {
		t.Errorf("expected 1 ServiceAccount, got %d", len(saList.Items))
	}
}

func TestDeployer_EnsureJob(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
		Privileged:         true, // Test privileged mode (default for agent deployment)
		NodeSelector: map[string]string{
			"nodeGroup": "customer-gpu",
		},
		Tolerations: []corev1.Toleration{
			{
				Key:      "dedicated",
				Operator: corev1.TolerationOpEqual,
				Value:    "user-workload",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		},
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	t.Run("create Job", func(t *testing.T) {
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("failed to create Job: %v", err)
		}

		job, err := clientset.BatchV1().Jobs(config.Namespace).
			Get(ctx, config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Job not found: %v", err)
		}

		// Verify Job spec
		if job.Spec.Template.Spec.ServiceAccountName != config.ServiceAccountName {
			t.Errorf("expected ServiceAccountName %q, got %q",
				config.ServiceAccountName, job.Spec.Template.Spec.ServiceAccountName)
		}

		// Verify host settings
		if !job.Spec.Template.Spec.HostPID {
			t.Error("expected HostPID to be true")
		}
		if !job.Spec.Template.Spec.HostNetwork {
			t.Error("expected HostNetwork to be true")
		}
		if !job.Spec.Template.Spec.HostIPC {
			t.Error("expected HostIPC to be true")
		}

		// Verify node selector
		if job.Spec.Template.Spec.NodeSelector["nodeGroup"] != "customer-gpu" {
			t.Errorf("expected nodeGroup=customer-gpu, got %v", job.Spec.Template.Spec.NodeSelector)
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
		if container.Image != config.Image {
			t.Errorf("expected image %q, got %q", config.Image, container.Image)
		}

		// Verify volumes
		if len(job.Spec.Template.Spec.Volumes) != 3 {
			t.Errorf("expected 3 volumes, got %d", len(job.Spec.Template.Spec.Volumes))
		}
	})

	t.Run("recreate Job deletes old one", func(t *testing.T) {
		// Create Job first time
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("first create failed: %v", err)
		}

		// Create Job second time - should delete and recreate
		if err := deployer.ensureJob(ctx); err != nil {
			t.Fatalf("second create failed: %v", err)
		}

		// Verify Job still exists (fake client doesn't support watch/wait,
		// but we can verify the Job exists)
		_, err := clientset.BatchV1().Jobs(config.Namespace).
			Get(ctx, config.JobName, metav1.GetOptions{})
		if err != nil {
			t.Errorf("Job should exist after recreate: %v", err)
		}
	})
}

func TestDeployer_EnsureJob_Unprivileged(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
		Privileged:         false, // Test unprivileged mode for PSS-restricted namespaces
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	if err := deployer.ensureJob(ctx); err != nil {
		t.Fatalf("failed to create Job: %v", err)
	}

	job, err := clientset.BatchV1().Jobs(config.Namespace).
		Get(ctx, config.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job not found: %v", err)
	}

	// Verify NO host settings (PSS-compliant)
	if job.Spec.Template.Spec.HostPID {
		t.Error("expected HostPID to be false in unprivileged mode")
	}
	if job.Spec.Template.Spec.HostNetwork {
		t.Error("expected HostNetwork to be false in unprivileged mode")
	}
	if job.Spec.Template.Spec.HostIPC {
		t.Error("expected HostIPC to be false in unprivileged mode")
	}

	// Verify pod security context
	psc := job.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("expected pod SecurityContext to be set")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("expected RunAsNonRoot to be true")
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("expected SeccompProfile to be RuntimeDefault")
	}

	// Verify container security context
	container := job.Spec.Template.Spec.Containers[0]
	csc := container.SecurityContext
	if csc == nil {
		t.Fatal("expected container SecurityContext to be set")
	}
	if csc.Privileged == nil || *csc.Privileged {
		t.Error("expected Privileged to be false")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("expected AllowPrivilegeEscalation to be false")
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Error("expected ReadOnlyRootFilesystem to be true")
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) == 0 {
		t.Error("expected capabilities to drop ALL")
	}

	// Verify only 1 volume (tmp, no hostPath)
	if len(job.Spec.Template.Spec.Volumes) != 1 {
		t.Errorf("expected 1 volume, got %d", len(job.Spec.Template.Spec.Volumes))
	}
	if job.Spec.Template.Spec.Volumes[0].HostPath != nil {
		t.Error("expected no hostPath volumes in unprivileged mode")
	}
}

func TestDeployer_Deploy(t *testing.T) {
	clientset := fake.NewClientset()

	// Mock SelfSubjectAccessReview to allow all permissions
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
				Reason:  "test permissions allowed",
			},
		}, nil
	})

	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Deploy should create all resources
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Verify ServiceAccount
	_, err := clientset.CoreV1().ServiceAccounts(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}

	// Verify Role
	_, err = clientset.RbacV1().Roles(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Role not created: %v", err)
	}

	// Verify RoleBinding
	_, err = clientset.RbacV1().RoleBindings(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("RoleBinding not created: %v", err)
	}

	// Verify ClusterRole
	_, err = clientset.RbacV1().ClusterRoles().
		Get(ctx, "eidos-node-reader", metav1.GetOptions{})
	if err != nil {
		t.Errorf("ClusterRole not created: %v", err)
	}

	// Verify ClusterRoleBinding
	_, err = clientset.RbacV1().ClusterRoleBindings().
		Get(ctx, "eidos-node-reader", metav1.GetOptions{})
	if err != nil {
		t.Errorf("ClusterRoleBinding not created: %v", err)
	}

	// Verify Job
	_, err = clientset.BatchV1().Jobs(config.Namespace).
		Get(ctx, config.JobName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Job not created: %v", err)
	}
}

func TestDeployer_Cleanup(t *testing.T) {
	clientset := fake.NewClientset()

	// Mock SelfSubjectAccessReview to allow all permissions
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
				Reason:  "test permissions allowed",
			},
		}, nil
	})

	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Deploy first
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Cleanup without enabled flag (should keep everything)
	if err := deployer.Cleanup(ctx, CleanupOptions{Enabled: false}); err != nil {
		t.Fatalf("Cleanup() failed: %v", err)
	}

	// Job should still exist (cleanup disabled)
	_, err := clientset.BatchV1().Jobs(config.Namespace).
		Get(ctx, config.JobName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Job should still exist when cleanup disabled: %v", err)
	}

	// ServiceAccount should still exist
	_, err = clientset.CoreV1().ServiceAccounts(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("ServiceAccount should still exist: %v", err)
	}

	// Cleanup with enabled flag
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() with Enabled failed: %v", cleanupErr)
	}

	// Job should be deleted
	_, err = clientset.BatchV1().Jobs(config.Namespace).
		Get(ctx, config.JobName, metav1.GetOptions{})
	if err == nil {
		t.Errorf("Job should be deleted")
	}
}

func TestDeployer_Cleanup_AttemptsAllDeletions(t *testing.T) {
	clientset := fake.NewClientset()

	// Mock SelfSubjectAccessReview to allow all permissions
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
				Reason:  "test permissions allowed",
			},
		}, nil
	})

	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Deploy first
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Manually delete the Job to simulate it already being cleaned up
	// This tests that cleanup continues to delete other resources
	if err := clientset.BatchV1().Jobs(config.Namespace).Delete(ctx, config.JobName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Failed to pre-delete Job: %v", err)
	}

	// Cleanup should still succeed (Job not found is ignored)
	// and should delete all RBAC resources
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() should succeed even when Job already deleted: %v", cleanupErr)
	}

	// Verify all RBAC resources were deleted
	_, err := clientset.CoreV1().ServiceAccounts(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("ServiceAccount should be deleted")
	}

	_, err = clientset.RbacV1().Roles(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("Role should be deleted")
	}

	_, err = clientset.RbacV1().RoleBindings(config.Namespace).
		Get(ctx, testName, metav1.GetOptions{})
	if err == nil {
		t.Error("RoleBinding should be deleted")
	}

	_, err = clientset.RbacV1().ClusterRoles().
		Get(ctx, clusterRoleName, metav1.GetOptions{})
	if err == nil {
		t.Error("ClusterRole should be deleted")
	}

	_, err = clientset.RbacV1().ClusterRoleBindings().
		Get(ctx, clusterRoleName, metav1.GetOptions{})
	if err == nil {
		t.Error("ClusterRoleBinding should be deleted")
	}
}

func TestDeployer_Cleanup_ReportsAllErrors(t *testing.T) {
	clientset := fake.NewClientset()

	// Don't create any resources - cleanup will try to delete non-existent resources
	// but ignoreNotFound should make these succeed
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/eidos:latest",
		Output:             "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Cleanup on empty cluster should succeed (not found errors are ignored)
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() should succeed when resources don't exist: %v", cleanupErr)
	}
}

func TestParseConfigMapName(t *testing.T) {
	tests := []struct {
		name          string
		uri           string
		wantNamespace string
		wantName      string
		wantErr       bool
	}{
		{
			name:          "valid URI",
			uri:           "cm://gpu-operator/eidos-snapshot",
			wantNamespace: "gpu-operator",
			wantName:      "eidos-snapshot",
			wantErr:       false,
		},
		{
			name:          "valid URI with hyphens",
			uri:           "cm://my-namespace/my-configmap",
			wantNamespace: "my-namespace",
			wantName:      "my-configmap",
			wantErr:       false,
		},
		{
			name:    "invalid prefix",
			uri:     "configmap://namespace/name",
			wantErr: true,
		},
		{
			name:    "missing namespace",
			uri:     "cm:///name",
			wantErr: true,
		},
		{
			name:    "missing name",
			uri:     "cm://namespace/",
			wantErr: true,
		},
		{
			name:    "no slashes",
			uri:     "cm://",
			wantErr: true,
		},
		{
			name:    "empty string",
			uri:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, name, err := parseConfigMapName(tt.uri)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseConfigMapName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if namespace != tt.wantNamespace {
					t.Errorf("namespace = %q, want %q", namespace, tt.wantNamespace)
				}
				if name != tt.wantName {
					t.Errorf("name = %q, want %q", name, tt.wantName)
				}
			}
		})
	}
}

func TestDeployer_GetSnapshot(t *testing.T) {
	// Create ConfigMap with snapshot data
	snapshotYAML := `apiVersion: eidos.nvidia.com/v1alpha1
kind: Snapshot
metadata:
  created: "2025-01-15T10:30:00Z"
measurements:
  - type: os
    subtypes:
      - name: release
        data:
          ID: ubuntu
`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eidos-snapshot",
			Namespace: "test-namespace",
		},
		Data: map[string]string{
			"snapshot.yaml": snapshotYAML,
		},
	}

	clientset := fake.NewClientset(cm)
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Get snapshot
	data, err := deployer.GetSnapshot(ctx)
	if err != nil {
		t.Fatalf("GetSnapshot() failed: %v", err)
	}

	if string(data) != snapshotYAML {
		t.Errorf("GetSnapshot() = %q, want %q", string(data), snapshotYAML)
	}
}

func TestDeployer_GetSnapshot_NotFound(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should fail because ConfigMap doesn't exist
	_, err := deployer.GetSnapshot(ctx)
	if err == nil {
		t.Error("GetSnapshot() should fail when ConfigMap doesn't exist")
	}
}

func TestDeployer_GetSnapshot_MissingKey(t *testing.T) {
	// Create ConfigMap without snapshot.yaml key
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eidos-snapshot",
			Namespace: "test-namespace",
		},
		Data: map[string]string{
			"wrong-key": "some data",
		},
	}

	clientset := fake.NewClientset(cm)
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should fail because key doesn't exist
	_, err := deployer.GetSnapshot(ctx)
	if err == nil {
		t.Error("GetSnapshot() should fail when snapshot.yaml key is missing")
	}
}

func TestDeployer_WaitForPodReady(t *testing.T) {
	// Create a Pod in Running state
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eidos-xyz",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"app.kubernetes.io/name": "eidos",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	clientset := fake.NewClientset(pod)
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should succeed because Pod is Running
	err := deployer.WaitForPodReady(ctx, 1*time.Second)
	if err != nil {
		t.Errorf("WaitForPodReady() failed: %v", err)
	}
}

func TestDeployer_WaitForPodReady_NoPod(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should timeout because no Pod exists
	err := deployer.WaitForPodReady(ctx, 100*time.Millisecond)
	if err == nil {
		t.Error("WaitForPodReady() should fail when no Pod exists")
	}
}

func TestDeployer_WaitForPodReady_PodFailed(t *testing.T) {
	// Create a Pod in Failed state
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eidos-xyz",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"app.kubernetes.io/name": "eidos",
			},
		},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Message: "container exited with error",
		},
	}

	clientset := fake.NewClientset(pod)
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should fail because Pod failed
	err := deployer.WaitForPodReady(ctx, 1*time.Second)
	if err == nil {
		t.Error("WaitForPodReady() should fail when Pod is in Failed state")
	}
}

func TestDeployer_StreamLogs_NoPod(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/eidos-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should fail because no Pod exists
	var buf bytes.Buffer
	err := deployer.StreamLogs(ctx, &buf, "[agent]")
	if err == nil {
		t.Error("StreamLogs() should fail when no Pod exists")
	}
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
