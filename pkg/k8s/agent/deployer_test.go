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

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testName = "aicr"

// testRunID is a fixed, well-formed run ID (the shape runid.Generate emits:
// UTC timestamp + 16 hex bytes). Tests that exercise production naming must
// set Config.RunID — leaving it empty falls back to unscoped names no
// production caller ever builds.
const testRunID = "20260821-142233-9f3a1c0b7e2d4a55"

func TestDeployer_EnsureRBAC(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Test Namespace creation
	t.Run("create Namespace", func(t *testing.T) {
		if err := deployer.ensureNamespace(ctx); err != nil {
			t.Fatalf("failed to create Namespace: %v", err)
		}

		ns, err := clientset.CoreV1().Namespaces().
			Get(ctx, config.Namespace, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Namespace not found: %v", err)
		}
		if ns.Labels["app.kubernetes.io/managed-by"] != "aicr" {
			t.Errorf("expected managed-by label 'aicr', got %q", ns.Labels["app.kubernetes.io/managed-by"])
		}
	})

	// Test Namespace idempotency
	t.Run("create Namespace idempotent", func(t *testing.T) {
		if err := deployer.ensureNamespace(ctx); err != nil {
			t.Fatalf("second create failed (not idempotent): %v", err)
		}
	})

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
			t.Errorf("expected subject name 'aicr', got %q", rb.Subjects[0].Name)
		}

		// Verify roleRef
		if rb.RoleRef.Name != testName {
			t.Errorf("expected roleRef name 'aicr', got %q", rb.RoleRef.Name)
		}
	})

	// Test ClusterRole creation
	t.Run("create ClusterRole", func(t *testing.T) {
		if err := deployer.ensureClusterRole(ctx); err != nil {
			t.Fatalf("failed to create ClusterRole: %v", err)
		}

		cr, err := clientset.RbacV1().ClusterRoles().
			Get(ctx, deployer.clusterRoleName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ClusterRole not found: %v", err)
		}

		// Default: nodes, pods, clusterpolicies, read-only Slinky CRs, and
		// official MariaDB CRs.
		if len(cr.Rules) != 5 {
			t.Errorf("expected 5 rules, got %d", len(cr.Rules))
		}
		ruleTests := []struct {
			name      string
			apiGroups []string
			resources []string
			verbs     []string
		}{
			{
				name:      "Slinky",
				apiGroups: []string{slinkyAPIGroup},
				resources: []string{
					slinkyControllerResource,
					slinkyNodeSetResource,
					slinkyLoginSetResource,
					slinkyRestAPIResource,
					slinkyAccountingResource,
				},
				verbs: []string{verbList},
			},
			{
				name:      "MariaDB",
				apiGroups: []string{mariaDBAPIGroup},
				resources: []string{mariaDBResource},
				verbs:     []string{verbList},
			},
		}
		for _, tt := range ruleTests {
			t.Run(tt.name, func(t *testing.T) {
				var actual *rbacv1.PolicyRule
				for i := range cr.Rules {
					rule := &cr.Rules[i]
					if slices.Equal(rule.APIGroups, tt.apiGroups) &&
						slices.Equal(rule.Resources, tt.resources) {

						actual = rule
						break
					}
				}
				if actual == nil {
					t.Fatalf(
						"expected %s resource list rule with API groups %v and resources %v",
						tt.name,
						tt.apiGroups,
						tt.resources,
					)
				}
				if !slices.Equal(actual.Verbs, tt.verbs) {
					t.Errorf("%s verbs = %v, want %v", tt.name, actual.Verbs, tt.verbs)
				}
			})
		}
	})

	// Discover-network mode pulls in l8k's extra cluster-scoped rules
	// (CRDs, bootstrap workload resources, pods/exec, nodes/patch,
	// nicdevices, nicclusterpolicies). Verify the rule appears for the
	// most discovery-specific resource (mellanox.com NicClusterPolicy
	// SSA patch) — that's the marker rule the snapshot Job needs to
	// run successfully under non-cluster-admin RBAC.
	t.Run("ClusterRole gains discovery rules when DiscoverNetwork is set", func(t *testing.T) {
		discoverClientset := fake.NewClientset()
		discoverConfig := config
		discoverConfig.DiscoverNetwork = true
		d := NewDeployer(discoverClientset, discoverConfig)
		if err := d.ensureClusterRole(ctx); err != nil {
			t.Fatalf("failed to create ClusterRole: %v", err)
		}
		cr, err := discoverClientset.RbacV1().ClusterRoles().
			Get(ctx, d.clusterRoleName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ClusterRole not found: %v", err)
		}
		hasNCPPatch := false
		hasPodsExec := false
		hasNodesPatch := false
		for _, r := range cr.Rules {
			for _, g := range r.APIGroups {
				if g == "mellanox.com" {
					for _, res := range r.Resources {
						if res == "nicclusterpolicies" {
							for _, v := range r.Verbs {
								if v == "patch" {
									hasNCPPatch = true
								}
							}
						}
					}
				}
			}
			for _, res := range r.Resources {
				if res == "pods/exec" {
					for _, v := range r.Verbs {
						if v == "create" {
							hasPodsExec = true
						}
					}
				}
				if res == "nodes" {
					for _, v := range r.Verbs {
						if v == "patch" {
							hasNodesPatch = true
						}
					}
				}
			}
		}
		if !hasNCPPatch {
			t.Error("expected mellanox.com/nicclusterpolicies patch rule")
		}
		if !hasPodsExec {
			t.Error("expected pods/exec create rule")
		}
		if !hasNodesPatch {
			t.Error("expected nodes/patch rule")
		}
	})

	// Test ClusterRoleBinding creation
	t.Run("create ClusterRoleBinding", func(t *testing.T) {
		if err := deployer.ensureClusterRoleBinding(ctx); err != nil {
			t.Fatalf("failed to create ClusterRoleBinding: %v", err)
		}

		crb, err := clientset.RbacV1().ClusterRoleBindings().
			Get(ctx, deployer.clusterRoleName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ClusterRoleBinding not found: %v", err)
		}

		// Verify subjects
		if len(crb.Subjects) != 1 {
			t.Errorf("expected 1 subject, got %d", len(crb.Subjects))
		}

		// Verify roleRef
		if crb.RoleRef.Name != deployer.clusterRoleName() {
			t.Errorf("expected roleRef name %q, got %q", deployer.clusterRoleName(), crb.RoleRef.Name)
		}
	})
}

