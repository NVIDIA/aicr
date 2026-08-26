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
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"sync"
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

// exactSAName is the operator-provisioned ServiceAccount the
// exact-ServiceAccount-mode cases name verbatim.
const exactSAName = "irsa-snapshotter"

// The two access-review resources the gate posts to. They are "created"
// over the REST API but persist nothing — the create IS the authorization
// query — which is why the write guard below exempts them.
const (
	selfReviewResource    = "selfsubjectaccessreviews"
	subjectReviewResource = "subjectaccessreviews"
)

// askedAccess is one authorization question a reactor observed, flattened
// out of whichever review kind carried it. subject is "" when the question
// came from a SelfSubjectAccessReview (i.e. it was asked about the caller).
type askedAccess struct {
	subject     string
	group       string
	resource    string
	subresource string
	verb        string
	namespace   string
}

// String renders a question the way an assertion failure should read.
func (a askedAccess) String() string {
	return fmt.Sprintf("%s %s (%s) subject=%q", a.verb,
		resourceLabel(a.resource, a.subresource, a.group), scopeLabel(a.namespace), a.subject)
}

// reviewRecorder collects every question the gate asked. CheckPermissions
// fans its checks out over an errgroup, so the reactors run on worker
// goroutines and every field must be mutex-guarded.
type reviewRecorder struct {
	mu    sync.Mutex
	asked []askedAccess
}

func (r *reviewRecorder) record(a askedAccess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, a)
}

// questions returns a copy of what was asked, taken under lock.
func (r *reviewRecorder) questions() []askedAccess {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]askedAccess, len(r.asked))
	copy(out, r.asked)
	return out
}

// asked reports whether a question matching pred was put to the apiserver.
func (r *reviewRecorder) asked1(pred func(askedAccess) bool) bool {
	for _, q := range r.questions() {
		if pred(q) {
			return true
		}
	}
	return false
}

// installReviewReactors answers every Self/SubjectAccessReview with allow(q)
// and records the question. A nil allow permits everything.
//
// Failures are reported with t.Errorf and handed back as the reactor's error
// rather than with t.Fatalf: the reactor runs on an errgroup worker, and
// Goexit there would leave the group waiting on a goroutine that never
// returns.
func installReviewReactors(t *testing.T, cs *fake.Clientset, allow func(askedAccess) bool) *reviewRecorder {
	t.Helper()
	rec := &reviewRecorder{}
	if allow == nil {
		allow = func(askedAccess) bool { return true }
	}

	answer := func(action k8stesting.Action) (askedAccess, bool, error) {
		create, ok := action.(k8stesting.CreateAction)
		if !ok {
			err := fmt.Errorf("action %T is not a CreateAction", action)
			t.Error(err)
			return askedAccess{}, false, err
		}
		var attrs *authv1.ResourceAttributes
		var subject string
		switch obj := create.GetObject().(type) {
		case *authv1.SelfSubjectAccessReview:
			attrs = obj.Spec.ResourceAttributes
		case *authv1.SubjectAccessReview:
			attrs, subject = obj.Spec.ResourceAttributes, obj.Spec.User
			if subject == "" {
				err := stderrors.New("SubjectAccessReview carries no User; it would answer for nobody")
				t.Error(err)
				return askedAccess{}, false, err
			}
		default:
			err := fmt.Errorf("object %T is not an access review", create.GetObject())
			t.Error(err)
			return askedAccess{}, false, err
		}
		if attrs == nil {
			err := stderrors.New("access review carries no ResourceAttributes")
			t.Error(err)
			return askedAccess{}, false, err
		}
		q := askedAccess{
			subject:     subject,
			group:       attrs.Group,
			resource:    attrs.Resource,
			subresource: attrs.Subresource,
			verb:        attrs.Verb,
			namespace:   attrs.Namespace,
		}
		rec.record(q)
		return q, allow(q), nil
	}

	cs.PrependReactor(verbCreate, selfReviewResource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		q, allowed, err := answer(action)
		if err != nil {
			return true, nil, err
		}
		return true, &authv1.SelfSubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: "caller policy for " + q.String()},
		}, nil
	})
	cs.PrependReactor(verbCreate, subjectReviewResource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		q, allowed, err := answer(action)
		if err != nil {
			return true, nil, err
		}
		return true, &authv1.SubjectAccessReview{
			Status: authv1.SubjectAccessReviewStatus{Allowed: allowed, Reason: "ServiceAccount policy for " + q.String()},
		}, nil
	})
	return rec
}

