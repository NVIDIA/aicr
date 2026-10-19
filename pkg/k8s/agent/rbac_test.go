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
	"maps"
	"slices"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
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
// The Forbidden branch is the load-bearing one, and it now fails closed.
// `serviceaccounts get` is a REQUIRED check in CheckPermissions, evaluated
// before this runs, so a Forbidden here means the authorizer's answer and
// the apiserver's behavior disagree. The old downgrade — continue in prefix
// mode — silently ran an operator who named their IRSA / Workload Identity
// ServiceAccount under a generated one carrying none of its annotations.
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
		// wantErrCode is the structured code the failure must carry. A
		// permission problem must surface as ErrCodeUnauthorized, not as
		// an opaque internal error.
		wantErrCode aicrerrors.ErrorCode
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
			name:        "forbidden Get fails closed instead of downgrading to a prefix",
			configured:  "irsa-snapshotter",
			seeded:      "irsa-snapshotter",
			getErr:      apierrors.NewForbidden(saGR, "irsa-snapshotter", stderrors.New("no get permission")),
			wantErr:     true,
			wantErrCode: aicrerrors.ErrCodeUnauthorized,
			notWantLog:  "aicr manages no RBAC for this run",
		},
		{
			name:        "unexpected Get error fails closed",
			configured:  "irsa-snapshotter",
			getErr:      apierrors.NewInternalError(stderrors.New("apiserver exploded")),
			wantErr:     true,
			wantErrCode: aicrerrors.ErrCodeInternal,
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
			if tt.wantErr && errorstest.ReportedCode(err) != tt.wantErrCode {
				t.Errorf("error = %v, want code %s", err, tt.wantErrCode)
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

// allowAllPermissionChecks makes every access review succeed so a test
// exercises Deploy past its Step 0 pre-flight. Both kinds are answered:
// SelfSubjectAccessReview covers the caller's verbs, and SubjectAccessReview
// covers the agent ServiceAccount's own rules, which the gate checks in
// exact-ServiceAccount mode.
func allowAllPermissionChecks(clientset *fake.Clientset) {
	clientset.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "pre-flight verb set granted"},
		}, nil
	})
	clientset.PrependReactor("create", "subjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.SubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: true, Reason: "ServiceAccount rules granted"},
		}, nil
	})
}

// TestDeploy_FailsWhenServiceAccountGetForbidden is the end-to-end shape of
// the closed hole: an explicitly-named ServiceAccount that cannot be read
// must stop the run, not be silently reinterpreted as a name prefix.
//
// The SelfSubjectAccessReview reactor says `serviceaccounts: get` is
// allowed while the Get itself returns Forbidden — the authorizer and the
// apiserver disagreeing. That is precisely the state the run must not guess
// through, because guessing "prefix" deploys the agent under a generated
// ServiceAccount carrying none of the named account's cloud credentials.
// ServiceAccountName is set because the Get is issued only when it is.
func TestDeploy_FailsWhenServiceAccountGetForbidden(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	clientset := fake.NewClientset()
	allowAllPermissionChecks(clientset)
	clientset.PrependReactor("get", "serviceaccounts", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "", Resource: resourceServiceAccounts}, testName,
			stderrors.New(`User "snapshot-runner" cannot get resource "serviceaccounts"`))
	})

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: testName,
		Image:              "aicr:test",
		RunID:              testRunID,
	})

	err := d.Deploy(ctx)
	if err == nil {
		t.Fatal("Deploy() error = nil; an unreadable, explicitly-named ServiceAccount must fail the run")
	}
	if errorstest.ReportedCode(err) != aicrerrors.ErrCodeUnauthorized {
		t.Errorf("Deploy() error code = %v, want ErrCodeUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("Deploy() error = %v, want it to say the run refuses to guess the mode", err)
	}

	// Nothing may have been written: the gate fails before Deploy's ensure*
	// chain runs. Read through the tracker, not the clientset — the reactor
	// above stands in for an identity that cannot read ServiceAccounts at
	// all, and that must not also blind the assertion.
	if _, jobErr := clientset.BatchV1().Jobs(testNamespace).Get(ctx, "aicr-"+testRunID, metav1.GetOptions{}); !apierrors.IsNotFound(jobErr) {
		t.Errorf("Job Get error = %v, want NotFound (no Job may be created)", jobErr)
	}
	saGVR := corev1.SchemeGroupVersion.WithResource(resourceServiceAccounts)
	if _, saErr := clientset.Tracker().Get(saGVR, testNamespace, "aicr-"+testRunID); !apierrors.IsNotFound(saErr) {
		t.Errorf("run-scoped ServiceAccount Get error = %v, want NotFound", saErr)
	}
	assertNoRBACObjects(ctx, t, clientset)
}

