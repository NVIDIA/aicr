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
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const (
	uid         = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
	chartDigest = "sha256:1c34b24e568ce41effdcd964e3613b278f2107444f9b89dd577dda10a07163a2"
	planDigest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var (
	testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	target  = Target{Namespace: "argocd", Project: "aicr", RuntimeName: "cluster"}
	values  = []byte(`{"crds":{"enabled":true},"ratio":0.5,"replicas":2}`)
)

func testPlan() (*plan.Document, *plan.Release) {
	p := &plan.Document{RuntimeUID: uid, DesiredStateDigest: planDigest, Engine: plan.EngineArgoCD, EnginePolicy: PolicyVersion}
	r := plan.Release{
		ID: "cert-manager", Component: "cert-manager", Phase: plan.PhasePrimary, TargetNamespace: "cert-manager",
		DependsOn: []string{"a", "b"}, OwnsCRDs: true,
		Chart:  plan.Chart{Source: plan.ChartMirrored, Repository: "oci://registry.local:5000/charts/cert-manager", Digest: chartDigest},
		Values: plan.ValuesContent("cert-manager", values).Ref(),
	}
	p.Releases = []plan.Release{r}
	return p, &p.Releases[0]
}

func TestApplicationCarriesPlanGraphAndPolicy(t *testing.T) {
	t.Parallel()
	p, r := testPlan()
	app, err := Application(target, p, r, values)
	if err != nil {
		t.Fatalf("Application() = %v", err)
	}
	a := app.GetAnnotations()
	if a[v1alpha1.AnnotationPlanDigest] != planDigest || a[v1alpha1.AnnotationRuntimeUID] != uid || a[v1alpha1.AnnotationDependsOn] != "a,b" ||
		a[v1alpha1.AnnotationOwnsCRDs] != "true" || a[v1alpha1.AnnotationPhase] != "Primary" || a[v1alpha1.AnnotationReleaseID] != "cert-manager" {

		t.Errorf("annotations = %v", a)
	}
	if len(app.GetFinalizers()) != 0 {
		t.Error("an Application must not carry the resources finalizer")
	}
	rev, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
	path, _, _ := unstructured.NestedString(app.Object, "spec", "source", "path")
	opts, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	if rev != chartDigest || path != "." || !slices.Equal(opts, []string{"ServerSideApply=true", "RespectIgnoreDifferences=true"}) {
		t.Errorf("source revision=%s path=%s syncOptions=%v", rev, path, opts)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(app.Object, "spec", "syncPolicy", "syncOptions"); !found {
		t.Error("no syncOptions")
	}
	if _, err = Application(target, p, r, []byte(`[1]`)); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("Application(list values) = %v", err)
	}
}

// The spec fingerprint follows what an Application deploys (source, Helm
// values and destination) and ignores the sync policy a suspension toggles
// and the status Argo CD writes.
func TestSpecDigest(t *testing.T) {
	t.Parallel()
	p, r := testPlan()
	app, err := Application(target, p, r, values)
	if err != nil {
		t.Fatalf("Application() = %v", err)
	}
	base := SpecDigest(app)
	if base == "" {
		t.Fatal("an Application has no fingerprint")
	}
	tests := []struct {
		name    string
		edit    func(*unstructured.Unstructured)
		changed bool
	}{
		{"automated sync off", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, false, "spec", "syncPolicy", "automated", "enabled")
		}, false},
		{"status", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "Synced", "status", "sync", "status")
		}, false},
		{"values", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, int64(3), "spec", "source", "helm", "valuesObject", "replicas")
		}, true},
		{"chart revision", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "sha256:"+strings.Repeat("0", 64), "spec", "source", "targetRevision")
		}, true},
		{"destination", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "default", "spec", "destination", "namespace")
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			edited := app.DeepCopy()
			tt.edit(edited)
			if got := SpecDigest(edited) != base; got != tt.changed {
				t.Errorf("fingerprint changed = %v, want %v", got, tt.changed)
			}
		})
	}
	if got := SpecDigest(&unstructured.Unstructured{Object: map[string]any{}}); got != "" {
		t.Errorf("SpecDigest(no spec) = %q", got)
	}
}