func TestDeployer_EnsureJob(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
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
}

func TestDeployer_EnsureJob_Unprivileged(t *testing.T) {
	clientset := fake.NewClientset()
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
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
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Deploy should create all resources
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Verify Namespace
	_, err := clientset.CoreV1().Namespaces().
		Get(ctx, config.Namespace, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Namespace not created: %v", err)
	}

	// Verify ServiceAccount
	_, err = clientset.CoreV1().ServiceAccounts(config.Namespace).
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
		Get(ctx, deployer.clusterRoleName(), metav1.GetOptions{})
	if err != nil {
		t.Errorf("ClusterRole not created: %v", err)
	}

	// Verify ClusterRoleBinding
	_, err = clientset.RbacV1().ClusterRoleBindings().
		Get(ctx, deployer.clusterRoleName(), metav1.GetOptions{})
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

// TestDeployUsesRunScopedNamesAndLabels verifies that Deploy() creates every
// object under a run-scoped name (prefix-runID) and that the Job's pod
// template carries the full label set, not just the Job object itself —
// Job labels do not propagate to the Pods a Job creates.
//
// Deviation from the plan's literal test: the brief's snippet omits the
// SelfSubjectAccessReview-allow reactor that every other Deploy()-calling
// test in this file installs. The fake clientset denies all permission
// checks by default (Status.Allowed defaults to false), so Deploy() fails
// at the Step-0 CheckPermissions gate before creating anything — a false
// RED unrelated to run-scoped naming. Added the same reactor used by
// TestDeployer_Deploy et al. so the test exercises the behavior it names.
func TestDeployUsesRunScopedNamesAndLabels(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
				Reason:  "test permissions allowed",
			},
		}, nil
	})
	d := NewDeployer(client, Config{
		Namespace: "test-ns",
		Image:     "aicr:test",
		RunID:     "20260821-142233-9f3a1c0b7e2d4a55",
	})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantSuffix := "-20260821-142233-9f3a1c0b7e2d4a55"
	job, err := client.BatchV1().Jobs("test-ns").Get(ctx, "aicr"+wantSuffix, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job not found under run-scoped name: %v", err)
	}
	for _, key := range []string{labels.Name, labels.ManagedBy, labels.Component, labels.RunID} {
		if _, ok := job.Spec.Template.Labels[key]; !ok {
			t.Errorf("pod template missing label %q", key)
		}
	}
	if _, err := client.RbacV1().ClusterRoles().Get(ctx, "aicr-node-reader"+wantSuffix, metav1.GetOptions{}); err != nil {
		t.Errorf("ClusterRole not found under run-scoped name: %v", err)
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

	// JobName / ServiceAccountName are prefixes: the objects Cleanup must
	// find are named "<prefix>-<RunID>", so RunID is set here to exercise
	// the same naming production uses.
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		RunID:              testRunID,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()
	scopedJob := testName + "-" + testRunID
	scopedSA := testName + "-" + testRunID

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
		Get(ctx, scopedJob, metav1.GetOptions{})
	if err != nil {
		t.Errorf("Job should still exist when cleanup disabled: %v", err)
	}

	// ServiceAccount should still exist
	_, err = clientset.CoreV1().ServiceAccounts(config.Namespace).
		Get(ctx, scopedSA, metav1.GetOptions{})
	if err != nil {
		t.Errorf("ServiceAccount should still exist: %v", err)
	}

	// Cleanup with enabled flag
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() with Enabled failed: %v", cleanupErr)
	}

	// Job should be deleted
	_, err = clientset.BatchV1().Jobs(config.Namespace).
		Get(ctx, scopedJob, metav1.GetOptions{})
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

	// RunID set for the same reason as TestDeployer_Cleanup: without it the
	// test asserts on bare names production never creates.
	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		RunID:              testRunID,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()
	scopedName := testName + "-" + testRunID

	// Deploy first
	if err := deployer.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() failed: %v", err)
	}

	// Manually delete the Job to simulate it already being cleaned up
	// This tests that cleanup continues to delete other resources
	if err := clientset.BatchV1().Jobs(config.Namespace).Delete(ctx, scopedName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Failed to pre-delete Job: %v", err)
	}

	// Cleanup should still succeed (Job not found is ignored)
	// and should delete all RBAC resources
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() should succeed even when Job already deleted: %v", cleanupErr)
	}

	// Verify all RBAC resources were deleted
	_, err := clientset.CoreV1().ServiceAccounts(config.Namespace).
		Get(ctx, scopedName, metav1.GetOptions{})
	if err == nil {
		t.Error("ServiceAccount should be deleted")
	}

	_, err = clientset.RbacV1().Roles(config.Namespace).
		Get(ctx, scopedName, metav1.GetOptions{})
	if err == nil {
		t.Error("Role should be deleted")
	}

	_, err = clientset.RbacV1().RoleBindings(config.Namespace).
		Get(ctx, scopedName, metav1.GetOptions{})
	if err == nil {
		t.Error("RoleBinding should be deleted")
	}

	_, err = clientset.RbacV1().ClusterRoles().
		Get(ctx, deployer.clusterRoleName(), metav1.GetOptions{})
	if err == nil {
		t.Error("ClusterRole should be deleted")
	}

	_, err = clientset.RbacV1().ClusterRoleBindings().
		Get(ctx, deployer.clusterRoleName(), metav1.GetOptions{})
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
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Cleanup on empty cluster should succeed (not found errors are ignored)
	if cleanupErr := deployer.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() should succeed when resources don't exist: %v", cleanupErr)
	}
}

