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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/discovery"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// toRuntime maps an operator-labeled object to its runtime's request.
func toRuntime(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[v1alpha1.LabelRuntime]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: name}}}
}

// toSingleton maps any object to the singleton runtime.
func toSingleton(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: v1alpha1.SingletonName}}}
}

// relevantNode passes Node events that change what discovery reads: a node
// added or removed, or a change of labels, taints, provider ID, node info or
// readiness. Heartbeats do not reconcile anything.
var relevantNode = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		old, okOld := e.ObjectOld.(*corev1.Node)
		updated, okNew := e.ObjectNew.(*corev1.Node)
		return !okOld || !okNew || discovery.Relevant(old, updated)
	},
}

// SetupWithManager registers the resolver: the runtime, its AICR Jobs and,
// with discovery, the Nodes that make up the placement groups.
func (r *ResolverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		Named("resolver").
		For(&v1alpha1.ClusterRuntime{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(toRuntime))
	if !r.Config.SkipDiscovery {
		b = b.Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(toSingleton), builder.WithPredicates(relevantNode))
	}
	err := b.Complete(r)
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "register the resolver", err)
	}
	return nil
}

// SetupWithManager registers the rollout controller: the runtime and its
// rollouts, and the engine's objects once their kind is served:
// a missing engine CRD never stops the manager, and the controller re-observes
// on a resync until the watch exists.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		Named("rollout").
		For(&v1alpha1.ClusterRuntime{}).
		Watches(&v1alpha1.RuntimeRollout{}, handler.EnqueueRequestsFromMapFunc(toRuntime)).
		Build(r)
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "register the rollout controller", err)
	}
	gvk := argocd.ApplicationGVK
	// The engine's objects get their own cache, built once their kind is
	// served: a cache over a kind the API server lacks fails when it is
	// built, which in the manager's cache would stop the manager.
	var engineCache cache.Cache
	w := &engineWatch{
		interval: defaults.EngineWatchPoll,
		served: func() bool {
			_, mapErr := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
			return mapErr == nil
		},
		register: func() error {
			if engineCache == nil {
				built, cacheErr := cache.New(mgr.GetConfig(), engineCacheOptions(mgr, r.Config.EngineNamespace))
				if cacheErr != nil {
					return cacheErr
				}
				if cacheErr = mgr.Add(built); cacheErr != nil {
					return cacheErr
				}
				engineCache = built
			}
			app := &unstructured.Unstructured{}
			app.SetGroupVersionKind(gvk)
			return c.Watch(source.Kind(engineCache, client.Object(app), handler.EnqueueRequestsFromMapFunc(toRuntime)))
		},
	}
	if err = mgr.Add(w); err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "register the engine watch", err)
	}
	return nil
}

// engineWatch registers the watch on engine objects once their kind is
// served, polling until then.
type engineWatch struct {
	interval time.Duration
	served   func() bool
	register func() error
}

// Start polls until the engine kind is served and the watch is registered,
// or the manager stops.
func (w *engineWatch) Start(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if w.served() {
			err := w.register()
			if err == nil {
				log.FromContext(ctx).Info("engine watches registered")
				return nil
			}
			log.FromContext(ctx).Info("engine watch registration will retry", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// CacheOptions scopes the manager's caches to what the operator's RBAC lets
// it read: its ConfigMaps in the operator namespace, its AICR Jobs
// in the Job namespaces, and Nodes stripped by the discovery transform. The
// engine's objects have their own cache (engineCacheOptions). Everything else
// is read uncached.
func CacheOptions(operatorNamespace string, jobNamespaces ...string) cache.Options {
	runs, _ := labels.Parse(v1alpha1.LabelRunID)
	jobs := map[string]cache.Config{}
	for _, ns := range jobNamespaces {
		jobs[ns] = cache.Config{LabelSelector: runs}
	}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{operatorNamespace: {LabelSelector: managedSelector()}}},
		&batchv1.Job{}:      {Namespaces: jobs},
		// Nodes are cluster-scoped; the cache keeps only what discovery reads.
		&corev1.Node{}: {Transform: discovery.TransformNode},
	}}
}

// engineCacheOptions scopes the engine objects' cache to those the operator
// labeled in the engine namespace.
func engineCacheOptions(mgr ctrl.Manager, engineNamespace string) cache.Options {
	return cache.Options{
		HTTPClient: mgr.GetHTTPClient(), Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(),
		DefaultNamespaces: map[string]cache.Config{engineNamespace: {LabelSelector: managedSelector()}},
	}
}

// managedSelector selects the objects the operator labeled.
func managedSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue})
}
