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

package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr"
	"github.com/NVIDIA/aicr/pkg/bundler"
)

// errCodeMessage decodes a structured-error JSON response body and returns
// its code and message, ignoring the per-request requestId/timestamp fields.
func errCodeMessage(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	return resp.Code, resp.Message
}

// validBundleRecipe is a hydrated RecipeResult POST body with a single
// resolvable component, sufficient to drive a full bundle generation.
const validBundleRecipe = `{
	"apiVersion": "aicr.nvidia.com/v1alpha1",
	"kind": "Recipe",
	"metadata": {
		"version": "v1.0.0",
		"appliedOverlays": ["base", "eks", "eks-training"]
	},
	"criteria": {
		"service": "eks",
		"accelerator": "h100",
		"intent": "training"
	},
	"componentRefs": [
		{
			"name": "gpu-operator",
			"version": "v25.3.3",
			"type": "helm",
			"source": "https://helm.ngc.nvidia.com/nvidia",
			"valuesFile": "components/gpu-operator/values.yaml"
		}
	]
}`

func newTestBundleHandler(t *testing.T) *bundleHandler {
	t.Helper()
	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return newBundleHandler(client, nil)
}

// zipEntryNames returns the sorted entry names from a zip archive body.
func zipEntryNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

// TestBundleHandler_MethodGate verifies only POST is accepted, matching the
// legacy handler.
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
// rejected with 400, matching the legacy handler.
func TestBundleHandler_EmptyComponentRefs(t *testing.T) {
	t.Parallel()
	h := newTestBundleHandler(t)

	body := `{"apiVersion": "aicr.nvidia.com/v1alpha1", "kind": "Recipe", "componentRefs": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d. Body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestBundleHandler_ParityWithLegacy is the criterion-#2 gate: the
// aicr.Client-backed /v1/bundle handler must produce the SAME HTTP response
// (status, content headers, zip entry layout) as the legacy
// bundler.(*DefaultBundler).HandleBundles for an identical POST.
func TestBundleHandler_ParityWithLegacy(t *testing.T) {
	t.Parallel()

	queries := []string{
		"",
		"deployer=argocd",
		"deployer=argocd-helm",
		"set=gpuoperator:gds.enabled=true",
	}

	for _, q := range queries {
		t.Run("query="+q, func(t *testing.T) {
			t.Parallel()

			target := "/v1/bundle"
			if q != "" {
				target += "?" + q
			}

			// Legacy path: direct DefaultBundler.
			legacy, err := bundler.New()
			if err != nil {
				t.Fatalf("bundler.New: %v", err)
			}
			legacyReq := httptest.NewRequest(http.MethodPost, target, strings.NewReader(validBundleRecipe))
			legacyReq.Header.Set("Content-Type", "application/json")
			legacyW := httptest.NewRecorder()
			legacy.HandleBundles(legacyW, legacyReq)

			// Facade path: aicr.Client-backed handler.
			h := newTestBundleHandler(t)
			facadeReq := httptest.NewRequest(http.MethodPost, target, strings.NewReader(validBundleRecipe))
			facadeReq.Header.Set("Content-Type", "application/json")
			facadeW := httptest.NewRecorder()
			h.HandleBundles(facadeW, facadeReq)

			if legacyW.Code != facadeW.Code {
				t.Fatalf("status mismatch: legacy=%d facade=%d. legacyBody=%s facadeBody=%s",
					legacyW.Code, facadeW.Code, legacyW.Body.String(), facadeW.Body.String())
			}
			// A non-200 outcome is still a valid parity case (a deployer that
			// rejects this minimal recipe must reject it the same way in both
			// paths); require the structured error code+message to match
			// (requestId/timestamp are per-request and intentionally ignored).
			if legacyW.Code != http.StatusOK {
				lc, lm := errCodeMessage(t, legacyW.Body.Bytes())
				fc, fm := errCodeMessage(t, facadeW.Body.Bytes())
				if lc != fc || lm != fm {
					t.Errorf("error mismatch: legacy=(%s,%q) facade=(%s,%q)", lc, lm, fc, fm)
				}
				return
			}

			// Content headers must match (X-Bundle-Duration is timing-dependent,
			// X-Bundle-Size can vary by a few bytes from non-deterministic
			// content — compare the stable headers only).
			for _, hdr := range []string{"Content-Type", "Content-Disposition"} {
				if l, f := legacyW.Header().Get(hdr), facadeW.Header().Get(hdr); l != f {
					t.Errorf("%s mismatch: legacy=%q facade=%q", hdr, l, f)
				}
			}

			// Zip entry layout must match exactly.
			legacyNames := zipEntryNames(t, legacyW.Body.Bytes())
			facadeNames := zipEntryNames(t, facadeW.Body.Bytes())
			if len(legacyNames) != len(facadeNames) {
				t.Fatalf("zip entry count mismatch: legacy=%v facade=%v", legacyNames, facadeNames)
			}
			for i := range legacyNames {
				if legacyNames[i] != facadeNames[i] {
					t.Errorf("zip entry %d mismatch: legacy=%q facade=%q", i, legacyNames[i], facadeNames[i])
				}
			}
		})
	}
}
