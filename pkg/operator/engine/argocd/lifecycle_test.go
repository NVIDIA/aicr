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

package argocd

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Only managers other than the operator that own a field the operator sets
// are reported; status writers and unrelated fields are not.
func TestForeignManagers(t *testing.T) {
	t.Parallel()
	p, r := testPlan()
	desired, err := Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	live := desired.DeepCopy()
	live.SetManagedFields([]metav1.ManagedFieldsEntry{
		{Manager: FieldManager, Operation: metav1.ManagedFieldsOperationApply,
			FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:project":{}}}`)},
		{Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:source":{"f:helm":{"f:valuesObject":{"f:replicas":{}}}}}}`)},
		{Manager: "argocd-application-controller", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: metav1.NewFieldsV1(`{"f:status":{"f:health":{}},"f:operation":{}}`)},
		{Manager: "admin", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: metav1.NewFieldsV1(`{"f:metadata":{"f:labels":{"f:team":{}}}}`)},
		{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationUpdate,
			FieldsV1: metav1.NewFieldsV1(`{"f:metadata":{"f:annotations":{"f:operator.aicr.run/plan-digest":{}}}}`)},
		{Manager: "status-writer", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status",
			FieldsV1: metav1.NewFieldsV1(`{"f:spec":{"f:project":{}}}`)},
	})
	got, err := foreignManagers(live, desired)
	if err != nil || !slices.Equal(got, []string{"kubectl", "kubectl-edit"}) {
		t.Fatalf("foreignManagers() = %v, %v", got, err)
	}
}

// A spec field another manager owns and the operator does not set is
// reported, because a server-side apply would keep it; fields the operator
// also sets, metadata and status are not.
func TestForeignSpecFields(t *testing.T) {
	t.Parallel()
	p, r := testPlan()
	desired, err := Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(manager, fields string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, FieldsV1: metav1.NewFieldsV1(fields)}
	}
	cases := []struct {
		name    string
		entries []metav1.ManagedFieldsEntry
		want    bool
	}{
		{"value files", []metav1.ManagedFieldsEntry{entry("kubectl-edit", `{"f:spec":{"f:source":{"f:helm":{"f:valueFiles":{}}}}}`)}, true},
		{"a plugin", []metav1.ManagedFieldsEntry{entry("kubectl-edit", `{"f:spec":{"f:source":{"f:plugin":{".":{},"f:name":{}}}}}`)}, true},
		{"a field the operator sets", []metav1.ManagedFieldsEntry{entry("kubectl-edit", `{"f:spec":{".":{},"f:project":{}}}`)}, false},
		{"metadata and status", []metav1.ManagedFieldsEntry{
			entry("admin", `{"f:metadata":{"f:labels":{"f:team":{}}}}`),
			entry("argocd-application-controller", `{"f:status":{"f:health":{}},"f:operation":{}}`),
		}, false},
		{"the operator's own", []metav1.ManagedFieldsEntry{entry(FieldManager, `{"f:spec":{"f:source":{"f:helm":{"f:valueFiles":{}}}}}`)}, false},
	}
	for _, tc := range cases {
		live := desired.DeepCopy()
		live.SetManagedFields(tc.entries)
		if got, err := foreignSpecFields(live, desired); err != nil || got != tc.want {
			t.Errorf("%s: foreignSpecFields() = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
}

// Verify writes a sync operation at the chart digest with the Application,
// and nothing while an operation is pending or running.
func TestVerifyStartsOneSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, r := testPlan()
	a := fakeAdapter(t)
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(ctx, p, r, values); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	rev, _, _ := unstructured.NestedString(live.Object, "operation", "sync", "revision")
	user, _, _ := unstructured.NestedString(live.Object, "operation", "initiatedBy", "username")
	opts, _, _ := unstructured.NestedStringSlice(live.Object, "operation", "sync", "syncOptions")
	if rev != chartDigest || user != FieldManager || !slices.Equal(opts, []string{"ServerSideApply=true", "RespectIgnoreDifferences=true"}) {
		t.Fatalf("operation = %v", live.Object["operation"])
	}

	version := live.GetResourceVersion()
	if err = a.Verify(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if again, _ := a.get(ctx, r.ID); again.GetResourceVersion() != version {
		t.Error("Verify wrote again while a sync was pending")
	}

	running := readyApp(t, testNow)
	if err = unstructured.SetNestedField(running.Object, "Running", "status", "operationState", "phase"); err != nil {
		t.Fatal(err)
	}
	b := fakeAdapter(t, running)
	if err = b.Verify(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.get(ctx, r.ID); after.Object["operation"] != nil {
		t.Error("Verify requested a sync while one was running")
	}
}

// Remove marks only the recorded object with the rollout and the resources
// finalizer, then deletes it; Retired reports it gone.
func TestRemoveRetiresOnlyTheRecordedObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, r := testPlan()
	app := readyApp(t, testNow)
	app.SetUID("11111111-2222-3333-4444-555555555555")
	a := fakeAdapter(t, app)

	if err := a.Remove(ctx, uid, r.ID, "99999999-2222-3333-4444-555555555555", "rollout-x-1"); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("Remove(other uid) = %v, want CONFLICT", err)
	}
	if err := a.Remove(ctx, "another-runtime", r.ID, string(app.GetUID()), "rollout-x-1"); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("Remove(other runtime) = %v, want CONFLICT", err)
	}
	if obs, err := a.Retired(ctx, r.ID, string(app.GetUID())); err != nil || obs.Reason != "DeletePending" {
		t.Fatalf("Retired() before Remove = %+v, %v", obs, err)
	}
	if err := a.Remove(ctx, uid, r.ID, string(app.GetUID()), "rollout-x-1"); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatalf("the finalizer should hold the Application while Argo CD deletes resources: %v", err)
	}
	if live.GetDeletionTimestamp() == nil || !slices.Contains(live.GetFinalizers(), ResourcesFinalizer) ||
		live.GetAnnotations()[v1alpha1.AnnotationDeleteRollout] != "rollout-x-1" {

		t.Fatalf("removed Application = %v", live.Object["metadata"])
	}
	if src, _, _ := unstructured.NestedMap(live.Object, "spec", "source"); src["targetRevision"] != chartDigest {
		t.Error("marking the Application for removal dropped the operator's spec")
	}
	if obs, _ := a.Retired(ctx, r.ID, string(app.GetUID())); obs.State != StateProgressing || obs.Reason != "Deleting" {
		t.Fatalf("Retired() while deleting = %+v", obs)
	}
	if err = a.Remove(ctx, uid, r.ID, string(app.GetUID()), "rollout-x-1"); err != nil {
		t.Fatalf("Remove() again while deleting = %v", err)
	}

	live.SetFinalizers(nil)
	if err = a.Writer.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	obs, err := a.Retired(ctx, r.ID, string(app.GetUID()))
	if err != nil || obs.State != StateReady || obs.Reason != ReasonRetired {
		t.Fatalf("Retired() after deletion = %+v, %v", obs, err)
	}
	if err = a.Remove(ctx, uid, r.ID, string(app.GetUID()), "rollout-x-1"); err != nil {
		t.Fatalf("Remove() of a gone object = %v", err)
	}
}

// Argo CD's settings are shared with every other Argo CD user: checking
// capabilities and writing Applications neither reads nor writes argocd-cm,
// so an administrator's own ignore rules survive.
func TestArgoCDSettingsAreLeftAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rules := map[string]string{"resource.customizations.ignoreDifferences.Secret": "jsonPointers:\n- /data\n"}
	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "argocd-cm"}, Data: rules}
	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(AppProjectGVK)
	project.SetNamespace("argocd")
	project.SetName("aicr")
	a := fakeAdapter(t, settings, project.DeepCopy())
	p, r := testPlan()
	if err := a.Capabilities(ctx); err != nil {
		t.Fatalf("Capabilities() = %v", err)
	}
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	got := &corev1.ConfigMap{}
	if err := a.Reader.Get(ctx, client.ObjectKeyFromObject(settings), got); err != nil || !maps.Equal(got.Data, rules) {
		t.Fatalf("argocd-cm after Capabilities and Ensure = %v, %v", got.Data, err)
	}
	if err := fakeAdapter(t, project.DeepCopy()).Capabilities(ctx); err != nil {
		t.Fatalf("Capabilities() without argocd-cm = %v", err)
	}
}

