// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package recipe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleQuery_MethodNotAllowed(t *testing.T) {
	builder := NewBuilder()

	methods := []string{http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/query", nil)
			w := httptest.NewRecorder()

			builder.HandleQuery(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			allow := w.Header().Get("Allow")
			if allow != "GET, POST" {
				t.Errorf("Allow header = %q, want %q", allow, "GET, POST")
			}
		})
	}
}

func TestHandleQuery_GET_WithSelector(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodGet, "/v1/query?service=eks&selector=criteria.service", nil)
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleQuery_GET_InvalidSelector(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodGet, "/v1/query?service=eks&selector=nonexistent.deep.path", nil)
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandleQuery_POST_EmptyBody(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleQuery_POST_ValidJSON(t *testing.T) {
	builder := NewBuilder()

	body := `{"criteria":{"service":"eks"},"selector":"criteria.service"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleQuery_POST_InvalidJSON(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader("{invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleQuery_GET_EmptySelector(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodGet, "/v1/query?service=eks&selector=", nil)
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	// Empty selector returns entire hydrated recipe
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleQuery_GET_NoCriteria(t *testing.T) {
	builder := NewBuilder()

	// No criteria params at all — should still work (returns default recipe)
	req := httptest.NewRequest(http.MethodGet, "/v1/query?selector=kind", nil)
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleQuery_POST_ValidYAML(t *testing.T) {
	builder := NewBuilder()

	body := "criteria:\n  service: eks\nselector: criteria.service\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleQuery_POST_NilCriteria(t *testing.T) {
	builder := NewBuilder()

	body := `{"selector":"criteria.service"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleQuery_GET_CacheHeaders(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodGet, "/v1/query?service=eks&selector=criteria.service", nil)
	w := httptest.NewRecorder()

	builder.HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	cc := w.Header().Get("Cache-Control")
	if cc == "" {
		t.Error("expected Cache-Control header to be set")
	}
}

func TestParseQueryRequestFromBody_NilBody(t *testing.T) {
	_, err := parseQueryRequestFromBody(nil, "application/json")
	if err == nil {
		t.Fatal("expected error for nil body")
	}
}
