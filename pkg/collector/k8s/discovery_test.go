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

package k8s

import (
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

const testDiscoveryGroupVersion = "example.test/v1"

// discoveryCancelTimeout is the deadline the cancellation test relies on to
// fire while discovery requests are still in flight — the expiry is the
// behavior under test, not a safety net.
const discoveryCancelTimeout = 100 * time.Millisecond

func TestCollectorDiscovery_CancelsAllDiscoveryRequests(t *testing.T) {
	tests := []struct {
		name      string
		blockPath string
		call      func(discovery.DiscoveryInterface) error
	}{
		{
			name:      "server groups",
			blockPath: "/apis",
			call: func(client discovery.DiscoveryInterface) error {
				_, err := client.ServerGroups()
				return err
			},
		},
		{
			name:      "group version resources",
			blockPath: "/apis/" + testDiscoveryGroupVersion,
			call: func(client discovery.DiscoveryInterface) error {
				_, err := client.ServerResourcesForGroupVersion(testDiscoveryGroupVersion)
				return err
			},
		},
		{
			name:      "server version",
			blockPath: "/version",
			call: func(client discovery.DiscoveryInterface) error {
				_, err := client.ServerVersion()
				return err
			},
		},
		{
			name:      "preferred resources",
			blockPath: "/apis/" + testDiscoveryGroupVersion,
			call: func(client discovery.DiscoveryInterface) error {
				_, err := client.ServerPreferredResources()
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			started := make(chan struct{})
			server := newDiscoveryServer(t, tt.blockPath, started)
			config := &rest.Config{Host: server.URL}
			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatalf("failed to create Kubernetes client: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			client, err := collectorDiscovery(ctx, clientset.Discovery(), config)
			if err != nil {
				t.Fatalf("collectorDiscovery() error = %v", err)
			}

			done := make(chan error, 1)
			go func() {
				done <- tt.call(client)
			}()

			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("discovery request did not reach blocking endpoint")
			}
			cancel()

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("discovery request error = nil, want context cancellation")
				}
				if !stderrors.Is(err, context.Canceled) {
					t.Errorf("discovery request error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("discovery request did not stop after collector context cancellation")
			}
		})
	}
}

func TestCollectorDiscovery_PreservesDiscoveryBehavior(t *testing.T) {
	server := newDiscoveryServer(t, "", nil)
	config := &rest.Config{Host: server.URL}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to create Kubernetes client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := collectorDiscovery(ctx, clientset.Discovery(), config)
	if err != nil {
		t.Fatalf("collectorDiscovery() error = %v", err)
	}

	groups, err := client.ServerGroups()
	if err != nil {
		t.Fatalf("ServerGroups() error = %v", err)
	}
	if len(groups.Groups) != 2 {
		t.Errorf("ServerGroups() returned %d groups, want 2", len(groups.Groups))
	}

	resources, err := client.ServerResourcesForGroupVersion(testDiscoveryGroupVersion)
	if err != nil {
		t.Fatalf("ServerResourcesForGroupVersion() error = %v", err)
	}
	if resources.GroupVersion != testDiscoveryGroupVersion {
		t.Errorf("resource group version = %q, want %q", resources.GroupVersion, testDiscoveryGroupVersion)
	}

	version, err := client.ServerVersion()
	if err != nil {
		t.Fatalf("ServerVersion() error = %v", err)
	}
	if version.GitVersion != "v1.33.0" {
		t.Errorf("server version = %q, want v1.33.0", version.GitVersion)
	}

	preferred, err := client.ServerPreferredResources()
	if err != nil {
		t.Fatalf("ServerPreferredResources() error = %v", err)
	}
	if len(preferred) != 2 {
		t.Errorf("ServerPreferredResources() returned %d lists, want 2", len(preferred))
	}
}

func TestCollectorDiscovery_EdgeCases(t *testing.T) {
	t.Run("fake discovery does not require a REST config", func(t *testing.T) {
		clientset := fakeclient.NewClientset()

		client, err := collectorDiscovery(context.Background(), clientset.Discovery(), nil)
		if err != nil {
			t.Fatalf("collectorDiscovery() error = %v", err)
		}
		if client != clientset.Discovery() {
			t.Error("collectorDiscovery() did not preserve fake discovery client")
		}
	})

	t.Run("nil discovery client is rejected", func(t *testing.T) {
		_, err := collectorDiscovery(context.Background(), nil, &rest.Config{})
		if err == nil {
			t.Fatal("collectorDiscovery() error = nil, want error")
		}
	})

	t.Run("missing REST config is rejected", func(t *testing.T) {
		server := newDiscoveryServer(t, "", nil)
		clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
		if err != nil {
			t.Fatalf("failed to create Kubernetes client: %v", err)
		}

		_, err = collectorDiscovery(context.Background(), clientset.Discovery(), nil)
		if err == nil {
			t.Fatal("collectorDiscovery() error = nil, want error")
		}
	})

	for _, tt := range []struct {
		name       string
		httpClient *http.Client
	}{
		{name: "nil HTTP client"},
		{name: "nil HTTP transport", httpClient: &http.Client{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newDiscoveryServer(t, "", nil)
			config := &rest.Config{Host: server.URL}
			clientset, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatalf("failed to create Kubernetes client: %v", err)
			}
			sourceRESTClient, ok := clientset.Discovery().RESTClient().(*rest.RESTClient)
			if !ok {
				t.Fatal("discovery REST client has unexpected type")
			}
			sourceRESTClient.Client = tt.httpClient

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client, err := collectorDiscovery(ctx, clientset.Discovery(), config)
			if err != nil {
				t.Fatalf("collectorDiscovery() error = %v", err)
			}
			version, err := client.ServerVersion()
			if err != nil {
				t.Fatalf("ServerVersion() error = %v", err)
			}
			if version.GitVersion != "v1.33.0" {
				t.Errorf("server version = %q, want v1.33.0", version.GitVersion)
			}
		})
	}
}

