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
// status and a Cache-Control header, and that both methods return the same
// resolved recipe for equivalent criteria.
func TestHandleRecipes_Success(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		contentType string
	}{
		{
			name:   "GET with criteria query parameters",
			method: http.MethodGet,
			target: "/v1/recipe?service=eks&accelerator=h100&intent=training",
		},
		{
			name:        "POST with strict criteria envelope",
			method:      http.MethodPost,
			target:      "/v1/recipe",
			body:        `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"}}`,
			contentType: "application/json",
		},
	}

	bodies := make(map[string]string, len(tests))

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

			h.HandleRecipes(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}
			if cc := w.Header().Get("Cache-Control"); cc == "" {
				t.Error("expected Cache-Control header to be set")
			}

			var got recipe.RecipeResult
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("failed to decode recipe result: %v; body: %s", err, w.Body.String())
			}
			if got.Kind != recipe.RecipeResultKind {
				t.Errorf("kind = %q, want %q", got.Kind, recipe.RecipeResultKind)
			}
			if len(got.ComponentRefs) == 0 {
				t.Error("expected resolved recipe to carry at least one component")
			}

			bodies[tt.method] = w.Body.String()
		})
	}

	// Asserting each response in isolation would still pass if POST resolved
	// different criteria than GET, which is the regression the two transports
	// exist to rule out. RecipeResult carries no timestamp or request ID, so
	// two equivalent requests must resolve to the same document.
	if len(bodies) != len(tests) {
		t.Fatalf("collected %d responses from %d cases; a subtest failed before "+
			"recording, so the cross-method comparison below is meaningless",
			len(bodies), len(tests))
	}

	// Compared byte-for-byte. This used to route through a helper that stripped
	// "any" from the echoed criteria, because GET emitted those dimensions and
	// POST omitted them. That asymmetry is gone, so the crutch is too -- and
	// its removal is the assertion that it stayed gone.
	if bodies[http.MethodGet] != bodies[http.MethodPost] {
		t.Errorf("GET and POST returned different documents for equivalent criteria\nGET:  %s\nPOST: %s",
			bodies[http.MethodGet], bodies[http.MethodPost])
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

// TestHandleQuery_MethodNotAllowed verifies non GET/POST returns 405 with an
// Allow header.
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
			if allow := w.Header().Get("Allow"); allow != "GET, HEAD, POST" {
				t.Errorf("Allow header = %q, want %q", allow, "GET, HEAD, POST")
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

// TestHandleRecipes_HeadMatchesGet asserts HEAD is accepted and produces the
// same status and headers as GET.
//
// Rejecting HEAD while accepting GET contradicted RFC 9110 section 9.1 and
// /metrics, which has allowed the pair since readOnly() was introduced. It also
// made two documented `curl -I` examples return 405, which is how it was found.
func TestHandleRecipes_HeadMatchesGet(t *testing.T) {
	h := newTestHandler(t, nil)
	const target = "/v1/recipe?service=eks&accelerator=h100&intent=training"

	responses := map[string]*httptest.ResponseRecorder{}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		h.HandleRecipes(w, httptest.NewRequest(method, target, nil))
		responses[method] = w

		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d; body: %s",
				method, w.Code, http.StatusOK, w.Body.String())
		}
	}

	for _, header := range []string{"Content-Type", "Cache-Control"} {
		get := responses[http.MethodGet].Header().Get(header)
		head := responses[http.MethodHead].Header().Get(header)
		if get != head {
			t.Errorf("%s header: GET=%q HEAD=%q; HEAD must describe the response "+
				"GET would return", header, get, head)
		}
	}
}

// TestHandleQuery_HeadIsAccepted covers the same for the query endpoint.
func TestHandleQuery_HeadIsAccepted(t *testing.T) {
	h := newTestHandler(t, nil)
	const target = "/v1/query?service=eks&accelerator=h100&intent=training" +
		"&selector=components.gpu-operator.values.driver.version"

	w := httptest.NewRecorder()
	h.HandleQuery(w, httptest.NewRequest(http.MethodHead, target, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d; body: %s",
			w.Code, http.StatusOK, w.Body.String())
	}
}

// TestRecipeHandlers_AllowHeaderNamesHead asserts a rejected method advertises
// HEAD, so a client reading Allow is not told to use a method the server now
// accepts.
func TestRecipeHandlers_AllowHeaderNamesHead(t *testing.T) {
	h := newTestHandler(t, nil)

	for name, handler := range map[string]http.HandlerFunc{
		"/v1/recipe": h.HandleRecipes,
		"/v1/query":  h.HandleQuery,
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(http.MethodDelete, name, nil))

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			if allow := w.Header().Get("Allow"); allow != "GET, HEAD, POST" {
				t.Errorf("Allow = %q, want %q", allow, "GET, HEAD, POST")
			}
		})
	}
}