// A plan is rebuilt under the engine policy its Applications were written
// with, so Applications an earlier operator wrote rebuild to the plan they
// carry; Applications of two policies cannot form one plan.
func TestRebuildKeepsTheWrittenPolicy(t *testing.T) {
	t.Parallel()
	written := func(policy, id string) unstructured.Unstructured {
		p, r := testPlan()
		r.DependsOn, r.ID, r.Component = nil, id, id
		p.EnginePolicy = policy
		app, err := Application(target, p, r, values)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := normalize(app.Object)
		if err != nil {
			t.Fatal(err)
		}
		app.Object = obj.(map[string]any)
		return *app
	}
	for _, policy := range []string{"argocd.v1", PolicyVersion} {
		doc, _, err := Rebuild(target, uid, []unstructured.Unstructured{written(policy, "cert-manager")})
		if err != nil || doc.EnginePolicy != policy {
			t.Errorf("Rebuild(written under %s) = %+v, %v", policy, doc, err)
		}
	}
	mixed := []unstructured.Unstructured{written(PolicyVersion, "cert-manager"), written("argocd.v1", "nfd")}
	if _, _, err := Rebuild(target, uid, mixed); operrors.ReasonOf(err) != ReasonApplicationEdited ||
		!strings.Contains(err.Error(), "argocd.v1") {

		t.Errorf("Rebuild(mixed policies) = %v", err)
	}
}

