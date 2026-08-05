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

package chainsaw

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// clusterFetcher implements ResourceFetcher using a dynamic Kubernetes client.
type clusterFetcher struct {
	client dynamic.Interface
	mapper meta.RESTMapper

	// mu guards lastReset. One fetcher is shared by up to
	// defaults.ChainsawMaxParallel component assertions.
	mu        sync.Mutex
	lastReset time.Time

	// now is swappable in tests; nil means time.Now.
	now func() time.Time
}

// NewClusterFetcher creates a ResourceFetcher that queries a live Kubernetes cluster.
func NewClusterFetcher(client dynamic.Interface, mapper meta.RESTMapper) ResourceFetcher {
	return &clusterFetcher{client: client, mapper: mapper}
}

// NewClusterFetcherForConfig builds a ResourceFetcher from a client
// configuration, constructing both the dynamic client it reads through and the
// discovery-backed RESTMapper it resolves scope with.
//
// Callers that already hold a dynamic client (the deployment validator injects
// one in tests) should use NewRESTMapperForConfig + NewClusterFetcher instead,
// so the mapper wiring is still shared.
func NewClusterFetcherForConfig(restConfig *rest.Config) (ResourceFetcher, error) {
	mapper, err := NewRESTMapperForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	dynClient, err := NewDynamicClientForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return NewClusterFetcher(dynClient, mapper), nil
}

// NewDynamicClientForConfig builds the dynamic client the fetcher reads
// through, carrying the same request bound NewRESTMapperForConfig applies.
// Exported for callers that construct the two halves separately (the
// deployment validator keeps its own client so ctx.DynamicClient stays an
// injection seam) — going through it keeps both halves bounded alike.
func NewDynamicClientForConfig(restConfig *rest.Config) (dynamic.Interface, error) {
	if restConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	dynClient, err := dynamic.NewForConfig(boundedConfig(restConfig))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}
	return dynClient, nil
}

// NewRESTMapperForConfig builds the discovery-backed RESTMapper the fetcher
// uses to resolve a GroupVersionKind to a resource and its scope. Discovery is
// deferred: no API call happens until the first mapping lookup.
func NewRESTMapperForConfig(restConfig *rest.Config) (meta.RESTMapper, error) {
	if restConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	discoveryClient, err := kubernetes.NewForConfig(boundedConfig(restConfig))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create discovery client", err)
	}

	return restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(discoveryClient.Discovery()),
	), nil
}

// boundedConfig returns a copy of restConfig with an explicit request timeout
// when the caller left one unset. The RESTMapper reaches the apiserver through
// the context-free DiscoveryInterface, so nothing else bounds those calls —
// client-go's own 32s discovery default is the only backstop, and it does not
// apply to the dynamic client at all. Copying keeps the caller's config
// untouched.
func boundedConfig(restConfig *rest.Config) *rest.Config {
	cfg := rest.CopyConfig(restConfig)
	if cfg.Timeout == 0 {
		cfg.Timeout = defaults.K8sClientRequestTimeout
	}
	return cfg
}

// resettableMapper is the subset of restmapper.DeferredDiscoveryRESTMapper the
// fetcher needs to invalidate a stale discovery cache.
type resettableMapper interface {
	Reset()
}

// resolveMapping resolves a GroupVersionKind to a REST mapping, distinguishing
// the two very different reasons the lookup can fail.
//
// A genuine no-match (the kind is not served by this cluster) is
// ErrCodeNotFound: for a negative `error:` assertion that is the happy path,
// because a kind the apiserver does not serve cannot hold a forbidden shape.
// Anything else — a discovery request that timed out, was forbidden, or hit a
// 5xx — is ErrCodeUnavailable. Collapsing those into NotFound, as this did
// before, let a discovery outage silently satisfy every negative assertion.
//
// A no-match is also retried once against fresh discovery: the mapper caches
// aggressively and the gate is a long-lived poller, so a CRD installed by the
// component being gated (the common case for an operator's own CRs) would
// otherwise read as "no such kind" until the process restarts.
func (f *clusterFetcher) resolveMapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mapping, err := f.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return mapping, nil
	}

	if !isGenuineNoMatch(err) {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to resolve REST mapping for %s", gvk), err)
	}

	// Retry after the refresh — and also when the cooldown denied this
	// caller a refresh, because a concurrent assertion may have just done
	// one. Skipping the retry there would let the race loser conclude
	// "absent" from a cache that has already been invalidated.
	refreshed := f.refreshDiscovery()
	mapping, err = f.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return mapping, nil
	}
	if !isGenuineNoMatch(err) {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to resolve REST mapping for %s after discovery retry (refreshed=%t)",
				gvk, refreshed), err)
	}

	return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("no REST mapping for %s", gvk), err)
}

