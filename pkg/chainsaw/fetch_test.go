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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// testMapper returns a RESTMapper that knows about the two GVKs the
// tests use: namespaced Pods and cluster-scoped Nodes.
func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "", Version: "v1"},
	})
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)
	return m
}

func newPod(namespace, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Pod")
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

func newNode(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Node")
	u.SetName(name)
	return u
}

func TestClusterFetcher_Fetch(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	tests := []struct {
		name         string
		objects      []runtime.Object
		apiVersion   string
		kind         string
		namespace    string
		resourceName string
		wantErrCode  errors.ErrorCode // "" == success
	}{
		{
			name:         "namespaced get hits the object",
			objects:      []runtime.Object{newPod("ns", "p1", nil)},
			apiVersion:   "v1",
			kind:         "Pod",
			namespace:    "ns",
			resourceName: "p1",
		},
		{
			name:         "cluster-scoped get hits the object",
			objects:      []runtime.Object{newNode("n1")},
			apiVersion:   "v1",
			kind:         "Node",
			resourceName: "n1",
		},
		{
			name:         "missing resource maps to ErrCodeNotFound",
			objects:      nil,
			apiVersion:   "v1",
			kind:         "Pod",
			namespace:    "ns",
			resourceName: "missing",
			wantErrCode:  errors.ErrCodeNotFound,
		},
		{
			name:         "invalid apiVersion maps to ErrCodeInvalidRequest",
			apiVersion:   "//bad//",
			kind:         "Pod",
			namespace:    "ns",
			resourceName: "p1",
			wantErrCode:  errors.ErrCodeInvalidRequest,
		},
		{
			name:         "unknown kind maps to ErrCodeNotFound (no REST mapping)",
			apiVersion:   "v1",
			kind:         "DoesNotExist",
			namespace:    "ns",
			resourceName: "x",
			wantErrCode:  errors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := dynamicfake.NewSimpleDynamicClient(scheme, tt.objects...)
			f := NewClusterFetcher(client, testMapper())
			obj, err := f.Fetch(context.Background(), tt.apiVersion, tt.kind, tt.namespace, tt.resourceName)
			if tt.wantErrCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if obj["metadata"].(map[string]any)["name"] != tt.resourceName {
					t.Errorf("returned object name mismatch: %v", obj["metadata"])
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error code %q, got nil", tt.wantErrCode)
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantErrCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantErrCode, err)
			}
		})
	}
}

// TestClusterFetcher_Fetch_TransientErrorMapsToUnavailable verifies that
// non-NotFound apiserver failures (e.g. 5xx, forbidden) surface as
// ErrCodeUnavailable so chainsaw `error:` blocks fail closed rather
// than treating a transient failure as "resource absent".
func TestClusterFetcher_Fetch_TransientErrorMapsToUnavailable(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClient(scheme)
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	})
	f := NewClusterFetcher(client, testMapper())
	_, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %T %v", err, err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("code = %q, want ErrCodeUnavailable (err=%v)", se.Code, err)
	}
}

