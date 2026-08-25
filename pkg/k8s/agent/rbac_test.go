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
	stderrors "errors"
	"log/slog"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testNamespace is the namespace every ServiceAccount-resolution test
// deploys into.
const testNamespace = "test-ns"

// captureLogs redirects the default slog logger into a buffer at debug level
// for the duration of the test and returns the buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// TestResolveServiceAccount covers every branch of the exact-if-exists
// resolution of Config.ServiceAccountName: whether the run adopts an
// operator-supplied ServiceAccount verbatim (and manages none of its
// permissions) or treats the value as a prefix and creates its own.
//
// The Forbidden branch is the load-bearing one: `serviceaccounts get` is NOT
// in CheckPermissions' requiredChecks, so an identity holding exactly the
// pre-flight verb set must still be able to deploy. It falls back to prefix
// mode, which is the mode that identity has the permissions for.
func TestResolveServiceAccount(t *testing.T) {
	saGR := schema.GroupResource{Group: "", Resource: "serviceaccounts"}

	tests := []struct {
		name string
		// configured is Config.ServiceAccountName.
		configured string
		// seeded, when non-empty, pre-creates a ServiceAccount of that
		// name in the namespace.
		seeded string
		// getErr, when non-nil, is returned by every ServiceAccount Get.
		getErr error
		// wantExisting is the ServiceAccount the run should adopt
		// verbatim; "" means prefix mode.
		wantExisting  string
		wantErr       bool
		wantLogSubstr string
		notWantLog    string
	}{
		{
			name:          "exact match adopts the operator's ServiceAccount",
			configured:    "irsa-snapshotter",
			seeded:        "irsa-snapshotter",
			wantExisting:  "irsa-snapshotter",
			wantLogSubstr: "aicr manages no RBAC for this run",
		},
		{
			name:       "no match keeps prefix mode",
			configured: "irsa-snapshotter",
			notWantLog: "aicr manages no RBAC for this run",
		},
		{
			name:       "unset name is never probed, even when the base name exists",
			seeded:     testName,
			notWantLog: "aicr manages no RBAC for this run",
		},
		{
			name:          "forbidden Get falls back to prefix mode",
			configured:    "irsa-snapshotter",
			seeded:        "irsa-snapshotter",
			getErr:        apierrors.NewForbidden(saGR, "irsa-snapshotter", stderrors.New("no get permission")),
			wantLogSubstr: "treating --service-account-name as a prefix",
			notWantLog:    "aicr manages no RBAC for this run",
		},
		{
			name:       "unexpected Get error fails closed",
			configured: "irsa-snapshotter",
			getErr:     apierrors.NewInternalError(stderrors.New("apiserver exploded")),
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			buf := captureLogs(t)

			clientset := fake.NewClientset()
			if tt.seeded != "" {
				if _, err := clientset.CoreV1().ServiceAccounts(testNamespace).Create(ctx, &corev1.ServiceAccount{
					ObjectMeta: metav1.ObjectMeta{Name: tt.seeded, Namespace: testNamespace},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatalf("seeding ServiceAccount: %v", err)
				}
			}
			if tt.getErr != nil {
				clientset.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}

			d := NewDeployer(clientset, Config{
				Namespace:          testNamespace,
				ServiceAccountName: tt.configured,
				RunID:              testRunID,
			})
			err := d.resolveServiceAccount(ctx)

			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveServiceAccount() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
				t.Errorf("error = %v, want ErrCodeInternal", err)
			}
			if got := d.existingServiceAccount(); got != tt.wantExisting {
				t.Errorf("existingServiceAccount() = %q, want %q", got, tt.wantExisting)
			}
			// managesRBAC is what gates Deploy's whole RBAC block, so
			// assert it rather than inferring it from the field.
			if got, want := d.managesRBAC(), tt.wantExisting == ""; got != want {
				t.Errorf("managesRBAC() = %v, want %v", got, want)
			}
			// The pod must run as whichever ServiceAccount was resolved.
			wantPodSA := tt.wantExisting
			if wantPodSA == "" {
				wantPodSA = d.saName()
			}
			if got := d.podServiceAccountName(); got != wantPodSA {
				t.Errorf("podServiceAccountName() = %q, want %q", got, wantPodSA)
			}

			if tt.wantLogSubstr != "" && !strings.Contains(buf.String(), tt.wantLogSubstr) {
				t.Errorf("log = %q, want it to contain %q", buf.String(), tt.wantLogSubstr)
			}
			if tt.notWantLog != "" && strings.Contains(buf.String(), tt.notWantLog) {
				t.Errorf("log = %q, want it NOT to contain %q", buf.String(), tt.notWantLog)
			}
		})
	}
}