// namespacesResource is the resource the Namespace reactors and action
// filters below match on.
const namespacesResource = "namespaces"

// forbidNamespaceVerbs makes every Namespace request with one of verbs fail
// Forbidden, the way the apiserver answers an identity holding no
// cluster-scoped namespace grant. Authorization runs before the object is
// looked up, so a create on an existing namespace is Forbidden, never
// AlreadyExists. "*" covers every verb.
func forbidNamespaceVerbs(clientset *fake.Clientset, verbs ...string) {
	for _, verb := range verbs {
		clientset.PrependReactor(verb, namespacesResource, func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: namespacesResource}, testNamespace,
				stderrors.New(`User "system:serviceaccount:aicr-discovery:snapshot-runner" cannot `+
					action.GetVerb()+` resource "namespaces" in API group "" at the cluster scope`))
		})
	}
}

// namespaceVerbs returns, in order, the verb of every Namespace request the
// clientset received.
func namespaceVerbs(clientset *fake.Clientset) []string {
	var verbs []string
	for _, a := range clientset.Actions() {
		if a.GetResource().Resource == namespacesResource {
			verbs = append(verbs, a.GetVerb())
		}
	}
	return verbs
}

// trackedNamespace reads a Namespace straight from the tracker, past any
// reactor that stands in for a caller who may not read it.
func trackedNamespace(t *testing.T, clientset *fake.Clientset, name string) *corev1.Namespace {
	t.Helper()
	obj, err := clientset.Tracker().Get(corev1.SchemeGroupVersion.WithResource(namespacesResource), "", name)
	if err != nil {
		t.Fatalf("reading Namespace %q from the tracker: %v", name, err)
	}
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		t.Fatalf("tracker returned %T, want *corev1.Namespace", obj)
	}
	return ns
}

// TestDeploy_ExistingServiceAccountLeavesNamespaceAlone is the contract
// from #3238: in exact-ServiceAccount mode the namespace belongs to whoever
// provisioned the ServiceAccount, so the run neither reads, creates nor
// relabels it, and a caller holding no cluster-scoped namespace grant
// completes Deploy.
func TestDeploy_ExistingServiceAccountLeavesNamespaceAlone(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	const saName = "snapshot-agent"
	provisioned := map[string]string{labelAppManagedBy: "Helm", "team": "platform"}
	clientset := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace, Labels: maps.Clone(provisioned)}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: testNamespace}},
	)
	allowAllPermissionChecks(clientset)
	forbidNamespaceVerbs(clientset, "*")

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: saName,
		Image:              "aicr:test",
		RunID:              testRunID,
	})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v; exact-ServiceAccount mode must not need any namespace permission", err)
	}

	if verbs := namespaceVerbs(clientset); len(verbs) != 0 {
		t.Errorf("Deploy issued %v on namespaces; exact-ServiceAccount mode must leave the namespace alone", verbs)
	}
	if got := trackedNamespace(t, clientset, testNamespace).Labels; !maps.Equal(got, provisioned) {
		t.Errorf("namespace labels = %v, want the provisioner's %v unchanged", got, provisioned)
	}
	if _, err := clientset.BatchV1().Jobs(testNamespace).Get(ctx, d.jobName(), metav1.GetOptions{}); err != nil {
		t.Errorf("Job not created: %v", err)
	}
}

