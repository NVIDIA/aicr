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
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
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
	if errorstest.ReportedCode(err) != errors.ErrCodeInternal {
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

// TestVerifyServiceAccountAgainstRBAC runs the SubjectAccessReview against
// envtest's real RBAC authorizer, so the grants it accepts and refuses are the
// apiserver's answers rather than a reactor's. Each refused case is a grant an
// administrator could plausibly mistake for enough: read access to every
// resource, and cluster-admin bound only inside the validation namespace.
//
// The authorizer answers from an informer cache, so a fresh binding is not
// visible at once. Every case with a grant first polls a probe the grant must
// allow; only then is the answer under test meaningful, in either direction.
// Each case also gets its own namespace, so the group grant cannot reach
// another case's ServiceAccount.
func TestVerifyServiceAccountAgainstRBAC(t *testing.T) {
	const sa = "validator"

	clusterWide := func(verb, group, resource string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Verb: verb, Group: group, Resource: resource}
	}
	everything := clusterWide("*", "*", "*")

	tests := []struct {
		name string
		// grant binds sa in ns and returns a probe the grant must allow.
		grant    func(t *testing.T, ns string) authorizationv1.ResourceAttributes
		missing  bool
		wantCode errors.ErrorCode
	}{
		{
			name: "bound to cluster-admin",
			grant: func(t *testing.T, ns string) authorizationv1.ResourceAttributes {
				bindClusterRole(t, ns, saSubject(ns, sa), clusterAdminRole)
				return everything
			},
		},
		{
			name: "cluster-admin granted to the namespace's ServiceAccount group",
			grant: func(t *testing.T, ns string) authorizationv1.ResourceAttributes {
				bindClusterRole(t, ns, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName,
					Name: groupServiceAccountsPrefix + ns}, clusterAdminRole)
				return everything
			},
		},
		{
			name: "read access to every resource",
			grant: func(t *testing.T, ns string) authorizationv1.ResourceAttributes {
				role := &rbacv1.ClusterRole{
					ObjectMeta: metav1.ObjectMeta{Name: ns + "-read-everything"},
					Rules: []rbacv1.PolicyRule{{
						APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"},
					}},
				}
				if _, err := testClientset.RbacV1().ClusterRoles().Create(context.Background(), role, metav1.CreateOptions{}); err != nil {
					t.Fatalf("create ClusterRole: %v", err)
				}
				t.Cleanup(func() {
					_ = testClientset.RbacV1().ClusterRoles().Delete(context.Background(), role.Name, metav1.DeleteOptions{})
				})
				bindClusterRole(t, ns, saSubject(ns, sa), role.Name)
				return clusterWide("list", "", "pods")
			},
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name: "cluster-admin bound only within the namespace",
			grant: func(t *testing.T, ns string) authorizationv1.ResourceAttributes {
				rb := &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: ns},
					Subjects:   []rbacv1.Subject{saSubject(ns, sa)},
					RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterAdminRole},
				}
				if _, err := testClientset.RbacV1().RoleBindings(ns).Create(context.Background(), rb, metav1.CreateOptions{}); err != nil {
					t.Fatalf("create RoleBinding: %v", err)
				}
				probe := everything
				probe.Namespace = ns
				return probe
			},
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name:     "unbound",
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name:     "missing",
			missing:  true,
			wantCode: errors.ErrCodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			if !tt.missing {
				obj := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: sa, Namespace: ns}}
				if _, err := testClientset.CoreV1().ServiceAccounts(ns).Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
					t.Fatalf("create ServiceAccount: %v", err)
				}
			}
			if tt.grant != nil {
				waitForAccess(t, ns, sa, tt.grant(t, ns))
			}

			err := VerifyServiceAccount(context.Background(), testClientset, ns, sa)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("VerifyServiceAccount() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("VerifyServiceAccount() error = nil, want %s", tt.wantCode)
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

func saSubject(namespace, name string) rbacv1.Subject {
	return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: name, Namespace: namespace}
}

// bindClusterRole binds role to subject cluster-wide under a name derived
// from ns, so cases cannot collide, and removes the binding when t ends.
func bindClusterRole(t *testing.T, ns string, subject rbacv1.Subject, role string) {
	t.Helper()
	ctx := context.Background()
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ns + "-" + role},
		Subjects:   []rbacv1.Subject{subject},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role},
	}
	if _, err := testClientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ClusterRoleBinding: %v", err)
	}
	t.Cleanup(func() {
		_ = testClientset.RbacV1().ClusterRoleBindings().Delete(ctx, crb.Name, metav1.DeleteOptions{})
	})
}

// waitForAccess polls until the apiserver allows the ServiceAccount the probe,
// proving its grant has reached the authorizer's cache.
func waitForAccess(t *testing.T, ns, sa string, probe authorizationv1.ResourceAttributes) {
	t.Helper()
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:               serviceAccountUserPrefix + ns + ":" + sa,
		Groups:             []string{groupServiceAccounts, groupServiceAccountsPrefix + ns, groupAuthenticated},
		ResourceAttributes: &probe,
	}}
	err := wait.PollUntilContextTimeout(context.Background(), 50*time.Millisecond, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			result, err := testClientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				return false, err
			}
			return result.Status.Allowed, nil
		})
	if err != nil {
		t.Fatalf("grant never allowed probe %+v: %v", probe, err)
	}
}

