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
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	authv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

const testName = "aicr"

// testRunID is a fixed, well-formed run ID (the shape runid.Generate emits:
// UTC timestamp + 16 hex bytes). Tests that exercise production naming must
// set Config.RunID — leaving it empty falls back to unscoped names no
// production caller ever builds.
const testRunID = "20260821-142233-9f3a1c0b7e2d4a55"

func TestDeployer_EnsureRBAC(t *testing.T) {
	clientset := fake.NewClientset()
	// ServiceAccountName and JobName are prefixes, not names: every
	// run-owned object lands at "<prefix>-<RunID>". Assertions below go
	// through the deployer's own name accessors so they track the names a
	// production caller actually gets.
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
			Get(ctx, deployer.saName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ServiceAccount not found: %v", err)
		}
		if sa.Name != deployer.saName() {
			t.Errorf("expected SA name %q, got %q", deployer.saName(), sa.Name)
		}
	})

	// Test Role creation
	t.Run("create Role", func(t *testing.T) {
		if err := deployer.ensureRole(ctx); err != nil {
			t.Fatalf("failed to create Role: %v", err)
		}

		role, err := clientset.RbacV1().Roles(config.Namespace).
			Get(ctx, deployer.roleName(), metav1.GetOptions{})
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
			Get(ctx, deployer.roleName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("RoleBinding not found: %v", err)
		}

		// Verify subjects
		if len(rb.Subjects) != 1 {
			t.Errorf("expected 1 subject, got %d", len(rb.Subjects))
		}
		if rb.Subjects[0].Name != deployer.saName() {
			t.Errorf("expected subject name %q, got %q", deployer.saName(), rb.Subjects[0].Name)
		}

		// Verify roleRef
		if rb.RoleRef.Name != deployer.roleName() {
			t.Errorf("expected roleRef name %q, got %q", deployer.roleName(), rb.RoleRef.Name)
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

		// Default: nodes, pods, clusterpolicies, read-only Slinky CRs,
		// official MariaDB CRs, and read-only apps/daemonsets (OKE legacy
		// device-plugin conflict evidence).
		if len(cr.Rules) != 6 {
			t.Errorf("expected 6 rules, got %d", len(cr.Rules))
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
			{
				name:      "DaemonSets",
				apiGroups: []string{"apps"},
				resources: []string{"daemonsets"},
				verbs:     []string{verbGet, verbList},
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
		RunID:              testRunID,
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
			Get(ctx, deployer.jobName(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Job not found: %v", err)
		}

		// Verify Job spec. The pod's ServiceAccountName is the run-scoped
		// SA name, not the configured prefix.
		if job.Spec.Template.Spec.ServiceAccountName != deployer.saName() {
			t.Errorf("expected ServiceAccountName %q, got %q",
				deployer.saName(), job.Spec.Template.Spec.ServiceAccountName)
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
		RunID:              testRunID,
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
		Get(ctx, deployer.jobName(), metav1.GetOptions{})
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
		RunID:              testRunID,
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
		Get(ctx, deployer.saName(), metav1.GetOptions{})
	if err != nil {
		t.Errorf("ServiceAccount not created: %v", err)
	}

	// Verify Role
	_, err = clientset.RbacV1().Roles(config.Namespace).
		Get(ctx, deployer.roleName(), metav1.GetOptions{})
	if err != nil {
		t.Errorf("Role not created: %v", err)
	}

	// Verify RoleBinding
	_, err = clientset.RbacV1().RoleBindings(config.Namespace).
		Get(ctx, deployer.roleName(), metav1.GetOptions{})
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
		Get(ctx, deployer.jobName(), metav1.GetOptions{})
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
	client := fake.NewClientset()
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
	client := fake.NewClientset()
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

// observedDelete is one delete action captured by spyOnDeletes.
type observedDelete struct {
	resource string
	name     string
	uid      *types.UID // nil when the delete carried no UID precondition
}

// spyOnDeletes installs a reactor over EVERY resource that records each
// outgoing delete's resource, name, and Preconditions.UID, then falls through
// to the default tracker delete. Reactors run under the fake Clientset's own
// lock, but Cleanup fans its deletes out concurrently, so the slice is
// mutex-guarded regardless; the returned accessor takes the same lock.
func spyOnDeletes(client *fake.Clientset) func() []observedDelete {
	var mu sync.Mutex
	var observed []observedDelete
	client.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		da, ok := action.(k8stesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		rec := observedDelete{resource: da.GetResource().Resource, name: da.GetName()}
		if da.DeleteOptions.Preconditions != nil && da.DeleteOptions.Preconditions.UID != nil {
			uid := *da.DeleteOptions.Preconditions.UID
			rec.uid = &uid
		}
		mu.Lock()
		observed = append(observed, rec)
		mu.Unlock()
		return false, nil, nil // not handled: fall through to the default tracker delete
	})
	return func() []observedDelete {
		mu.Lock()
		defer mu.Unlock()
		out := make([]observedDelete, len(observed))
		copy(out, observed)
		return out
	}
}

// TestCleanupPassesUIDPrecondition verifies every delete Cleanup issues
// carries Preconditions.UID set to the UID recorded at create time — for all
// seven kinds deleteCreatedObject dispatches on, not just one. A reactor
// scoped to a single resource would leave the other six dispatch arms free to
// drop the precondition unnoticed, so the spy here is installed over "*".
//
// The fake clientset's ObjectTracker neither assigns UIDs on Create nor
// enforces Preconditions on Delete (it ignores DeleteOptions entirely), so
// this records known UIDs directly via recordCreated and inspects the
// outgoing delete actions rather than relying on tracker behavior.
func TestCleanupPassesUIDPrecondition(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	deletes := spyOnDeletes(client)

	// One object per kind, each with a distinct UID so a dispatch arm that
	// passed some OTHER entry's UID would be caught as well as one that
	// passed none.
	created := []struct {
		kind     string
		name     string
		resource string
		uid      types.UID
	}{
		{kindServiceAccount, "aicr-sa", "serviceaccounts", "sa-uid-123"},
		{kindRole, "aicr-role", "roles", "role-uid-123"},
		{kindRoleBinding, "aicr-rb", "rolebindings", "rb-uid-123"},
		{kindClusterRole, "aicr-cr", "clusterroles", "cr-uid-123"},
		{kindClusterRoleBinding, "aicr-crb", "clusterrolebindings", "crb-uid-123"},
		{kindJob, "aicr-job", "jobs", "job-uid-123"},
		{kindConfigMap, "aicr-agent-snapshot", "configmaps", "cm-uid-123"},
	}

	d := NewDeployer(client, Config{Namespace: "test-ns"})
	for _, c := range created {
		d.recordCreated(c.kind, c.name, c.uid)
	}

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	observed := deletes()
	if len(observed) != len(created) {
		t.Fatalf("Cleanup issued %d deletes, want %d: %+v", len(observed), len(created), observed)
	}
	for _, c := range created {
		t.Run(c.kind, func(t *testing.T) {
			idx := slices.IndexFunc(observed, func(o observedDelete) bool {
				return o.resource == c.resource && o.name == c.name
			})
			if idx < 0 {
				t.Fatalf("Cleanup issued no delete for %s %q (resource %q); observed: %+v",
					c.kind, c.name, c.resource, observed)
			}
			got := observed[idx]
			if got.uid == nil {
				t.Fatalf("%s delete did not carry Preconditions.UID", c.kind)
			}
			if *got.uid != c.uid {
				t.Errorf("%s delete Preconditions.UID = %q, want %q", c.kind, *got.uid, c.uid)
			}
		})
	}
}

// TestCleanupResolvesUnconfirmedEntryBeforeDeleting covers the
// lost-Create-response path: recordIntent enters an object BEFORE its Create,
// so an entry can reach Cleanup with no UID and no Create response ever having
// named it. The run-scoped name alone is not ownership evidence — it says what
// this run WOULD have created, not what is standing there now — so Cleanup
// must recover the UID from the live object and prove that object was created
// by THIS invocation before deleting anything. Ownership takes both halves: a
// label mismatch and a missing UID each refuse the delete on their own.
//
// The sharpest row is the same-RunID replacement. Config.RunID is public,
// caller-settable SDK surface that `aicr validate` and pinned e2e runs share
// on purpose, so a second invocation stamps identical name, managed-by,
// component and run-id labels — every key that scopes a run. Only
// labels.InvocationID, generated inside NewDeployer and settable through no
// Config field, tells the two apart, and this test is what pins that: seeding
// the object from a twin Deployer with the same Config is exactly the sequence
// that a label-set check without the invocation ID would adopt and delete.
//
// The UID precondition does not cover it. Deleting by bare name with no
// Preconditions would collect the replacement outright, and pinning to the UID
// this Get returned only rules out a replacement made AFTER the Get — one
// standing there before it is simply what the Get hands back. Passing &"" for
// a missing UID would be worse than useless: the apiserver would compare the
// empty UID against the live object's real one, reject every attempt with a
// Conflict, and ignoreNotFoundOrConflict would swallow that as success.
func TestCleanupResolvesUnconfirmedEntryBeforeDeleting(t *testing.T) {
	const ns = "test-ns"
	saName := NewDeployer(fake.NewClientset(), Config{Namespace: ns, RunID: testRunID}).saName()

	tests := []struct {
		name string
		// seedLabels builds the labels of the object standing at saName.
		// It is a function of the two Deployers rather than a literal map
		// because the interesting cases differ ONLY in the invocation ID:
		// run is the invocation that calls Cleanup, twin is a second
		// invocation configured identically to it — same RunID, same
		// names, so byte-identical run labels. noSeed leaves the name
		// empty instead.
		seedLabels func(run, twin *Deployer) map[string]string
		seedUID    types.UID
		noSeed     bool
		runID      *string // nil: testRunID
		wantDelete bool
		wantUID    types.UID // expected Preconditions.UID when wantDelete
		wantWarn   bool
	}{
		{
			name:       "this invocation's object is deleted pinned to the recovered UID",
			seedLabels: func(run, _ *Deployer) map[string]string { return run.objectLabels() },
			seedUID:    types.UID("ours-uid"),
			wantDelete: true,
			wantUID:    types.UID("ours-uid"),
		},
		{
			name:       "nothing at the name issues no delete",
			noSeed:     true,
			wantDelete: false,
		},
		{
			// THE REGRESSION. Another invocation replaced the object at
			// this name before Cleanup ran, and because Config.RunID is
			// public, caller-settable SDK surface that `aicr validate`
			// and pinned e2e runs deliberately share, its labels are
			// identical on every key the run scoping uses: name,
			// managed-by, component AND run-id. Only the invocation ID
			// differs.
			//
			// The UID precondition cannot save this one. The entry is
			// unconfirmed, so no UID was captured at create time; the
			// replacement is simply what the Get returns, and the delete
			// would be pinned to the replacement's own UID and succeed.
			// Ownership has to be settled by the labels, before any
			// delete is issued.
			name:       "a same-RunID replacement from another invocation survives",
			seedLabels: func(_, twin *Deployer) map[string]string { return twin.objectLabels() },
			seedUID:    types.UID("replacement-uid"),
			wantDelete: false,
			wantWarn:   true,
		},
		{
			name: "a replacement carrying another run's ID survives",
			seedLabels: func(_, _ *Deployer) map[string]string {
				return map[string]string{
					labels.Name:         labels.ValueAICR,
					labels.ManagedBy:    labels.ValueAICR,
					labels.Component:    labels.ValueSnapshotAgent,
					labels.RunID:        "20260822-090000-0011223344556677",
					labels.InvocationID: "20260822-090000-8899aabbccddeeff",
				}
			},
			seedUID:    types.UID("replacement-uid"),
			wantDelete: false,
			wantWarn:   true,
		},
		{
			name:       "an unlabeled object under the same name survives",
			seedLabels: func(_, _ *Deployer) map[string]string { return nil },
			seedUID:    types.UID("operators-uid"),
			wantDelete: false,
			wantWarn:   true,
		},
		{
			// Labels alone would clear this object — they are this
			// invocation's own — but a real apiserver always assigns a
			// UID, so a missing one means the delete cannot be pinned.
			// Refusing is the point: the fallback would be the bare-name
			// delete this path exists to prevent.
			name:       "this invocation's own labels without a UID still survive",
			seedLabels: func(run, _ *Deployer) map[string]string { return run.objectLabels() },
			wantDelete: false,
			wantWarn:   true,
		},
		{
			// An empty Config.RunID is not a wildcard. The seed carries
			// this invocation's own labels MINUS the run-ID key, so every
			// remaining comparison passes against an empty RunID — the
			// invocation ID included, and "" == "" for the run ID.
			// createdByThisInvocation must still match nothing, because a
			// run with no ID has no ownership to prove. Deleting that
			// guard fails this row; a label-less seed would not.
			name: "an empty RunID proves ownership of nothing",
			seedLabels: func(run, _ *Deployer) map[string]string {
				l := run.objectLabels()
				delete(l, labels.RunID)
				return l
			},
			seedUID:    types.UID("operators-uid"),
			runID:      ptr.To(""),
			wantDelete: false,
			wantWarn:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			logs := captureLogs(t)
			client := fake.NewClientset()

			runID := testRunID
			if tt.runID != nil {
				runID = *tt.runID
			}
			cfg := Config{Namespace: ns, RunID: runID}
			run := NewDeployer(client, cfg)
			// Same clientset, same Config, separate NewDeployer call:
			// everything a second invocation reusing this run's RunID
			// would have, including its own invocation ID.
			twin := NewDeployer(client, cfg)
			if run.invocationID == twin.invocationID {
				t.Fatal("precondition: two Deployers must not share an invocation ID")
			}

			if !tt.noSeed {
				if _, err := client.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{
						Name: saName, Namespace: ns, UID: tt.seedUID,
						Labels: tt.seedLabels(run, twin),
					},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			deletes := spyOnDeletes(client)

			run.recordIntent(kindServiceAccount, saName)

			if err := run.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
				t.Fatalf("Cleanup() error = %v", err)
			}

			observed := deletes()
			if !tt.wantDelete {
				if len(observed) != 0 {
					t.Fatalf("Cleanup issued %d deletes, want none: %+v", len(observed), observed)
				}
				if !tt.noSeed {
					live, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
					if err != nil {
						t.Fatalf("Cleanup deleted an object this run cannot prove it created: %v", err)
					}
					if live.UID != tt.seedUID {
						t.Errorf("surviving object UID = %q, want %q", live.UID, tt.seedUID)
					}
				}
			} else {
				if len(observed) != 1 {
					t.Fatalf("Cleanup issued %d deletes, want 1: %+v", len(observed), observed)
				}
				if observed[0].uid == nil || *observed[0].uid != tt.wantUID {
					t.Errorf("delete Preconditions.UID = %v, want %q", observed[0].uid, tt.wantUID)
				}
			}

			gotWarn := strings.Contains(logs.String(), "cannot prove this run created")
			if gotWarn != tt.wantWarn {
				t.Errorf("warned about an ambiguous orphan = %v, want %v; logs: %s", gotWarn, tt.wantWarn, logs.String())
			}
			if tt.wantWarn && !strings.Contains(logs.String(), saName) {
				t.Errorf("warning does not name the object left behind; logs: %s", logs.String())
			}
		})
	}
}

// TestCreatedByThisInvocationRequiresThisInvocationsLabel pins the predicate
// Cleanup's adoption branch turns on, including the two shapes the end-to-end
// Cleanup test cannot reach: a Deployer built as a struct literal rather than
// through NewDeployer, which has no invocation ID at all.
//
// Every row that must be refused is refused because of a MISSING or DIFFERENT
// invocation ID while all four run labels agree — the reusable-label case. A
// row that also differed on run-id would prove nothing about this check.
func TestCreatedByThisInvocationRequiresThisInvocationsLabel(t *testing.T) {
	cfg := Config{Namespace: "test-ns", RunID: testRunID}
	d := NewDeployer(fake.NewClientset(), cfg)
	twin := NewDeployer(fake.NewClientset(), cfg)

	// A Deployer that never went through NewDeployer: no invocation ID, and
	// so nothing to prove authorship with. Its own objectLabels() omit the
	// key entirely, so both sides of the comparison are "" — the shape that
	// must NOT read as agreement.
	literal := &Deployer{clientset: fake.NewClientset(), config: cfg}

	tests := []struct {
		name      string
		d         *Deployer
		objLabels map[string]string
		want      bool
	}{
		{
			name:      "this invocation's own labels",
			d:         d,
			objLabels: d.objectLabels(),
			want:      true,
		},
		{
			name:      "a twin invocation sharing the RunID",
			d:         d,
			objLabels: twin.objectLabels(),
			want:      false,
		},
		{
			name: "run labels with the invocation ID missing",
			d:    d,
			objLabels: map[string]string{
				labels.Name:      labels.ValueAICR,
				labels.ManagedBy: labels.ValueAICR,
				labels.Component: labels.ValueSnapshotAgent,
				labels.RunID:     testRunID,
			},
			want: false,
		},
		{
			name:      "a Deployer with no invocation ID against its own labels",
			d:         literal,
			objLabels: literal.objectLabels(),
			want:      false,
		},
		{
			name:      "a Deployer with no invocation ID against a real one's labels",
			d:         literal,
			objLabels: d.objectLabels(),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.createdByThisInvocation(tt.objLabels); got != tt.want {
				t.Errorf("createdByThisInvocation(%v) = %v, want %v", tt.objLabels, got, tt.want)
			}
		})
	}
}

// TestCleanupSurfacesIntentResolutionGetError fails closed on an apiserver
// error other than NotFound while resolving an unconfirmed entry: cleanup can
// neither prove the object is ours nor prove it is gone, so it must report the
// failure rather than delete blind or silently skip.
func TestCleanupSurfacesIntentResolutionGetError(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	client.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver exploded"))
	})
	deletes := spyOnDeletes(client)

	d := NewDeployer(client, Config{Namespace: "test-ns", RunID: testRunID})
	d.recordIntent(kindServiceAccount, d.saName())

	err := d.Cleanup(ctx, CleanupOptions{Enabled: true})
	if err == nil {
		t.Fatal("Cleanup() error = nil, want the unexpected Get error surfaced")
	}
	if !strings.Contains(err.Error(), d.saName()) {
		t.Errorf("error %q does not name the unresolved object", err)
	}
	if observed := deletes(); len(observed) != 0 {
		t.Errorf("Cleanup issued %d deletes despite an unresolvable entry: %+v", len(observed), observed)
	}
}

// TestEnsureRecordsIntentBeforeCreate is the reason recordIntent exists: an
// apiserver that commits a Create but never delivers the response (client
// timeout, apiserver rollout, LB 502/504, connection reset) must not leave an
// object nothing will ever delete. The run-scoped name means no later run
// reclaims it, so the orphan would be permanent.
//
// The reactor below reproduces exactly that: the object is written into the
// tracker — with a UID, as a real apiserver assigns and the fake ObjectTracker
// does not — and THEN an error is returned, so ensureServiceAccount fails while
// the ServiceAccount exists. Cleanup must still delete it, and (since the
// Create response never named it) must delete it pinned to the UID it
// recovers from the live object, not by bare name.
func TestEnsureRecordsIntentBeforeCreate(t *testing.T) {
	ctx := context.Background()
	const ns = "test-ns"
	client := fake.NewClientset()
	deletes := spyOnDeletes(client)

	d := NewDeployer(client, Config{Namespace: ns, RunID: testRunID})
	saName := d.saName()
	committedUID := types.UID(saName + "-uid")

	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		sa, ok := ca.GetObject().(*corev1.ServiceAccount)
		if !ok {
			return false, nil, nil
		}
		// Commit the object the way a real apiserver would, UID and all...
		sa.UID = committedUID
		if err := client.Tracker().Create(ca.GetResource(), sa, ns); err != nil {
			return true, nil, err
		}
		// ...then lose the response on the way back to the client.
		return true, nil, syscall.ECONNRESET
	})

	if err := d.ensureServiceAccount(ctx); err == nil {
		t.Fatal("ensureServiceAccount() = nil error, want the simulated lost response")
	}

	if _, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{}); err != nil {
		t.Fatalf("test precondition: the ServiceAccount must exist in the cluster despite the "+
			"failed call, Get err = %v", err)
	}

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := client.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Cleanup leaked the ServiceAccount created by the lost-response Create; Get err = %v", err)
	}

	observed := deletes()
	if len(observed) != 1 {
		t.Fatalf("Cleanup issued %d deletes, want 1: %+v", len(observed), observed)
	}
	if observed[0].uid == nil || *observed[0].uid != committedUID {
		t.Errorf("delete Preconditions.UID = %v, want the UID recovered from the live object (%q)",
			observed[0].uid, committedUID)
	}
}