// TestDeploy_ExistingServiceAccountCreatesAndDeletesNoRBAC is the end-to-end
// contract of exact-ServiceAccount mode: aicr will not add or remove
// permissions on a ServiceAccount it did not create.
//
// It asserts the whole chain rather than just the flag — no RBAC object of
// any kind is created, the created-set holds none of those kinds, and
// therefore Cleanup (which builds its delete list from exactly that set)
// leaves the operator's ServiceAccount and everything around it alone. The
// Cleanup assertion is deliberately not "cleanup skips these kinds": there is
// no such branch in Cleanup, and the point is that none is needed.
func TestDeploy_ExistingServiceAccountCreatesAndDeletesNoRBAC(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	const saName = "irsa-snapshotter"
	const roleARN = "arn:aws:iam::123456789012:role/aicr-snapshot"

	clientset := fake.NewClientset()
	allowAllPermissionChecks(clientset)
	if _, err := clientset.CoreV1().ServiceAccounts(testNamespace).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        saName,
			Namespace:   testNamespace,
			Annotations: map[string]string{"eks.amazonaws.com/role-arn": roleARN},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding ServiceAccount: %v", err)
	}

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: saName,
		Image:              "aicr:test",
		RunID:              testRunID,
		DiscoverNetwork:    true,
	})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	// The Job — the one object this run still owns — must run as the
	// operator's ServiceAccount verbatim, not as "<prefix>-<runID>".
	job, err := clientset.BatchV1().Jobs(testNamespace).Get(ctx, d.jobName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != saName {
		t.Errorf("pod ServiceAccountName = %q, want %q", got, saName)
	}

	// Nothing run-scoped exists for any RBAC kind.
	if sas, listErr := clientset.CoreV1().ServiceAccounts(testNamespace).List(ctx, metav1.ListOptions{}); listErr != nil {
		t.Fatalf("listing ServiceAccounts: %v", listErr)
	} else if len(sas.Items) != 1 || sas.Items[0].Name != saName {
		t.Errorf("ServiceAccounts = %v, want only the pre-existing %q", saNames(sas.Items), saName)
	}
	assertNoRBACObjects(ctx, t, clientset)

	// The created-set is what Cleanup deletes from, so its contents are
	// the actual guarantee.
	for _, kind := range []string{kindServiceAccount, kindRole, kindRoleBinding, kindClusterRole, kindClusterRoleBinding} {
		if d.hasCreated(kind) {
			t.Errorf("created-set holds a %s entry; Cleanup would delete an object this run did not create", kind)
		}
	}

	if cleanupErr := d.Cleanup(ctx, CleanupOptions{Enabled: true}); cleanupErr != nil {
		t.Fatalf("Cleanup() error = %v", cleanupErr)
	}

	sa, err := clientset.CoreV1().ServiceAccounts(testNamespace).Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Cleanup deleted the operator's ServiceAccount: %v", err)
	}
	if sa.Annotations["eks.amazonaws.com/role-arn"] != roleARN {
		t.Errorf("IRSA annotation = %q, want %q", sa.Annotations["eks.amazonaws.com/role-arn"], roleARN)
	}
	assertNoRBACObjects(ctx, t, clientset)
}

// TestDeploy_AbsentServiceAccountKeepsPrefixBehavior pins the other half of
// exact-if-exists: a --service-account-name naming nothing that exists is
// still a prefix, and the run creates and owns the full run-scoped RBAC set
// exactly as it did before.
func TestDeploy_AbsentServiceAccountKeepsPrefixBehavior(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	clientset := fake.NewClientset()
	allowAllPermissionChecks(clientset)

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: "irsa-snapshotter",
		Image:              "aicr:test",
		RunID:              testRunID,
	})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	scoped := "irsa-snapshotter-" + testRunID
	if d.saName() != scoped {
		t.Fatalf("saName() = %q, want %q", d.saName(), scoped)
	}
	if _, err := clientset.CoreV1().ServiceAccounts(testNamespace).Get(ctx, scoped, metav1.GetOptions{}); err != nil {
		t.Errorf("run-scoped ServiceAccount %q not created: %v", scoped, err)
	}
	if _, err := clientset.RbacV1().Roles(testNamespace).Get(ctx, scoped, metav1.GetOptions{}); err != nil {
		t.Errorf("run-scoped Role %q not created: %v", scoped, err)
	}
	if _, err := clientset.RbacV1().RoleBindings(testNamespace).Get(ctx, scoped, metav1.GetOptions{}); err != nil {
		t.Errorf("run-scoped RoleBinding %q not created: %v", scoped, err)
	}
	if _, err := clientset.RbacV1().ClusterRoles().Get(ctx, d.clusterRoleName(), metav1.GetOptions{}); err != nil {
		t.Errorf("run-scoped ClusterRole %q not created: %v", d.clusterRoleName(), err)
	}
	if _, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, d.clusterRoleName(), metav1.GetOptions{}); err != nil {
		t.Errorf("run-scoped ClusterRoleBinding %q not created: %v", d.clusterRoleName(), err)
	}

	job, err := clientset.BatchV1().Jobs(testNamespace).Get(ctx, d.jobName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Job not created: %v", err)
	}
	if got := job.Spec.Template.Spec.ServiceAccountName; got != scoped {
		t.Errorf("pod ServiceAccountName = %q, want %q", got, scoped)
	}
}

