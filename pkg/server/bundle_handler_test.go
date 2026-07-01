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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

func newTestBundleHandler(t *testing.T) *bundleHandler {
	t.Helper()
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return newBundleHandler(client, nil)
}

// TestBundleHandler_MethodGate verifies only POST is accepted.
func TestBundleHandler_MethodGate(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(method, "/v1/bundle", nil)
			w := httptest.NewRecorder()
			h.HandleBundles(w, req)
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			if allow := w.Header().Get("Allow"); allow != http.MethodPost {
				t.Errorf("Allow = %q, want %q", allow, http.MethodPost)
			}
		})
	}
}

// TestBundleHandler_EmptyComponentRefs verifies a recipe with no components is
// rejected with 400.
func TestBundleHandler_EmptyComponentRefs(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	body := `{"apiVersion": "aicr.run/v1alpha2", "kind": "Recipe", "componentRefs": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestBundleHandler_IncoherentComponentRef verifies the HTTP decode-to-bundle
// path rejects an incoherent ref (a Helm component carrying a Kustomize tag)
// with 400 rather than producing a mismatched bundle. Pins issue #1584 at the
// POST /v1/bundle boundary.
func TestBundleHandler_IncoherentComponentRef(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	body := `{"apiVersion": "aicr.run/v1alpha2", "kind": "Recipe", "componentRefs": [` +
		`{"name": "gpu-operator", "type": "Helm", "version": "v1", "tag": "v2"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}