// Each Application carries the comparison exceptions AICR's releases need,
// and Argo CD respects them when it syncs: no argocd-cm key is shared with
// other Argo CD users. A plan written under the first policy renders
// as it was written, without them.
func TestApplicationIgnoresControllerFilledFields(t *testing.T) {
	t.Parallel()
	p, r := testPlan()
	app, err := Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	ignores, _, _ := unstructured.NestedSlice(app.Object, "spec", "ignoreDifferences")
	want := map[string]string{
		"Secret/kubeflow-trainer-webhook-cert": "/data", "Secret/jobset-webhook-server-cert": "/data",
		"storage.k8s.io/CSIDriver/": "/spec/preventPodSchedulingIfMissing",
	}
	got := map[string]string{}
	for _, i := range ignores {
		m := i.(map[string]any)
		key := m["kind"].(string) + "/"
		if g, ok := m["group"].(string); ok {
			key = g + "/" + key
		}
		if n, ok := m["name"].(string); ok {
			key += n
		}
		pointers := m["jsonPointers"].([]any)
		got[key] = pointers[0].(string)
	}
	if !maps.Equal(got, want) {
		t.Errorf("ignoreDifferences = %v, want %v", got, want)
	}

	p.EnginePolicy = "argocd.v1"
	app, err = Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	opts, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	if _, found, _ := unstructured.NestedFieldNoCopy(app.Object, "spec", "ignoreDifferences"); found || !slices.Equal(opts, []string{"ServerSideApply=true"}) {
		t.Errorf("a plan of policy argocd.v1 renders ignores %t, syncOptions %v", found, opts)
	}
}

// readyApp is an Application Argo CD reports as current at the chart digest.
func readyApp(t *testing.T, at time.Time) *unstructured.Unstructured {
	t.Helper()
	return readyAppWith(t, at, values)
}

// readyAppWith is readyApp for the given release values.
func readyAppWith(t *testing.T, at time.Time, vals []byte) *unstructured.Unstructured {
	t.Helper()
	p, r := testPlan()
	app, err := Application(target, p, r, vals)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip through JSON as the API server would return it.
	obj, err := normalize(app.Object)
	if err != nil {
		t.Fatal(err)
	}
	app.Object = obj.(map[string]any)
	src, _, _ := unstructured.NestedMap(app.Object, "spec", "source")
	dst, _, _ := unstructured.NestedMap(app.Object, "spec", "destination")
	stamp := at.UTC().Format(time.RFC3339)
	app.Object["status"] = map[string]any{
		"sync": map[string]any{
			"status": "Synced", "revision": chartDigest,
			"comparedTo": map[string]any{"source": src, "destination": dst},
		},
		"health":       map[string]any{"status": "Healthy"},
		"reconciledAt": stamp,
		"operationState": map[string]any{
			"phase": "Succeeded", "startedAt": stamp,
			"syncResult": map[string]any{"revision": chartDigest},
		},
	}
	return app
}

// Argo CD records the source it compared without null map entries, while a
// Helm valuesObject keeps the null that deletes a chart default: the
// comparison ignores them, or such a release is never Ready.
func TestObserveIgnoresNullsArgoCDDrops(t *testing.T) {
	t.Parallel()
	step := time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)
	vals := []byte(`{"affinity":{"nodeAffinity":null},"replicas":2}`)
	p, r := testPlan()
	desired, err := Application(target, p, r, vals)
	if err != nil {
		t.Fatal(err)
	}
	app := readyAppWith(t, step.Add(time.Minute), vals)
	if err := unstructured.SetNestedField(app.Object, map[string]any{}, "status", "sync", "comparedTo", "source", "helm", "valuesObject", "affinity"); err != nil {
		t.Fatal(err)
	}
	if got := observe(app, desired, r, step, false); got.State != StateReady {
		t.Fatalf("observe() = %+v, want Ready", got)
	}
	if err := unstructured.SetNestedField(app.Object, int64(3), "status", "sync", "comparedTo", "source", "helm", "valuesObject", "replicas"); err != nil {
		t.Fatal(err)
	}
	if got := observe(app, desired, r, step, false); got.Reason != "ComparisonStale" {
		t.Fatalf("observe() with a real difference = %+v, want ComparisonStale", got)
	}
}