// isGenuineNoMatch reports whether err means "this cluster does not serve that
// kind" as opposed to "discovery could not tell us".
//
// The distinction is load-bearing because a no-match resolves to
// ErrCodeNotFound, which is the immediate-pass path for a negative `error:`
// assertion. client-go reports a partial discovery failure (an aggregated
// APIService that is down, or one the ServiceAccount cannot reach) as a
// NoKindMatchError wrapped in ErrGroupDiscoveryFailed, so keying on
// meta.IsNoMatchError alone would let an unreachable API group silently
// satisfy every negative assertion.
func isGenuineNoMatch(err error) bool {
	if !meta.IsNoMatchError(err) {
		return false
	}
	var groupErr *discovery.ErrGroupDiscoveryFailed
	return !stderrors.As(err, &groupErr) && !discovery.IsGroupDiscoveryFailedError(err)
}

// refreshDiscovery invalidates the mapper's cached discovery data, reporting
// whether it did. Rate-limited to once per defaults.DiscoveryRefreshCooldown:
// an assertion for a kind that genuinely does not exist retries every
// AssertRetryInterval (5s) for the whole assert window, and refreshing on each
// of those would turn one missing CRD into a discovery storm against the
// apiserver.
func (f *clusterFetcher) refreshDiscovery() bool {
	resettable, ok := f.mapper.(resettableMapper)
	if !ok {
		return false
	}

	now := time.Now
	if f.now != nil {
		now = f.now
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if t := now(); f.lastReset.IsZero() || t.Sub(f.lastReset) >= defaults.DiscoveryRefreshCooldown {
		f.lastReset = t
		resettable.Reset()
		return true
	}
	return false
}

func (f *clusterFetcher) Fetch(ctx context.Context, apiVersion, kind, namespace, name string) (map[string]interface{}, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q", apiVersion), err)
	}

	gvk := gv.WithKind(kind)
	mapping, err := f.resolveMapping(gvk)
	if err != nil {
		return nil, err
	}

	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = f.client.Resource(mapping.Resource).Namespace(namespace)
	} else {
		resource = f.client.Resource(mapping.Resource)
	}

	obj, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Preserve the distinction between a true 404 and any other
		// API failure. Negative health checks (chainsaw `error:`
		// blocks) treat NotFound as the happy path and must fail
		// closed on transient errors (timeouts, 5xx, forbidden) —
		// otherwise a flaky apiserver silently passes a check that
		// should have caught the forbidden shape.
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("%s %s/%s not found", kind, namespace, name), err)
		}
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to get %s %s/%s", kind, namespace, name), err)
	}

	return obj.UnstructuredContent(), nil
}

// List enumerates resources of the given kind in the given namespace,
// optionally narrowed by label match. labels is a string→string map
// converted to the canonical "k=v,k=v" Kubernetes label selector
// format. An empty labels map yields no selector (all resources of the
// kind in the namespace).
//
// Returns an empty slice (not error) when no resources match; callers
// distinguish "no matches" from "list failed".
func (f *clusterFetcher) List(ctx context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]interface{}, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q", apiVersion), err)
	}

	gvk := gv.WithKind(kind)
	mapping, err := f.resolveMapping(gvk)
	if err != nil {
		return nil, err
	}

	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = f.client.Resource(mapping.Resource).Namespace(namespace)
	} else {
		resource = f.client.Resource(mapping.Resource)
	}

	opts := metav1.ListOptions{}
	if len(labels) > 0 {
		parts := make([]string, 0, len(labels))
		for k, v := range labels {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		opts.LabelSelector = strings.Join(parts, ",")
	}

	list, err := resource.List(ctx, opts)
	if err != nil {
		// Mirror Fetch's mapping. ErrCodeInternal is reserved for a shape
		// mismatch — resourceObservedErr keys on it to latch off the
		// absent-resource grace — so a transient List failure must not carry
		// it, or an API blip would disable the fast-fail grace for every
		// list-based assert (#2039).
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("%s not found in namespace %q", gvk, namespace), err)
		}
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to list %s in namespace %q", gvk, namespace), err)
	}

	out := make([]map[string]interface{}, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].UnstructuredContent())
	}
	return out, nil
}
