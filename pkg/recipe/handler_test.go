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

package recipe

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
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

// TestHandleRecipes_POST_BodyTooLarge verifies that a POST body exceeding
// defaults.MaxRecipePOSTBytes is rejected with HTTP 413 and a structured
// INVALID_REQUEST error code carrying the exact configured cap.
//
// Uses a valid JSON envelope wrapping a giant string so io.ReadAll inside
// the handler reaches the MaxBytesReader limit; this exercises the
// *http.MaxBytesError detection path deterministically rather than relying
// on early JSON syntax errors.
func TestHandleRecipes_POST_BodyTooLarge(t *testing.T) {
	builder := NewBuilder()

	oversize := int(defaults.MaxRecipePOSTBytes) + 1024
	prefix := `{"service":"`
	suffix := `"}`
	padding := strings.Repeat("a", oversize-len(prefix)-len(suffix))
	body := prefix + padding + suffix

	req := httptest.NewRequest(http.MethodPost, "/v1/recipe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	builder.HandleRecipes(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d. Body: %s",
			w.Code, http.StatusRequestEntityTooLarge, w.Body.String())
	}

	var resp struct {
		Code    string `json:"code"`
		Details struct {
			LimitBytes int64 `json:"limit_bytes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if resp.Code != string(aicrerrors.ErrCodeInvalidRequest) {
		t.Errorf("error code = %q, want %q", resp.Code, aicrerrors.ErrCodeInvalidRequest)
	}
	if resp.Details.LimitBytes != defaults.MaxRecipePOSTBytes {
		t.Errorf("limit_bytes = %d, want %d", resp.Details.LimitBytes, defaults.MaxRecipePOSTBytes)
	}
}
