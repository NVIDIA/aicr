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

	evidenceindex "github.com/NVIDIA/aicr/pkg/evidence/index"
)

func TestRequireEvidenceQueryParameter(t *testing.T) {
	h := newTestHandler(t, nil)

	const unattested = "service=eks&accelerator=h100&os=ubuntu&intent=training"
	const attested = "service=eks&accelerator=gb200&os=ubuntu&intent=training"
	const profiledBase = "service=aks&accelerator=h100&os=ubuntu&intent=training"
	const profiledLeaf = profiledBase + "&platform=kubeflow"
	const recipeBody = `{"criteria":{"service":"eks","accelerator":"h100","os":"ubuntu","intent":"training"}}`
	const queryBody = `{"criteria":{"service":"eks","accelerator":"gb200","os":"ubuntu","intent":"training"},"selector":"deploymentOrder"}`

	tests := []struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
		wantText   string
	}{
		{
			name:       "absent parameter resolves an unattested recipe",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + unattested,
			wantStatus: http.StatusOK,
		},
		{
			name:       "false does not enable the check",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + unattested + "&requireEvidence=false",
			wantStatus: http.StatusOK,
		},
		{
			name:       "true rejects an unattested recipe",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + unattested + "&requireEvidence=true",
			wantStatus: http.StatusNotFound,
			wantText:   `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name:       "yes is a truthy spelling",
			method:     http.MethodHead,
			target:     "/v1/recipe?" + unattested + "&requireEvidence=yes",
			wantStatus: http.StatusNotFound,
			wantText:   `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name:       "true accepts a recipe with a pointer",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + attested + "&requireEvidence=true",
			wantStatus: http.StatusOK,
		},
		{
			name:       "query is not a bypass",
			method:     http.MethodGet,
			target:     "/v1/query?" + unattested + "&selector=deploymentOrder&requireEvidence=on",
			wantStatus: http.StatusNotFound,
			wantText:   "has no published evidence",
		},
		{
			name:       "query accepts a recipe with a pointer",
			method:     http.MethodGet,
			target:     "/v1/query?" + attested + "&selector=deploymentOrder&requireEvidence=1",
			wantStatus: http.StatusOK,
		},
		{
			name:       "base pointer does not cover the default profile",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + profiledBase + "&requireEvidence=true",
			wantStatus: http.StatusNotFound,
			wantText:   "gpustack-azure-managed",
		},
		{
			name:       "profiled recipe with its own pointer resolves",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + profiledLeaf + "&requireEvidence=true",
			wantStatus: http.StatusOK,
		},
		{
			name:       "unrecognized spelling is rejected",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + attested + "&requireEvidence=maybe",
			wantStatus: http.StatusBadRequest,
			wantText:   "requireEvidence",
		},
		{
			name:       "conflicting repeats are rejected",
			method:     http.MethodGet,
			target:     "/v1/recipe?" + attested + "&requireEvidence=true&requireEvidence=false",
			wantStatus: http.StatusBadRequest,
			wantText:   "conflicting values",
		},
		{
			name:       "POST query parameter rejects an unattested recipe",
			method:     http.MethodPost,
			target:     "/v1/recipe?requireEvidence=true",
			body:       recipeBody,
			wantStatus: http.StatusNotFound,
			wantText:   `recipe "h100-eks-ubuntu-training" has no published evidence`,
		},
		{
			name:       "POST query accepts a recipe with a pointer",
			method:     http.MethodPost,
			target:     "/v1/query?requireEvidence=true",
			body:       queryBody,
			wantStatus: http.StatusOK,
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
			if strings.Contains(tt.target, "/v1/query") {
				h.HandleQuery(w, req)
			} else {
				h.HandleRecipes(w, req)
			}
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantText == "" {
				return
			}
			var resp errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v; body: %s", err, w.Body.String())
			}
			if !strings.Contains(resp.Message, tt.wantText) {
				t.Fatalf("message = %q, want text %q", resp.Message, tt.wantText)
			}
			if tt.wantStatus == http.StatusNotFound {
				if resp.Code != "NOT_FOUND" {
					t.Fatalf("error code = %q, want NOT_FOUND", resp.Code)
				}
				if !strings.Contains(resp.Message, evidenceindex.HealthDocURL) {
					t.Fatalf("message = %q, want the recipe-health link", resp.Message)
				}
			}
		})
	}
}