func TestObserveCurrentRevisionReadiness(t *testing.T) {
	t.Parallel()
	step := time.Date(2026, 10, 14, 12, 0, 0, 700_000_000, time.UTC)
	after := step.Add(5 * time.Second)
	before := step.Add(-time.Hour)
	set := func(path ...string) func(*unstructured.Unstructured, any) {
		return func(u *unstructured.Unstructured, v any) {
			if err := unstructured.SetNestedField(u.Object, v, path...); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name   string
		at     time.Time
		verify bool
		mutate func(*unstructured.Unstructured)
		want   State
		reason string
	}{
		{name: "current", at: after, want: StateReady},
		{name: "same second as the step", at: step.Truncate(time.Second), want: StateReady},
		{name: "older plan digest", at: after, mutate: func(u *unstructured.Unstructured) {
			u.SetAnnotations(map[string]string{v1alpha1.AnnotationPlanDigest: "sha256:old", v1alpha1.AnnotationRuntimeUID: uid})
		}, want: StateProgressing, reason: "SpecNotWritten"},
		{name: "old values in spec", at: after, mutate: func(u *unstructured.Unstructured) {
			set("spec", "source", "helm", "valuesObject", "replicas")(u, int64(3))
		}, want: StateProgressing, reason: ReasonSpecDrifted},
		{name: "automated sync disabled by hand", at: after, mutate: func(u *unstructured.Unstructured) {
			set("spec", "syncPolicy", "automated", "enabled")(u, false)
		}, want: StateProgressing, reason: ReasonSpecDrifted},
		{name: "ownership label removed", at: after, mutate: func(u *unstructured.Unstructured) {
			u.SetLabels(map[string]string{v1alpha1.LabelRuntime: "cluster"})
		}, want: StateProgressing, reason: ReasonSpecDrifted},
		{name: "compared against old values", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "sync", "comparedTo", "source", "helm", "valuesObject", "replicas")(u, int64(1))
		}, want: StateProgressing, reason: "ComparisonStale"},
		{name: "compared against old destination", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "sync", "comparedTo", "destination", "namespace")(u, "other")
		}, want: StateProgressing, reason: "ComparisonStale"},
		{name: "synced to an older revision", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "sync", "revision")(u, "sha256:old")
		}, want: StateProgressing, reason: "NotSynced"},
		{name: "out of sync", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "sync", "status")(u, "OutOfSync")
		}, want: StateProgressing, reason: "NotSynced"},
		{name: "degraded", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "health", "status")(u, "Degraded")
		}, want: StateProgressing, reason: "HealthDegraded"},
		{name: "comparison error", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "conditions")(u, []any{map[string]any{"type": "ComparisonError", "message": "x"}})
		}, want: StateProgressing, reason: "ComparisonError"},
		{name: "sync running", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Running")
		}, want: StateProgressing, reason: "SyncRunning"},
		{name: "sync failed for this step", at: after, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
		}, want: StateFailed, reason: "SyncFailed"},
		{name: "evidence older than the step", at: before, want: StateProgressing, reason: "ReconcileStale"},
		{name: "old operation but fresh reconcile", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "reconciledAt")(u, after.Format(time.RFC3339))
		}, want: StateReady},
		{name: "old failed operation, fresh reconcile", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
			set("status", "reconciledAt")(u, after.Format(time.RFC3339))
		}, want: StateReady},
		{name: "failed before the step, still out of sync", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
			set("status", "sync", "status")(u, "OutOfSync")
		}, want: StateProgressing, reason: ReasonPriorSyncFailed},
		{name: "failed before the step, sync error kept", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Error")
			set("status", "conditions")(u, []any{map[string]any{"type": "SyncError", "message": "x"}})
		}, want: StateProgressing, reason: ReasonPriorSyncFailed},
		{name: "failed before the step, revision only in the operation", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
			set("status", "operationState", "syncResult", "revision")(u, "")
			set("status", "operationState", "operation", "sync", "revision")(u, chartDigest)
			set("status", "sync", "status")(u, "OutOfSync")
		}, want: StateProgressing, reason: ReasonPriorSyncFailed},
		{name: "failed before the step, retry pending", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
			set("status", "sync", "status")(u, "OutOfSync")
			set("operation", "sync", "revision")(u, chartDigest)
		}, want: StateProgressing, reason: "NotSynced"},
		{name: "older revision failed before the step", at: before, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
			set("status", "operationState", "syncResult", "revision")(u, "sha256:old")
			set("status", "sync", "status")(u, "OutOfSync")
		}, want: StateProgressing, reason: "NotSynced"},
		{name: "verify: failed before the step", at: before, verify: true, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
			set("status", "sync", "status")(u, "OutOfSync")
		}, want: StateProgressing, reason: ReasonSyncNotStarted},
		{name: "no status", at: after, mutate: func(u *unstructured.Unstructured) { delete(u.Object, "status") },
			want: StateProgressing, reason: "NotSynced"},
		{name: "verify: sync since the step", at: after, verify: true, want: StateReady},
		{name: "verify: fresh reconcile without a sync", at: before, verify: true, mutate: func(u *unstructured.Unstructured) {
			set("status", "reconciledAt")(u, after.Format(time.RFC3339))
		}, want: StateProgressing, reason: ReasonSyncNotStarted},
		{name: "verify: sync requested, not started", at: before, verify: true, mutate: func(u *unstructured.Unstructured) {
			set("operation", "sync", "revision")(u, chartDigest)
		}, want: StateProgressing, reason: "SyncPending"},
		{name: "verify: gate failed", at: after, verify: true, mutate: func(u *unstructured.Unstructured) {
			set("status", "operationState", "phase")(u, "Failed")
		}, want: StateFailed, reason: "SyncFailed"},
	}
	p, r := testPlan()
	desired, err := Application(target, p, r, values)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		app := readyApp(t, tt.at)
		if tt.mutate != nil {
			tt.mutate(app)
		}
		got := observe(app, desired, r, step, tt.verify)
		if got.State != tt.want || (tt.reason != "" && got.Reason != tt.reason) {
			t.Errorf("%s: observe() = %+v, want %s %s", tt.name, got, tt.want, tt.reason)
		}
	}
}