// seedServiceAccount pre-creates the operator-provisioned ServiceAccount
// that puts a Deployer into exact-ServiceAccount mode. Its namespace and
// name are fixed rather than parameters: every caller wants the one
// ServiceAccount that testNamespace/exactSAName names, and passing the same
// two constants at each call site is what unparam flags.
func seedServiceAccount(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	if _, err := cs.CoreV1().ServiceAccounts(testNamespace).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: exactSAName, Namespace: testNamespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding ServiceAccount %s/%s: %v", testNamespace, exactSAName, err)
	}
}

// hasCheck reports whether results holds an answered check matching pred.
func hasCheck(results []permissionCheck, pred func(permissionCheck) bool) bool {
	for _, r := range results {
		if pred(r) {
			return true
		}
	}
	return false
}

// isCallerRBACCheck matches a caller-side check on one of the five RBAC
// kinds the run creates and deletes in prefix mode.
func isCallerRBACCheck(p permissionCheck) bool {
	if p.Subject != "" {
		return false
	}
	switch p.Resource {
	case resourceRoles, resourceRoleBindings, resourceClusterRoles, resourceClusterRoleBindings:
		return true
	case resourceServiceAccounts:
		return p.Verb == verbCreate || p.Verb == verbDelete
	default:
		return false
	}
}

// TestCheckPermissions_PrefixModeRequiresRBACCreateAndDelete pins hole B: the
// deferred Cleanup is registered before Deploy and always runs, issuing a
// UID-pinned delete for every RBAC object the run created. An identity with
// create-but-not-delete used to pass a green pre-flight, deploy, and then
// leak a full run-scoped RBAC set — cluster-scoped objects included — once
// per run.
func TestCheckPermissions_PrefixModeRequiresRBACCreateAndDelete(t *testing.T) {
	// Every (resource, verb, cluster-scoped?) triple prefix mode must
	// demand of the caller. Each is denied on its own to prove it is
	// individually load-bearing rather than incidentally covered.
	required := []struct {
		resource string
		verb     string
		cluster  bool
	}{
		{resourceServiceAccounts, verbCreate, false},
		{resourceServiceAccounts, verbDelete, false},
		{resourceRoles, verbCreate, false},
		{resourceRoles, verbDelete, false},
		{resourceRoleBindings, verbCreate, false},
		{resourceRoleBindings, verbDelete, false},
		{resourceClusterRoles, verbCreate, true},
		{resourceClusterRoles, verbDelete, true},
		{resourceClusterRoleBindings, verbCreate, true},
		{resourceClusterRoleBindings, verbDelete, true},
	}

	for _, req := range required {
		t.Run(req.verb+" "+req.resource, func(t *testing.T) {
			clientset := fake.NewClientset()
			rec := installReviewReactors(t, clientset, func(q askedAccess) bool {
				return q.resource != req.resource || q.verb != req.verb
			})

			d := NewDeployer(clientset, Config{
				Namespace:          testNamespace,
				ServiceAccountName: exactSAName, // nothing seeded: prefix mode
				RunID:              testRunID,
			})
			results, err := d.CheckPermissions(context.Background())
			if err == nil {
				t.Fatalf("CheckPermissions() error = nil; %s %s must be required in prefix mode", req.verb, req.resource)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeUnauthorized, "")) {
				t.Errorf("error code = %v, want ErrCodeUnauthorized", err)
			}

			wantScope := scopeLabel(testNamespace)
			if req.cluster {
				wantScope = scopeLabel("")
			}
			wantLine := fmt.Sprintf("%s cannot %q %s (%s)", callerSubjectLabel, req.verb,
				resourceLabel(req.resource, "", rbacGroupFor(req.resource)), wantScope)
			if !strings.Contains(err.Error(), wantLine) {
				t.Errorf("error = %v\nwant a line containing %q", err, wantLine)
			}
			carriesDenied := func(p permissionCheck) bool {
				return p.Resource == req.resource && p.Verb == req.verb && !p.Allowed
			}
			if !hasCheck(results, carriesDenied) {
				t.Errorf("results do not carry the denied %s %s check", req.verb, req.resource)
			}
			// The gate must have resolved the mode first, which means it
			// asked whether it may read ServiceAccounts.
			askedSAGet := func(q askedAccess) bool {
				return q.subject == "" && q.resource == resourceServiceAccounts && q.verb == verbGet
			}
			if !rec.asked1(askedSAGet) {
				t.Error("gate never asked for `serviceaccounts: get`, so it cannot have resolved the mode")
			}
		})
	}
}

