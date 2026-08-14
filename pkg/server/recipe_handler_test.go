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
	"os"
	"path/filepath"
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

func newProfileTestHandler(t *testing.T) *recipeHandler {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "overlays"), 0o755); err != nil {
		t.Fatalf("setup overlays directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"),
		[]byte("components: []\n"), 0o600); err != nil {
		t.Fatalf("setup registry.yaml: %v", err)
	}
	overlay := []byte(`apiVersion: aicr.run/v1alpha3
kind: RecipeMetadata
metadata:
  name: profile-eks
spec:
  criteria:
    service: eks
  profile:
    name: gpuStack
    default: driver-installed
    values:
      driver-installed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: false
      operator-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: true
`)
	if err := os.WriteFile(filepath.Join(dir, "overlays", "profile-eks.yaml"),
		overlay, 0o600); err != nil {
		t.Fatalf("setup profile overlay: %v", err)
	}
	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.FilesystemSource(dir)),
		aicr.WithVersion("test"),
	)
	if err != nil {
		t.Fatalf("failed to construct profile test client: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("client close failed: %v", closeErr)
		}
	})
	return newRecipeHandler(client, nil)
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
	}{
		{
			name:   "GET h100 training",
			method: http.MethodGet,
			target: "/v1/recipe?service=eks&accelerator=h100&intent=training",
		},
		{
			name:        "POST h100 training JSON",
			method:      http.MethodPost,
			target:      "/v1/recipe",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.run/v1alpha2","spec":{"service":"eks","accelerator":"h100","intent":"training"}}`,
			contentType: "application/json",
		},
		{
			name:   "POST JSON without content type preserves legacy default",
			method: http.MethodPost,
			target: "/v1/recipe",
			body:   `{"kind":"RecipeCriteria","apiVersion":"aicr.run/v1alpha2","spec":{"service":"eks","accelerator":"h100","intent":"training"}}`,
		},
		{
			name:        "POST JSON with text plain preserves legacy fallback",
			method:      http.MethodPost,
			target:      "/v1/recipe",
			body:        `{"kind":"RecipeCriteria","apiVersion":"aicr.run/v1alpha2","spec":{"service":"eks","accelerator":"h100","intent":"training"}}`,
			contentType: "text/plain",
		},
	}

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
			// Verify body is a recipe result with components.
			var result recipe.RecipeResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatalf("failed to decode recipe result: %v; body: %s", err, w.Body.String())
			}
			if len(result.ComponentRefs) == 0 {
				t.Error("expected at least one component in resolved recipe")
			}
		})
	}
}