func TestClusterFetcher_List(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	// Register the list kind so the fake client can route the List op.
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PodList"}, &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "NodeList"}, &unstructured.UnstructuredList{})

	freshPods := func() []runtime.Object {
		return []runtime.Object{
			newPod("ns", "match-1", map[string]string{"app": "foo", "tier": "web"}),
			newPod("ns", "match-2", map[string]string{"app": "foo", "tier": "db"}),
			newPod("ns", "no-match", map[string]string{"app": "bar"}),
			newPod("other", "elsewhere", map[string]string{"app": "foo"}),
		}
	}
	freshNodes := func() []runtime.Object {
		return []runtime.Object{newNode("n1"), newNode("n2")}
	}

	tests := []struct {
		name       string
		apiVersion string
		kind       string
		namespace  string
		labels     map[string]string
		wantNames  []string
	}{
		{
			name:       "namespaced list with no selector returns all in ns",
			apiVersion: "v1",
			kind:       "Pod",
			namespace:  "ns",
			wantNames:  []string{"match-1", "match-2", "no-match"},
		},
		{
			name:       "label selector filters to matching items",
			apiVersion: "v1",
			kind:       "Pod",
			namespace:  "ns",
			labels:     map[string]string{"app": "foo"},
			wantNames:  []string{"match-1", "match-2"},
		},
		{
			name:       "multi-key selector narrows further",
			apiVersion: "v1",
			kind:       "Pod",
			namespace:  "ns",
			labels:     map[string]string{"app": "foo", "tier": "web"},
			wantNames:  []string{"match-1"},
		},
		{
			name:       "cluster-scoped list ignores namespace argument",
			apiVersion: "v1",
			kind:       "Node",
			namespace:  "",
			wantNames:  []string{"n1", "n2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var objs []runtime.Object
			if tt.kind == "Node" {
				objs = freshNodes()
			} else {
				objs = freshPods()
			}
			client := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
			f := NewClusterFetcher(client, testMapper())
			items, err := f.List(context.Background(), tt.apiVersion, tt.kind, tt.namespace, tt.labels)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			got := make([]string, 0, len(items))
			for _, it := range items {
				if md, ok := it["metadata"].(map[string]any); ok {
					if n, ok := md["name"].(string); ok {
						got = append(got, n)
					}
				}
			}
			if !sameStringSet(got, tt.wantNames) {
				t.Errorf("names = %v, want %v", got, tt.wantNames)
			}
		})
	}
}

func TestClusterFetcher_List_ErrorCodes(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClient(scheme)
	f := NewClusterFetcher(client, testMapper())

	tests := []struct {
		name        string
		apiVersion  string
		kind        string
		wantErrCode errors.ErrorCode
	}{
		{"invalid apiVersion", "//bad//", "Pod", errors.ErrCodeInvalidRequest},
		{"unknown kind", "v1", "DoesNotExist", errors.ErrCodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.List(context.Background(), tt.apiVersion, tt.kind, "ns", nil)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantErrCode {
				t.Errorf("code = %q, want %q", se.Code, tt.wantErrCode)
			}
		})
	}
}

// TestClusterFetcher_List_APIErrorCodes verifies List mirrors Fetch's mapping
// of apiserver failures: a true 404 is ErrCodeNotFound, anything else is
// ErrCodeUnavailable. Neither may be ErrCodeInternal — resourceObservedErr
// keys on that code to latch off the absent-resource grace, so a transient
// List failure carrying it would disable the fast-fail path for every
// list-based assert (#2039).
func TestClusterFetcher_List_APIErrorCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		reactorErr  error
		wantErrCode errors.ErrorCode
	}{
		{
			name:        "transient apiserver failure maps to ErrCodeUnavailable",
			reactorErr:  apierrors.NewServiceUnavailable("apiserver down"),
			wantErrCode: errors.ErrCodeUnavailable,
		},
		{
			name:        "forbidden maps to ErrCodeUnavailable",
			reactorErr:  apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "p1", stderrors.New("denied")),
			wantErrCode: errors.ErrCodeUnavailable,
		},
		{
			name:        "not found maps to ErrCodeNotFound",
			reactorErr:  apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "p1"),
			wantErrCode: errors.ErrCodeNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			scheme.AddKnownTypeWithName(
				schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PodList"}, &unstructured.UnstructuredList{})
			client := dynamicfake.NewSimpleDynamicClient(scheme)
			client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.reactorErr
			})
			f := NewClusterFetcher(client, testMapper())

			_, err := f.List(context.Background(), "v1", "Pod", "ns", nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantErrCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantErrCode, err)
			}
			if resourceObservedErr(err) {
				t.Errorf("resourceObservedErr = true for %v; a List failure must not latch the absent-resource grace", err)
			}
		})
	}
}

// flakyMapper fails RESTMapping a configurable number of times before
// delegating to a working mapper, and records how often Reset was called.
type flakyMapper struct {
	meta.RESTMapper
	failures  int
	err       error
	resets    int
	resetHeal bool // when true, Reset makes the next lookup succeed
}