func fakeAdapter(t *testing.T, objs ...client.Object) *Adapter {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(ApplicationGVK, meta.RESTScopeNamespace)
	mapper.Add(AppProjectGVK, meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(objs...).Build()
	return &Adapter{Writer: c, Reader: c, Mapper: mapper, Target: target}
}

func TestEnsureWritesOwnedAndRefusesForeign(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, r := testPlan()
	a := fakeAdapter(t)
	ensured, err := a.Ensure(ctx, p, r, values)
	if err != nil || ensured.Application.GetName() != "cert-manager" || len(ensured.Overridden) != 0 {
		t.Fatalf("Ensure() = %+v, %v", ensured, err)
	}
	obs, err := a.Observe(ctx, p, r, values, time.Now(), false)
	if err != nil || obs.State != StateProgressing {
		t.Fatalf("Observe() after Ensure = %+v, %v; want Progressing until Argo CD reports", obs, err)
	}
	if live, _ := a.get(ctx, r.ID); live.GetAnnotations()[RefreshAnnotation] != "" {
		t.Error("creating the Application requested a refresh")
	}
	if _, err = a.Ensure(ctx, p, r, values); err != nil {
		t.Fatalf("second Ensure() = %v", err)
	}
	// The same spec starts no sync: Ensure asks Argo CD to refresh, outside
	// the desired state, so drift detection never sees the annotation.
	live, err := a.get(ctx, r.ID)
	if err != nil || live.GetAnnotations()[RefreshAnnotation] != "normal" {
		t.Fatalf("second Ensure() did not request a refresh: %v", err)
	}
	desired, _ := Application(target, p, r, values)
	if drifted(live, desired) {
		t.Error("the refresh request reads as drift")
	}

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(ApplicationGVK)
	foreign.SetNamespace("argocd")
	foreign.SetName("cert-manager")
	foreign.SetAnnotations(map[string]string{v1alpha1.AnnotationRuntimeUID: "someone-else"})
	b := fakeAdapter(t, foreign)
	_, err = b.Ensure(ctx, p, r, values)
	if operrors.CodeOf(err) != operrors.ErrCodeConflict || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("Ensure(foreign) = %v, want CONFLICT", err)
	}
	if err = b.Verify(ctx, p, r, values); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("Verify(foreign) = %v, want CONFLICT", err)
	}
	if _, err = b.Observe(ctx, p, r, values, time.Now(), false); operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("Observe(foreign) = %v, want CONFLICT", err)
	}
	inv, err := a.Inventory(ctx, uid)
	if err != nil || len(inv) != 1 || inv[0].ReleaseID != "cert-manager" || inv[0].PlanDigest != planDigest {
		t.Fatalf("Inventory() = %+v, %v", inv, err)
	}
	if inv, _ = b.Inventory(ctx, uid); len(inv) != 0 {
		t.Fatalf("Inventory() listed a foreign Application: %+v", inv)
	}
	missing, err := fakeAdapter(t).Observe(ctx, p, r, values, time.Now(), false)
	if err != nil || missing.State != StateMissing {
		t.Fatalf("Observe(missing) = %+v, %v", missing, err)
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	project := &unstructured.Unstructured{}
	project.SetGroupVersionKind(AppProjectGVK)
	project.SetNamespace("argocd")
	project.SetName("aicr")
	if err := fakeAdapter(t, project).Capabilities(ctx); err != nil {
		t.Fatalf("Capabilities() = %v", err)
	}
	if err := fakeAdapter(t).Capabilities(ctx); operrors.CodeOf(err) != operrors.ErrCodeNotFound && operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Capabilities(no project) = %v", err)
	}
	empty := &Adapter{Mapper: meta.NewDefaultRESTMapper(nil), Target: target}
	if err := empty.Capabilities(ctx); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
		t.Fatalf("Capabilities(no CRD) = %v", err)
	}
}