func TestProfileAwareRecipeEndpoints(t *testing.T) {
	h := newProfileTestHandler(t)

	t.Run("v2 GET selects explicit value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v2/recipe?service=eks&accelerator=h100&intent=training&profile=gpuStack%3Doperator-managed",
			nil,
		)
		w := httptest.NewRecorder()
		h.HandleRecipesV2(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var result recipe.RecipeResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if result.APIVersion != recipe.RecipeProfileAPIVersion ||
			result.Metadata.SelectedProfile == nil ||
			result.Metadata.SelectedProfile.Value != "operator-managed" {

			t.Fatalf("profile result apiVersion=%q selected=%#v",
				result.APIVersion, result.Metadata.SelectedProfile)
		}
	})

	t.Run("v2 POST applies default", func(t *testing.T) {
		body := `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"}}`
		req := httptest.NewRequest(http.MethodPost, "/v2/recipe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleRecipesV2(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var result recipe.RecipeResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode result: %v", err)
		}
		if result.Metadata.SelectedProfile == nil ||
			result.Metadata.SelectedProfile.Value != "driver-installed" {

			t.Fatalf("default selectedProfile = %#v", result.Metadata.SelectedProfile)
		}
	})

	for _, tt := range []struct {
		name   string
		target string
		body   string
	}{
		{
			name:   "v2 POST selects profile from query",
			target: "/v2/recipe?profile=gpuStack%3Doperator-managed",
			body:   `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"}}`,
		},
		{
			name:   "v2 POST accepts agreeing query and body profiles",
			target: "/v2/recipe?profile=gpuStack%3Doperator-managed",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"profile":"gpuStack=operator-managed"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.HandleRecipesV2(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			var result recipe.RecipeResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if result.Metadata.SelectedProfile == nil ||
				result.Metadata.SelectedProfile.Value != "operator-managed" {

				t.Fatalf("selectedProfile = %#v, want operator-managed",
					result.Metadata.SelectedProfile)
			}
		})
	}

	t.Run("v2 query exposes selected profile", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v2/query?service=eks&accelerator=h100&intent=training&"+
				"profile=gpuStack%3Doperator-managed&selector=metadata.selectedProfile.value",
			nil,
		)
		w := httptest.NewRecorder()
		h.HandleQueryV2(w, req)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `"operator-managed"` {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
	})

	for _, tt := range []struct {
		name   string
		target string
		body   string
	}{
		{
			name:   "v2 POST query selects profile from query",
			target: "/v2/query?profile=gpuStack%3Doperator-managed",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"selector":"metadata.selectedProfile.value"}`,
		},
		{
			name:   "v2 POST query accepts agreeing query and body profiles",
			target: "/v2/query?profile=gpuStack%3Doperator-managed",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"profile":"gpuStack=operator-managed","selector":"metadata.selectedProfile.value"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.HandleQueryV2(w, req)
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `"operator-managed"` {
				t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
			}
		})
	}

	t.Run("v2 GET query requires selector presence", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v2/query?service=eks&accelerator=h100&intent=training",
			nil,
		)
		w := httptest.NewRecorder()
		h.HandleQueryV2(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v2 POST query requires selector presence", func(t *testing.T) {
		body := `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"}}`
		req := httptest.NewRequest(http.MethodPost, "/v2/query", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleQueryV2(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v2 query accepts an explicitly empty selector", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v2/query?service=eks&accelerator=h100&intent=training&selector=",
			nil,
		)
		w := httptest.NewRecorder()
		h.HandleQueryV2(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var hydrated map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &hydrated); err != nil {
			t.Fatalf("decode hydrated recipe: %v", err)
		}
		if _, ok := hydrated["components"]; !ok {
			t.Fatalf("hydrated recipe keys = %v, want components", keysOf(hydrated))
		}
	})

	t.Run("v2 POST query accepts an explicitly empty selector", func(t *testing.T) {
		body := `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},"selector":""}`
		req := httptest.NewRequest(http.MethodPost, "/v2/query", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleQueryV2(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var hydrated map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &hydrated); err != nil {
			t.Fatalf("decode hydrated recipe: %v", err)
		}
		if _, ok := hydrated["components"]; !ok {
			t.Fatalf("hydrated recipe keys = %v, want components", keysOf(hydrated))
		}
	})

	t.Run("v1 rejects resolved profiled composition", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/recipe?service=eks&accelerator=h100&intent=training", nil)
		w := httptest.NewRecorder()
		h.HandleRecipes(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("v1 rejects explicit profile input", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/v1/recipe?service=eks&profile=gpuStack%3Doperator-managed", nil)
		w := httptest.NewRecorder()
		h.HandleRecipes(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
		}
	})
	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{
			name:   "v2 rejects unknown query parameter",
			method: http.MethodGet,
			target: "/v2/recipe?service=eks&profie=gpuStack%3Dx",
		},
		{
			name:   "v2 rejects malformed raw query",
			method: http.MethodGet,
			target: "/v2/recipe?service=eks&profile=gpuStack%3Doperator-managed;ignored=x",
		},
		{
			name:   "v2 rejects conflicting repeated profile",
			method: http.MethodGet,
			target: "/v2/recipe?service=eks&profile=a%3Db&profile=a%3Dc",
		},
		{
			name:   "v2 rejects unknown envelope field",
			method: http.MethodPost,
			target: "/v2/recipe",
			body:   `{"criteria":{"service":"eks"},"profie":"gpuStack=x"}`,
		},
		{
			name:   "v2 rejects unknown POST query parameter",
			method: http.MethodPost,
			target: "/v2/recipe?profie=gpuStack%3Dx",
			body:   `{"criteria":{"service":"eks"}}`,
		},
		{
			name:   "v2 rejects conflicting POST profile surfaces",
			method: http.MethodPost,
			target: "/v2/recipe?profile=gpuStack%3Doperator-managed",
			body:   `{"criteria":{"service":"eks"},"profile":"gpuStack=driver-installed"}`,
		},
		{
			name:   "v2 rejects negative nodes in POST criteria",
			method: http.MethodPost,
			target: "/v2/recipe",
			body:   `{"criteria":{"service":"eks","nodes":-1}}`,
		},
		{
			name:   "v1 rejects top-level POST profile",
			method: http.MethodPost,
			target: "/v1/recipe",
			body:   `{"profile":"gpuStack=x","spec":{"service":"eks"}}`,
		},
		{
			name:   "v1 rejects POST profile query parameter",
			method: http.MethodPost,
			target: "/v1/recipe?profile=gpuStack%3Dx",
			body: `{"kind":"RecipeCriteria","apiVersion":"aicr.run/v1alpha2",` +
				`"spec":{"service":"eks"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			if strings.HasPrefix(tt.target, "/v2/") {
				h.HandleRecipesV2(w, req)
			} else {
				h.HandleRecipes(w, req)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}

	queryRejections := []struct {
		name   string
		v2     bool
		target string
		body   string
	}{
		{
			name:   "v2 query rejects unknown POST query parameter",
			v2:     true,
			target: "/v2/query?profie=gpuStack%3Dx",
			body: `{"criteria":{"service":"eks"},"selector":` +
				`"metadata.selectedProfile.value"}`,
		},
		{
			name:   "v2 query rejects conflicting POST profile surfaces",
			v2:     true,
			target: "/v2/query?profile=gpuStack%3Doperator-managed",
			body: `{"criteria":{"service":"eks"},"profile":"gpuStack=driver-installed",` +
				`"selector":"metadata.selectedProfile.value"}`,
		},
		{
			name:   "v2 query rejects negative nodes in POST criteria",
			v2:     true,
			target: "/v2/query",
			body: `{"criteria":{"service":"eks","nodes":-1},` +
				`"selector":"metadata.selectedProfile.value"}`,
		},
		{
			name:   "v1 query rejects POST profile query parameter",
			target: "/v1/query?profile=gpuStack%3Dx",
			body: `{"criteria":{"service":"eks"},"selector":` +
				`"metadata.selectedProfile.value"}`,
		},
	}
	for _, tt := range queryRejections {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			if tt.v2 {
				h.HandleQueryV2(w, req)
			} else {
				h.HandleQuery(w, req)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}

	unsupportedContentTypes := []struct {
		name        string
		query       bool
		contentType string
		body        string
	}{
		{
			name: "v2 recipe rejects missing content type",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"}}`,
		},
		{
			name:        "v2 recipe rejects unsupported content type",
			contentType: "text/plain",
			body:        `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"}}`,
		},
		{
			name:  "v2 query rejects missing content type",
			query: true,
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"selector":"metadata.selectedProfile.value"}`,
		},
		{
			name:        "v2 query rejects unsupported content type",
			query:       true,
			contentType: "text/plain",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"selector":"metadata.selectedProfile.value"}`,
		},
	}
	for _, tt := range unsupportedContentTypes {
		t.Run(tt.name, func(t *testing.T) {
			target := "/v2/recipe"
			if tt.query {
				target = "/v2/query"
			}
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			w := httptest.NewRecorder()
			if tt.query {
				h.HandleQueryV2(w, req)
			} else {
				h.HandleRecipesV2(w, req)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}

	nullProfileRejections := []struct {
		name        string
		query       bool
		contentType string
		body        string
	}{
		{
			name:        "v2 recipe rejects JSON null profile",
			contentType: "application/json",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"profile":null}`,
		},
		{
			name:        "v2 recipe rejects YAML null profile",
			contentType: "application/x-yaml",
			body: "criteria:\n  service: eks\n  accelerator: h100\n  intent: training\n" +
				"profile: null\n",
		},
		{
			name:        "v2 query rejects JSON null profile",
			query:       true,
			contentType: "application/json",
			body: `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},` +
				`"profile":null,"selector":"metadata.selectedProfile.value"}`,
		},
		{
			name:        "v2 query rejects YAML null profile",
			query:       true,
			contentType: "application/x-yaml",
			body: "criteria:\n  service: eks\n  accelerator: h100\n  intent: training\n" +
				"profile: null\nselector: metadata.selectedProfile.value\n",
		},
	}
	for _, tt := range nullProfileRejections {
		t.Run(tt.name, func(t *testing.T) {
			target := "/v2/recipe"
			if tt.query {
				target = "/v2/query"
			}
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			w := httptest.NewRecorder()
			if tt.query {
				h.HandleQueryV2(w, req)
			} else {
				h.HandleRecipesV2(w, req)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleRecipes_SlurmAccountingModeRoutes(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name        string
		v2          bool
		method      string
		target      string
		body        string
		wantStatus  int
		wantVersion string
		wantMode    recipe.AccountingMode
	}{
		{
			name:       "v1 GET rejects accounting mode",
			method:     http.MethodGet,
			target:     "/v1/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm&slurmAccountingMode=customer-managed",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "v1 POST rejects accounting mode",
			method:     http.MethodPost,
			target:     "/v1/recipe?slurmAccountingMode=customer-managed",
			body:       `{"kind":"RecipeCriteria","apiVersion":"aicr.run/v1alpha2","spec":{"service":"eks","accelerator":"h100","intent":"training","os":"ubuntu","platform":"slurm"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:        "v1 GET keeps legacy Slurm response",
			method:      http.MethodGet,
			target:      "/v1/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm",
			wantStatus:  http.StatusOK,
			wantVersion: recipe.RecipeAPIVersion,
		},
		{
			name:        "v2 GET accepts accounting mode",
			v2:          true,
			method:      http.MethodGet,
			target:      "/v2/recipe?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm&slurmAccountingMode=customer-managed",
			wantStatus:  http.StatusOK,
			wantVersion: recipe.ConfiguredRecipeResultAPIVersion,
			wantMode:    recipe.AccountingModeCustomerManaged,
		},
		{
			name:        "v2 POST accepts accounting mode",
			v2:          true,
			method:      http.MethodPost,
			target:      "/v2/recipe?slurmAccountingMode=customer-managed",
			body:        `{"criteria":{"service":"eks","accelerator":"h100","intent":"training","os":"ubuntu","platform":"slurm"}}`,
			wantStatus:  http.StatusOK,
			wantVersion: recipe.ConfiguredRecipeResultAPIVersion,
			wantMode:    recipe.AccountingModeCustomerManaged,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, strings.NewReader(tt.body))
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			if tt.v2 {
				h.HandleRecipesV2(w, req)
			} else {
				h.HandleRecipes(w, req)
			}
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				return
			}
			var result recipe.RecipeResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			mode, present := result.AccountingMode()
			if tt.wantMode == "" {
				if present {
					t.Fatalf("AccountingMode() = %q, true; want absent", mode)
				}
			} else if !present || mode != tt.wantMode {
				t.Fatalf("AccountingMode() = %q, %v; want %q, true", mode, present, tt.wantMode)
			}
			if result.APIVersion != tt.wantVersion {
				t.Errorf("apiVersion = %q, want %q", result.APIVersion, tt.wantVersion)
			}
		})
	}
}