// TestCleanupDeletesOnlyWhatItCreated verifies Cleanup builds its delete
// list from the created-set (Deploy's own objects), not from configured
// names — a foreign object that happens to share no name with this run
// must survive even though Cleanup is enabled.
//
// Deviation from the plan's literal test: as with
// TestDeployUsesRunScopedNamesAndLabels above, the brief's snippet omits
// the SelfSubjectAccessReview-allow reactor. Without it the fake clientset
// denies all permission checks by default, so Deploy() fails at the Step-0
// CheckPermissions gate before creating anything — a false RED unrelated to
// created-set cleanup scoping. Added the same reactor used elsewhere in
// this file.
func TestCleanupDeletesOnlyWhatItCreated(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{
				Allowed: true,
				Reason:  "test permissions allowed",
			},
		}, nil
	})

	// A foreign object sharing no name with this run must survive.
	foreign := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "aicr-other", Namespace: "test-ns"}}
	if _, err := client.CoreV1().ServiceAccounts("test-ns").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	d := NewDeployer(client, Config{Namespace: "test-ns", Image: "aicr:test", RunID: "20260821-142233-9f3a1c0b7e2d4a55"})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := client.CoreV1().ServiceAccounts("test-ns").Get(ctx, "aicr-other", metav1.GetOptions{}); err != nil {
		t.Errorf("Cleanup deleted a ServiceAccount it did not create: %v", err)
	}
	if _, err := client.CoreV1().ServiceAccounts("test-ns").Get(ctx, "aicr-20260821-142233-9f3a1c0b7e2d4a55", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Cleanup did not delete its own ServiceAccount, err = %v", err)
	}
}

