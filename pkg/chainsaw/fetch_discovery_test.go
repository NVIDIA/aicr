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
	"sync"
	"testing"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/openapi"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// stubDiscovery is a minimal discovery.DiscoveryInterface that serves a fixed
// group list and can fail resource discovery for chosen group-versions. It
// exists so a test can drive the REAL restmapper.DeferredDiscoveryRESTMapper
// over a real memory.MemCacheClient — the wiring NewClusterFetcherForConfig
// builds — instead of hand-constructing a mapper error shape.
type stubDiscovery struct {
	groups []metav1.APIGroup
	// resources maps "<group>/<version>" (or "v1") to the kinds it serves.
	resources map[string][]metav1.APIResource
	// failures maps the same key to a discovery error, modeling an
	// aggregated APIService that is down or unreachable by this identity.
	failures map[string]error
}

func (d *stubDiscovery) RESTClient() restclient.Interface { return nil }

func (d *stubDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	return &metav1.APIGroupList{Groups: d.groups}, nil
}

func (d *stubDiscovery) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	if err, ok := d.failures[groupVersion]; ok {
		return nil, err
	}
	return &metav1.APIResourceList{
		GroupVersion: groupVersion,
		APIResources: d.resources[groupVersion],
	}, nil
}

func (d *stubDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return discovery.ServerGroupsAndResources(d)
}

func (d *stubDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return discovery.ServerPreferredResources(d)
}

func (d *stubDiscovery) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return discovery.ServerPreferredNamespacedResources(d)
}

func (d *stubDiscovery) ServerVersion() (*version.Info, error) { return &version.Info{}, nil }

func (d *stubDiscovery) OpenAPISchema() (*openapi_v2.Document, error) {
	return &openapi_v2.Document{}, nil
}

func (d *stubDiscovery) OpenAPIV3() openapi.Client { return nil }

func (d *stubDiscovery) WithLegacy() discovery.DiscoveryInterface { return d }

// newStubDiscovery serves core v1 (Pods) plus an aggregated group. When
// aggregatedErr is non-nil, discovery for that group fails the way an
// unreachable APIService does.
func newStubDiscovery(aggregatedErr error) *stubDiscovery {
	d := &stubDiscovery{
		groups: []metav1.APIGroup{
			{
				Name:     "",
				Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "v1", Version: "v1"}},
			},
			{
				Name:     "nvidia.com",
				Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "nvidia.com/v1", Version: "v1"}},
			},
		},
		resources: map[string][]metav1.APIResource{
			"v1": {{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"get", "list"}}},
			"nvidia.com/v1": {
				{Name: "clusterpolicies", Kind: "ClusterPolicy", Namespaced: false, Verbs: metav1.Verbs{"get", "list"}},
			},
		},
		failures: map[string]error{},
	}
	if aggregatedErr != nil {
		d.failures["nvidia.com/v1"] = aggregatedErr
	}
	return d
}

// newRealMapper wires the exact pair NewClusterFetcherForConfig builds: a
// DeferredDiscoveryRESTMapper over a MemCacheClient, both backed by the same
// discovery client.
func newRealMapper(d discovery.DiscoveryInterface) (meta.RESTMapper, discovery.CachedDiscoveryInterface) {
	cached := memory.NewMemCacheClient(d)
	return restmapper.NewDeferredDiscoveryRESTMapper(cached), cached
}

// TestPartialDiscovery_ProducesBareNoKindMatch pins the client-go behavior the
// classification has to defend against, so this stays honest if client-go ever
// changes: restmapper.GetAPIGroupResources drops ErrGroupDiscoveryFailed
// whenever partial results exist, so a kind in the failed group surfaces from
// the deferred mapper as a BARE NoKindMatchError — indistinguishable, from the
// error alone, from a kind the cluster genuinely does not serve.
func TestPartialDiscovery_ProducesBareNoKindMatch(t *testing.T) {
	t.Parallel()

	disco := newStubDiscovery(stderrors.New("the server is currently unable to handle the request"))
	mapper, _ := newRealMapper(disco)

	_, err := mapper.RESTMapping(schema.GroupKind{Group: "nvidia.com", Kind: "ClusterPolicy"}, "v1")
	if err == nil {
		t.Fatal("expected a mapping error for a kind in a group discovery could not enumerate")
	}
	if !meta.IsNoMatchError(err) {
		t.Fatalf("err = %v, want a NoKindMatchError", err)
	}
	if _, ok := stderrors.AsType[*discovery.ErrGroupDiscoveryFailed](err); ok {
		t.Fatalf("client-go now propagates ErrGroupDiscoveryFailed through the mapper (%v); "+
			"the ServerGroupsAndResources probe in resolveMapping may be redundant", err)
	}
}