// TestDeploy_PrefixModeUsesExistingNamespaceWithoutCreate pins the read
// before the create: a prefix-mode caller that may read the namespace but
// not create namespaces completes Deploy when the namespace already exists
// and is labeled, because a create-first flow would be refused Forbidden.
func TestDeploy_PrefixModeUsesExistingNamespaceWithoutCreate(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	clientset := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   testNamespace,
		Labels: map[string]string{labelAppManagedBy: appName},
	}})
	allowAllPermissionChecks(clientset)
	forbidNamespaceVerbs(clientset, verbCreate, verbPatch, verbUpdate)

	d := NewDeployer(clientset, Config{
		Namespace: testNamespace,
		Image:     "aicr:test",
		RunID:     testRunID,
	})
	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy() error = %v; an existing, labeled namespace must need no namespace write", err)
	}

	if got, want := namespaceVerbs(clientset), []string{verbGet}; !slices.Equal(got, want) {
		t.Errorf("namespace requests = %v, want %v", got, want)
	}
	if !d.hasCreated(kindServiceAccount) {
		t.Error("prefix mode created no run-scoped ServiceAccount")
	}
}

// TestDeploy_PrefixModeMissingNamespaceNotCreatable keeps the failure
// explicit when the namespace does not exist and the caller may not create
// it: a permission error, not an internal one, and nothing created after it.
func TestDeploy_PrefixModeMissingNamespaceNotCreatable(t *testing.T) {
	ctx := context.Background()
	captureLogs(t)

	clientset := fake.NewClientset()
	allowAllPermissionChecks(clientset)
	forbidNamespaceVerbs(clientset, verbCreate)

	d := NewDeployer(clientset, Config{
		Namespace: testNamespace,
		Image:     "aicr:test",
		RunID:     testRunID,
	})
	err := d.Deploy(ctx)
	if err == nil {
		t.Fatal("Deploy() error = nil; a missing namespace the caller cannot create must fail the run")
	}
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeUnauthorized)
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("Deploy() error = %v, want it to say the namespace does not exist", err)
	}
	if _, jobErr := clientset.BatchV1().Jobs(testNamespace).Get(ctx, d.jobName(), metav1.GetOptions{}); !apierrors.IsNotFound(jobErr) {
		t.Errorf("Job Get error = %v, want NotFound", jobErr)
	}
	assertNoRBACObjects(ctx, t, clientset)
}