// TestCleanupPassesUIDPrecondition verifies every delete Cleanup issues
// carries Preconditions.UID set to the UID recorded at create time. The
// fake clientset's ObjectTracker neither assigns UIDs on Create nor
// enforces Preconditions on Delete (it ignores DeleteOptions entirely), so
// this records a known UID directly via recordCreated and spies on the
// outgoing delete action rather than relying on tracker behavior.
func TestCleanupPassesUIDPrecondition(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	const wantUID = types.UID("sa-uid-123")
	var sawUID types.UID
	var sawPreconditions bool
	client.PrependReactor("delete", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		da, ok := action.(k8stesting.DeleteActionImpl)
		if ok && da.DeleteOptions.Preconditions != nil && da.DeleteOptions.Preconditions.UID != nil {
			sawPreconditions = true
			sawUID = *da.DeleteOptions.Preconditions.UID
		}
		return false, nil, nil // not handled: fall through to the default tracker delete
	})

	d := NewDeployer(client, Config{Namespace: "test-ns"})
	d.recordCreated(kindServiceAccount, "aicr-sa", wantUID)

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if !sawPreconditions {
		t.Fatal("ServiceAccount delete did not carry Preconditions.UID")
	}
	if sawUID != wantUID {
		t.Errorf("Preconditions.UID = %q, want %q", sawUID, wantUID)
	}
}

// TestCleanupTreatsConflictAsSuccess verifies a Conflict response (the UID
// precondition did not match — the name now belongs to a different object)
// is treated as success, same as NotFound, rather than surfaced as a
// Cleanup failure.
func TestCleanupTreatsConflictAsSuccess(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "test permissions allowed"},
		}, nil
	})
	client.PrependReactor("delete", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "serviceaccounts"}, "aicr", errors.New("uid mismatch"))
	})

	d := NewDeployer(client, Config{Namespace: "test-ns", Image: "aicr:test", RunID: "20260821-142233-9f3a1c0b7e2d4a55"})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() should treat a Conflict delete response as success, got: %v", err)
	}
}

// TestRecordCreatedAndJobUID verifies jobUID() returns the zero UID before
// any Job is recorded, and the recorded Job's UID afterward — even when
// other kinds have been recorded too.
func TestRecordCreatedAndJobUID(t *testing.T) {
	d := NewDeployer(fake.NewSimpleClientset(), Config{Namespace: "test-ns"})

	if got := d.jobUID(); got != "" {
		t.Fatalf("jobUID() before any Job recorded = %q, want zero UID", got)
	}

	d.recordCreated(kindServiceAccount, "aicr-sa", types.UID("sa-uid"))
	if got := d.jobUID(); got != "" {
		t.Fatalf("jobUID() after recording a non-Job kind = %q, want zero UID", got)
	}

	d.recordCreated(kindJob, "aicr-job", types.UID("job-uid"))
	if got := d.jobUID(); got != "job-uid" {
		t.Fatalf("jobUID() = %q, want %q", got, "job-uid")
	}
}

// TestCreatedSnapshotIsDefensiveCopy verifies createdSnapshot returns a copy
// that mutation cannot use to corrupt the Deployer's internal created-set.
func TestCreatedSnapshotIsDefensiveCopy(t *testing.T) {
	d := NewDeployer(fake.NewSimpleClientset(), Config{Namespace: "test-ns"})
	d.recordCreated(kindServiceAccount, "aicr-sa", types.UID("sa-uid"))

	snap := d.createdSnapshot()
	if len(snap) != 1 {
		t.Fatalf("createdSnapshot() length = %d, want 1", len(snap))
	}
	snap[0].name = "mutated"

	again := d.createdSnapshot()
	if again[0].name != "aicr-sa" {
		t.Fatalf("createdSnapshot() mutation leaked into Deployer state: got %q, want %q", again[0].name, "aicr-sa")
	}
}

