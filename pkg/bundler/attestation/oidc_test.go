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

package attestation

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchAmbientOIDCToken(t *testing.T) {
	// Mock GitHub Actions OIDC endpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer token
		if r.Header.Get("Authorization") != "Bearer test-request-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Verify audience parameter
		if r.URL.Query().Get("audience") != "sigstore" {
			http.Error(w, "bad audience", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":"mock-oidc-token"}`)
	}))
	defer server.Close()

	token, err := FetchAmbientOIDCToken(context.Background(), server.URL, "test-request-token")
	if err != nil {
		t.Fatalf("FetchAmbientOIDCToken() error: %v", err)
	}
	if token != "mock-oidc-token" {
		t.Errorf("FetchAmbientOIDCToken() = %q, want %q", token, "mock-oidc-token")
	}
}

func TestFetchAmbientOIDCToken_EmptyURL(t *testing.T) {
	_, err := FetchAmbientOIDCToken(context.Background(), "", "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with empty URL should return error")
	}
}

func TestFetchAmbientOIDCToken_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := FetchAmbientOIDCToken(context.Background(), server.URL, "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with server error should return error")
	}
}

func TestFetchAmbientOIDCToken_EmptyTokenResponse(t *testing.T) {
	// Server returns valid JSON but with empty token value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":""}`)
	}))
	defer server.Close()

	_, err := FetchAmbientOIDCToken(context.Background(), server.URL, "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with empty token value should return error")
	}
}

func TestFetchAmbientOIDCToken_NullTokenResponse(t *testing.T) {
	// Server returns valid JSON but with null/missing token value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	_, err := FetchAmbientOIDCToken(context.Background(), server.URL, "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with missing token value should return error")
	}
}

func TestFetchAmbientOIDCToken_LargeErrorBody(t *testing.T) {
	// Server returns error with a body larger than MaxErrorBodySize — should be truncated, not panic
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		// Write more than 4096 bytes
		for i := 0; i < 500; i++ {
			fmt.Fprint(w, "error detail padding ")
		}
	}))
	defer server.Close()

	_, err := FetchAmbientOIDCToken(context.Background(), server.URL, "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with forbidden response should return error")
	}
}

func TestFetchAmbientOIDCToken_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := FetchAmbientOIDCToken(ctx, "http://localhost:1", "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with cancelled context should return error")
	}
}

func TestFetchAmbientOIDCToken_InvalidResponseJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `not json`)
	}))
	defer server.Close()

	_, err := FetchAmbientOIDCToken(context.Background(), server.URL, "token")
	if err == nil {
		t.Error("FetchAmbientOIDCToken() with invalid JSON response should return error")
	}
}

// TestFetchDeviceCodeOIDCToken_CancelledContext pins the canceled-context
// path of the device-code flow. We assert errors.Is(err, context.Canceled) so
// a regression that collapses cancel into the deadline branch (or any
// unrelated failure) is caught here rather than silently passing.
func TestFetchDeviceCodeOIDCToken_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := FetchDeviceCodeOIDCToken(ctx, io.Discard)
	if err == nil {
		t.Fatal("FetchDeviceCodeOIDCToken() with canceled context should return error")
	}
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("FetchDeviceCodeOIDCToken() error = %v, want wrapped context.Canceled", err)
	}
}