// TestEnsureDiscardsIntentOnAlreadyExists is recordIntent's counterweight: an
// AlreadyExists response is the one outcome that proves the object at that
// name is NOT ours (a duplicate RunID, or a 16-byte random collision). Keeping
// the intent entry would hand this run a bare-name delete of another run's
// object — strictly worse than the leak recordIntent prevents.
func TestEnsureDiscardsIntentOnAlreadyExists(t *testing.T) {
	ctx := context.Background()
	const ns = "test-ns"
	client := fake.NewClientset()

	d := NewDeployer(client, Config{Namespace: ns, RunID: testRunID})
	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(
			schema.GroupResource{Resource: "serviceaccounts"}, d.saName())
	})

	if err := d.ensureServiceAccount(ctx); err == nil {
		t.Fatal("ensureServiceAccount() = nil error, want AlreadyExists to be reported")
	}

	if got := d.createdSnapshot(); len(got) != 0 {
		t.Errorf("created-set = %+v, want empty — an AlreadyExists object was not created by "+
			"this run and must not enter its delete list", got)
	}
}

// TestCleanupTreatsConflictAsSuccess verifies a Conflict response (the UID
// precondition did not match — the name now belongs to a different object)
// is treated as success, same as NotFound, rather than surfaced as a
// Cleanup failure.
func TestCleanupTreatsConflictAsSuccess(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
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
	d := NewDeployer(fake.NewClientset(), Config{Namespace: "test-ns"})

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
	d := NewDeployer(fake.NewClientset(), Config{Namespace: "test-ns"})
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
	d := NewDeployer(fake.NewClientset(), Config{Namespace: "test-ns"})

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
//
// The sweep is licensed by this run holding a CONFIRMED Job — the only thing
// that can produce a staging ConfigMap is the in-pod agent that Job runs — so
// the Job is recorded here as Deploy would have recorded it. The seeded
// ConfigMap carries the label set pkg/serializer's ConfigMapWriter actually
// stamps from inside the pod: app.kubernetes.io/name plus component and
// version, and NO aicr.run/run-id.
func TestCleanupSweepsUnrecordedStagingConfigMap(t *testing.T) {
	ctx := context.Background()
	name := StagingConfigMapName(testRunID)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-namespace",
			UID:       types.UID("staging-uid"),
			Labels:    stagingConfigMapLabels(),
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
	seedConfirmedJob(t, ctx, clientset, d)
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

// seedConfirmedJob records the Job in d's created-set the way a successful
// Create would AND stands the object itself up in the cluster, because the
// staging-ConfigMap sweep requires both: the confirmed entry says this
// invocation created a Job at that name, and the live object carrying the same
// UID says that Job is still the one holding the name (see stillHoldsJob).
// Recording without seeding would leave every sweep test passing vacuously,
// with the sweep never running at all.
// seedConfirmedJob stands up the Job this Deployer would have created and
// records it as a confirmed create. The UID is fixed rather than a parameter:
// every caller wants "this invocation's live Job", and the specific value is
// never asserted on.
func seedConfirmedJob(t *testing.T, ctx context.Context, clientset kubernetes.Interface, d *Deployer) {
	const uid = types.UID("job-uid")
	t.Helper()
	d.recordCreated(kindJob, d.jobName(), uid)
	if _, err := clientset.BatchV1().Jobs(d.config.Namespace).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.jobName(),
			Namespace: d.config.Namespace,
			UID:       uid,
			Labels:    d.objectLabels(),
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed Job %q: %v", d.jobName(), err)
	}
}

// stagingConfigMapLabels returns the label set pkg/serializer's
// ConfigMapWriter stamps on the staging ConfigMap it writes from inside the
// agent pod (Serialize in pkg/serializer/configmap.go). Deliberately NOT
// objectLabels(): that object is written by the in-pod agent rather than by
// this controller, so it carries neither aicr.run/run-id nor managed-by —
// which is why the sweep's ownership evidence is the confirmed Job, and why
// the check on the object itself can only be app.kubernetes.io/name.
func stagingConfigMapLabels() map[string]string {
	return map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.Component: header.KindSnapshot.String(),
	}
}

