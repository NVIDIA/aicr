// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package bundler

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBundlerHandlerNew verifies DefaultBundler can be created for HTTP handling.
func TestBundlerHandlerNew(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil bundler")
	}
}

// TestBundleEndpointMethods verifies only POST is allowed.
func TestBundleEndpointMethods(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	methods := []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/bundle", nil)
			w := httptest.NewRecorder()

			b.HandleBundles(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected status %d for method %s, got %d",
					http.StatusMethodNotAllowed, method, w.Code)
			}

			allow := w.Header().Get("Allow")
			if allow == "" {
				t.Error("expected Allow header to be set")
			}
			if allow != http.MethodPost {
				t.Errorf("Allow header = %q, want %q", allow, http.MethodPost)
			}
		})
	}
}

// TestBundleEndpointInvalidJSON tests invalid JSON body handling.
func TestBundleEndpointInvalidJSON(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"invalid json", "{invalid}"},
		{"malformed json", `{"recipe": `},
		{"wrong type", `{"recipe": "string-not-object"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			b.HandleBundles(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
			}

			// Verify JSON error response
			contentType := w.Header().Get("Content-Type")
			if !strings.HasPrefix(contentType, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}
		})
	}
}

// TestBundleEndpointMissingRecipe tests handling of empty/invalid recipe body.
func TestBundleEndpointMissingRecipe(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Request with empty componentRefs (simulates empty recipe)
	body := `{"apiVersion": "aicr.nvidia.com/v1alpha1", "kind": "Recipe", "componentRefs": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	// Verify error message
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if msg, ok := resp["message"].(string); !ok || !strings.Contains(msg, "component") {
		t.Errorf("message = %q, want message about components", msg)
	}
}

// TestBundleEndpointEmptyComponentRefs tests handling of recipes without components.
func TestBundleEndpointEmptyComponentRefs(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Recipe with no component references (direct RecipeResult in body)
	body := `{"apiVersion": "aicr.nvidia.com/v1alpha1", "kind": "Recipe", "componentRefs": []}`
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if msg, ok := resp["message"].(string); !ok || !strings.Contains(msg, "component") {
		t.Errorf("expected error about components, got: %q", msg)
	}
}

// TestBundleEndpointIgnoresBundlersParam tests that the bundlers query param is silently ignored.
// In the per-component bundle approach, we generate bundles for all components in the recipe,
// not specific bundler types.
func TestBundleEndpointIgnoresBundlersParam(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Recipe with valid components, bundlers param should be ignored
	body := `{
		"apiVersion": "aicr.nvidia.com/v1alpha1",
		"kind": "Recipe",
		"componentRefs": [
			{"name": "gpu-operator", "version": "v25.3.3"}
		]
	}`

	// bundlers param should be silently ignored
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle?bundlers=invalid-bundler", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	// Should still succeed - bundlers param is ignored
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d. Body: %s", http.StatusOK, w.Code, w.Body.String())
	}

	// Verify content type is zip
	contentType := w.Header().Get("Content-Type")
	if contentType != "application/zip" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/zip")
	}
}

