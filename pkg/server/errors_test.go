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

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
)

func TestHTTPStatusFromCode(t *testing.T) {
	tests := []struct {
		name string
		code eidoserrors.ErrorCode
		want int
	}{
		{"invalid request", eidoserrors.ErrCodeInvalidRequest, http.StatusBadRequest},
		{"unauthorized", eidoserrors.ErrCodeUnauthorized, http.StatusUnauthorized},
		{"not found", eidoserrors.ErrCodeNotFound, http.StatusNotFound},
		{"method not allowed", eidoserrors.ErrCodeMethodNotAllowed, http.StatusMethodNotAllowed},
		{"rate limit", eidoserrors.ErrCodeRateLimitExceeded, http.StatusTooManyRequests},
		{"unavailable", eidoserrors.ErrCodeUnavailable, http.StatusServiceUnavailable},
		{"timeout", eidoserrors.ErrCodeTimeout, http.StatusGatewayTimeout},
		{"internal", eidoserrors.ErrCodeInternal, http.StatusInternalServerError},
		{"unknown defaults to internal", eidoserrors.ErrorCode("SOMETHING_ELSE"), http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTTPStatusFromCode(tt.code); got != tt.want {
				t.Fatalf("HTTPStatusFromCode(%q) = %d, want %d", tt.code, got, tt.want)
			}
		})
	}
}

func TestRetryableFromCode(t *testing.T) {
	tests := []struct {
		name string
		code eidoserrors.ErrorCode
		want bool
	}{
		{"invalid request", eidoserrors.ErrCodeInvalidRequest, false},
		{"unauthorized", eidoserrors.ErrCodeUnauthorized, false},
		{"not found", eidoserrors.ErrCodeNotFound, false},
		{"method not allowed", eidoserrors.ErrCodeMethodNotAllowed, false},
		{"timeout", eidoserrors.ErrCodeTimeout, true},
		{"unavailable", eidoserrors.ErrCodeUnavailable, true},
		{"rate limit", eidoserrors.ErrCodeRateLimitExceeded, true},
		{"internal", eidoserrors.ErrCodeInternal, true},
		{"unknown defaults false", eidoserrors.ErrorCode("SOMETHING_ELSE"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableFromCode(tt.code); got != tt.want {
				t.Fatalf("retryableFromCode(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestMergeDetails(t *testing.T) {
	t.Run("both empty returns nil", func(t *testing.T) {
		if got := mergeDetails(nil, nil); got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
		if got := mergeDetails(map[string]any{}, map[string]any{}); got != nil {
			t.Fatalf("expected nil, got %#v", got)
		}
	})

	t.Run("merges and second overwrites", func(t *testing.T) {
		a := map[string]any{"a": 1, "shared": "old"}
		b := map[string]any{"b": 2, "shared": "new"}

		got := mergeDetails(a, b)
		if got == nil {
			t.Fatal("expected map, got nil")
		}
		if got["a"].(int) != 1 {
			t.Fatalf("expected a=1, got %#v", got["a"])
		}
		if got["b"].(int) != 2 {
			t.Fatalf("expected b=2, got %#v", got["b"])
		}
		if got["shared"].(string) != "new" {
			t.Fatalf("expected shared to be overwritten to 'new', got %#v", got["shared"])
		}
	})
}

func TestWriteError_WritesErrorResponse(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), contextKeyRequestID, "req-123"))
	w := httptest.NewRecorder()

	WriteError(w, req, http.StatusBadRequest, eidoserrors.ErrCodeInvalidRequest, "bad request", false, map[string]any{"k": "v"})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, w.Code)
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Code != string(eidoserrors.ErrCodeInvalidRequest) {
		t.Fatalf("expected code %q, got %q", eidoserrors.ErrCodeInvalidRequest, resp.Code)
	}
	if resp.Message != "bad request" {
		t.Fatalf("expected message %q, got %q", "bad request", resp.Message)
	}
	if resp.RequestID != "req-123" {
		t.Fatalf("expected requestId %q, got %q", "req-123", resp.RequestID)
	}
	if resp.Retryable {
		t.Fatalf("expected retryable=false, got true")
	}
	if resp.Details == nil || resp.Details["k"].(string) != "v" {
		t.Fatalf("expected details to include k=v, got %#v", resp.Details)
	}
}

func TestWriteErrorFromErr_StructuredErrorMapsStatusAndDetails(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	cause := errors.New("db is down")
	err := eidoserrors.WrapWithContext(eidoserrors.ErrCodeUnavailable, "service unavailable", cause, map[string]any{"component": "db"})

	WriteErrorFromErr(w, req, err, "fallback", map[string]any{"extra": "yes"})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, w.Code)
	}

	var resp ErrorResponse
	if uerr := json.Unmarshal(w.Body.Bytes(), &resp); uerr != nil {
		t.Fatalf("failed to unmarshal response: %v", uerr)
	}

	if resp.Code != string(eidoserrors.ErrCodeUnavailable) {
		t.Fatalf("expected code %q, got %q", eidoserrors.ErrCodeUnavailable, resp.Code)
	}
	if resp.Message != "service unavailable" {
		t.Fatalf("expected message %q, got %q", "service unavailable", resp.Message)
	}
	if !resp.Retryable {
		t.Fatalf("expected retryable=true")
	}
	if resp.Details == nil {
		t.Fatalf("expected details, got nil")
	}
	if resp.Details["component"].(string) != "db" {
		t.Fatalf("expected component=db, got %#v", resp.Details["component"])
	}
	if resp.Details["extra"].(string) != "yes" {
		t.Fatalf("expected extra=yes, got %#v", resp.Details["extra"])
	}
	if resp.Details["error"].(string) != "db is down" {
		t.Fatalf("expected error cause propagated, got %#v", resp.Details["error"])
	}
}

func TestWriteErrorFromErr_NilError(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	WriteErrorFromErr(w, req, nil, "fallback msg", nil)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Message != "fallback msg" {
		t.Fatalf("expected message %q, got %q", "fallback msg", resp.Message)
	}
}

func TestWriteErrorFromErr_StructuredErrorEmptyMessage(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	err := eidoserrors.New(eidoserrors.ErrCodeNotFound, "")
	WriteErrorFromErr(w, req, err, "fallback", nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, w.Code)
	}

	var resp ErrorResponse
	if uerr := json.Unmarshal(w.Body.Bytes(), &resp); uerr != nil {
		t.Fatalf("failed to unmarshal: %v", uerr)
	}
	if resp.Message != "fallback" {
		t.Fatalf("expected fallback message, got %q", resp.Message)
	}
}

func TestWriteErrorFromErr_NonStructuredFallsBackToInternal(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	WriteErrorFromErr(w, req, errors.New("boom"), "fallback", map[string]any{"x": "y"})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, w.Code)
	}

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Code != string(eidoserrors.ErrCodeInternal) {
		t.Fatalf("expected code %q, got %q", eidoserrors.ErrCodeInternal, resp.Code)
	}
	if !resp.Retryable {
		t.Fatalf("expected retryable=true")
	}
	if resp.Details == nil || resp.Details["x"].(string) != "y" {
		t.Fatalf("expected details to include x=y, got %#v", resp.Details)
	}
	if resp.Details["error"].(string) != "boom" {
		t.Fatalf("expected details error=boom, got %#v", resp.Details["error"])
	}
}