func (m *flakyMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if m.failures > 0 {
		m.failures--
		return nil, m.err
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

func (m *flakyMapper) Reset() {
	m.resets++
	if m.resetHeal {
		m.failures = 0
	}
}

// TestClusterFetcher_MappingErrorCodes is a fail-closed guard. A REST-mapping
// failure used to be reported as ErrCodeNotFound regardless of cause — and
// NotFound is the HAPPY path for a negative `error:` assertion, so a discovery
// outage silently satisfied every negative health check. Only a genuine
// no-match may be NotFound; anything else must be ErrCodeUnavailable.
func TestClusterFetcher_MappingErrorCodes(t *testing.T) {
	t.Parallel()

	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "nvidia.com", Kind: "ClusterPolicy"},
		SearchedVersions: []string{"v1"},
	}

	tests := []struct {
		name     string
		mapErr   error
		wantCode errors.ErrorCode
	}{
		{
			name:     "genuine no-match is NotFound",
			mapErr:   noMatch,
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:     "discovery outage is Unavailable, not NotFound",
			mapErr:   stderrors.New("Get \"https://10.0.0.1:443/api\": dial tcp: i/o timeout"),
			wantCode: errors.ErrCodeUnavailable,
		},
		{
			name:     "forbidden discovery is Unavailable",
			mapErr:   apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", stderrors.New("denied")),
			wantCode: errors.ErrCodeUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A fresh fetcher per call. Both entry points must classify the
			// same cause identically, which is what this asserts — but the
			// discovery-refresh cooldown is fetcher state, so reusing one
			// instance would make the second call exercise the
			// cooldown-denied path instead of the classification under test.
			// That path has its own coverage below.
			newFetcher := func() ResourceFetcher {
				scheme := runtime.NewScheme()
				scheme.AddKnownTypeWithName(
					schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PodList"}, &unstructured.UnstructuredList{})
				client := dynamicfake.NewSimpleDynamicClient(scheme)
				return NewClusterFetcher(client, &flakyMapper{RESTMapper: testMapper(), failures: 99, err: tt.mapErr})
			}

			for _, call := range []struct {
				name string
				run  func() error
			}{
				{"Fetch", func() error {
					_, err := newFetcher().Fetch(context.Background(), "v1", "Pod", "ns", "p1")
					return err
				}},
				{"List", func() error {
					_, err := newFetcher().List(context.Background(), "v1", "Pod", "ns", nil)
					return err
				}},
			} {
				err := call.run()
				if err == nil {
					t.Fatalf("%s: expected error, got nil", call.name)
				}
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("%s: expected StructuredError, got %T %v", call.name, err, err)
				}
				if se.Code != tt.wantCode {
					t.Errorf("%s: code = %q, want %q (err=%v)", call.name, se.Code, tt.wantCode, err)
				}
			}
		})
	}
}