// TestVerifyServiceAccountRequest pins the review's question: the
// ServiceAccount's own username, the virtual groups bindings commonly target,
// and wildcard verb, group and resource at cluster scope.
func TestVerifyServiceAccountRequest(t *testing.T) {
	const ns, name = "validation", "admin-sa"
	cs := k8sfake.NewSimpleClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})

	var got *authorizationv1.SubjectAccessReview
	cs.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review, ok := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		if !ok {
			t.Fatalf("created object is %T, want *SubjectAccessReview", action.(clienttesting.CreateAction).GetObject())
		}
		got = review.DeepCopy()
		review.Status.Allowed = true
		return true, review, nil
	})

	if err := VerifyServiceAccount(context.Background(), cs, ns, name); err != nil {
		t.Fatalf("VerifyServiceAccount() error = %v", err)
	}
	if got == nil {
		t.Fatal("no SubjectAccessReview was created")
	}
	if want := "system:serviceaccount:validation:admin-sa"; got.Spec.User != want {
		t.Errorf("User = %q, want %q", got.Spec.User, want)
	}
	wantGroups := []string{"system:serviceaccounts", "system:serviceaccounts:validation", "system:authenticated"}
	if !slices.Equal(got.Spec.Groups, wantGroups) {
		t.Errorf("Groups = %v, want %v", got.Spec.Groups, wantGroups)
	}
	attrs := got.Spec.ResourceAttributes
	if attrs == nil {
		t.Fatal("ResourceAttributes = nil")
	}
	if attrs.Verb != "*" || attrs.Group != "*" || attrs.Resource != "*" || attrs.Namespace != "" {
		t.Errorf("ResourceAttributes = %+v, want verb, group and resource \"*\" at cluster scope", attrs)
	}
}

// TestVerifyServiceAccountRejectsDisabledTokenAutomount pins that a
// ServiceAccount whose pods would get no token is refused before any review:
// the validator pod spec does not override the ServiceAccount's setting.
func TestVerifyServiceAccountRejectsDisabledTokenAutomount(t *testing.T) {
	const ns, name = "validation", "admin-sa"
	automount := false
	cs := k8sfake.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: name, Namespace: ns},
		AutomountServiceAccountToken: &automount,
	})

	err := VerifyServiceAccount(context.Background(), cs, ns, name)
	if err == nil {
		t.Fatal("VerifyServiceAccount() error = nil, want ErrCodeInvalidRequest")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if !strings.Contains(err.Error(), "automountServiceAccountToken") {
		t.Errorf("error = %v, want it to name automountServiceAccountToken", err)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "subjectaccessreviews" {
			t.Error("a review was issued for a ServiceAccount that had already been refused")
		}
	}
}

// TestVerifyServiceAccountFailsClosed covers the outcomes envtest cannot
// produce on demand. Each must refuse, and none may write anything: the only
// create allowed is the non-persisted SubjectAccessReview.
func TestVerifyServiceAccountFailsClosed(t *testing.T) {
	const ns, name = "validation", "admin-sa"
	forbidden := func(resource string) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, name, stderrors.New("denied"))
	}

	tests := []struct {
		name        string
		canceled    bool
		expired     bool
		getErr      error
		reviewErr   error
		denyReason  string
		wantCode    errors.ErrorCode
		wantMessage string
	}{
		{
			name:     "caller cancels during the read",
			canceled: true,
			getErr:   context.Canceled,
			wantCode: errors.ErrCodeCanceled,
		},
		{
			name:     "deadline passes during the read",
			expired:  true,
			getErr:   context.DeadlineExceeded,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name:     "apiserver unreachable",
			getErr:   &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			wantCode: errors.ErrCodeUnavailable,
		},
		{
			name:     "caller's credentials rejected",
			getErr:   apierrors.NewUnauthorized("token has expired"),
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name:        "caller may not read ServiceAccounts",
			getErr:      forbidden("serviceaccounts"),
			wantCode:    errors.ErrCodeUnauthorized,
			wantMessage: "get serviceaccounts",
		},
		{
			name:     "ServiceAccount read fails",
			getErr:   stderrors.New("etcd timeout"),
			wantCode: errors.ErrCodeInternal,
		},
		{
			name:        "caller may not create SubjectAccessReviews",
			reviewErr:   forbidden("subjectaccessreviews"),
			wantCode:    errors.ErrCodeUnauthorized,
			wantMessage: "create subjectaccessreviews",
		},
		{
			name:      "review fails",
			reviewErr: stderrors.New("webhook unavailable"),
			wantCode:  errors.ErrCodeInternal,
		},
		{
			name:        "review denies",
			denyReason:  "no RBAC policy matched",
			wantCode:    errors.ErrCodeUnauthorized,
			wantMessage: "kubectl create clusterrolebinding <name> --clusterrole=cluster-admin --serviceaccount=validation:admin-sa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := k8sfake.NewSimpleClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			if tt.getErr != nil {
				cs.PrependReactor("get", "serviceaccounts", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}
			cs.PrependReactor("create", "subjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if tt.reviewErr != nil {
					return true, nil, tt.reviewErr
				}
				review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
				review.Status.Allowed = false
				review.Status.Reason = tt.denyReason
				return true, review, nil
			})

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.canceled {
				cancel()
			}
			if tt.expired {
				var expire context.CancelFunc
				ctx, expire = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer expire()
			}

			err := VerifyServiceAccount(ctx, cs, ns, name)
			if err == nil {
				t.Fatalf("VerifyServiceAccount() error = nil, want %s", tt.wantCode)
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if tt.wantMessage != "" && !strings.Contains(err.Error(), tt.wantMessage) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantMessage)
			}
			if tt.denyReason != "" && !strings.Contains(err.Error(), tt.denyReason) {
				t.Errorf("error = %v, want the apiserver's reason %q", err, tt.denyReason)
			}

			for _, a := range cs.Actions() {
				if a.GetVerb() == "get" || (a.GetVerb() == "create" && a.GetResource().Resource == "subjectaccessreviews") {
					continue
				}
				t.Errorf("VerifyServiceAccount() issued %s %s; it must only read", a.GetVerb(), a.GetResource().Resource)
			}
		})
	}
}