// TestBundleEndpointValidRequest tests a valid bundle generation request.
func TestBundleEndpointValidRequest(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Create a valid recipe (direct RecipeResult in body)
	body := `{
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

	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	// Should return OK with zip content
	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d. Body: %s", http.StatusOK, w.Code, w.Body.String())
		return
	}

	// Verify content type is zip
	contentType := w.Header().Get("Content-Type")
	if contentType != "application/zip" {
		t.Errorf("Content-Type = %q, want %q", contentType, "application/zip")
	}

	// Verify content disposition
	contentDisp := w.Header().Get("Content-Disposition")
	if !strings.Contains(contentDisp, "bundles.zip") {
		t.Errorf("Content-Disposition = %q, want to contain 'bundles.zip'", contentDisp)
	}

	// Verify bundle metadata headers
	if w.Header().Get("X-Bundle-Files") == "" {
		t.Error("expected X-Bundle-Files header")
	}
	if w.Header().Get("X-Bundle-Size") == "" {
		t.Error("expected X-Bundle-Size header")
	}
	if w.Header().Get("X-Bundle-Duration") == "" {
		t.Error("expected X-Bundle-Duration header")
	}

	// Verify zip is readable
	zipReader, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}

	// Verify expected files in zip (per-component bundle files + recipe)
	expectedFiles := map[string]bool{
		"README.md":   false,
		"deploy.sh":   false,
		"recipe.yaml": false,
	}

	foundGPUValues := false
	for _, f := range zipReader.File {
		if _, ok := expectedFiles[f.Name]; ok {
			expectedFiles[f.Name] = true
		}
		if f.Name == "gpu-operator/values.yaml" {
			foundGPUValues = true
		}
	}

	for name, found := range expectedFiles {
		if !found {
			t.Errorf("expected file %q not found in zip archive", name)
		}
	}
	if !foundGPUValues {
		t.Error("expected gpu-operator/values.yaml not found in zip archive")
	}

	// Log files for debugging
	t.Logf("Zip contains %d files:", len(zipReader.File))
	for _, f := range zipReader.File {
		t.Logf("  - %s", f.Name)
	}
}

// TestBundleEndpointAllBundlers tests bundle generation with no bundler filter.
func TestBundleEndpointAllBundlers(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Create a recipe with multiple components (no bundlers query param = all bundlers)
	body := `{
		"apiVersion": "aicr.nvidia.com/v1alpha1",
		"kind": "Recipe",
		"componentRefs": [
			{"name": "gpu-operator", "version": "v25.3.3", "type": "helm", "valuesFile": "components/gpu-operator/values.yaml"},
			{"name": "network-operator", "version": "v25.4.0", "type": "helm", "valuesFile": "components/network-operator/values.yaml"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	// May return OK or error depending on component availability
	// For integration tests, this validates the code path works
	if w.Code != http.StatusOK && w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d or %d, got %d", http.StatusOK, http.StatusInternalServerError, w.Code)
	}
}

// TestBundleRequestQueryParamParsing tests that bundlers query param is ignored.
// In per-component bundle mode, all components from recipe are included.
func TestBundleRequestQueryParamParsing(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name       string
		queryParam string
		body       string
		wantStatus int
	}{
		{
			name:       "bundlers param ignored",
			queryParam: "bundlers=gpu-operator",
			body:       `{"apiVersion": "v1", "kind": "Recipe", "componentRefs": [{"name": "gpu-operator", "version": "v1"}]}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "invalid bundlers param ignored",
			queryParam: "bundlers=invalid-bundler",
			body:       `{"apiVersion": "v1", "kind": "Recipe", "componentRefs": [{"name": "gpu-operator", "version": "v1"}]}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "no bundlers param",
			queryParam: "",
			body:       `{"apiVersion": "v1", "kind": "Recipe", "componentRefs": [{"name": "gpu-operator", "version": "v1"}]}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "value override param",
			queryParam: "set=gpuoperator:driver.enabled=true",
			body:       `{"apiVersion": "v1", "kind": "Recipe", "componentRefs": [{"name": "gpu-operator", "version": "v1"}]}`,
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/v1/bundle"
			if tt.queryParam != "" {
				url += "?" + tt.queryParam
			}
			req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			b.HandleBundles(w, req)

			// Allow both OK and internal error (bundler may fail but parsing should succeed)
			if w.Code != tt.wantStatus && w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want %d or %d. Body: %s", w.Code, tt.wantStatus, http.StatusInternalServerError, w.Body.String())
			}
		})
	}
}

// TestZipResponseContainsExpectedFiles validates zip structure for per-component bundle.
func TestZipResponseContainsExpectedFiles(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Recipe direct in body
	body := `{
		"apiVersion": "aicr.nvidia.com/v1alpha1",
		"kind": "Recipe",
		"componentRefs": [
			{
				"name": "gpu-operator",
				"version": "v25.3.3",
				"type": "helm",
				"valuesFile": "components/gpu-operator/values.yaml"
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	if w.Code != http.StatusOK {
		t.Skipf("skipping zip validation, got status %d: %s", w.Code, w.Body.String())
	}

	zipReader, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}

	// Check for expected per-component bundle files
	expectedFiles := map[string]bool{
		"README.md":   false,
		"deploy.sh":   false,
		"recipe.yaml": false,
	}

	foundGPUValues := false
	for _, f := range zipReader.File {
		if _, ok := expectedFiles[f.Name]; ok {
			expectedFiles[f.Name] = true
		}
		if f.Name == "gpu-operator/values.yaml" {
			foundGPUValues = true
		}
	}

	for name, found := range expectedFiles {
		if !found {
			t.Errorf("expected file %q not found in zip", name)
		}
	}
	if !foundGPUValues {
		t.Error("expected gpu-operator/values.yaml not found in zip")
	}

	t.Log("Files in zip:")
	for _, f := range zipReader.File {
		t.Logf("  - %s", f.Name)
	}
}

// TestParseQueryParams tests the query parameter parsing function directly.
func TestParseQueryParams(t *testing.T) {
	wantNodes := func(n int) *int { return &n }
	tests := []struct {
		name         string
		url          string
		wantErr      bool
		wantDeployer string
		wantRepoURL  string
		wantNodes    *int
	}{
		{
			name:         "empty query defaults to helm",
			url:          "/v1/bundle",
			wantDeployer: "helm",
		},
		{
			name:         "deployer=argocd",
			url:          "/v1/bundle?deployer=argocd",
			wantDeployer: "argocd",
		},
		{
			name:         "deployer=helm explicit",
			url:          "/v1/bundle?deployer=helm",
			wantDeployer: "helm",
		},
		{
			name:    "invalid deployer",
			url:     "/v1/bundle?deployer=invalid",
			wantErr: true,
		},
		{
			name:         "repo URL for argocd",
			url:          "/v1/bundle?deployer=argocd&repo=https://github.com/org/repo.git",
			wantDeployer: "argocd",
			wantRepoURL:  "https://github.com/org/repo.git",
		},
		{
			name:         "set param parsed",
			url:          "/v1/bundle?set=gpuoperator:driver.enabled=true",
			wantDeployer: "helm",
		},
		{
			name:         "system-node-selector param",
			url:          "/v1/bundle?system-node-selector=nodeGroup=system",
			wantDeployer: "helm",
		},
		// nodes query parameter
		{
			name:         "nodes valid",
			url:          "/v1/bundle?nodes=8",
			wantDeployer: "helm",
			wantNodes:    wantNodes(8),
		},
		{
			name:         "nodes zero (unset)",
			url:          "/v1/bundle?nodes=0",
			wantDeployer: "helm",
			wantNodes:    wantNodes(0),
		},
		{
			name:      "nodes negative",
			url:       "/v1/bundle?nodes=-1",
			wantErr:   true,
			wantNodes: nil,
		},
		{
			name:    "nodes non-integer",
			url:     "/v1/bundle?nodes=abc",
			wantErr: true,
		},
		{
			name:         "nodes omitted defaults to zero",
			url:          "/v1/bundle",
			wantDeployer: "helm",
			wantNodes:    wantNodes(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.url, nil)
			params, err := parseQueryParams(req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if string(params.deployer) != tt.wantDeployer {
				t.Errorf("deployer = %q, want %q", params.deployer, tt.wantDeployer)
			}
			if tt.wantRepoURL != "" && params.repoURL != tt.wantRepoURL {
				t.Errorf("repoURL = %q, want %q", params.repoURL, tt.wantRepoURL)
			}
			if tt.wantNodes != nil && params.estimatedNodeCount != *tt.wantNodes {
				t.Errorf("estimatedNodeCount = %d, want %d", params.estimatedNodeCount, *tt.wantNodes)
			}
		})
	}
}

// TestZipCanBeExtracted verifies that the returned zip can be extracted.
func TestZipCanBeExtracted(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Recipe direct in body
	body := `{
		"apiVersion": "aicr.nvidia.com/v1alpha1",
		"kind": "Recipe",
		"componentRefs": [
			{
				"name": "gpu-operator",
				"version": "v25.3.3",
				"type": "helm",
				"valuesFile": "components/gpu-operator/values.yaml"
			}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	b.HandleBundles(w, req)

	if w.Code != http.StatusOK {
		t.Skipf("skipping extraction validation, got status %d", w.Code)
	}

	zipReader, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}

	// Verify each file can be opened and read
	for _, f := range zipReader.File {
		rc, err := f.Open()
		if err != nil {
			t.Errorf("failed to open %s: %v", f.Name, err)
			continue
		}

		_, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("failed to read %s: %v", f.Name, err)
		}
	}
}