func TestHandleQuery_SlurmAccountingModeRoutes(t *testing.T) {
	h := newTestHandler(t, nil)

	tests := []struct {
		name       string
		v2         bool
		target     string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "v1 rejects accounting mode",
			target:     "/v1/query?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm&slurmAccountingMode=customer-managed&selector=apiVersion",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "v1 keeps legacy Slurm response",
			target:     "/v1/query?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm&selector=apiVersion",
			wantStatus: http.StatusOK,
			wantBody:   `"aicr.run/v1alpha2"`,
		},
		{
			name:       "v2 accepts accounting mode",
			v2:         true,
			target:     "/v2/query?service=eks&accelerator=h100&intent=training&os=ubuntu&platform=slurm&slurmAccountingMode=customer-managed&selector=configuration.slurm.accounting.mode",
			wantStatus: http.StatusOK,
			wantBody:   `"customer-managed"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.target, nil)
			w := httptest.NewRecorder()
			if tt.v2 {
				h.HandleQueryV2(w, req)
			} else {
				h.HandleQuery(w, req)
			}
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantBody != "" && strings.TrimSpace(w.Body.String()) != tt.wantBody {
				t.Fatalf("body = %q, want %q", strings.TrimSpace(w.Body.String()), tt.wantBody)
			}
		})
	}
}

// TestHandleRecipes_MethodNotAllowed verifies non GET/POST returns 405 with an
// Allow header.
func TestHandleRecipes_MethodNotAllowed(t *testing.T) {
	h := newTestHandler(t, nil)

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/v1/recipe", nil)
			w := httptest.NewRecorder()

			h.HandleRecipes(w, req)

			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
			}
			if allow := w.Header().Get("Allow"); allow != "GET, POST" {
				t.Errorf("Allow header = %q, want %q", allow, "GET, POST")
			}
		})
	}
}

// TestHandleRecipes_AllowListRejection verifies an out-of-allowlist criterion is
// rejected with a 400 carrying the underlying allowlist error message.
func TestHandleRecipes_AllowListRejection(t *testing.T) {
	// Allow only a100; h100 falls outside and must be rejected.
	facadeAllow := &aicr.AllowLists{
		Accelerators: []string{string(recipe.CriteriaAcceleratorA100)},
	}
	h := newTestHandler(t, facadeAllow)

	const target = "/v1/recipe?accelerator=h100&intent=training"

	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	h.HandleRecipes(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}

	errResp := decodeErrorBody(t, w.Body.Bytes())
	// The allowlist error carries its own message; the fallback "Criteria
	// value not allowed" is only used when the inner message is empty.
	if errResp.Message != "accelerator type not allowed" {
		t.Errorf("message = %q, want %q", errResp.Message, "accelerator type not allowed")
	}
}

// TestV1ProfileDetectionPreservesLegacyContracts verifies the v2-only profile
// pre-detector preserves established v1 parse errors and the query route's
// legacy YAML-without-content-type behavior.
func TestV1ProfileDetectionPreservesLegacyContracts(t *testing.T) {
	h := newTestHandler(t, nil)
	tests := []struct {
		name        string
		target      string
		body        string
		contentType string
		handle      http.HandlerFunc
		wantError   string
	}{
		{
			name:        "recipe empty body",
			target:      "/v1/recipe",
			contentType: "application/json",
			handle:      h.HandleRecipes,
			wantError:   "[INVALID_REQUEST] request body is empty",
		},
		{
			name:        "query empty body",
			target:      "/v1/query",
			contentType: "application/json",
			handle:      h.HandleQuery,
			wantError:   "[INVALID_REQUEST] request body cannot be empty",
		},
		{
			name:   "recipe YAML profile without content type preserves JSON default",
			target: "/v1/recipe",
			body: "profile: gpuStack=driver-installed\n" +
				"spec:\n" +
				"  service: eks\n",
			handle:    h.HandleRecipes,
			wantError: "[INVALID_REQUEST] failed to parse JSON body: invalid character 'p' looking for beginning of value",
		},
		{
			name:   "query YAML profile without content type",
			target: "/v1/query",
			body: "profile: gpuStack=driver-installed\n" +
				"criteria:\n" +
				"  service: eks\n" +
				"selector: components.gpu-operator.values.driver.enabled\n",
			handle:    h.HandleQuery,
			wantError: "[INVALID_REQUEST] profile selection is available only on /v2/query",
		},
		{
			name:        "recipe profile detected despite ignored out-of-range number",
			target:      "/v1/recipe",
			body:        `{"profile":"gpuStack=operator-managed","unmapped":1e999,"spec":{"service":"eks"}}`,
			contentType: "application/json",
			handle:      h.HandleRecipes,
			wantError:   "[INVALID_REQUEST] profile selection is available only on /v2/recipe",
		},
		{
			name:        "query profile detected despite ignored out-of-range number",
			target:      "/v1/query",
			body:        `{"profile":"gpuStack=operator-managed","unmapped":1e999,"criteria":{"service":"eks"},"selector":"components"}`,
			contentType: "application/json",
			handle:      h.HandleQuery,
			wantError:   "[INVALID_REQUEST] profile selection is available only on /v2/query",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			w := httptest.NewRecorder()

			tt.handle(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
			}
			var response errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if got := response.Details[keyError]; got != tt.wantError {
				t.Fatalf("details.error = %q, want %q", got, tt.wantError)
			}
		})
	}
}

// TestHandleQuery_Success verifies GET and POST query against a selector return
// the selected value.
func TestHandleQuery_Success(t *testing.T) {
	h := newTestHandler(t, nil)

	const selector = "components.gpu-operator.values.driver.version"

	tests := []struct {
		name        string
		method      string
		target      string
		body        string
		contentType string
	}{
		{
			name:   "GET with selector",
			method: http.MethodGet,
			target: "/v1/query?service=eks&accelerator=h100&intent=training&selector=" + selector,
		},
		{
			// QueryRequest.Criteria is a *recipe.Criteria (flat fields), NOT a
			// RecipeCriteria envelope — a nested {kind,apiVersion,spec} body would
			// unmarshal to empty criteria and silently resolve the wrong recipe.
			name:        "POST with selector",
			method:      http.MethodPost,
			target:      "/v1/query",
			body:        `{"criteria":{"service":"eks","accelerator":"h100","intent":"training"},"selector":"` + selector + `"}`,
			contentType: "application/json",
		},
		{
			name:   "POST YAML without content type preserves legacy default",
			method: http.MethodPost,
			target: "/v1/query",
			body: "criteria:\n" +
				"  service: eks\n" +
				"  accelerator: h100\n" +
				"  intent: training\n" +
				"selector: " + selector + "\n",
		},
		{
			name:        "POST YAML with text plain preserves legacy fallback",
			method:      http.MethodPost,
			target:      "/v1/query",
			contentType: "text/plain",
			body: "criteria:\n" +
				"  service: eks\n" +
				"  accelerator: h100\n" +
				"  intent: training\n" +
				"selector: " + selector + "\n",
		},
	}

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
			body:   `{"kind":"RecipeCriteria","spec":{}}`,
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

func TestNormalizeLegacyRecipeResultDoesNotMutateBorrowedResult(t *testing.T) {
	original := &recipe.RecipeResult{
		APIVersion: recipe.ConfiguredRecipeResultAPIVersion,
		Configuration: &recipe.RecipeConfiguration{
			Slurm: &recipe.SlurmConfiguration{
				Accounting: &recipe.SlurmAccountingConfiguration{
					Mode: recipe.AccountingModeDisabled,
				},
			},
		},
	}
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ".")
	original.BindDataProvider(provider)

	projected := normalizeLegacyRecipeResult(original, false)
	if projected == original {
		t.Fatal("normalizeLegacyRecipeResult() returned borrowed result, want independent projection")
	}
	if original.Configuration == nil ||
		original.APIVersion != recipe.ConfiguredRecipeResultAPIVersion {

		t.Fatal("normalizeLegacyRecipeResult() mutated borrowed result")
	}
	if projected.Configuration != nil || projected.APIVersion != recipe.RecipeAPIVersion {
		t.Errorf("legacy projection = %#v, want v1alpha2 without configuration", projected)
	}
	if projected.DataProvider() != provider {
		t.Error("legacy projection did not preserve the bound DataProvider instance")
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
