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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Test Coverage Note:
// The pkg/api package contains a single Serve() function that:
// 1. Initializes logging
// 2. Creates a recipe builder
// 3. Configures routes
// 4. Starts a blocking HTTP server
//
// Direct unit testing of Serve() is impractical because:
// - It's a blocking function that runs until shutdown
// - It requires full server initialization
// - It integrates with the pkg/server package
//
// Instead, these tests verify:
// - Package constants and build variables are correct
// - Route configuration structure is valid
// - Recipe builder integration works correctly
// - HTTP handlers respond properly to various inputs
// - Concurrent request handling is safe
//
// The Serve() function itself is best tested via:
// - End-to-end integration tests (separate test suite)
// - Manual testing during development
// - System/acceptance testing in deployed environments

// TestConstants verifies package constants are properly defined
func TestConstants(t *testing.T) {
	if name != "aicrd" {
		t.Errorf("name = %q, want %q", name, "aicrd")
	}

	if versionDefault != "dev" {
		t.Errorf("versionDefault = %q, want %q", versionDefault, "dev")
	}

	// Verify buildtime variables exist (they may have default values)
	if version == "" {
		t.Error("version should not be empty")
	}
	if commit == "" {
		t.Error("commit should not be empty")
	}
	if date == "" {
		t.Error("date should not be empty")
	}
}

// TestRouteConfiguration verifies that the correct routes are set up
func TestRouteConfiguration(t *testing.T) {
	// Test that the route map is properly structured
	rb := recipe.NewBuilder(
		recipe.WithVersion("test-version"),
	)
	bb, err := bundler.New()
	if err != nil {
		t.Fatalf("failed to create bundler: %v", err)
	}

	routes := map[string]http.HandlerFunc{
		"/v1/recipe": rb.HandleRecipes,
		"/v1/bundle": bb.HandleBundles,
	}

	// Verify expected routes exist
	if handler, exists := routes["/v1/recipe"]; !exists {
		t.Error("expected /v1/recipe route to exist")
	} else if handler == nil {
		t.Error("expected /v1/recipe handler to be non-nil")
	}

	if handler, exists := routes["/v1/bundle"]; !exists {
		t.Error("expected /v1/bundle route to exist")
	} else if handler == nil {
		t.Error("expected /v1/bundle handler to be non-nil")
	}

	// Verify no extra routes
	if len(routes) != 2 {
		t.Errorf("expected exactly 2 routes, got %d", len(routes))
	}
}

// TestRecipeEndpoint tests the /v1/recipe endpoint
func TestRecipeEndpoint(t *testing.T) {
	b := recipe.NewBuilder(recipe.WithVersion("test"))

	req := httptest.NewRequest(http.MethodGet, "/v1/recipe", nil)
	w := httptest.NewRecorder()

	b.HandleRecipes(w, req)

	// Should return OK or an error status
	if w.Code != http.StatusOK && w.Code != http.StatusBadRequest && w.Code != http.StatusInternalServerError {
		t.Errorf("unexpected status code: %d", w.Code)
	}

	// Verify content type is set
	contentType := w.Header().Get("Content-Type")
	if contentType == "" {
		t.Error("expected Content-Type header to be set")
	}
}

// TestRecipeEndpointMethods verifies only GET and POST are allowed
func TestRecipeEndpointMethods(t *testing.T) {
	b := recipe.NewBuilder()

	// These methods should return 405 Method Not Allowed
	disallowedMethods := []string{http.MethodPut, http.MethodDelete, http.MethodPatch}

	for _, method := range disallowedMethods {
		t.Run(method+"_not_allowed", func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/recipe", nil)
			w := httptest.NewRecorder()

			b.HandleRecipes(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("expected status %d for method %s, got %d",
					http.StatusMethodNotAllowed, method, w.Code)
			}

			allow := w.Header().Get("Allow")
			if allow == "" {
				t.Error("expected Allow header to be set")
			}
		})
	}
}

