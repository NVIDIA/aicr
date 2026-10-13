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

package client

import (
	"context"
	stderrors "errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

var testTopologyGVR = schema.GroupVersionResource{Group: "grove.io", Version: "v1alpha1", Resource: "clustertopologies"}

// sentinel stands in for spec and Secret data a full object list would carry.
// No error may quote it.
const sentinel = "c2VjcmV0LXNlbnRpbmVs"

const validMetadataList = `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1",` +
	`"metadata":{"continue":"next-page"},"items":[{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",` +
	`"metadata":{"name":"topology-1","namespace":"tenant-a","labels":{"app":"grove"}}}]}`

// metadataListServer returns a MetadataListClient for an httptest server
// running handler.
func metadataListServer(t *testing.T, handler http.HandlerFunc) *MetadataListClient {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewMetadataListClientForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("NewMetadataListClientForConfig: %v", err)
	}

	return c
}

func writeBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestNewMetadataListClientForConfig(t *testing.T) {
	t.Parallel()

	t.Run("nil config is rejected", func(t *testing.T) {
		t.Parallel()

		got, err := NewMetadataListClientForConfig(nil)
		if got != nil {
			t.Errorf("client = %v, want nil on error", got)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("a config with no timeout gets the bounded one", func(t *testing.T) {
		t.Parallel()

		got, err := NewMetadataListClientForConfig(&rest.Config{Host: "https://127.0.0.1:6443"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.client.Client.Timeout != defaults.K8sClientRequestTimeout {
			t.Errorf("request timeout = %v, want %v", got.client.Client.Timeout, defaults.K8sClientRequestTimeout)
		}
		if got.limit != defaults.MetadataListResponseLimit {
			t.Errorf("limit = %d, want %d", got.limit, defaults.MetadataListResponseLimit)
		}
		if got.retries != defaults.MetadataListRetries || got.retryInterval != defaults.MetadataListRetryInterval {
			t.Errorf("retries = %d every %v, want %d every %v", got.retries, got.retryInterval,
				defaults.MetadataListRetries, defaults.MetadataListRetryInterval)
		}
	})
}

// TestMetadataListClientAdvertisesOnlyMetadataLists checks every media type
// the Accept header offers. A bare application/json among them lets the
// apiserver answer with full objects.
func TestMetadataListClientAdvertisesOnlyMetadataLists(t *testing.T) {
	t.Parallel()

	accepts := make(chan string, 1)
	c := metadataListServer(t, func(w http.ResponseWriter, r *http.Request) {
		accepts <- r.Header.Get("Accept")
		writeBody(w, http.StatusOK, validMetadataList)
	})
	if _, err := c.List(t.Context(), testTopologyGVR, metav1.ListOptions{}); err != nil {
		t.Fatalf("List: %v", err)
	}

	var accept string
	select {
	case accept = <-accepts:
	default:
		t.Fatal("List sent no request")
	}
	if accept == "" {
		t.Fatal("Accept header is empty, which the apiserver reads as any representation")
	}
	for _, offered := range strings.Split(accept, ",") {
		mediaType, params, err := mime.ParseMediaType(offered)
		if err != nil {
			t.Fatalf("Accept entry %q does not parse: %v", offered, err)
		}
		if params["as"] != "PartialObjectMetadataList" || params["g"] != "meta.k8s.io" || params["v"] != "v1" {
			t.Errorf("Accept entry %q offers %s as something other than a meta.k8s.io/v1 "+
				"PartialObjectMetadataList, full Accept %q", offered, mediaType, accept)
		}
	}
}

func TestMetadataListClientList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		gvr      schema.GroupVersionResource
		wantPath string
	}{
		{"a core resource", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "/api/v1/configmaps"},
		{"a group resource", testTopologyGVR, "/apis/grove.io/v1alpha1/clustertopologies"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requests := make(chan *url.URL, 1)
			c := metadataListServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL
				writeBody(w, http.StatusOK, validMetadataList)
			})

			list, err := c.List(t.Context(), tt.gvr, metav1.ListOptions{Limit: 200, Continue: "this-page"})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var requested *url.URL
			select {
			case requested = <-requests:
			default:
				t.Fatal("List sent no request")
			}
			gotPath := requested.Path
			gotLimit := requested.Query().Get("limit")
			gotContinue := requested.Query().Get("continue")
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q, a cluster-wide List", gotPath, tt.wantPath)
			}
			if gotLimit != "200" || gotContinue != "this-page" {
				t.Errorf("limit = %q, continue = %q, want 200 and this-page", gotLimit, gotContinue)
			}
			if list.Continue != "next-page" {
				t.Errorf("Continue = %q, want next-page", list.Continue)
			}
			if len(list.Items) != 1 || list.Items[0].Name != "topology-1" || list.Items[0].Namespace != "tenant-a" ||
				list.Items[0].Labels["app"] != "grove" {

				t.Errorf("Items = %#v, want tenant-a/topology-1 with its labels", list.Items)
			}
		})
	}
}