// TestCleanupDuplicateRunIDKeepsFirstRunsStagingConfigMap is the
// duplicate-RunID failure case. Config.RunID is public SDK surface and
// deliberately settable (pinned e2e/chainsaw runs), so a second run can
// resolve the first run's exact staging name.
//
// Run B reuses run A's RunID and fails on its very first AlreadyExists, before
// recording anything. Its deferred Cleanup still runs — Cleanup is registered
// before Deploy — and must not sweep the staging ConfigMap run A is still
// using: run B never created a Job, so nothing it did could have produced a
// ConfigMap at that name.
func TestCleanupDuplicateRunIDKeepsFirstRunsStagingConfigMap(t *testing.T) {
	ctx := context.Background()
	const ns = "test-namespace"
	stagingName := StagingConfigMapName(testRunID)

	client := fake.NewClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "test permissions allowed"},
		}, nil
	})

	runA := NewDeployer(client, Config{
		Namespace:           ns,
		Image:               "aicr:test",
		RunID:               testRunID,
		Output:              "cm://" + ns + "/" + stagingName,
		OwnsOutputConfigMap: true,
	})
	if err := runA.Deploy(ctx); err != nil {
		t.Fatalf("run A Deploy() error = %v", err)
	}
	// Run A's in-pod agent has staged its result; Deploy() itself never
	// writes this object, so seed it the way the agent would.
	if _, err := client.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stagingName,
			Namespace: ns,
			UID:       types.UID("run-a-staging-uid"),
			Labels:    stagingConfigMapLabels(),
		},
		Data: map[string]string{"snapshot.yaml": "run A's snapshot"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed run A staging ConfigMap: %v", err)
	}

	deletes := spyOnDeletes(client)

	// Run B pins the SAME run ID and therefore collides on run A's
	// ServiceAccount, the first object Deploy creates.
	runB := NewDeployer(client, Config{
		Namespace:           ns,
		Image:               "aicr:test",
		RunID:               testRunID,
		Output:              "cm://" + ns + "/" + stagingName,
		OwnsOutputConfigMap: true,
	})
	if err := runB.Deploy(ctx); err == nil {
		t.Fatal("run B Deploy() = nil error, want AlreadyExists on the duplicate RunID")
	}
	if err := runB.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("run B Cleanup() error = %v", err)
	}

	cm, err := client.CoreV1().ConfigMaps(ns).Get(ctx, stagingName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("run B's failed Cleanup deleted run A's staging ConfigMap: %v", err)
	}
	if cm.UID != types.UID("run-a-staging-uid") {
		t.Errorf("staging ConfigMap UID = %q, want run A's %q", cm.UID, "run-a-staging-uid")
	}
	if observed := deletes(); len(observed) != 0 {
		t.Errorf("run B's Cleanup issued %d deletes despite creating nothing: %+v", len(observed), observed)
	}
}