func TestDiscoveryTimeout(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		configured time.Duration
		want       time.Duration
		wantErr    bool
	}{
		{
			name: "no deadline uses collector timeout",
			ctx:  context.Background(),
			want: defaults.CollectorK8sTimeout,
		},
		{
			name:       "no deadline preserves configured timeout",
			ctx:        context.Background(),
			configured: time.Second,
			want:       time.Second,
		},
		{
			name:       "deadline caps configured timeout",
			ctx:        contextWithDeadline(t, 100*time.Millisecond),
			configured: time.Second,
			want:       100 * time.Millisecond,
		},
		{
			name:    "expired deadline returns timeout error",
			ctx:     expiredContext(t),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := discoveryTimeout(tt.ctx, tt.configured)
			if tt.wantErr {
				if err == nil {
					t.Fatal("discoveryTimeout() error = nil, want timeout")
				}
				if !errors.IsTransient(err) {
					t.Errorf("discoveryTimeout() error = %v, want timeout", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("discoveryTimeout() error = %v", err)
			}
			if got <= 0 || got > tt.want {
				t.Errorf("discoveryTimeout() = %v, want positive duration at most %v", got, tt.want)
			}
		})
	}
}

func TestCollectorContextTransport_ContextLivesThroughResponseBody(t *testing.T) {
	collectorCtx := t.Context()

	var requestCtx context.Context
	transport := &collectorContextTransport{
		ctx: collectorCtx,
		base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			requestCtx = request.Context()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		}),
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	select {
	case <-requestCtx.Done():
		t.Fatal("request context canceled before response body was consumed")
	default:
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("response body read error = %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("response body = %q, want %q", body, "ok")
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatalf("response body close error = %v", closeErr)
	}
	select {
	case <-requestCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("request context remained live after response body close")
	}

	var nilBodyRequestCtx context.Context
	nilBodyTransport := &collectorContextTransport{
		ctx: collectorCtx,
		base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			nilBodyRequestCtx = request.Context()
			return &http.Response{StatusCode: http.StatusNoContent}, nil
		}),
	}
	nilBodyResponse, err := nilBodyTransport.RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if nilBodyResponse.Body != nil {
		if closeErr := nilBodyResponse.Body.Close(); closeErr != nil {
			t.Errorf("response body close error = %v", closeErr)
		}
		t.Error("response body is non-nil, want nil")
	}
	select {
	case <-nilBodyRequestCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("request context remained live without a response body")
	}
}