// TestMetadataListClientRejects covers every response that is not a
// meta.k8s.io/v1 PartialObjectMetadataList. client-go's metadata client
// decodes most of these into one, keeping whatever metadata they carry.
func TestMetadataListClientRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		status            int
		body              string
		wantNotAcceptable bool
	}{
		{
			name:   "a full object list with specs",
			status: http.StatusOK,
			body: `{"kind":"ClusterTopologyList","apiVersion":"grove.io/v1alpha1","items":[` +
				`{"metadata":{"name":"topology-1"},"spec":{"token":"` + sentinel + `"}}]}`,
		},
		{
			name:   "a full Secret list",
			status: http.StatusOK,
			body: `{"kind":"SecretList","apiVersion":"v1","items":[` +
				`{"metadata":{"name":"creds","namespace":"tenant-a"},"data":{"password":"` + sentinel + `"}}]}`,
		},
		{
			name:   "a list with no kind",
			status: http.StatusOK,
			body:   `{"items":[{"metadata":{"name":"topology-1"},"spec":{"token":"` + sentinel + `"}}]}`,
		},
		{
			name:   "a single PartialObjectMetadata",
			status: http.StatusOK,
			body:   `{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{"name":"topology-1"}}`,
		},
		{
			name:   "a metadata list in another version",
			status: http.StatusOK,
			body:   `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1beta1","items":[]}`,
		},
		{
			name:   "a body that is not JSON",
			status: http.StatusOK,
			body:   "k8s\x00\n\x0fv1\x12\nSecretList" + sentinel,
		},
		{
			name:   "a 406",
			status: http.StatusNotAcceptable,
			body: `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotAcceptable","code":406,` +
				`"message":"only the following media types are accepted: application/json"}`,
			wantNotAcceptable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := metadataListServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeBody(w, tt.status, tt.body)
			})

			list, err := c.List(t.Context(), testTopologyGVR, metav1.ListOptions{})
			if list != nil {
				t.Errorf("List returned %#v, want nothing from a response it rejects", list)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
			if err != nil && strings.Contains(err.Error(), sentinel) {
				t.Errorf("error = %q, which quotes the response body", err)
			}
			if tt.wantNotAcceptable && !apierrors.IsNotAcceptable(err) {
				t.Errorf("error = %v, want the 406 kept in its chain", err)
			}
		})
	}
}

func TestMetadataListClientRejectsAnOversizedList(t *testing.T) {
	t.Parallel()

	c := metadataListServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeBody(w, http.StatusOK, validMetadataList)
	})
	c.limit = int64(len(validMetadataList)) - 1

	list, err := c.List(t.Context(), testTopologyGVR, metav1.ListOptions{})
	if list != nil {
		t.Errorf("List returned %#v, want nothing from a truncated read", list)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
	if err != nil && !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to name the size limit", err)
	}
}

// TestMetadataListClientKeepsContextErrors checks that an aborted request
// reports the abort, so a caller can tell an operator's Ctrl-C from an
// unavailable apiserver.
func TestMetadataListClientKeepsContextErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ctx      func(t *testing.T) context.Context
		wantErr  error
		wantCode errors.ErrorCode
	}{
		{
			name: "deadline",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				t.Cleanup(cancel)

				return ctx
			},
			wantErr:  context.DeadlineExceeded,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			name: "canceled",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				time.AfterFunc(50*time.Millisecond, cancel)
				t.Cleanup(cancel)

				return ctx
			},
			wantErr:  context.Canceled,
			wantCode: errors.ErrCodeCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := metadataListServer(t, func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			})

			_, err := c.List(tt.ctx(t), testTopologyGVR, metav1.ListOptions{})
			if !stderrors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want it to chain %v", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

// dropConnection closes w's connection without writing a response.
func dropConnection(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("Hijack: %v", err)
		return
	}
	_ = conn.Close()
}

func TestMetadataListClientRetriesADroppedConnection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		drops        int32
		retries      int
		status       int
		wantRequests int32
		wantCode     errors.ErrorCode
	}{
		{"one drop is retried", 1, 2, http.StatusOK, 2, ""},
		{"drops past the budget fail", 5, 2, http.StatusOK, 3, errors.ErrCodeUnavailable},
		{"an error response is not retried", 0, 2, http.StatusForbidden, 1, errors.ErrCodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			c := metadataListServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) <= tt.drops {
					dropConnection(t, w)
					return
				}
				writeBody(w, tt.status, validMetadataList)
			})
			c.retries = tt.retries
			c.retryInterval = time.Millisecond

			list, err := c.List(t.Context(), testTopologyGVR, metav1.ListOptions{})
			if got := requests.Load(); got != tt.wantRequests {
				t.Errorf("requests = %d, want %d", got, tt.wantRequests)
			}
			if tt.wantCode != "" {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list.Items) != 1 || list.Items[0].Name != "topology-1" {
				t.Errorf("Items = %#v, want topology-1 from the retried request", list.Items)
			}
		})
	}
}

func TestMetadataListClientStopsRetryingOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var requests atomic.Int32
	c := metadataListServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		dropConnection(t, w)
		cancel()
	})
	c.retryInterval = time.Hour

	_, err := c.List(ctx, testTopologyGVR, metav1.ListOptions{})
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to chain context.Canceled", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1, with no retry after the cancel", got)
	}
}

func TestMetadataListClientRejectsAnIncompleteResource(t *testing.T) {
	t.Parallel()

	c, err := NewMetadataListClientForConfig(&rest.Config{Host: "https://127.0.0.1:6443"})
	if err != nil {
		t.Fatalf("NewMetadataListClientForConfig: %v", err)
	}
	for _, gvr := range []schema.GroupVersionResource{
		{Group: "grove.io", Resource: "clustertopologies"},
		{Group: "grove.io", Version: "v1alpha1"},
	} {
		_, err := c.List(t.Context(), gvr, metav1.ListOptions{})
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	}
}