// TestRecordCreatedConcurrentSafe exercises recordCreated from many
// goroutines at once so `go test -race` can catch a data race on the
// created-set if the locking is ever removed or narrowed incorrectly.
func TestRecordCreatedConcurrentSafe(t *testing.T) {
	d := NewDeployer(fake.NewSimpleClientset(), Config{Namespace: "test-ns"})

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.recordCreated(kindConfigMap, fmt.Sprintf("cm-%d", i), types.UID(fmt.Sprintf("uid-%d", i)))
		}(i)
	}
	wg.Wait()

	if got := len(d.createdSnapshot()); got != n {
		t.Fatalf("createdSnapshot() length = %d, want %d", got, n)
	}
}

// TestGetSnapshotFromConfigMap_RecordsUID_WhenOwned verifies
// getSnapshotFromConfigMap enters the staging ConfigMap into the
// created-set (for a UID-pinned Cleanup delete) only when
// Config.OwnsOutputConfigMap is true — a caller-supplied `cm://` output is
// the caller's artifact and must never be deleted by this Deployer.
func TestGetSnapshotFromConfigMap_RecordsUID_WhenOwned(t *testing.T) {
	tests := []struct {
		name       string
		ownsOutput bool
		wantRecord bool
	}{
		{name: "owned output is recorded", ownsOutput: true, wantRecord: true},
		{name: "caller-supplied output is not recorded", ownsOutput: false, wantRecord: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "aicr-snapshot",
					Namespace: "test-namespace",
					UID:       types.UID("cm-uid"),
				},
				Data: map[string]string{"snapshot.yaml": "data"},
			}
			clientset := fake.NewClientset(cm)
			d := NewDeployer(clientset, Config{
				Namespace:           "test-namespace",
				Output:              "cm://test-namespace/aicr-snapshot",
				OwnsOutputConfigMap: tt.ownsOutput,
			})

			if _, err := d.getSnapshotFromConfigMap(context.Background()); err != nil {
				t.Fatalf("getSnapshotFromConfigMap() error = %v", err)
			}

			snap := d.createdSnapshot()
			gotRecorded := len(snap) == 1 && snap[0].kind == kindConfigMap && snap[0].uid == types.UID("cm-uid")
			if gotRecorded != tt.wantRecord {
				t.Errorf("recorded = %v (snapshot = %+v), want %v", gotRecorded, snap, tt.wantRecord)
			}
		})
	}
}

// TestCleanupDeletesStagingConfigMapWhenOwned verifies Cleanup deletes the
// staging ConfigMap once getSnapshotFromConfigMap has recorded it (i.e.
// Config.OwnsOutputConfigMap was true), exercising deleteStagingConfigMap's
// dispatch from Cleanup end to end.
func TestCleanupDeletesStagingConfigMapWhenOwned(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aicr-snapshot",
			Namespace: "test-namespace",
			UID:       types.UID("cm-uid"),
		},
		Data: map[string]string{"snapshot.yaml": "data"},
	}
	clientset := fake.NewClientset(cm)
	d := NewDeployer(clientset, Config{
		Namespace:           "test-namespace",
		Output:              "cm://test-namespace/aicr-snapshot",
		OwnsOutputConfigMap: true,
	})

	if _, err := d.getSnapshotFromConfigMap(ctx); err != nil {
		t.Fatalf("getSnapshotFromConfigMap() error = %v", err)
	}

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := clientset.CoreV1().ConfigMaps("test-namespace").Get(ctx, "aicr-snapshot", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Cleanup did not delete the owned staging ConfigMap, err = %v", err)
	}
}

