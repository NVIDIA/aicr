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

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// newTestHandler builds a recipeHandler backed by an embedded-source client.
// The optional allowLists fences criteria values; pass nil to allow all.
func newTestHandler(t *testing.T, allowLists *aicr.AllowLists) *recipeHandler {
	t.Helper()
	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion("test"),
		aicr.WithAllowLists(allowLists),
	)
	if err != nil {
		t.Fatalf("failed to construct aicr client: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("client close failed: %v", closeErr)
		}
	})
	return newRecipeHandler(client, allowLists)
}

// TestHandleRecipes_Success verifies GET and POST resolve a recipe with a 200
// status and a Cache-Control header.
func TestHandleRecipes_Success(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		contentType string
	}{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
				req.Header.Set("Content-Type", tt.contentType)
			} else {
				req = httptest.NewRequest(tt.method, tt.target, nil)
			}
			w := httptest.NewRecorder()

			h.HandleQuery(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); cc == "" {
				t.Error("expected Cache-Control header to be set")
			}
			// The selected value should be a non-empty JSON scalar (the
			// driver version string).
			var selected any
			if err := json.Unmarshal(w.Body.Bytes(), &selected); err != nil {
				t.Fatalf("failed to decode selected value: %v; body: %s", err, w.Body.String())
			}
			if s, ok := selected.(string); !ok || s == "" {
				t.Errorf("expected non-empty string selected value, got %v (%T)", selected, selected)
			}
		})
	}
}

// TestHandleQuery_POSTCriteriaTakesEffect proves the facade-backed query POST
// resolves criteria from the flat body — i.e. the POST criteria actually take
// effect rather than unmarshalling to empty criteria.
func TestHandleQuery_POSTCriteriaTakesEffect(t *testing.T) {
	const body = `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},"selector":"components.gpu-operator.values.driver.version"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newTestHandler(t, nil).HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var selected string
	if err := json.Unmarshal(w.Body.Bytes(), &selected); err != nil || selected == "" {
		t.Fatalf("expected non-empty resolved driver version, got %q (err %v)", w.Body.String(), err)
	}
}

// TestHandleQuery_SelectorNotFound verifies a missing selector path returns 404
// (not a 5xx), preserving the legacy handler's hydrate-vs-select error split.
func TestHandleQuery_SelectorNotFound(t *testing.T) {
	h := newTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet,
		"/v1/query?service=eks&accelerator=h100&intent=training&selector=components.does.not.exist", nil)
	w := httptest.NewRecorder()

	h.HandleQuery(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// TestHandleQuery_SelectorPresence verifies an omitted selector is rejected
// while an explicitly empty selector returns the entire hydrated recipe.
func TestHandleQuery_SelectorPresence(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name       string
		target     string
		wantStatus int
		wantError  string
	}{
		{
			name:       "missing selector",
			target:     "/v1/query?service=eks&accelerator=h100&intent=training",
			wantStatus: http.StatusBadRequest,
			wantError:  "[INVALID_REQUEST] selector is required on /v1/query",
		},
		{
			name:       "explicitly empty selector",
			target:     "/v1/query?service=eks&accelerator=h100&intent=training&selector=",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			w := httptest.NewRecorder()

			h.HandleQuery(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantError != "" {
				errResp := decodeErrorBody(t, w.Body.Bytes())
				if errResp.Code != "INVALID_REQUEST" {
					t.Errorf("code = %q, want INVALID_REQUEST", errResp.Code)
				}
				if got := errResp.Details[keyError]; got != tt.wantError {
					t.Errorf("details.error = %q, want %q", got, tt.wantError)
				}
				return
			}

			var hydrated map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &hydrated); err != nil {
				t.Fatalf("failed to decode hydrated recipe: %v; body: %s", err, w.Body.String())
			}
			if _, ok := hydrated["components"]; !ok {
				t.Errorf("expected hydrated recipe to contain a components key; got keys %v", keysOf(hydrated))
			}
		})
	}
}

// TestHandleQuery_MethodNotAllowed verifies non GET/POST returns 405 with an
// Allow header.
// TestHandleRecipes_EmptyCriteriaRejected verifies that GET and POST /v1/recipe
// with no criteria dimensions (Specificity()==0) return 400 "no criteria provided"
// rather than silently resolving to the base recipe.
func TestHandleRecipes_EmptyCriteriaRejected(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{
			name:   "GET with no criteria params",
			method: http.MethodGet,
			target: "/v1/recipe",
		},
		{
			name:   "POST with empty criteria object",
			method: http.MethodPost,
			target: "/v1/recipe",
			body:   `{"criteria":{}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tt.method, tt.target, nil)
			}
			w := httptest.NewRecorder()
			h.HandleRecipes(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "no criteria provided") {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), "no criteria provided")
			}
		})
	}
}

// TestHandleQuery_EmptyCriteriaRejected is a regression test that verifies
// /v1/query applies the same minimum-specificity guard as /v1/recipe and the
// CLI. A request with no criteria dimensions (Specificity()==0) must return
// 400 rather than silently resolving and returning the base recipe's value.
func TestHandleQuery_EmptyCriteriaRejected(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{
			name:   "GET with only selector — no criteria",
			method: http.MethodGet,
			target: "/v1/query?selector=components.gpu-operator.values.driver.version",
		},
		{
			name:   "POST with empty criteria object",
			method: http.MethodPost,
			target: "/v1/query",
			body:   `{"criteria":{},"selector":"components.gpu-operator.values.driver.version"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tt.method, tt.target, nil)
			}
			w := httptest.NewRecorder()
			h.HandleQuery(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "no criteria provided") {
				t.Errorf("body = %q, want it to contain %q", w.Body.String(), "no criteria provided")
			}
		})
	}
}

func TestHandleQuery_MethodNotAllowed(t *testing.T) {
	h := newTestHandler(t, nil)

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/query", nil)
			w := httptest.NewRecorder()

			h.HandleQuery(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			if allow := w.Header().Get("Allow"); allow != "GET, POST" {
				t.Errorf("Allow header = %q, want %q", allow, "GET, POST")
			}
		})
	}
}

// TestHandleQuery_AllowListRejection verifies query enforces allowlists with the
// same message as the legacy handler.
func TestHandleQuery_AllowListRejection(t *testing.T) {
	facadeAllow := &aicr.AllowLists{
		Accelerators: []string{string(recipe.CriteriaAcceleratorA100)},
	}
	h := newTestHandler(t, facadeAllow)

	const target = "/v1/query?accelerator=h100&intent=training&selector=components.gpu-operator"

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	h.HandleQuery(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	errResp := decodeErrorBody(t, w.Body.Bytes())
	if errResp.Message != "accelerator type not allowed" {
		t.Errorf("message = %q, want %q", errResp.Message, "accelerator type not allowed")
	}
}

// errBody is the subset of the structured error response used for parity asserts.
type errBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func decodeErrorBody(t *testing.T, b []byte) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("failed to decode error body: %v; body: %s", err, string(b))
	}
	return e
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
