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

package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"aicr.run/pkg/operator/defaults"
	"aicr.run/pkg/operator/engine/argocd"
)

// Each controller runs one worker; only the rollout controller bounds a
// reconcile, because the resolver's chart mirroring does not resume.
func TestControllerOptions(t *testing.T) {
	t.Parallel()
	resolver, rollout := resolverControllerOptions(), rolloutControllerOptions()
	if resolver.MaxConcurrentReconciles != 1 || resolver.ReconciliationTimeout != 0 {
		t.Errorf("resolver options = %+v, want one worker and no reconcile timeout", resolver)
	}
	if rollout.MaxConcurrentReconciles != 1 || rollout.ReconciliationTimeout != defaults.RolloutReconcileTimeout {
		t.Errorf("rollout options = %+v, want one worker and a %s reconcile timeout", rollout, defaults.RolloutReconcileTimeout)
	}
}

// An Application update that only refreshes what Argo CD rewrites on every
// reconcile wakes no controller; any other change, and every create and
// delete, does. The Application CRD has no status subresource, so a refresh
// also moves metadata.generation.
func TestRelevantApplication(t *testing.T) {
	t.Parallel()
	app := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "gpu-operator", "resourceVersion": "1", "generation": int64(1),
			"managedFields": []any{map[string]any{"manager": "argocd-application-controller"}}},
		"spec": map[string]any{"project": "aicr"},
		"status": map[string]any{"reconciledAt": "2026-10-29T10:00:00Z", "observedAt": "2026-10-29T10:00:00Z",
			"sync": map[string]any{"status": "Synced"}},
	}}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	changed := func(mutate func(o map[string]any)) *unstructured.Unstructured {
		c := app.DeepCopy()
		mutate(c.Object)
		return c
	}
	set := func(value any, path ...string) func(o map[string]any) {
		return func(o map[string]any) { _ = unstructured.SetNestedField(o, value, path...) }
	}
	cases := []struct {
		name    string
		updated *unstructured.Unstructured
		want    bool
	}{
		{"a resync", app.DeepCopy(), false},
		{"a refresh", changed(func(o map[string]any) {
			set("2026-10-29T10:03:00Z", "status", "reconciledAt")(o)
			set("2026-10-29T10:03:00Z", "status", "observedAt")(o)
			set("2", "metadata", "resourceVersion")(o)
			set(int64(2), "metadata", "generation")(o)
			set([]any{map[string]any{"manager": "argocd-server"}}, "metadata", "managedFields")(o)
		}), false},
		{"a sync status", changed(set("OutOfSync", "status", "sync", "status")), true},
		{"a health status", changed(set("Degraded", "status", "health", "status")), true},
		{"a spec change", changed(func(o map[string]any) {
			set("default", "spec", "project")(o)
			set(int64(2), "metadata", "generation")(o)
		}), true},
		{"an annotation", changed(set(map[string]any{"a": "b"}, "metadata", "annotations")), true},
	}
	for _, tc := range cases {
		if got := relevantApplication.Update(event.UpdateEvent{ObjectOld: app, ObjectNew: tc.updated}); got != tc.want {
			t.Errorf("%s: relevant = %t, want %t", tc.name, got, tc.want)
		}
	}
	if !relevantApplication.Create(event.CreateEvent{Object: app}) || !relevantApplication.Delete(event.DeleteEvent{Object: app}) {
		t.Error("an Application create or delete is dropped")
	}
	if app.GetResourceVersion() != "1" {
		t.Error("the predicate changed the cached Application")
	}
	typed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	if !relevantApplication.Update(event.UpdateEvent{ObjectOld: typed, ObjectNew: typed}) {
		t.Error("an update of an object that is not unstructured is dropped")
	}
}

// Argo CD's write of status.reconciledAt, served by a real API server with
// the fixture's Argo CD CRDs, wakes no controller; a sync status change still
// does. Like the pinned Argo CD chart's, the fixture's Application CRD has no
// status subresource, so the API server moves metadata.generation on that
// write.
func TestRelevantApplicationDropsAServedRefresh(t *testing.T) {
	ctx := testContext(t)
	installArgoCDCRDs(t)
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	app.SetName("refreshed")
	app.SetNamespace("argocd")
	_ = unstructured.SetNestedField(app.Object, map[string]any{"destination": map[string]any{"server": "https://kubernetes.default.svc"},
		"project": "default", "source": map[string]any{"repoURL": "oci://r/c", "targetRevision": "1"}}, "spec")
	_ = unstructured.SetNestedField(app.Object, map[string]any{"reconciledAt": "2026-11-22T10:00:00Z",
		"sync": map[string]any{"status": "Synced"}}, "status")
	if err := k8s.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	read := func() *unstructured.Unstructured {
		t.Helper()
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(argocd.ApplicationGVK)
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	write := func(from *unstructured.Unstructured, value string, path ...string) *unstructured.Unstructured {
		t.Helper()
		next := from.DeepCopy()
		_ = unstructured.SetNestedField(next.Object, value, path...)
		if err := k8s.Update(ctx, next); err != nil {
			t.Fatal(err)
		}
		return read()
	}

	old := read()
	refreshed := write(old, "2026-11-22T10:03:00Z", "status", "reconciledAt")
	if refreshed.GetGeneration() == old.GetGeneration() {
		t.Fatalf("the refresh kept generation %d, so the fixture no longer reproduces a served refresh", old.GetGeneration())
	}
	if relevantApplication.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: refreshed}) {
		t.Errorf("a refresh is relevant (generation %d to %d)", old.GetGeneration(), refreshed.GetGeneration())
	}
	outOfSync := write(refreshed, "OutOfSync", "status", "sync", "status")
	if !relevantApplication.Update(event.UpdateEvent{ObjectOld: refreshed, ObjectNew: outOfSync}) {
		t.Error("a sync status change is dropped")
	}
}

// installArgoCDCRDs serves the Argo CD CRDs for one test and removes them
// after it, waiting for the deletion, which completes asynchronously and
// which other tests need.
func installArgoCDCRDs(t *testing.T) {
	t.Helper()
	crds, err := envtest.InstallCRDs(restConfig, envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("..", "..", "..", "tests", "operator", "chart", "testdata", "argocd-crds.yaml")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envtest.UninstallCRDs(restConfig, envtest.CRDInstallOptions{CRDs: crds})
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
			if getErr := k8s.Get(context.Background(), client.ObjectKey{Name: "applications.argoproj.io"}, crd); apierrors.IsNotFound(getErr) {
				return
			}
		}
		t.Error("the Argo CD CRDs were not removed")
	})
}