// TestCleanupSweepKeepsForeignConfigMapAtStagingName is the sweep's own
// fail-closed check, downstream of the confirmed-Job gate: a ConfigMap parked
// at this run's staging name that does not carry app.kubernetes.io/name=aicr
// was not written by pkg/serializer's in-pod writer, so this run did not
// produce it. It must survive, and the operator must hear about it.
func TestCleanupSweepKeepsForeignConfigMapAtStagingName(t *testing.T) {
	ctx := context.Background()
	const ns = "test-namespace"
	name := StagingConfigMapName(testRunID)
	logs := captureLogs(t)

	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       types.UID("someone-elses-uid"),
			Labels:    map[string]string{"app.kubernetes.io/name": "not-aicr"},
		},
		Data: map[string]string{"unrelated": "data"},
	})

	d := NewDeployer(client, Config{
		Namespace:           ns,
		RunID:               testRunID,
		Output:              "cm://" + ns + "/" + name,
		OwnsOutputConfigMap: true,
	})
	seedConfirmedJob(t, ctx, client, d)

	if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("Cleanup deleted a ConfigMap this run did not write: %v", err)
	}
	if !strings.Contains(logs.String(), name) {
		t.Errorf("no warning naming the ConfigMap left behind; logs: %s", logs.String())
	}
}