// Rebuild names the Application that keeps a plan from being rebuilt:
// another runtime's, one without the operator's annotations, or one with an
// unreadable annotation. Suspension alone changes nothing.
func TestRebuildRejects(t *testing.T) {
	t.Parallel()
	p, r := testPlan()
	r.DependsOn = nil
	app, err := Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := normalize(app.Object)
	if err != nil {
		t.Fatal(err)
	}
	app.Object = obj.(map[string]any)
	if err = unstructured.SetNestedField(app.Object, false, "spec", "syncPolicy", "automated", "enabled"); err != nil {
		t.Fatal(err)
	}
	doc, contents, err := Rebuild(target, uid, []unstructured.Unstructured{*app})
	if err != nil || len(doc.Releases) != 1 || doc.Releases[0].ID != "cert-manager" || len(contents) != 1 || !doc.Releases[0].OwnsCRDs {
		t.Fatalf("Rebuild() = %+v, %v", doc, err)
	}
	live, _, err := fakeAdapter(t, app).RebuildLive(context.Background(), uid)
	if err != nil || live.DesiredStateDigest != doc.DesiredStateDigest {
		t.Fatalf("RebuildLive() = %+v, %v", live, err)
	}
	if _, _, err = fakeAdapter(t).RebuildLive(context.Background(), uid); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Fatalf("RebuildLive(none) = %v", err)
	}
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"another runtime": func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{v1alpha1.AnnotationRuntimeUID: "other"})
		},
		"no release ID": func(u *unstructured.Unstructured) {
			a := u.GetAnnotations()
			delete(a, v1alpha1.AnnotationReleaseID)
			u.SetAnnotations(a)
		},
		"unreadable owns-crds": func(u *unstructured.Unstructured) {
			a := u.GetAnnotations()
			a[v1alpha1.AnnotationOwnsCRDs] = "maybe"
			u.SetAnnotations(a)
		},
		"edited values": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, int64(9), "spec", "source", "helm", "valuesObject", "replicas")
			a := u.GetAnnotations()
			a[v1alpha1.AnnotationDependsOn] = "missing"
			u.SetAnnotations(a)
		},
	} {
		bad := app.DeepCopy()
		mutate(bad)
		if _, _, err = Rebuild(target, uid, []unstructured.Unstructured{*bad}); err == nil {
			t.Errorf("%s: Rebuild() accepted it", name)
		}
	}
	// A value, chart or namespace edited by hand rebuilds to another release
	// than the one written, and the object is named.
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"values": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, int64(9), "spec", "source", "helm", "valuesObject", "replicas")
		},
		"namespace": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "other", "spec", "destination", "namespace")
		},
		"chart": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, planDigest, "spec", "source", "targetRevision")
		},
	} {
		edited := app.DeepCopy()
		mutate(edited)
		_, _, err = Rebuild(target, uid, []unstructured.Unstructured{*edited})
		if operrors.ReasonOf(err) != ReasonApplicationEdited || !strings.Contains(err.Error(), "argocd/cert-manager: was edited out of band") {
			t.Errorf("%s edited: Rebuild() = %v", name, err)
		}
	}
}

