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
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
	"aicr.run/pkg/operator/engine/argocd"
	"aicr.run/pkg/operator/plan"
)

// enqueued counts the requests each controller is handed, without
// reconciling them.
type enqueued struct {
	mu     sync.Mutex
	counts map[string]int
}

func (e *enqueued) as(controller string) func(reconcile.Reconciler) reconcile.Reconciler {
	return func(reconcile.Reconciler) reconcile.Reconciler {
		return reconcile.Func(func(_ context.Context, req reconcile.Request) (reconcile.Result, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.counts[controller+"/"+req.Name]++
			return reconcile.Result{}, nil
		})
	}
}

func (e *enqueued) count(key string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counts[key]
}

// Every watch the manager registers, with the operator's cache options,
// enqueues the runtime its object belongs to, and the predicates drop what
// neither controller reads: a Node heartbeat, and Argo CD's refresh of an
// unchanged Application.
func TestControllerWiring(t *testing.T) {
	ctx := testContext(t)
	installArgoCDCRDs(t)
	skip := true
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 k8s.Scheme(),
		Cache:                  CacheOptions(operatorNS, jobsNS, discoveryNS),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skip},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := &enqueued{counts: map[string]int{}}
	cfg := testConfig()
	cfg.SkipDiscovery = false
	if err = (&ResolverReconciler{Client: k8s, Reader: k8s, Config: cfg, wrap: seen.as("resolver")}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if err = (&RolloutReconciler{Client: k8s, Reader: k8s, Config: cfg, wrap: seen.as("rollout")}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mctx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = mgr.Start(mctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	enqueues := func(what string, act func(), keys ...string) {
		t.Helper()
		before := make([]int, len(keys))
		for i, k := range keys {
			before[i] = seen.count(k)
		}
		act()
		for i, k := range keys {
			for deadline := time.Now().Add(15 * time.Second); seen.count(k) <= before[i]; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("%s did not enqueue %s", what, k)
				}
			}
		}
	}
	quiet := func(what string, act func(), key string) {
		t.Helper()
		before := seen.count(key)
		act()
		time.Sleep(2 * time.Second)
		if got := seen.count(key); got != before {
			t.Errorf("%s enqueued %s %d times", what, key, got-before)
		}
	}
	create := func(obj client.Object) {
		t.Helper()
		if err := k8s.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = k8s.Delete(context.WithoutCancel(ctx), obj, client.PropagationPolicy(metav1.DeletePropagationBackground))
		})
	}

	enqueues("a runtime", func() { freshRuntime(t, ctx, stabilized) }, "resolver/"+v1alpha1.SingletonName, "rollout/"+v1alpha1.SingletonName)

	enqueues("a rollout", func() {
		endedTo(t, ctx, &v1alpha1.ClusterRuntime{ObjectMeta: metav1.ObjectMeta{Name: "wired-rollout", UID: "00000000-0000-0000-0000-0000000000aa"}},
			"rollout-00000000000000aa-1", "", plan.DigestOf([]byte("wired")), v1alpha1.PhaseSucceeded)
	}, "rollout/wired-rollout")

	enqueues("an AICR Job", func() {
		create(&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "wired-job", Namespace: jobsNS,
				Labels: map[string]string{v1alpha1.LabelRunID: "wired", v1alpha1.LabelRuntime: "wired-job"}},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "aicr", Image: "registry.test/aicr:v1"}},
			}}},
		})
	}, "resolver/wired-job")

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "wired-node", Labels: map[string]string{"pool": "gpu"}}}
	enqueues("a Node", func() { create(node) }, "resolver/"+v1alpha1.SingletonName)
	enqueues("a Node's labels", func() {
		node.Labels["pool"] = "system"
		if err := k8s.Update(ctx, node); err != nil {
			t.Fatal(err)
		}
	}, "resolver/"+v1alpha1.SingletonName)
	quiet("a Node heartbeat", func() {
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue,
			LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now()}}
		if err := k8s.Status().Update(ctx, node); err != nil {
			t.Fatal(err)
		}
	}, "resolver/"+v1alpha1.SingletonName)
	enqueues("a Node's removal", func() {
		if err := k8s.Delete(ctx, node); err != nil {
			t.Fatal(err)
		}
	}, "resolver/"+v1alpha1.SingletonName)

	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	app.SetName("wired-app")
	app.SetNamespace(cfg.EngineNamespace)
	app.SetLabels(map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue, v1alpha1.LabelRuntime: "wired-app"})
	_ = unstructured.SetNestedField(app.Object, map[string]any{"destination": map[string]any{"server": "https://kubernetes.default.svc"},
		"project": "default", "source": map[string]any{"repoURL": "oci://r/c", "targetRevision": "1"}}, "spec")
	enqueues("an operator Application", func() { create(app) }, "rollout/wired-app")
	update := func(path []string, value string) {
		t.Helper()
		_ = unstructured.SetNestedField(app.Object, value, path...)
		if err := k8s.Update(ctx, app); err != nil {
			t.Fatal(err)
		}
	}
	enqueues("an Application's sync status", func() { update([]string{"status", "sync", "status"}, "Synced") }, "rollout/wired-app")
	quiet("a refresh of an unchanged Application", func() {
		update([]string{"status", "reconciledAt"}, time.Now().UTC().Format(time.RFC3339))
	}, "rollout/wired-app")
	enqueues("an Application's spec", func() { update([]string{"spec", "source", "targetRevision"}, "2") }, "rollout/wired-app")
}