// TestClusterFetcher_MappingRefreshesOnNoMatch covers the stale-discovery case
// that matters for the long-lived readiness gate: a CRD installed by the very
// component being gated appears after the mapper's cache was populated. Without
// a reset the gate reports "no such kind" until the process restarts.
func TestClusterFetcher_MappingRefreshesOnNoMatch(t *testing.T) {
	t.Parallel()

	noMatch := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "", Kind: "Pod"},
		SearchedVersions: []string{"v1"},
	}

	t.Run("no-match retries once against refreshed discovery", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		client := dynamicfake.NewSimpleDynamicClient(scheme, newPod("ns", "p1", nil))
		mapper := &flakyMapper{RESTMapper: testMapper(), failures: 1, err: noMatch, resetHeal: true}
		f := NewClusterFetcher(client, mapper)

		if _, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1"); err != nil {
			t.Fatalf("Fetch after discovery refresh: %v", err)
		}
		if mapper.resets != 1 {
			t.Errorf("Reset called %d times, want 1", mapper.resets)
		}
	})

	t.Run("still-missing kind reports NotFound after one refresh", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		client := dynamicfake.NewSimpleDynamicClient(scheme)
		mapper := &flakyMapper{RESTMapper: testMapper(), failures: 99, err: noMatch}
		f := NewClusterFetcher(client, mapper)

		_, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var se *errors.StructuredError
		if !stderrors.As(err, &se) || se.Code != errors.ErrCodeNotFound {
			t.Errorf("err = %v, want ErrCodeNotFound", err)
		}
		if mapper.resets != 1 {
			t.Errorf("Reset called %d times, want exactly 1", mapper.resets)
		}
	})

	// A kind that never appears is retried every AssertRetryInterval for the
	// whole assert window. Refreshing discovery on each of those retries would
	// turn one missing CRD into a discovery storm, so the refresh is
	// rate-limited.
	t.Run("repeated no-match refreshes at most once per cooldown", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		client := dynamicfake.NewSimpleDynamicClient(scheme)
		mapper := &flakyMapper{RESTMapper: testMapper(), failures: 99, err: noMatch}

		clock := time.Now()
		f := &clusterFetcher{client: client, mapper: mapper, now: func() time.Time { return clock }}

		// Retries that all land inside a single cooldown window: the loop
		// advances AssertRetryInterval (5s) per iteration and stops short of
		// DiscoveryRefreshCooldown (60s), so exactly one refresh is allowed.
		retries := int(defaults.DiscoveryRefreshCooldown / defaults.AssertRetryInterval)
		for range retries {
			_, _ = f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
			clock = clock.Add(defaults.AssertRetryInterval)
		}
		if mapper.resets != 1 {
			t.Errorf("Reset called %d times across %d retries inside one %v cooldown, want 1",
				mapper.resets, retries, defaults.DiscoveryRefreshCooldown)
		}

		// Past the cooldown, a refresh is allowed again.
		clock = clock.Add(defaults.DiscoveryRefreshCooldown)
		_, _ = f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
		if mapper.resets != 2 {
			t.Errorf("Reset called %d times after the cooldown elapsed, want 2", mapper.resets)
		}
	})

	// A caller denied a refresh by the cooldown must still re-read the mapper:
	// a concurrent assertion may have just refreshed it, and concluding
	// "absent" from an already-invalidated cache is the fail-open direction
	// for a negative assertion.
	t.Run("cooldown-denied caller still retries the lookup", func(t *testing.T) {
		t.Parallel()
		scheme := runtime.NewScheme()
		client := dynamicfake.NewSimpleDynamicClient(scheme, newPod("ns", "p1", nil))
		mapper := &flakyMapper{RESTMapper: testMapper(), failures: 1, err: noMatch}

		clock := time.Now()
		f := &clusterFetcher{client: client, mapper: mapper, now: func() time.Time { return clock }}
		// Burn the cooldown so the next call is denied a reset.
		f.lastReset = clock

		if _, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if mapper.resets != 0 {
			t.Errorf("Reset called %d times, want 0 (cooldown active)", mapper.resets)
		}
	})
}

