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

// groupDiscoverer is the subset of discovery.DiscoveryInterface the fetcher
// needs to tell a genuine no-match apart from a kind hidden by a partial
// discovery failure. Narrowed to one method so tests can supply a double
// without implementing the whole DiscoveryInterface.
type groupDiscoverer interface {
	ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error)
}

// clusterFetcher implements ResourceFetcher using a dynamic Kubernetes client.
type clusterFetcher struct {
	client dynamic.Interface
	mapper meta.RESTMapper

	// discovery backs the partial-failure probe in groupDiscoveryFailure. It
	// MUST be the same cached client the mapper resolves through, or a Reset()
	// would invalidate one cache and leave the probe reading the other's
	// stale view. Nil disables the probe — see NewClusterFetcher.
	discovery groupDiscoverer

	// mu guards lastReset and resetGen. One fetcher is shared by up to
	// defaults.ChainsawMaxParallel component assertions.
	mu        sync.Mutex
	lastReset time.Time
	// resetGen counts completed discovery invalidations. resolveMapping
	// samples it before its first lookup and after the refresh attempt: an
	// increase proves some goroutine invalidated the cache in between, so
	// the retry read discovery performed after this call started.
	resetGen uint64

	// now is swappable in tests; nil means time.Now.
	now func() time.Time
}

// ClusterFetcherOption configures a cluster fetcher.
type ClusterFetcherOption func(*clusterFetcher)

// WithGroupDiscovery supplies the discovery client the fetcher consults when
// the RESTMapper reports no match for a kind, so a group that discovery could
// not enumerate is reported as ErrCodeUnavailable rather than "this cluster
// does not serve that kind".
//
// Pass the SAME cached discovery client that backs the mapper. Production
// callers get this wiring for free from NewClusterFetcherForConfig /
// NewClusterFetcherWithClient.
func WithGroupDiscovery(d groupDiscoverer) ClusterFetcherOption {
	return func(f *clusterFetcher) { f.discovery = d }
}

// NewClusterFetcher creates a ResourceFetcher that queries a live Kubernetes
// cluster.
//
// Without WithGroupDiscovery the fetcher cannot tell a genuine no-match apart
// from a kind whose API group failed discovery, and classifies a bare
// NoKindMatchError as ErrCodeNotFound. That is the fail-open direction for a
// negative assertion, so every production path builds the fetcher through
// NewClusterFetcherForConfig or NewClusterFetcherWithClient, which wire the
// probe. This constructor remains the injection seam for tests that supply a
// hand-built RESTMapper.
func NewClusterFetcher(client dynamic.Interface, mapper meta.RESTMapper, opts ...ClusterFetcherOption) ResourceFetcher {
	f := &clusterFetcher{client: client, mapper: mapper}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// NewClusterFetcherForConfig builds a ResourceFetcher from a client
// configuration, constructing the dynamic client it reads through, the
// discovery-backed RESTMapper it resolves scope with, and the partial-discovery
// probe that keeps a no-match honest.
//
// Callers that already hold a dynamic client (the deployment validator keeps
// ctx.DynamicClient as an injection seam) should use NewClusterFetcherWithClient
// so the mapper and probe wiring is still shared.
func NewClusterFetcherForConfig(restConfig *rest.Config) (ResourceFetcher, error) {
	dynClient, err := NewDynamicClientForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	return NewClusterFetcherWithClient(dynClient, restConfig)
}

// NewClusterFetcherWithClient builds a ResourceFetcher around a caller-supplied
// dynamic client, deriving the RESTMapper and its discovery probe from
// restConfig. Both are backed by one cached discovery client, so invalidating
// the mapper's cache also invalidates the probe's view.
func NewClusterFetcherWithClient(client dynamic.Interface, restConfig *rest.Config) (ResourceFetcher, error) {
	mapper, disco, err := newRESTMapperAndDiscovery(restConfig)
	if err != nil {
		return nil, err
	}
	return NewClusterFetcher(client, mapper, WithGroupDiscovery(disco)), nil
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
//
// Prefer NewClusterFetcherWithClient when the mapper is destined for a fetcher:
// it also wires the partial-discovery probe, which a mapper alone cannot carry.
func NewRESTMapperForConfig(restConfig *rest.Config) (meta.RESTMapper, error) {
	mapper, _, err := newRESTMapperAndDiscovery(restConfig)
	return mapper, err
}

// newRESTMapperAndDiscovery builds the deferred RESTMapper together with the
// cached discovery client backing it. Returning both is what lets the fetcher
// ask the very cache the mapper resolved through whether a no-match came from
// a group discovery could not enumerate; two independently-constructed caches
// would drift across a Reset().
func newRESTMapperAndDiscovery(restConfig *rest.Config) (meta.RESTMapper, discovery.CachedDiscoveryInterface, error) {
	if restConfig == nil {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest, "no kubernetes client configuration available")
	}

	discoveryClient, err := kubernetes.NewForConfig(boundedConfig(restConfig))
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to create discovery client", err)
	}

	cached := memory.NewMemCacheClient(discoveryClient.Discovery())
	return restmapper.NewDeferredDiscoveryRESTMapper(cached), cached, nil
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
//
// Reaching ErrCodeNotFound therefore requires BOTH conditions to hold:
//
//  1. the retry ran against discovery performed after this call started (the
//     reset generation advanced), and
//  2. the group the kind lives in was fully enumerated by that discovery pass.
//
// Anything short of that is ErrCodeUnavailable. Both guards close a fail-open
// path: a cooldown-denied refresh could otherwise conclude "absent" from a
// cache predating the CRD's installation, and client-go silently drops
// ErrGroupDiscoveryFailed whenever partial results exist, so a kind in an
// unreachable group surfaces as a bare NoKindMatchError.
func (f *clusterFetcher) resolveMapping(ctx context.Context, gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	genBefore := f.resetGeneration()

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
	refreshed, genAfter := f.refreshDiscovery()
	mapping, err = f.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err == nil {
		return mapping, nil
	}
	if !isGenuineNoMatch(err) {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to resolve REST mapping for %s after discovery retry (refreshed=%t)",
				gvk, refreshed), err)
	}

	// The cache was never invalidated between this call's first lookup and
	// its retry, so both reads may predate the kind's installation. A CRD
	// created inside DiscoveryRefreshCooldown is exactly this case, and the
	// bundler-emitted --stability-window can latch Ready before the next
	// refresh is permitted. Unresolved, not absent.
	if genAfter == genBefore {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("no REST mapping for %s and discovery could not be refreshed "+
				"within the %s cooldown; treating as unresolved rather than absent",
				gvk, defaults.DiscoveryRefreshCooldown), err)
	}

	if groupErr := f.groupDiscoveryFailure(ctx, gvk.Group); groupErr != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("no REST mapping for %s, but discovery for API group %q is incomplete",
				gvk, gvk.Group), groupErr)
	}

	return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("no REST mapping for %s", gvk), err)
}