// TestClusterFetcher_PartialDiscoveryClassification is the fail-closed guard
// for that shape, driven end to end through the production mapper wiring. A
// kind whose own API group failed discovery must be ErrCodeUnavailable —
// NotFound is the immediate-pass path for a negative `error:` assertion, so
// classifying it that way lets an unreachable API group satisfy every negative
// health check.
func TestClusterFetcher_PartialDiscoveryClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		aggregatedErr error
		kind          string
		apiVersion    string
		wantCode      errors.ErrorCode
	}{
		{
			name:          "kind in the failed group is Unavailable",
			aggregatedErr: stderrors.New("the server is currently unable to handle the request"),
			apiVersion:    "nvidia.com/v1",
			kind:          "ClusterPolicy",
			wantCode:      errors.ErrCodeUnavailable,
		},
		{
			// The narrow scope matters: real clusters routinely carry one
			// broken aggregated APIService. Treating any partial failure as
			// "everything unresolved" would strand every negative assertion
			// on a permanently degraded cluster.
			name:          "kind absent from a healthy group is still NotFound",
			aggregatedErr: stderrors.New("the server is currently unable to handle the request"),
			apiVersion:    "v1",
			kind:          "ConfigMap",
			wantCode:      errors.ErrCodeNotFound,
		},
		{
			name:          "kind absent with discovery fully healthy is NotFound",
			aggregatedErr: nil,
			apiVersion:    "v1",
			kind:          "ConfigMap",
			wantCode:      errors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			disco := newStubDiscovery(tt.aggregatedErr)
			mapper, cached := newRealMapper(disco)
			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			f := NewClusterFetcher(client, mapper, WithGroupDiscovery(cached))

			_, err := f.Fetch(context.Background(), tt.apiVersion, tt.kind, "ns", "x")
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantCode, err)
			}
		})
	}
}

// TestClusterFetcher_WithoutProbe_PartialDiscoveryFallsBack documents the
// boundary of the injection seam: NewClusterFetcher with no discovery client
// cannot see the partial failure and reports NotFound. Every production path
// wires the probe (NewClusterFetcherForConfig / NewClusterFetcherWithClient);
// this test exists so removing that wiring is a visible behavior change rather
// than a silent one.
func TestClusterFetcher_WithoutProbe_PartialDiscoveryFallsBack(t *testing.T) {
	t.Parallel()

	disco := newStubDiscovery(stderrors.New("the server is currently unable to handle the request"))
	mapper, _ := newRealMapper(disco)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	f := NewClusterFetcher(client, mapper) // no WithGroupDiscovery

	_, err := f.Fetch(context.Background(), "nvidia.com/v1", "ClusterPolicy", "", "x")
	var se *errors.StructuredError
	if !stderrors.As(err, &se) || se.Code != errors.ErrCodeNotFound {
		t.Fatalf("err = %v, want ErrCodeNotFound without a discovery probe", err)
	}
}

// staleThenHealingMapper models the production stale-cache case exactly: the
// kind is missing until Reset() is called, after which it resolves. Every field
// is mutex-guarded because one fetcher is shared by up to ChainsawMaxParallel
// goroutines, and the concurrency subtest below drives it from all of them.
type staleThenHealingMapper struct {
	mu     sync.Mutex
	healed bool
	resets int
	inner  meta.RESTMapper
}

func (m *staleThenHealingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	m.mu.Lock()
	healed := m.healed
	m.mu.Unlock()
	if !healed {
		return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	return m.inner.RESTMapping(gk, versions...)
}

func (m *staleThenHealingMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*meta.RESTMapping, error) {
	return m.inner.RESTMappings(gk, versions...)
}

func (m *staleThenHealingMapper) KindFor(r schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return m.inner.KindFor(r)
}

func (m *staleThenHealingMapper) KindsFor(r schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return m.inner.KindsFor(r)
}

func (m *staleThenHealingMapper) ResourceFor(i schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return m.inner.ResourceFor(i)
}

func (m *staleThenHealingMapper) ResourcesFor(i schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return m.inner.ResourcesFor(i)
}

func (m *staleThenHealingMapper) ResourceSingularizer(resource string) (string, error) {
	return m.inner.ResourceSingularizer(resource)
}

// Reset is the only thing that heals the mapper, mirroring
// DeferredDiscoveryRESTMapper.Reset dropping its delegate so the next lookup
// rebuilds from fresh discovery.
func (m *staleThenHealingMapper) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets++
	m.healed = true
}

// resetCount reports how many times Reset landed, which is what the
// single-flight cooldown assertions key on.
func (m *staleThenHealingMapper) resetCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resets
}