// rbacGroupFor returns the API group a resource lives in, for building the
// exact message line the gate is expected to render.
func rbacGroupFor(resource string) string {
	if resource == resourceServiceAccounts {
		return ""
	}
	return rbacAPIGroup
}

// TestCheckPermissions_ExactModeSkipsCallerRBACVerbs is the other half of the
// mode split. In exact-ServiceAccount mode aicr creates and deletes no RBAC
// at all, so demanding create/delete on the five kinds would fail operators
// who legitimately hold none of those grants — the very operators the exact
// mode exists for.
func TestCheckPermissions_ExactModeSkipsCallerRBACVerbs(t *testing.T) {
	clientset := fake.NewClientset()
	seedServiceAccount(t, clientset)

	// Deny every caller-side RBAC verb outright. A correct gate never asks.
	rec := installReviewReactors(t, clientset, func(q askedAccess) bool {
		if q.subject != "" {
			return true
		}
		switch q.resource {
		case resourceRoles, resourceRoleBindings, resourceClusterRoles, resourceClusterRoleBindings:
			return false
		case resourceServiceAccounts:
			return q.verb == verbGet
		default:
			return true
		}
	})

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: exactSAName,
		RunID:              testRunID,
	})
	results, err := d.CheckPermissions(context.Background())
	if err != nil {
		t.Fatalf("CheckPermissions() error = %v, want nil (exact mode needs no RBAC create/delete)", err)
	}
	if d.managesRBAC() {
		t.Fatal("managesRBAC() = true; the seeded ServiceAccount should have put the run in exact mode")
	}
	if hasCheck(results, isCallerRBACCheck) {
		t.Error("gate demanded a caller RBAC verb in exact-ServiceAccount mode")
	}
	askedCallerRBAC := func(q askedAccess) bool {
		return q.subject == "" && q.resource == resourceClusterRoles
	}
	if rec.asked1(askedCallerRBAC) {
		t.Error("gate asked the apiserver about clusterroles in exact-ServiceAccount mode")
	}

	// It must instead have asked the ServiceAccount's own questions, and
	// asked them of the ServiceAccount rather than of the caller.
	subject := serviceAccountUsername(testNamespace, exactSAName)
	askedClusterRoles := func(q askedAccess) bool {
		return q.subject == subject && q.resource == resourceNodes && q.verb == verbList
	}
	if !rec.asked1(askedClusterRoles) {
		t.Errorf("gate never asked whether %s may list nodes", subject)
	}
}

// TestCheckPermissions_ServiceAccountGetIsRequired pins hole A at the gate.
// Without `serviceaccounts: get` the ServiceAccount mode is unknowable, and
// the old behavior — treat an unreadable ServiceAccount as a name prefix —
// silently ran an operator's explicitly-named IRSA / Workload Identity
// account as a generated one carrying none of its cloud annotations.
func TestCheckPermissions_ServiceAccountGetIsRequired(t *testing.T) {
	clientset := fake.NewClientset()
	seedServiceAccount(t, clientset)
	rec := installReviewReactors(t, clientset, func(q askedAccess) bool {
		return q.resource != resourceServiceAccounts || q.verb != verbGet
	})

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: exactSAName,
		RunID:              testRunID,
	})
	results, err := d.CheckPermissions(context.Background())
	if err == nil {
		t.Fatal("CheckPermissions() error = nil; a caller that cannot read ServiceAccounts must fail the gate")
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeUnauthorized, "")) {
		t.Errorf("error code = %v, want ErrCodeUnauthorized", err)
	}
	for _, want := range []string{
		fmt.Sprintf("%s cannot %q %s", callerSubjectLabel, verbGet, resourceServiceAccounts),
		"mode-specific permissions were not evaluated",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to contain %q", err, want)
		}
	}

	// It must NOT have silently downgraded to prefix mode and carried on:
	// no mode-specific question may have been asked, and the run must not
	// have adopted the seeded ServiceAccount either.
	if hasCheck(results, isCallerRBACCheck) {
		t.Error("gate evaluated prefix-mode RBAC verbs despite being unable to resolve the mode")
	}
	if rec.asked1(func(q askedAccess) bool { return q.subject != "" }) {
		t.Error("gate issued a SubjectAccessReview despite being unable to resolve the mode")
	}
	if d.existingServiceAccount() != "" {
		t.Errorf("existingServiceAccount() = %q, want \"\" (nothing may be resolved)", d.existingServiceAccount())
	}
}