// Ensure converges the spec exactly: a field another manager added, which a
// server-side apply would leave, is removed, and a foreign suspension is
// forced back.
func TestEnsureReplacesForeignFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, r := testPlan()
	app := readyApp(t, testNow)
	if err := unstructured.SetNestedField(app.Object, false, "spec", "syncPolicy", "automated", "enabled"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(app.Object, []any{map[string]any{"kind": "Secret"}}, "spec", "ignoreDifferences"); err != nil {
		t.Fatal(err)
	}
	a := fakeAdapter(t, app)
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	live, err := a.get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	desired, _ := Application(target, p, r, values)
	if !jsonEqual(live.Object["spec"], desired.Object["spec"]) {
		t.Fatalf("spec after Ensure = %v", live.Object["spec"])
	}
}

// Suspension turns automated sync off and back on for the runtime's own
// Applications only, writes nothing when already in the requested state,
// and a later Ensure reports no foreign override.
func TestSuspendTogglesAutomatedSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, r := testPlan()
	a := fakeAdapter(t)
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	foreign, err := Application(target, &plan.Document{RuntimeUID: "11111111-2222-3333-4444-555555555555", DesiredStateDigest: p.DesiredStateDigest},
		&plan.Release{ID: "other", Phase: plan.PhasePrimary, TargetNamespace: "other", Chart: r.Chart}, values)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Writer.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	enabled := func(name string) bool {
		live, getErr := a.get(ctx, name)
		if getErr != nil {
			t.Fatal(getErr)
		}
		return automatedEnabled(live)
	}

	changed, err := a.Suspend(ctx, p.RuntimeUID, true)
	if err != nil || !slices.Equal(changed, []string{"cert-manager"}) || enabled("cert-manager") || !enabled("other") {
		t.Fatalf("Suspend(true) = %v, %v; enabled cert-manager %t, other %t", changed, err, enabled("cert-manager"), enabled("other"))
	}
	if changed, err = a.Suspend(ctx, p.RuntimeUID, true); err != nil || len(changed) != 0 {
		t.Fatalf("a second Suspend(true) = %v, %v", changed, err)
	}
	if changed, err = a.Suspend(ctx, p.RuntimeUID, false); err != nil || !slices.Equal(changed, []string{"cert-manager"}) || !enabled("cert-manager") {
		t.Fatalf("Suspend(false) = %v, %v", changed, err)
	}
	ensured, err := a.Ensure(ctx, p, r, values)
	if err != nil || len(ensured.Overridden) != 0 {
		t.Fatalf("Ensure after a resume = %+v, %v", ensured, err)
	}
}

// ownedApps writes an owned Application for each release ID.
func ownedApps(t *testing.T, ctx context.Context, a *Adapter, ids ...string) {
	t.Helper()
	for _, id := range ids {
		p, r := testPlan()
		r.ID, r.Component, r.DependsOn = id, id, nil
		if _, err := a.Ensure(ctx, p, r, values); err != nil {
			t.Fatal(err)
		}
	}
}