// TestCleanupSweepsUnrecordedStagingConfigMap covers the leak path: the
// in-pod agent wrote the staging ConfigMap, but the run failed (Job timeout,
// wait error, canceled context) before getSnapshotFromConfigMap could observe
// its UID, so nothing was recorded. With run-scoped naming that would leak one
// ConfigMap per failed run, so Cleanup Gets it by its run-scoped name and
// deletes it pinned to the UID that Get returned.
func TestCleanupSweepsUnrecordedStagingConfigMap(t *testing.T) {
	ctx := context.Background()
	name := StagingConfigMapName(testRunID)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
			UID:       types.UID("staging-uid"),
		},
		Data: map[string]string{"snapshot.yaml": "data"},
	}
	clientset := fake.NewClientset(cm)

	var sawUIDPrecondition bool
	clientset.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(k8stesting.DeleteActionImpl)
		if !ok || del.DeleteOptions.Preconditions == nil || del.DeleteOptions.Preconditions.UID == nil {
			return false, nil, nil
		}
		if *del.DeleteOptions.Preconditions.UID == types.UID("staging-uid") {
			sawUIDPrecondition = true
		}
		return false, nil, nil
	})

	d := NewDeployer(clientset, Config{
		Namespace:           "test-namespace",
		RunID:               testRunID,
		Output:              "cm://test-namespace/" + name,
		OwnsOutputConfigMap: true,
	})

	// Deliberately no getSnapshotFromConfigMap call: this is the failed run.
	if d.hasCreated(kindConfigMap) {
		t.Fatal("precondition: created-set must not hold the staging ConfigMap")
	}

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := clientset.CoreV1().ConfigMaps("test-namespace").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Cleanup leaked the staging ConfigMap, Get err = %v", err)
	}
	if !sawUIDPrecondition {
		t.Error("staging ConfigMap delete was not pinned to the observed UID")
	}
}

// TestCleanupSkipsStagingConfigMapSweepWhenNotOwned asserts the sweep stays
// ownership-scoped: a caller-supplied cm:// Output is the caller's artifact
// (OwnsOutputConfigMap false) and must survive Cleanup even when it happens to
// carry this run's staging name.
func TestCleanupSkipsStagingConfigMapSweepWhenNotOwned(t *testing.T) {
	ctx := context.Background()
	name := StagingConfigMapName(testRunID)
	clientset := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
			UID:       types.UID("callers-uid"),
		},
		Data: map[string]string{"snapshot.yaml": "data"},
	})

	d := NewDeployer(clientset, Config{
		Namespace:           "test-namespace",
		RunID:               testRunID,
		Output:              "cm://test-namespace/" + name,
		OwnsOutputConfigMap: false,
	})

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := clientset.CoreV1().ConfigMaps("test-namespace").Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("Cleanup deleted a ConfigMap this run does not own: %v", err)
	}
}

// TestCleanupSweepNoOpWhenStagingConfigMapAbsent covers the common failure
// shape — the Job never got far enough to write anything — where the sweep's
// Get is a NotFound and Cleanup must still report success.
func TestCleanupSweepNoOpWhenStagingConfigMapAbsent(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()

	d := NewDeployer(clientset, Config{
		Namespace:           "test-namespace",
		RunID:               testRunID,
		Output:              "cm://test-namespace/" + StagingConfigMapName(testRunID),
		OwnsOutputConfigMap: true,
	})

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v, want nil when the staging ConfigMap was never written", err)
	}
}