// TestCheckPermissions_ServiceAccountSubjectFailureNamesTheSubject covers
// requirement 3: the agent pod runs as the ServiceAccount, so the gate must
// verify that identity's own permissions with a SubjectAccessReview, not the
// caller's with a SelfSubjectAccessReview. In exact mode aicr grants nothing
// and the operator was supposed to have applied
// `--add-roles-to-service-account`; "you never applied those manifests" is
// far better caught here than in a pod minutes later.
func TestCheckPermissions_ServiceAccountSubjectFailureNamesTheSubject(t *testing.T) {
	clientset := fake.NewClientset()
	seedServiceAccount(t, clientset)
	installReviewReactors(t, clientset, func(q askedAccess) bool {
		// The caller is fully privileged; the ServiceAccount is missing
		// exactly the cluster-scoped node read the agent cannot work without.
		return q.subject == "" || q.resource != resourceNodes
	})

	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: exactSAName,
		RunID:              testRunID,
	})
	results, err := d.CheckPermissions(context.Background())
	if err == nil {
		t.Fatal("CheckPermissions() error = nil; a ServiceAccount missing `nodes: list` must fail the gate")
	}

	subject := serviceAccountUsername(testNamespace, exactSAName)
	for _, want := range []string{
		fmt.Sprintf("agent ServiceAccount %q cannot %q %s (%s)", subject, verbList, resourceNodes, scopeLabel("")),
		"--add-roles-to-service-account " + exactSAName,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v\nwant it to contain %q", err, want)
		}
	}
	// A caller-scoped answer must never be substituted for the subject's.
	askedSubjectNodes := func(p permissionCheck) bool {
		return p.Subject == subject && p.Resource == resourceNodes && !p.Allowed && !p.Unverified
	}
	if !hasCheck(results, askedSubjectNodes) {
		t.Error("results carry no denied ServiceAccount-subject check for nodes")
	}
}

// TestCheckPermissions_SubjectAccessReviewForbiddenReportsAndContinues covers
// the meta-permission honestly. Creating a SubjectAccessReview is itself a
// privilege; a caller without it learns nothing about the ServiceAccount.
// Silently skipping would be the same defect class as hole A — a missing
// answer read as a passing one — so the gate records the checks as
// unverified, warns, and lets the run proceed to fail visibly in-pod.
func TestCheckPermissions_SubjectAccessReviewForbiddenReportsAndContinues(t *testing.T) {
	clientset := fake.NewClientset()
	seedServiceAccount(t, clientset)
	installReviewReactors(t, clientset, nil)
	// Prepended after installReviewReactors, so it wins for this resource.
	clientset.PrependReactor(verbCreate, subjectReviewResource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "authorization.k8s.io", Resource: subjectReviewResource}, "",
			stderrors.New(`User "snapshot-runner" cannot create resource "subjectaccessreviews"`))
	})

	logs := captureLogs(t)
	d := NewDeployer(clientset, Config{
		Namespace:          testNamespace,
		ServiceAccountName: exactSAName,
		RunID:              testRunID,
	})
	results, err := d.CheckPermissions(context.Background())
	if err != nil {
		t.Fatalf("CheckPermissions() error = %v, want nil (an unanswerable check is not a failed one)", err)
	}

	var unverified int
	for _, r := range results {
		if r.Unverified {
			unverified++
			if r.Subject == "" {
				t.Error("a caller check was marked unverified; only ServiceAccount checks may be")
			}
		}
	}
	if unverified == 0 {
		t.Fatal("no check was marked Unverified; the gate silently dropped the ServiceAccount's permissions")
	}

	for _, want := range []string{
		"could not verify the agent ServiceAccount's own permissions",
		"cannot create resource \\\"subjectaccessreviews\\\"",
		"kubectl auth can-i --list --as " + serviceAccountUsername(testNamespace, exactSAName),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning log = %s\nwant it to contain %q", logs.String(), want)
		}
	}
}