// refusing makes the adapter's writes to one Application fail with err.
func refusing(a *Adapter, name string, err error) {
	a.Writer = interceptor.NewClient(a.Writer.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == name {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

var applications = schema.GroupResource{Group: ApplicationGVK.Group, Resource: "applications"}

// A refused write to one Application leaves the others suspended, and the
// error names the one left unchanged and is permanent, so the controller
// goes on reconciling.
func TestSuspendContinuesPastARefusedApplication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := fakeAdapter(t)
	ownedApps(t, ctx, a, "a", "b", "c")
	refusing(a, "a", apierrors.NewForbidden(applications, "a", errors.New("denied by admission policy")))

	changed, err := a.Suspend(ctx, uid, true)
	if !slices.Equal(changed, []string{"b", "c"}) || !operrors.IsPermanent(err) || !strings.Contains(err.Error(), "Applications a") {
		t.Fatalf("Suspend() = %v, %v; want b and c changed and a permanent error naming a", changed, err)
	}
	for name, want := range map[string]bool{"a": true, "b": false, "c": false} {
		if live, getErr := a.get(ctx, name); getErr != nil || automatedEnabled(live) != want {
			t.Errorf("%s: automated sync enabled = %t, want %t (%v)", name, automatedEnabled(live), want, getErr)
		}
	}
}

// Foreign names other runtimes' objects; Adopt moves exactly the recorded
// object to the new runtime, drops orphaned-from, and refuses a recreated
// object or one another runtime owns.
func TestForeignAndAdopt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const newUID = "99999999-8888-7777-6666-555555555555"
	p, r := testPlan()
	app, err := Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	app.SetUID("obj-1")
	annotations := app.GetAnnotations()
	annotations[v1alpha1.AnnotationOrphanedFrom] = uid
	app.SetAnnotations(annotations)
	a := fakeAdapter(t, app)

	foreign, err := a.Foreign(ctx, newUID)
	if err != nil || len(foreign) != 1 || !slices.Equal(foreign[uid], []string{"cert-manager"}) {
		t.Fatalf("Foreign() = %v, %v", foreign, err)
	}
	if own, _ := a.Foreign(ctx, uid); len(own) != 0 {
		t.Fatalf("Foreign(owner) = %v", own)
	}
	if err = a.Adopt(ctx, uid, newUID, "cert-manager", "obj-other"); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("Adopt(recreated) = %v", err)
	}
	if err = a.Adopt(ctx, "someone-else", newUID, "cert-manager", "obj-1"); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("Adopt(wrong owner) = %v", err)
	}
	if err = a.Adopt(ctx, uid, newUID, "cert-manager", "obj-1"); err != nil {
		t.Fatalf("Adopt() = %v", err)
	}
	live, err := a.get(ctx, "cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	got := live.GetAnnotations()
	if got[v1alpha1.AnnotationRuntimeUID] != newUID || got[v1alpha1.AnnotationOrphanedFrom] != "" || got[v1alpha1.AnnotationPlanDigest] != p.DesiredStateDigest {
		t.Fatalf("annotations after Adopt = %v", got)
	}
	if err = a.Adopt(ctx, uid, newUID, "cert-manager", "obj-1"); err != nil {
		t.Fatalf("a repeated Adopt() = %v", err)
	}
	if foreign, _ = a.Foreign(ctx, newUID); len(foreign) != 0 {
		t.Fatalf("Foreign() after Adopt = %v", foreign)
	}
}

// Orphan annotates the runtime's own Applications with orphaned-from, leaves
// their spec and other runtimes' objects alone, and is idempotent.
func TestOrphanAnnotatesOwnObjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, r := testPlan()
	a := fakeAdapter(t)
	if _, err := a.Ensure(ctx, p, r, values); err != nil {
		t.Fatal(err)
	}
	foreign, err := Application(target, &plan.Document{RuntimeUID: "11111111-2222-3333-4444-555555555555", DesiredStateDigest: p.DesiredStateDigest},
		&plan.Release{ID: "other", Phase: plan.PhasePrimary, TargetNamespace: "other", Chart: r.Chart}, values)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Writer.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	before, err := a.get(ctx, "cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		names, oErr := a.Orphan(ctx, p.RuntimeUID)
		if oErr != nil || !slices.Equal(names, []string{"cert-manager"}) {
			t.Fatalf("Orphan() = %v, %v", names, oErr)
		}
	}
	after, err := a.get(ctx, "cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.get(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	annotated := after.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] == p.RuntimeUID && jsonEqual(after.Object["spec"], before.Object["spec"])
	if !annotated || other.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] != "" {
		t.Fatalf("after Orphan: %v; other %v", after.GetAnnotations(), other.GetAnnotations())
	}
}

