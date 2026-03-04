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

func TestHandleRecipes_MethodNotAllowed(t *testing.T) {
	builder := NewBuilder()

	methods := []string{http.MethodPut, http.MethodDelete, http.MethodPatch}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/recipe", nil)
			w := httptest.NewRecorder()

			builder.HandleRecipes(w, req)

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

func TestHandleRecipes_GET_DefaultCriteria(t *testing.T) {
	builder := NewBuilder()

	// GET with no params returns default recipe (not an error)
	req := httptest.NewRequest(http.MethodGet, "/v1/recipe", nil)
	w := httptest.NewRecorder()

	builder.HandleRecipes(w, req)

	// Should succeed — empty GET returns default criteria
	if w.Code == http.StatusMethodNotAllowed {
		t.Errorf("unexpected 405 for GET request")
	}
}

func TestHandleRecipes_POST_EmptyBody(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodPost, "/v1/recipe", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleRecipes(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRecipes_POST_InvalidJSON(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodPost, "/v1/recipe", strings.NewReader("{invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleRecipes(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleRecipes_GET_ValidCriteria(t *testing.T) {
	builder := NewBuilder()

	req := httptest.NewRequest(http.MethodGet,
		"/v1/recipe?service=eks&accelerator=h100&intent=training", nil)
	w := httptest.NewRecorder()

	builder.HandleRecipes(w, req)

	// Should succeed (200) or return an error from BuildFromCriteria,
	// but should NOT be 400 or 405
	if w.Code == http.StatusMethodNotAllowed || w.Code == http.StatusBadRequest {
		t.Errorf("unexpected status %d for valid criteria", w.Code)
	}

	// Verify cache header is set on success
	if w.Code == http.StatusOK {
		cc := w.Header().Get("Cache-Control")
		if cc == "" {
			t.Error("Cache-Control header not set on successful response")
		}
	}
}
