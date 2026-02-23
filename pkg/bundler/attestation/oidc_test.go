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

package attestation

import (
	"context"
	"fmt"
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