// TestClusterFetcher_PartialDiscoveryFailureIsUnavailable covers the narrower
// fail-open path inside the no-match classification: client-go reports an
// unreachable aggregated APIService as a NoKindMatchError wrapped in
// ErrGroupDiscoveryFailed. Treating that as NotFound would let a down API group
// satisfy every negative `error:` assertion.
func TestClusterFetcher_PartialDiscoveryFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	groupErr := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: stderrors.New("the server is currently unable to handle the request"),
		},
	}

	tests := []struct {
		name     string
		mapErr   error
		wantCode errors.ErrorCode
	}{
		{
			name:     "plain no-match stays NotFound",
			mapErr:   &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: "Pod"}, SearchedVersions: []string{"v1"}},
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:     "no-match caused by failed group discovery is Unavailable",
			mapErr:   fmt.Errorf("%w: %w", groupErr, &meta.NoKindMatchError{GroupKind: schema.GroupKind{Kind: "Pod"}, SearchedVersions: []string{"v1"}}),
			wantCode: errors.ErrCodeUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			client := dynamicfake.NewSimpleDynamicClient(scheme)
			mapper := &flakyMapper{RESTMapper: testMapper(), failures: 99, err: tt.mapErr}
			f := NewClusterFetcher(client, mapper)

			_, err := f.Fetch(context.Background(), "v1", "Pod", "ns", "p1")
			if err == nil {
				t.Fatal("expected error, got nil")
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

// TestNewClusterFetcherForConfig covers the client wiring shared by the
// readiness gate (cmd/gate) and the deployment validator. Neither constructor
// contacts the apiserver and discovery is deferred, so an unreachable host
// still yields a usable fetcher; only a configuration client-go rejects at
// transport construction fails.
func TestNewClusterFetcherForConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		config     *rest.Config
		wantErr    bool
		wantErrMsg string
		wantCode   errors.ErrorCode
	}{
		{
			name:   "well-formed config yields a fetcher",
			config: &rest.Config{Host: "https://127.0.0.1:6443"},
		},
		{
			name:   "unreachable host still builds (discovery is deferred)",
			config: &rest.Config{Host: "https://127.0.0.1:1"},
		},
		{
			name:     "nil config is an invalid request",
			config:   nil,
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "conflicting auth strategies are rejected at transport construction",
			config: &rest.Config{
				Host: "https://127.0.0.1:6443",
				// client-go rejects exactly this pair — see
				// rest/transport.go: "execProvider and authProvider cannot
				// be used in combination". BearerToken alongside either is
				// NOT an error (BearerTokenFile simply takes precedence), so
				// this is the only credential conflict worth pinning.
				ExecProvider: &clientcmdapi.ExecConfig{
					Command:    "true",
					APIVersion: "client.authentication.k8s.io/v1",
				},
				AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "gcp"},
			},
			wantErr:    true,
			wantErrMsg: "execProvider and authProvider cannot be used in combination",
			wantCode:   errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewClusterFetcherForConfig(tt.config)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("NewClusterFetcherForConfig: %v", err)
				}
				if got == nil {
					t.Error("fetcher is nil")
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("expected StructuredError, got %T %v", err, err)
			}
			if se.Code != tt.wantCode {
				t.Errorf("code = %q, want %q (err=%v)", se.Code, tt.wantCode, err)
			}
			if tt.wantErrMsg != "" && !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErrMsg)
			}
		})
	}
}

// TestBoundedConfig pins the request bound applied to the fetcher's clients:
// the RESTMapper's DiscoveryInterface calls take no context, so without this
// they are governed only by client-go's own 32s discovery default — and the
// dynamic client gets no client-level bound at all.
func TestBoundedConfig(t *testing.T) {
	t.Parallel()

	t.Run("unset timeout is bounded", func(t *testing.T) {
		t.Parallel()
		in := &rest.Config{Host: "https://127.0.0.1:6443"}
		got := boundedConfig(in)
		if got.Timeout != defaults.K8sClientRequestTimeout {
			t.Errorf("Timeout = %v, want %v", got.Timeout, defaults.K8sClientRequestTimeout)
		}
		if in.Timeout != 0 {
			t.Errorf("caller config mutated: Timeout = %v, want 0", in.Timeout)
		}
	})

	t.Run("caller timeout is preserved", func(t *testing.T) {
		t.Parallel()
		in := &rest.Config{Host: "https://127.0.0.1:6443", Timeout: 5 * time.Second}
		if got := boundedConfig(in); got.Timeout != 5*time.Second {
			t.Errorf("Timeout = %v, want 5s", got.Timeout)
		}
	})

	t.Run("bound never loosens client-go's discovery default", func(t *testing.T) {
		t.Parallel()
		const clientGoDiscoveryDefault = 32 * time.Second
		if defaults.K8sClientRequestTimeout > clientGoDiscoveryDefault {
			t.Errorf("K8sClientRequestTimeout = %v exceeds client-go's %v discovery default, "+
				"so setting it explicitly would weaken the only bound on context-free discovery calls",
				defaults.K8sClientRequestTimeout, clientGoDiscoveryDefault)
		}
	})
}

// Suppress unused-import warning for metav1 when none of the helpers
// reference it directly (the fake client wires it internally).
var _ = metav1.ObjectMeta{}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		if m[s] == 0 {
			return false
		}
		m[s]--
	}
	return true
}
