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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

var testBundleZipHeaders = []string{
	"Content-Disposition",
	"X-Bundle-Files",
	"X-Bundle-Size",
	"X-Bundle-Duration",
}

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

func TestBundleHandler_StreamZipFailureBeforeCommit(t *testing.T) {
	tests := []struct {
		name       string
		streamErr  error
		wantStatus int
		wantCode   aicrerrors.ErrorCode
	}{
		{
			name:       "integrity failure becomes internal",
			streamErr:  aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "private bundle path is unmanaged"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   aicrerrors.ErrCodeInternal,
		},
		{
			name:       "internal failure remains internal",
			streamErr:  aicrerrors.New(aicrerrors.ErrCodeInternal, "private archive implementation failure"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   aicrerrors.ErrCodeInternal,
		},
		{
			name:       "timeout remains timeout",
			streamErr:  aicrerrors.New(aicrerrors.ErrCodeTimeout, "private archive deadline detail"),
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   aicrerrors.ErrCodeTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &bundleHandler{
				streamZip: func(_ context.Context, w http.ResponseWriter, _ string, _ *result.Output) error {
					w.Header().Set("Content-Type", "application/zip")
					w.Header().Set("Content-Disposition", "attachment; filename=private.zip")
					w.Header().Set("X-Bundle-Files", "99")
					w.Header().Set("X-Bundle-Size", "999")
					w.Header().Set("X-Bundle-Duration", "private-duration")
					return tt.streamErr
				},
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/bundle", nil)
			recorder := httptest.NewRecorder()
			h.writeZipResponse(req.Context(), recorder, req, "unused", &result.Output{})

			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}
			for _, header := range testBundleZipHeaders {
				if value := recorder.Header().Get(header); value != "" {
					t.Errorf("header %s = %q, want empty", header, value)
				}
			}
			if strings.Contains(recorder.Body.String(), "private") {
				t.Fatalf("response leaked private archive error: %s", recorder.Body.String())
			}
			var response errorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if response.Code != string(tt.wantCode) {
				t.Errorf("error code = %q, want %q", response.Code, tt.wantCode)
			}
		})
	}
}

func TestBundleHandler_StreamZipFailureAfterCommit(t *testing.T) {
	h := &bundleHandler{
		streamZip: func(_ context.Context, w http.ResponseWriter, _ string, _ *result.Output) error {
			w.Header().Set("Content-Type", "application/zip")
			if _, err := w.Write([]byte("partial zip")); err != nil {
				return err
			}
			return aicrerrors.New(aicrerrors.ErrCodeInternal, "private archive failure")
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", nil)
	recorder := httptest.NewRecorder()
	h.writeZipResponse(req.Context(), recorder, req, "unused", &result.Output{})

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got, want := recorder.Body.String(), "partial zip"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", contentType)
	}
}