// TestCleanupSweepRequiresThisInvocationsLiveJob is the staging ConfigMap's
// half of the same-RunID problem. That object is written by the in-pod agent
// through pkg/serializer, which stamps neither aicr.run/run-id nor
// aicr.run/invocation-id, so the evidence createdByThisInvocation uses does not
// exist on it and cannot be put there — the agent image may be a different
// aicr version than the controller.
//
// The evidence is the Job instead, and a confirmed Job ENTRY is not enough: it
// records that this invocation created a Job at that name once, not that the
// object standing there now is that Job. If this run's Job was deleted and a
// second invocation reusing the RunID created its own, the ConfigMap at the
// shared staging name is the second invocation's live artifact. Only one Job
// can hold a name at a time, so requiring the recorded UID to still be the
// live one settles it — and every other answer fails closed, leaving a
// ConfigMap an operator can remove rather than deleting a running run's.
func TestCleanupSweepRequiresThisInvocationsLiveJob(t *testing.T) {
	const ns = "test-namespace"
	const ourJobUID = types.UID("run-a-job-uid")

	tests := []struct {
		name        string
		recordUID   types.UID // UID this invocation's Create response returned
		seedLiveJob bool      // stand a Job up at this run's Job name
		liveJobUID  types.UID // its UID, when seeded
		wantSwept   bool
	}{
		{
			name:        "the Job this invocation created still holds the name",
			recordUID:   ourJobUID,
			seedLiveJob: true,
			liveJobUID:  ourJobUID,
			wantSwept:   true,
		},
		{
			name:        "another invocation replaced the Job at this name",
			recordUID:   ourJobUID,
			seedLiveJob: true,
			liveJobUID:  types.UID("run-b-job-uid"),
			wantSwept:   false,
		},
		{
			name:      "the Job is gone entirely",
			recordUID: ourJobUID,
			wantSwept: false,
		},
		{
			// A real apiserver always assigns a UID, so a confirmed
			// entry without one means the evidence is missing. Matching
			// it against an equally empty live UID would be agreement
			// between two absences, not proof of anything.
			name:        "no recorded Job UID to match against",
			seedLiveJob: true,
			liveJobUID:  "",
			wantSwept:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			name := StagingConfigMapName(testRunID)
			client := fake.NewClientset(&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: ns,
					UID:       types.UID("staging-uid"),
					Labels:    stagingConfigMapLabels(),
				},
				Data: map[string]string{"snapshot.yaml": "data"},
			})

			d := NewDeployer(client, Config{
				Namespace:           ns,
				RunID:               testRunID,
				Output:              "cm://" + ns + "/" + name,
				OwnsOutputConfigMap: true,
			})
			// The entry the Create response produced for THIS invocation.
			d.recordCreated(kindJob, d.jobName(), tt.recordUID)
			// What is actually standing at that name when Cleanup runs.
			if tt.seedLiveJob {
				if _, err := client.BatchV1().Jobs(ns).Create(ctx, &batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{
						Name: d.jobName(), Namespace: ns, UID: tt.liveJobUID,
					},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seed Job: %v", err)
				}
			}

			if err := d.Cleanup(ctx, CleanupOptions{Enabled: true}); err != nil {
				t.Fatalf("Cleanup() error = %v", err)
			}

			_, err := client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
			swept := apierrors.IsNotFound(err)
			if err != nil && !swept {
				t.Fatalf("reading the staging ConfigMap back: %v", err)
			}
			if swept != tt.wantSwept {
				t.Errorf("staging ConfigMap swept = %v, want %v", swept, tt.wantSwept)
			}
		})
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
	seedConfirmedJob(t, ctx, clientset, d)

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
	seedConfirmedJob(t, ctx, clientset, d)

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
		RunID:              testRunID,
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
		RunID:              testRunID,
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