func TestKubernetesCollector_BlockedDiscoveryHonorsDeadline(t *testing.T) {
	t.Setenv("NODE_NAME", "test-node")

	var startedOnce sync.Once
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)

	config := &rest.Config{Host: server.URL}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to create Kubernetes client: %v", err)
	}
	collector := &Collector{
		ClientSet:  clientset,
		RestConfig: config,
	}

	ctx, cancel := context.WithTimeout(context.Background(), discoveryCancelTimeout)
	defer cancel()
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, collectErr := collector.Collect(ctx)
		done <- result{err: collectErr}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("collector did not issue a Kubernetes API request")
	}

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("Collect() error = nil, want deadline error")
		}
		if !errors.IsTransient(got.err) {
			t.Errorf("Collect() error = %v, want transient timeout", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Collect() did not stop after its context deadline")
	}
}

func TestKubernetesCollector_CustomResourceCancellationFailsCollection(t *testing.T) {
	t.Setenv("NODE_NAME", "test-node")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	collector := createTestCollector()
	collector.slinkyDiscovery = &deadlineDiscovery{
		done:    ctx.Done(),
		started: started,
	}

	type result struct {
		measurementPresent bool
		err                error
	}
	done := make(chan result, 1)
	go func() {
		measurement, collectErr := collector.Collect(ctx)
		done <- result{
			measurementPresent: measurement != nil,
			err:                collectErr,
		}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Slinky discovery did not start")
	}
	cancel()
	select {
	case got := <-done:
		if got.measurementPresent {
			t.Error("Collect() measurement is non-nil, want nil after deadline")
		}
		if got.err == nil {
			t.Fatal("Collect() error = nil, want deadline error")
		}
		if !errors.IsTransient(got.err) {
			t.Errorf("Collect() error = %v, want transient timeout", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Collect() did not stop after custom-resource discovery deadline")
	}
}

func newDiscoveryServer(t *testing.T, blockPath string, started chan<- struct{}) *httptest.Server {
	t.Helper()

	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == blockPath {
			if started != nil {
				startedOnce.Do(func() {
					close(started)
				})
			}
			<-request.Context().Done()
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		var body string
		switch request.URL.Path {
		case "/api":
			body = `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`
		case "/apis":
			body = `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"example.test","versions":[{"groupVersion":"example.test/v1","version":"v1"}],"preferredVersion":{"groupVersion":"example.test/v1","version":"v1"}}]}`
		case "/api/v1":
			body = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[]}`
		case "/apis/" + testDiscoveryGroupVersion:
			body = `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"example.test/v1","resources":[{"name":"examples","kind":"Example","namespaced":true,"verbs":["get","list"]}]}`
		case "/version":
			body = `{"major":"1","minor":"33","gitVersion":"v1.33.0","platform":"linux/amd64","goVersion":"go1.26.0"}`
		default:
			http.NotFound(writer, request)
			return
		}
		if _, err := writer.Write([]byte(body)); err != nil {
			t.Errorf("failed to write discovery response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type deadlineDiscovery struct {
	done    <-chan struct{}
	started chan<- struct{}
}

func (d *deadlineDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	close(d.started)
	<-d.done
	return nil, context.DeadlineExceeded
}

func (d *deadlineDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return nil, context.DeadlineExceeded
}

func contextWithDeadline(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func expiredContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	cancel()
	t.Cleanup(cancel)
	return ctx
}

// TestCancellationErr pins the invariant that motivated cancellationErr: a
// cancellation must be reported even when only an *outer* context in the chain
// has observed it yet.
//
// The race this guards against is timing-dependent and therefore cannot be
// reproduced reliably in a test — parent-to-child propagation loses the race
// roughly 1 run in 10,000. So rather than stress the scheduler, the cases below
// encode the resulting states directly. The "caller canceled, derived still
// live" case is exactly the mid-propagation snapshot that made
// TestKubernetesCollector_CustomResourceCancellationFailsCollection flaky in CI.
func TestCancellationErr(t *testing.T) {
	live := context.Background()

	tests := []struct {
		name    string
		ctxs    []context.Context
		wantErr bool
	}{
		{"all live", []context.Context{live, live, live}, false},
		{"caller cancelled, derived still live", []context.Context{live, live, expiredContext(t)}, true},
		{"middle cancelled only", []context.Context{live, expiredContext(t), live}, true},
		{"errgroup context cancelled only (sibling failure)", []context.Context{expiredContext(t), live, live}, true},
		{"whole chain cancelled", []context.Context{expiredContext(t), expiredContext(t), expiredContext(t)}, true},
		{"no contexts", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cancellationErr(tt.ctxs...)
			if (err != nil) != tt.wantErr {
				t.Errorf("cancellationErr() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