// TestCheckPermissions_ReportsEveryMissingPermissionAtOnce pins requirement
// 4: an operator fixing permissions should get the complete list in one run
// rather than discovering them one denial at a time.
func TestCheckPermissions_ReportsEveryMissingPermissionAtOnce(t *testing.T) {
	clientset := fake.NewClientset()
	denied := map[string]string{
		resourceClusterRoles:        verbDelete,
		resourceClusterRoleBindings: verbDelete,
		resourceJobs:                verbCreate,
		resourcePods:                verbWatch,
	}
	installReviewReactors(t, clientset, func(q askedAccess) bool {
		return denied[q.resource] != q.verb
	})

	d := NewDeployer(clientset, Config{
		Namespace: testNamespace,
		RunID:     testRunID,
	})
	_, err := d.CheckPermissions(context.Background())
	if err == nil {
		t.Fatal("CheckPermissions() error = nil, want the four denied permissions reported")
	}
	for resource, verb := range denied {
		line := fmt.Sprintf("cannot %q %s", verb, resource)
		if !strings.Contains(err.Error(), line) {
			t.Errorf("error = %v\nwant it to report %q too", err, line)
		}
	}
	// Prefix mode is what this Deployer is in, so the remediation must point
	// at the exact-ServiceAccount escape hatch rather than at manifests.
	if !strings.Contains(err.Error(), "--service-account-name") {
		t.Errorf("error = %v\nwant prefix-mode remediation naming --service-account-name", err)
	}
}

// TestCheckPermissions_IssuesNoWriteBeforeTheGateCloses is the structural
// guarantee behind resolving the ServiceAccount inside the gate: resolving
// is a read, and fail-before-mutate is about not WRITING before validation.
// A reactor rejects every create/update/patch/delete of a real object, so any
// regression that mutates during the pre-flight fails here rather than in
// production.
func TestCheckPermissions_IssuesNoWriteBeforeTheGateCloses(t *testing.T) {
	tests := []struct {
		name    string
		exact   bool
		allow   func(askedAccess) bool
		wantErr bool
	}{
		{name: "prefix mode, all granted"},
		{name: "exact mode, all granted", exact: true},
		{
			name:    "prefix mode, denied",
			allow:   func(q askedAccess) bool { return q.resource != resourceClusterRoles },
			wantErr: true,
		},
		{
			name:    "exact mode, ServiceAccount denied",
			exact:   true,
			allow:   func(q askedAccess) bool { return q.subject == "" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			if tt.exact {
				// Seeded through the tracker before the guard is armed.
				seedServiceAccount(t, clientset)
			}
			installReviewReactors(t, clientset, tt.allow)

			var mu sync.Mutex
			var writes []string
			clientset.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
				switch action.GetVerb() {
				case verbCreate, verbUpdate, verbPatch, verbDelete, "deletecollection":
				default:
					return false, nil, nil
				}
				// Access reviews are "created" over the REST API but
				// persist nothing — they are the authorization query
				// itself. Everything else is a real mutation.
				if res := action.GetResource().Resource; res == selfReviewResource || res == subjectReviewResource {
					return false, nil, nil
				}
				write := action.GetVerb() + " " + action.GetResource().Resource
				mu.Lock()
				writes = append(writes, write)
				mu.Unlock()
				return true, nil, fmt.Errorf("pre-flight issued a write: %s", write)
			})

			d := NewDeployer(clientset, Config{
				Namespace:           testNamespace,
				ServiceAccountName:  exactSAName,
				RunID:               testRunID,
				OwnsOutputConfigMap: true,
				Output:              "cm://" + testNamespace + "/" + StagingConfigMapName(testRunID),
			})
			_, err := d.CheckPermissions(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckPermissions() error = %v, wantErr %v", err, tt.wantErr)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(writes) > 0 {
				t.Errorf("pre-flight mutated the cluster before the gate closed: %v", writes)
			}
		})
	}
}