// The orphaned-from annotation is best effort: an Application refused it for
// good is skipped and named, and the rest are annotated. A transient
// failure still fails Orphan, so the deletion retries.
func TestOrphanSkipsARefusedApplication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name          string
		err           error
		wantAnnotated []string
		wantReason    string
		wantTransient bool
	}{
		{"admission denied", apierrors.NewForbidden(applications, "a", errors.New("denied by admission policy")),
			[]string{"b", "c"}, ReasonOrphanAnnotationRefused, false},
		{"invalid", apierrors.NewInvalid(ApplicationGVK.GroupKind(), "a", nil), []string{"b", "c"}, ReasonOrphanAnnotationRefused, false},
		{"unavailable", apierrors.NewServiceUnavailable("apiserver overloaded"), nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := fakeAdapter(t)
			ownedApps(t, ctx, a, "a", "b", "c")
			refusing(a, "a", tt.err)
			annotated, err := a.Orphan(ctx, uid)
			if !slices.Equal(annotated, tt.wantAnnotated) || operrors.ReasonOf(err) != tt.wantReason || operrors.IsTransient(err) != tt.wantTransient {
				t.Fatalf("Orphan() = %v, %v; want %v, reason %q", annotated, err, tt.wantAnnotated, tt.wantReason)
			}
			if tt.wantReason != "" && !strings.Contains(err.Error(), "Applications a refused") {
				t.Errorf("error = %v, want it to name a", err)
			}
			for _, name := range tt.wantAnnotated {
				if live, getErr := a.get(ctx, name); getErr != nil || live.GetAnnotations()[v1alpha1.AnnotationOrphanedFrom] != uid {
					t.Errorf("%s is not annotated orphaned-from: %v", name, getErr)
				}
			}
		})
	}
}

// An adoption interrupted mid-way leaves some Applications with the adopting
// runtime and the rest with the earlier one: naming both rebuilds the plan
// they share; another runtime's Application is still refused.
func TestRebuildSpansAnInterruptedAdoption(t *testing.T) {
	t.Parallel()
	const adopter = "11111111-2222-3333-4444-555555555555"
	app := func(id, owner string) unstructured.Unstructured {
		p, r := testPlan()
		r.DependsOn, r.ID, r.Component = nil, id, id
		a, err := Application(target, p, r, values)
		if err != nil {
			t.Fatal(err)
		}
		obj, err := normalize(a.Object)
		if err != nil {
			t.Fatal(err)
		}
		a.Object = obj.(map[string]any)
		annotations := a.GetAnnotations()
		annotations[v1alpha1.AnnotationRuntimeUID] = owner
		a.SetAnnotations(annotations)
		return *a
	}
	apps := []unstructured.Unstructured{app("cert-manager", adopter), app("nfd", uid)}
	doc, _, err := Rebuild(target, uid, apps, adopter)
	if err != nil || len(doc.Releases) != 2 {
		t.Fatalf("Rebuild(both runtimes) = %+v, %v", doc, err)
	}
	live, _, err := fakeAdapter(t, apps[0].DeepCopy(), apps[1].DeepCopy()).RebuildLive(context.Background(), uid, adopter)
	if err != nil || live.DesiredStateDigest != doc.DesiredStateDigest {
		t.Fatalf("RebuildLive(both runtimes) = %+v, %v", live, err)
	}
	if _, _, err = Rebuild(target, uid, apps); operrors.ReasonOf(err) != ReasonApplicationEdited {
		t.Fatalf("Rebuild(an unnamed runtime's Application) = %v", err)
	}
}