// saNames projects a ServiceAccount list to its names for error messages.
func saNames(items []corev1.ServiceAccount) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, items[i].Name)
	}
	return out
}

// assertNoRBACObjects fails when any Role, RoleBinding, ClusterRole or
// ClusterRoleBinding exists in the cluster.
func assertNoRBACObjects(ctx context.Context, t *testing.T, clientset *fake.Clientset) {
	t.Helper()
	roles, err := clientset.RbacV1().Roles(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing Roles: %v", err)
	}
	if len(roles.Items) != 0 {
		t.Errorf("Roles = %d, want 0", len(roles.Items))
	}
	rbs, err := clientset.RbacV1().RoleBindings(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing RoleBindings: %v", err)
	}
	if len(rbs.Items) != 0 {
		t.Errorf("RoleBindings = %d, want 0", len(rbs.Items))
	}
	crs, err := clientset.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing ClusterRoles: %v", err)
	}
	if len(crs.Items) != 0 {
		t.Errorf("ClusterRoles = %d, want 0", len(crs.Items))
	}
	crbs, err := clientset.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing ClusterRoleBindings: %v", err)
	}
	if len(crbs.Items) != 0 {
		t.Errorf("ClusterRoleBindings = %d, want 0", len(crbs.Items))
	}
}

// allowAllPermissionChecks makes every SelfSubjectAccessReview succeed so a
// test exercises Deploy past its Step 0 pre-flight.
func allowAllPermissionChecks(clientset *fake.Clientset) {
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "pre-flight verb set granted"},
		}, nil
	})
}

// TestDeploy_SucceedsWhenServiceAccountGetForbidden is the end-to-end shape of
// the same bug: an identity authorized for exactly CheckPermissions'
// requiredChecks (which do not include `serviceaccounts get`) passes the
// pre-flight, so Deploy must not then fail on the exact-if-exists Get.
// ServiceAccountName is set because that Get is issued only when it is —
// leaving it empty would make the test pass without reaching the branch.
func TestDeploy_SucceedsWhenServiceAccountGetForbidden(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	clientset := fake.NewClientset()
	allowAllPermissionChecks(clientset)
	clientset.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "", Resource: "serviceaccounts"}, testName,
			stderrors.New(`User "snapshot-runner" cannot get resource "serviceaccounts"`))
	})

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: testName,
		Image:              "aicr:test",
		RunID:              testRunID,
	})

	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v, want nil (the exact-if-exists Get must not gate deployment)", err)
	}

	if _, err := clientset.BatchV1().Jobs(testNamespace).Get(ctx, "aicr-"+testRunID, metav1.GetOptions{}); err != nil {
		t.Errorf("Job not created: %v", err)
	}
	// Forbidden must fall back to prefix mode, which still creates the
	// run-scoped RBAC set.
	if !d.managesRBAC() {
		t.Error("managesRBAC() = false; a Forbidden Get must not be read as an adopted ServiceAccount")
	}
	// Read through the tracker, not the clientset: the reactor above stands
	// in for an identity that cannot read ServiceAccounts at all, and that
	// must not also blind the assertion.
	saGVR := corev1.SchemeGroupVersion.WithResource("serviceaccounts")
	if _, err := clientset.Tracker().Get(saGVR, testNamespace, "aicr-"+testRunID); err != nil {
		t.Errorf("run-scoped ServiceAccount not created: %v", err)
	}
}