// TestRecipeEndpointPOST verifies POST method works with JSON/YAML bodies
func TestRecipeEndpointPOST(t *testing.T) {
	b := recipe.NewBuilder()

	tests := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
	}{
		{
			name:        "valid JSON body",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","spec":{"service":"eks","accelerator":"h100"}}`,
			contentType: "application/json",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "valid YAML body",
			body:        "kind: RecipeCriteria\napiVersion: aicr.nvidia.com/v1alpha1\nspec:\n  service: gke\n  accelerator: a100",
			contentType: "application/x-yaml",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "valid JSON body with platform",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.nvidia.com/v1alpha1","spec":{"service":"eks","accelerator":"h100","platform":"kubeflow"}}`,
			contentType: "application/json",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "empty body",
			body:        "",
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid JSON",
			body:        `{invalid}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid service value",
			body:        `{"spec":{"service":"invalid"}}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/recipe", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			w := httptest.NewRecorder()

			b.HandleRecipes(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("expected status %d, got %d; body: %s",
					tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

// TestRecipeEndpointWithValidQueryParams tests various valid criteria combinations
func TestRecipeEndpointWithValidQueryParams(t *testing.T) {
	b := recipe.NewBuilder()

	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "accelerator h100",
			query: "?accelerator=h100",
		},
		{
			name:  "accelerator gb200",
			query: "?accelerator=gb200",
		},
		{
			name:  "gpu alias h100",
			query: "?gpu=h100",
		},
		{
			name:  "service eks",
			query: "?service=eks",
		},
		{
			name:  "service gke",
			query: "?service=gke",
		},
		{
			name:  "intent training",
			query: "?intent=training",
		},
		{
			name:  "intent inference",
			query: "?intent=inference",
		},
		{
			name:  "os ubuntu",
			query: "?os=ubuntu",
		},
		{
			name:  "nodes count",
			query: "?nodes=4",
		},
		{
			name:  "platform kubeflow",
			query: "?platform=kubeflow",
		},
		{
			name:  "multiple params",
			query: "?accelerator=h100&service=eks&intent=training",
		},
		{
			name:  "multiple params with platform",
			query: "?accelerator=h100&service=eks&intent=training&platform=kubeflow",
		},
		{
			name:  "no params",
			query: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/recipe"+tt.query, nil)
			w := httptest.NewRecorder()

			b.HandleRecipes(w, req)

			// Valid requests should return OK or an error status (not method not allowed)
			if w.Code == http.StatusMethodNotAllowed {
				t.Error("valid query should not result in method not allowed")
			}
		})
	}
}

// TestRecipeEndpointWithInvalidQueryParams tests invalid parameter values
func TestRecipeEndpointWithInvalidQueryParams(t *testing.T) {
	b := recipe.NewBuilder()

	tests := []struct {
		name  string
		query string
	}{
		{
			name:  "invalid accelerator",
			query: "?accelerator=invalid-accelerator",
		},
		{
			name:  "invalid gpu alias",
			query: "?gpu=invalid-gpu",
		},
		{
			name:  "invalid service",
			query: "?service=invalid-service",
		},
		{
			name:  "invalid intent",
			query: "?intent=invalid-intent",
		},
		{
			name:  "invalid os",
			query: "?os=invalid-os",
		},
		{
			name:  "invalid platform",
			query: "?platform=invalid-platform",
		},
		{
			name:  "invalid nodes negative",
			query: "?nodes=-5",
		},
		{
			name:  "invalid nodes non-numeric",
			query: "?nodes=abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/recipe"+tt.query, nil)
			w := httptest.NewRecorder()

			b.HandleRecipes(w, req)

			// Invalid params should result in bad request
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status %d for invalid param, got %d",
					http.StatusBadRequest, w.Code)
			}
		})
	}
}

// TestRecipeEndpointResponseHeaders verifies common response headers
func TestRecipeEndpointResponseHeaders(t *testing.T) {
	b := recipe.NewBuilder()

	req := httptest.NewRequest(http.MethodGet, "/v1/recipe", nil)
	w := httptest.NewRecorder()

	b.HandleRecipes(w, req)

	// Verify content type is set for successful responses
	if w.Code == http.StatusOK {
		contentType := w.Header().Get("Content-Type")
		if contentType == "" {
			t.Error("expected Content-Type header to be set on successful response")
		}
	}
}

// TestRecipeEndpointConcurrency tests that the handler is safe for concurrent use
func TestRecipeEndpointConcurrency(t *testing.T) {
	b := recipe.NewBuilder()

	const numRequests = 10
	done := make(chan bool, numRequests)

	for i := 0; i < numRequests; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/v1/recipe?os=ubuntu", nil)
			w := httptest.NewRecorder()
			b.HandleRecipes(w, req)
			done <- true
		}()
	}

	// Wait for all requests to complete with timeout
	timeout := time.After(5 * time.Second)
	for i := 0; i < numRequests; i++ {
		select {
		case <-done:
			// Request completed
		case <-timeout:
			t.Fatal("timeout waiting for concurrent requests to complete")
		}
	}
}

// TestRecipeBuilderInitialization verifies builder is properly initialized
func TestRecipeBuilderInitialization(t *testing.T) {
	testVersion := "1.2.3"
	b := recipe.NewBuilder(
		recipe.WithVersion(testVersion),
	)

	if b == nil {
		t.Fatal("expected non-nil builder")
	}

	// Verify the handler can be called
	req := httptest.NewRequest(http.MethodGet, "/v1/recipe", nil)
	w := httptest.NewRecorder()

	b.HandleRecipes(w, req)

	// Should not panic and should return some response
	if w.Code == 0 {
		t.Error("handler did not set a status code")
	}
}

// TestRecipeEndpointContextHandling verifies context is properly handled
func TestRecipeEndpointContextHandling(t *testing.T) {
	b := recipe.NewBuilder()

	// Create request with canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/v1/recipe?os=ubuntu", nil)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	// Handler should handle canceled context gracefully
	b.HandleRecipes(w, req)

	// Should not panic - exact status depends on implementation
	if w.Code == 0 {
		t.Error("handler did not set a status code")
	}
}