// TestCheckPermissions_ConfigMapDeleteGatedOnOwnership pins the gate on the
// `configmaps: delete` verb. CheckPermissions fails closed, so an
// unconditional entry would make Deploy return ErrCodeUnauthorized at Step 0
// for a caller who supplied their own `cm://` output URI — a run that never
// deletes a ConfigMap at all, because both Cleanup's staging sweep and
// getSnapshotFromConfigMap's created-set record are gated on
// Config.OwnsOutputConfigMap.
func TestCheckPermissions_ConfigMapDeleteGatedOnOwnership(t *testing.T) {
	tests := []struct {
		name              string
		ownsOutput        bool
		wantCMDeleteCheck bool
		wantErr           bool
	}{
		{
			name:              "owns output ConfigMap requires configmaps delete",
			ownsOutput:        true,
			wantCMDeleteCheck: true,
			// The identity below is denied exactly `configmaps: delete`,
			// so a required check makes the whole pre-flight fail.
			wantErr: true,
		},
		{
			name:              "caller-supplied output ConfigMap does not require configmaps delete",
			ownsOutput:        false,
			wantCMDeleteCheck: false,
			wantErr:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			// Deny only `configmaps: delete`; allow everything else. This
			// models the least-privilege identity the gate exists for.
			installReviewReactors(t, clientset, func(q askedAccess) bool {
				return q.resource != resourceCM || q.verb != verbDelete
			})

			deployer := NewDeployer(clientset, Config{
				Namespace:           testNamespace,
				RunID:               testRunID,
				OwnsOutputConfigMap: tt.ownsOutput,
			})

			checks, err := deployer.CheckPermissions(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckPermissions() error = %v, wantErr %v", err, tt.wantErr)
			}

			gotCMDelete := hasCheck(checks, func(p permissionCheck) bool {
				return p.Resource == resourceCM && p.Verb == verbDelete
			})
			if gotCMDelete != tt.wantCMDeleteCheck {
				t.Errorf("configmaps delete check present = %v, want %v", gotCMDelete, tt.wantCMDeleteCheck)
			}
		})
	}
}

// TestCheckPermissions_AllGrantedPassesBothModes is the happy path, and also
// asserts the gate covers the run's non-RBAC work — the Job it creates and
// waits on, the pods it discovers and streams logs from, and the ConfigMap
// it reads the snapshot back out of.
func TestCheckPermissions_AllGrantedPassesBothModes(t *testing.T) {
	tests := []struct {
		name  string
		exact bool
	}{
		{name: "prefix mode"},
		{name: "exact-ServiceAccount mode", exact: true},
	}

	want := []askedAccess{
		{group: batchAPIGroup, resource: resourceJobs, verb: verbCreate, namespace: testNamespace},
		{group: batchAPIGroup, resource: resourceJobs, verb: verbWatch, namespace: testNamespace},
		{group: batchAPIGroup, resource: resourceJobs, verb: verbDelete, namespace: testNamespace},
		{resource: resourcePods, verb: verbList, namespace: testNamespace},
		{resource: resourcePods, subresource: subresourceLog, verb: verbGet, namespace: testNamespace},
		{resource: resourceCM, verb: verbGet, namespace: testNamespace},
		{resource: resourceServiceAccounts, verb: verbGet, namespace: testNamespace},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			if tt.exact {
				seedServiceAccount(t, clientset)
			}
			rec := installReviewReactors(t, clientset, nil)

			d := NewDeployer(clientset, Config{
				Namespace:          testNamespace,
				ServiceAccountName: exactSAName,
				RunID:              testRunID,
			})
			results, err := d.CheckPermissions(context.Background())
			if err != nil {
				t.Fatalf("CheckPermissions() error = %v, want nil", err)
			}
			if len(results) == 0 {
				t.Fatal("CheckPermissions() returned no checks")
			}
			for _, r := range results {
				if !r.Allowed {
					t.Errorf("check %s %s (%s) denied under an allow-all policy", r.Verb, r.Resource, r.Namespace)
				}
			}
			for _, q := range want {
				if !rec.asked1(func(got askedAccess) bool { return got == q }) {
					t.Errorf("gate never asked: %s", q)
				}
			}
		})
	}
}

