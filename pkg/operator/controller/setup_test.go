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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
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
// delete, does.
func TestRelevantApplication(t *testing.T) {
	t.Parallel()
	app := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "gpu-operator", "resourceVersion": "1",
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
			set([]any{map[string]any{"manager": "argocd-server"}}, "metadata", "managedFields")(o)
		}), false},
		{"a sync status", changed(set("OutOfSync", "status", "sync", "status")), true},
		{"a health status", changed(set("Degraded", "status", "health", "status")), true},
		{"a spec change", changed(set("default", "spec", "project")), true},
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