// TestEnsureNamespace covers every branch of the prefix-mode namespace flow:
// read first, create only when the namespace is absent or unreadable, and
// label an existing namespace only when the label is missing.
func TestEnsureNamespace(t *testing.T) {
	nsGR := schema.GroupResource{Resource: namespacesResource}

	tests := []struct {
		name string
		// seedLabels, when non-nil, pre-creates the namespace with them.
		seedLabels map[string]string
		// forbid lists the Namespace verbs the caller does not hold.
		forbid []string
		// reactors installs any further Namespace reactors.
		reactors    func(*fake.Clientset)
		wantVerbs   []string
		wantErrCode aicrerrors.ErrorCode
		wantErrMsg  string
		// wantLabels is the namespace's labels afterwards; nil skips the
		// check (the namespace is not expected to exist).
		wantLabels map[string]string
	}{
		{
			name:       "absent namespace is created with the label",
			wantVerbs:  []string{verbGet, verbCreate},
			wantLabels: map[string]string{labelAppManagedBy: appName},
		},
		{
			name:       "existing labeled namespace needs no write",
			seedLabels: map[string]string{labelAppManagedBy: appName},
			forbid:     []string{verbCreate, verbPatch, verbUpdate},
			wantVerbs:  []string{verbGet},
			wantLabels: map[string]string{labelAppManagedBy: appName},
		},
		{
			name:       "existing unlabeled namespace is labeled without create",
			seedLabels: map[string]string{"foo": "bar"},
			forbid:     []string{verbCreate},
			wantVerbs:  []string{verbGet, verbPatch},
			wantLabels: map[string]string{"foo": "bar", labelAppManagedBy: appName},
		},
		{
			name:        "absent namespace the caller may not create fails explicitly",
			forbid:      []string{verbCreate},
			wantVerbs:   []string{verbGet, verbCreate},
			wantErrCode: aicrerrors.ErrCodeUnauthorized,
			wantErrMsg:  "does not exist and the caller may not create it",
		},
		{
			name:       "unreadable namespace falls back to create",
			forbid:     []string{verbGet},
			wantVerbs:  []string{verbGet, verbCreate},
			wantLabels: map[string]string{labelAppManagedBy: appName},
		},
		{
			name:        "unreadable namespace the caller may not create fails explicitly",
			seedLabels:  map[string]string{labelAppManagedBy: appName},
			forbid:      []string{verbGet, verbCreate},
			wantVerbs:   []string{verbGet, verbCreate},
			wantErrCode: aicrerrors.ErrCodeUnauthorized,
			wantErrMsg:  "may neither read nor create",
		},
		{
			name:        "existing namespace the caller may create but not read fails explicitly",
			seedLabels:  map[string]string{labelAppManagedBy: appName},
			forbid:      []string{verbGet},
			wantVerbs:   []string{verbGet, verbCreate, verbGet},
			wantErrCode: aicrerrors.ErrCodeUnauthorized,
			wantErrMsg:  "exists but the caller may not read it",
			wantLabels:  map[string]string{labelAppManagedBy: appName},
		},
		{
			name:        "namespace labeled by another manager the caller may not patch fails explicitly",
			seedLabels:  map[string]string{labelAppManagedBy: "Helm"},
			forbid:      []string{verbPatch},
			wantVerbs:   []string{verbGet, verbPatch},
			wantErrCode: aicrerrors.ErrCodeUnauthorized,
			wantErrMsg:  "may not label namespace",
			wantLabels:  map[string]string{labelAppManagedBy: "Helm"},
		},
		{
			name:       "namespace created after the read is labeled",
			seedLabels: map[string]string{"foo": "bar"},
			reactors: func(clientset *fake.Clientset) {
				var reads int
				clientset.PrependReactor(verbGet, namespacesResource, func(k8stesting.Action) (bool, runtime.Object, error) {
					reads++
					if reads == 1 {
						return true, nil, apierrors.NewNotFound(nsGR, testNamespace)
					}
					return false, nil, nil
				})
			},
			wantVerbs:  []string{verbGet, verbCreate, verbGet, verbPatch},
			wantLabels: map[string]string{"foo": "bar", labelAppManagedBy: appName},
		},
		{
			name: "unexpected read error fails without a create",
			reactors: func(clientset *fake.Clientset) {
				clientset.PrependReactor(verbGet, namespacesResource, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewInternalError(stderrors.New("apiserver exploded"))
				})
			},
			wantVerbs:   []string{verbGet},
			wantErrCode: aicrerrors.ErrCodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []runtime.Object
			if tt.seedLabels != nil {
				objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
					Name: testNamespace, Labels: maps.Clone(tt.seedLabels),
				}})
			}
			clientset := fake.NewClientset(objs...)
			forbidNamespaceVerbs(clientset, tt.forbid...)
			if tt.reactors != nil {
				tt.reactors(clientset)
			}

			d := NewDeployer(clientset, Config{Namespace: testNamespace, RunID: testRunID})
			err := d.ensureNamespace(context.Background())

			if tt.wantErrCode != "" {
				if err == nil {
					t.Fatalf("ensureNamespace() error = nil, want %s", tt.wantErrCode)
				}
				errorstest.WantReportedCode(t, err, tt.wantErrCode)
				if !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("ensureNamespace() error = %v, want it to contain %q", err, tt.wantErrMsg)
				}
			} else if err != nil {
				t.Fatalf("ensureNamespace() error = %v", err)
			}

			if got := namespaceVerbs(clientset); !slices.Equal(got, tt.wantVerbs) {
				t.Errorf("namespace requests = %v, want %v", got, tt.wantVerbs)
			}
			if tt.wantLabels != nil {
				if got := trackedNamespace(t, clientset, testNamespace).Labels; !maps.Equal(got, tt.wantLabels) {
					t.Errorf("namespace labels = %v, want %v", got, tt.wantLabels)
				}
			}
		})
	}
}