// TestReviewAccess covers the two review kinds one question at a time: a
// caller check must go out as a SelfSubjectAccessReview and a ServiceAccount
// check as a SubjectAccessReview naming that subject, because the former
// cannot answer for anyone but the caller.
func TestReviewAccess(t *testing.T) {
	subject := serviceAccountUsername(testNamespace, exactSAName)

	tests := []struct {
		name    string
		check   accessCheck
		allowed bool
	}{
		{
			name:    "caller check allowed",
			check:   accessCheck{group: batchAPIGroup, resource: resourceJobs, verb: verbCreate, namespace: testNamespace},
			allowed: true,
		},
		{
			name:  "caller check denied",
			check: accessCheck{group: batchAPIGroup, resource: resourceJobs, verb: verbCreate, namespace: testNamespace},
		},
		{
			name:    "ServiceAccount subresource check allowed",
			check:   accessCheck{resource: resourcePods, subresource: "exec", verb: verbCreate, subject: subject},
			allowed: true,
		},
		{
			name:  "ServiceAccount cluster-scoped check denied",
			check: accessCheck{resource: resourceNodes, verb: verbList, subject: subject},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset()
			rec := installReviewReactors(t, clientset, func(askedAccess) bool { return tt.allowed })

			d := NewDeployer(clientset, Config{Namespace: testNamespace, RunID: testRunID})
			allowed, reason, err := d.reviewAccess(context.Background(), tt.check)
			if err != nil {
				t.Fatalf("reviewAccess() error = %v", err)
			}
			if allowed != tt.allowed {
				t.Errorf("reviewAccess() allowed = %v, want %v", allowed, tt.allowed)
			}
			if reason == "" {
				t.Error("reviewAccess() reason is empty; the authorizer's explanation must be carried through")
			}

			asked := rec.questions()
			if len(asked) != 1 {
				t.Fatalf("questions asked = %d, want exactly 1", len(asked))
			}
			want := askedAccess{
				subject:     tt.check.subject,
				group:       tt.check.group,
				resource:    tt.check.resource,
				subresource: tt.check.subresource,
				verb:        tt.check.verb,
				namespace:   tt.check.namespace,
			}
			if asked[0] != want {
				t.Errorf("asked %s, want %s", asked[0], want)
			}
		})
	}
}

// TestDedupeChecks pins that the gate never asks the apiserver the same
// question twice, while keeping questions that differ only in scope: an
// operator who applied the Role but not the ClusterRole must fail exactly
// the cluster-scoped one.
func TestDedupeChecks(t *testing.T) {
	nsPods := accessCheck{resource: resourcePods, verb: verbList, namespace: testNamespace}
	clusterPods := accessCheck{resource: resourcePods, verb: verbList}

	got := dedupeChecks([]accessCheck{nsPods, clusterPods, nsPods, clusterPods, nsPods})
	want := []accessCheck{nsPods, clusterPods}
	if len(got) != len(want) {
		t.Fatalf("dedupeChecks() returned %d checks, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("check %d = %+v, want %+v (first-seen order must be preserved)", i, got[i], want[i])
		}
	}
}

// TestServiceAccountChecksTrackTheGrantedRules pins the anti-drift property:
// the questions asked of the agent ServiceAccount are derived from the same
// namespacedRules / clusterRules definitions the run-scoped Role and
// ClusterRole are built from, so the gate cannot fall behind what the agent
// needs. --discover-network widens the rule set, and the gate must widen
// with it.
func TestServiceAccountChecksTrackTheGrantedRules(t *testing.T) {
	subject := serviceAccountUsername(testNamespace, exactSAName)

	for _, discover := range []bool{false, true} {
		name := "baseline"
		if discover {
			name = "discover-network"
		}
		t.Run(name, func(t *testing.T) {
			d := NewDeployer(fake.NewClientset(), Config{
				Namespace:       testNamespace,
				RunID:           testRunID,
				DiscoverNetwork: discover,
			})
			got := make(map[accessCheck]struct{})
			for _, c := range d.serviceAccountChecks(subject) {
				got[c] = struct{}{}
			}

			nsChecks := checksFromRules(namespacedRules(), testNamespace, subject)
			clusterChecks := checksFromRules(clusterRules(discover), "", subject)
			wantAll := make([]accessCheck, 0, len(nsChecks)+len(clusterChecks))
			wantAll = append(wantAll, nsChecks...)
			wantAll = append(wantAll, clusterChecks...)
			for _, c := range wantAll {
				if _, ok := got[c]; !ok {
					t.Errorf("granted rule not checked: %+v", c)
				}
				if c.subject != subject {
					t.Errorf("check %+v is not asked of the ServiceAccount", c)
				}
			}

			// pods/exec is the rule most easily lost to naive splitting:
			// the apiserver evaluates it as resource "pods", subresource
			// "exec", never as a resource literally named "pods/exec".
			execCheck := accessCheck{resource: resourcePods, subresource: "exec", verb: verbCreate, subject: subject}
			if _, ok := got[execCheck]; ok != discover {
				t.Errorf("pods/exec check present = %v, want %v", ok, discover)
			}
		})
	}
}