// TestClusterFetcher_CooldownDeniedNoMatchIsUnavailable covers the second
// fail-open path in the no-match classification. resolveMapping retries after a
// denied refresh against the SAME un-invalidated delegate, so a CRD installed
// inside DiscoveryRefreshCooldown reads as absent — and the bundler-emitted
// --stability-window=30s can latch Ready well before the 60s cooldown lets
// discovery catch up. A no-match that could not be re-read against fresh
// discovery is unresolved, not absent.
func TestClusterFetcher_CooldownDeniedNoMatchIsUnavailable(t *testing.T) {
	t.Parallel()

	t.Run("first lookup refreshes and reports the genuine verdict", func(t *testing.T) {
		t.Parallel()
		mapper := &staleThenHealingMapper{inner: testMapper()}
		clock := time.Now()
		f := &clusterFetcher{
			client: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), newPod("ns", "p1", nil)),
			mapper: mapper,
			now:    func() time.Time { return clock },
		}

		// The refresh heals the mapper, so the retry resolves and the fetch
		// reaches the (empty) dynamic client rather than the mapper.
		if _, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1"); err != nil {
			t.Fatalf("Fetch after a granted refresh: %v", err)
		}
		if resets := mapper.resetCount(); resets != 1 {
			t.Errorf("Reset called %d times, want 1", resets)
		}
	})

	t.Run("cooldown-denied no-match is Unavailable, not NotFound", func(t *testing.T) {
		t.Parallel()
		// Never heals: the kind is genuinely absent as far as this cache
		// knows, which is exactly the ambiguous state.
		mapper := &flakyMapper{RESTMapper: testMapper(), failures: 99,
			err: &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: "Pod"}, SearchedVersions: []string{"v1"}}}
		clock := time.Now()
		f := &clusterFetcher{
			client: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
			mapper: mapper,
			now:    func() time.Time { return clock },
		}
		// Burn the cooldown so this caller is denied its own refresh and no
		// other goroutine advances the generation.
		f.lastReset = clock

		_, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
		var se *errors.StructuredError
		if !stderrors.As(err, &se) {
			t.Fatalf("expected StructuredError, got %T %v", err, err)
		}
		if se.Code != errors.ErrCodeUnavailable {
			t.Errorf("code = %q, want %q — a no-match read from a cache that could not be "+
				"invalidated may predate the kind's installation (err=%v)", se.Code, errors.ErrCodeUnavailable, err)
		}
		if mapper.resets != 0 {
			t.Errorf("Reset called %d times, want 0 (cooldown active)", mapper.resets)
		}
	})

	t.Run("past the cooldown the verdict becomes NotFound again", func(t *testing.T) {
		t.Parallel()
		mapper := &flakyMapper{RESTMapper: testMapper(), failures: 99,
			err: &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: "Pod"}, SearchedVersions: []string{"v1"}}}
		clock := time.Now()
		f := &clusterFetcher{
			client: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
			mapper: mapper,
			now:    func() time.Time { return clock },
		}
		f.lastReset = clock

		if _, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1"); !isUnavailable(err) {
			t.Fatalf("inside the cooldown: err = %v, want Unavailable", err)
		}

		clock = clock.Add(defaults.DiscoveryRefreshCooldown)
		_, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeNotFound {
			t.Fatalf("after the cooldown: err = %v, want ErrCodeNotFound", err)
		}
	})

	// The single-flight cooldown is shared by ChainsawMaxParallel goroutines
	// on one fetcher. A caller denied its own refresh must still benefit from
	// the winner's: the generation it observes has advanced, so its retry read
	// post-invalidation discovery and may report the genuine verdict.
	t.Run("concurrent callers share one refresh without a stale verdict", func(t *testing.T) {
		t.Parallel()
		mapper := &staleThenHealingMapper{inner: testMapper()}
		f := &clusterFetcher{
			client: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), newPod("ns", "p1", nil)),
			mapper: mapper,
		}

		var wg sync.WaitGroup
		errs := make([]error, defaults.ChainsawMaxParallel)
		for i := range errs {
			wg.Go(func() {
				_, errs[i] = f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
			})
		}
		wg.Wait()

		resets := mapper.resetCount()
		if resets != 1 {
			t.Errorf("Reset called %d times across %d concurrent callers, want exactly 1 "+
				"(the cooldown is single-flight)", resets, len(errs))
		}
		// Whoever raced, nobody may conclude "absent": the kind resolves once
		// the single refresh lands, and a loser either resolves it too or
		// reports Unavailable — never NotFound.
		for i, err := range errs {
			if err == nil {
				continue
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("caller %d: expected StructuredError, got %T %v", i, err, err)
			}
			if se.Code == errors.ErrCodeNotFound {
				t.Errorf("caller %d concluded NotFound from a cache it could not prove fresh: %v", i, err)
			}
		}
	})
}

func isUnavailable(err error) bool {
	return stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, ""))
}

// TestClusterFetcher_ProbeHonorsContext covers the bound on the partial-discovery
// probe. DiscoveryInterface is context-free, so on a cold cache
// ServerGroupsAndResources fans out one request per group-version with nothing
// tying the set to the assertion deadline. An already-expired context cannot
// prove the group healthy, so the probe reports the cause instead of issuing
// those requests — Unavailable, never NotFound.
func TestClusterFetcher_ProbeHonorsContext(t *testing.T) {
	t.Parallel()

	disco := newStubDiscovery(nil)
	mapper, cached := newRealMapper(disco)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	f := NewClusterFetcher(client, mapper, WithGroupDiscovery(cached))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // expired before the probe would run

	// ConfigMap is absent from the stub's healthy core group, so a live probe
	// would clear it to NotFound. With the context already done it must not.
	_, err := f.Fetch(ctx, "v1", "ConfigMap", "ns", "x")
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Errorf("err = %v, want code %s — an expired context cannot prove a group was enumerated",
			err, errors.ErrCodeUnavailable)
	}
}