// TestCleanupSweepSurfacesUnexpectedGetError fails closed: an apiserver error
// other than NotFound while looking for the staging ConfigMap means cleanup
// cannot prove the object is gone, so it must be reported rather than
// silently swallowed.
func TestCleanupSweepSurfacesUnexpectedGetError(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewClientset()
	clientset.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver exploded"))
	})

	d := NewDeployer(clientset, Config{
		Namespace:           "test-namespace",
		RunID:               testRunID,
		Output:              "cm://test-namespace/" + StagingConfigMapName(testRunID),
		OwnsOutputConfigMap: true,
	})

	err := d.Cleanup(ctx, CleanupOptions{Enabled: true})
	if err == nil {
		t.Fatal("Cleanup() error = nil, want the unexpected Get error surfaced")
	}
	if !strings.Contains(err.Error(), StagingConfigMapName(testRunID)) {
		t.Errorf("error %q does not name the staging ConfigMap", err)
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
			uri:           "cm://gpu-operator/aicr-snapshot",
			wantNamespace: "gpu-operator",
			wantName:      "aicr-snapshot",
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
			namespace, name, err := pod.ParseConfigMapURI(tt.uri)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseConfigMapURI() error = %v, wantErr %v", err, tt.wantErr)
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
	snapshotYAML := `apiVersion: aicr.run/v1alpha2
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
			Name:      "aicr-snapshot",
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
		Output:    "cm://test-namespace/aicr-snapshot",
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
		Output:    "cm://test-namespace/aicr-snapshot",
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
			Name:      "aicr-snapshot",
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
		Output:    "cm://test-namespace/aicr-snapshot",
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
	// Create a Pod in Running state with Ready condition
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aicr-xyz",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labels.Name:  labels.ValueAICR,
				labels.RunID: "",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	clientset := fake.NewClientset(pod)
	config := Config{
		Namespace: "test-namespace",
		JobName:   testName,
		Output:    "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	// Should succeed because Pod is Ready
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
		Output:    "cm://test-namespace/aicr-snapshot",
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
			Name:      "aicr-xyz",
			Namespace: "test-namespace",
			Labels: map[string]string{
				labels.Name:  labels.ValueAICR,
				labels.RunID: "",
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
		Output:    "cm://test-namespace/aicr-snapshot",
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
		Output:    "cm://test-namespace/aicr-snapshot",
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

func TestDeployer_Deploy_NetworkError(t *testing.T) {
	clientset := fake.NewClientset()

	// Mock SelfSubjectAccessReview to return a network error (API server unreachable)
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Addr: &net.TCPAddr{
				IP:   net.ParseIP("98.95.33.159"),
				Port: 443,
			},
			Err: syscall.ECONNREFUSED,
		}
	})

	config := Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
	}
	deployer := NewDeployer(clientset, config)
	ctx := context.Background()

	err := deployer.Deploy(ctx)
	if err == nil {
		t.Fatal("Deploy() should fail with network error")
	}

	// Verify error code is ErrCodeUnavailable (not ErrCodeUnauthorized)
	var structErr *aicrerrors.StructuredError
	if !errors.As(err, &structErr) {
		t.Fatalf("expected StructuredError, got %T: %v", err, err)
	}
	if structErr.Code != aicrerrors.ErrCodeUnavailable {
		t.Errorf("expected error code %q, got %q", aicrerrors.ErrCodeUnavailable, structErr.Code)
	}

	// Verify actionable message
	if !strings.Contains(err.Error(), "cannot reach Kubernetes API server") {
		t.Errorf("expected actionable message about API server, got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "VPN") {
		t.Errorf("expected VPN hint in message, got: %s", err.Error())
	}
}

func TestDeployer_ValidateRuntimeClass(t *testing.T) {
	tests := []struct {
		name             string
		runtimeClassName string
		createRC         bool
		wantErr          bool
		wantCode         aicrerrors.ErrorCode
	}{
		{
			name:             "exists",
			runtimeClassName: "nvidia",
			createRC:         true,
			wantErr:          false,
		},
		{
			name:             "not found",
			runtimeClassName: "nvidia",
			createRC:         false,
			wantErr:          true,
			wantCode:         aicrerrors.ErrCodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()

			if tt.createRC {
				rc := &nodev1.RuntimeClass{
					ObjectMeta: metav1.ObjectMeta{Name: tt.runtimeClassName},
					Handler:    tt.runtimeClassName,
				}
				if _, err := clientset.NodeV1().RuntimeClasses().Create(
					context.Background(), rc, metav1.CreateOptions{},
				); err != nil {
					t.Fatalf("failed to create RuntimeClass: %v", err)
				}
			}

			deployer := NewDeployer(clientset, Config{RuntimeClassName: tt.runtimeClassName})
			err := deployer.validateRuntimeClass(context.Background())

			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRuntimeClass() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var structErr *aicrerrors.StructuredError
				if !errors.As(err, &structErr) {
					t.Fatalf("expected StructuredError, got %T: %v", err, err)
				}
				if structErr.Code != tt.wantCode {
					t.Errorf("error code = %q, want %q", structErr.Code, tt.wantCode)
				}
			}
		})
	}
}

func TestDeployer_Deploy_RuntimeClassNotFound(t *testing.T) {
	clientset := fake.NewClientset()

	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})

	deployer := NewDeployer(clientset, Config{
		Namespace:          "test-namespace",
		ServiceAccountName: testName,
		JobName:            testName,
		Image:              "ghcr.io/nvidia/aicr-validator:latest",
		Output:             "cm://test-namespace/aicr-snapshot",
		RuntimeClassName:   "nvidia",
	})

	err := deployer.Deploy(context.Background())
	if err == nil {
		t.Fatal("Deploy() should fail when RuntimeClass does not exist")
	}

	if !strings.Contains(err.Error(), "RuntimeClass") {
		t.Errorf("expected RuntimeClass in error message, got: %s", err.Error())
	}
}

// Helper function
func containsVerb(verbs []string, verb string) bool {
	return slices.Contains(verbs, verb)
}