// groupDiscoveryFailure reports the discovery error covering group, or nil when
// discovery enumerated it completely (or no probe is wired).
//
// Scoped to the one group deliberately. Real clusters routinely carry a broken
// aggregated APIService (a scaled-to-zero metrics adapter is the classic), and
// treating any partial failure as "everything is unresolved" would strand every
// negative assertion on a permanently degraded cluster. Only a kind whose OWN
// group could not be enumerated is ambiguous.
//
// The probe normally reads the cache the mapper's retry just repopulated, so it
// costs no request. It takes ctx anyway because DiscoveryInterface is
// context-free: on a cold cache ServerGroupsAndResources fans out one request
// per group-version, each bounded only by boundedConfig's per-request timeout,
// with nothing tying the set to the assertion deadline. A ctx already past its
// deadline cannot prove the group healthy, so it reports the cause instead of
// probing — the fail-closed direction.
func (f *clusterFetcher) groupDiscoveryFailure(ctx context.Context, group string) error {
	if f.discovery == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeUnavailable,
			"context expired before API group discovery could be verified", err)
	}
	_, _, err := f.discovery.ServerGroupsAndResources()
	if err == nil {
		return nil
	}
	var groupErr *discovery.ErrGroupDiscoveryFailed
	if !stderrors.As(err, &groupErr) {
		// Discovery failed outright rather than per-group. The kind cannot
		// be declared absent on the strength of that.
		return err
	}
	for gv, gvErr := range groupErr.Groups {
		if gv.Group == group {
			return errors.Wrap(errors.ErrCodeUnavailable, gv.String(), gvErr)
		}
	}
	return nil
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

// resetGeneration returns the number of discovery invalidations completed so
// far. resolveMapping samples it around its refresh attempt to decide whether
// a surviving no-match was read from post-invalidation discovery.
func (f *clusterFetcher) resetGeneration() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resetGen
}

// refreshDiscovery invalidates the mapper's cached discovery data, reporting
// whether it did and the reset generation as of return. Rate-limited to once
// per defaults.DiscoveryRefreshCooldown: an assertion for a kind that genuinely
// does not exist retries every AssertRetryInterval (5s) for the whole assert
// window, and refreshing on each of those would turn one missing CRD into a
// discovery storm against the apiserver.
//
// The returned generation is what makes the single-flight cooldown safe to
// share across the defaults.ChainsawMaxParallel goroutines on one fetcher: a
// caller denied its own refresh still sees the generation advanced by whichever
// goroutine won, and so knows its retry read fresh discovery.
func (f *clusterFetcher) refreshDiscovery() (bool, uint64) {
	resettable, ok := f.mapper.(resettableMapper)
	if !ok {
		// Nothing to invalidate. Report the generation as advanced so a
		// non-resettable mapper (a test double, or a caller-supplied static
		// mapper) keeps its pre-existing "a no-match is absent" semantics
		// instead of stalling on a cooldown it can never clear.
		f.mu.Lock()
		defer f.mu.Unlock()
		return false, f.resetGen + 1
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
		f.resetGen++
		return true, f.resetGen
	}
	return false, f.resetGen
}

func (f *clusterFetcher) Fetch(ctx context.Context, apiVersion, kind, namespace, name string) (map[string]any, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q", apiVersion), err)
	}

	gvk := gv.WithKind(kind)
	mapping, err := f.resolveMapping(ctx, gvk)
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
func (f *clusterFetcher) List(ctx context.Context, apiVersion, kind, namespace string, labels map[string]string) ([]map[string]any, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid apiVersion %q", apiVersion), err)
	}

	gvk := gv.WithKind(kind)
	mapping, err := f.resolveMapping(ctx, gvk)
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

	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, list.Items[i].UnstructuredContent())
	}
	return out, nil
}
