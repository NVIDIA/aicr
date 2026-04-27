// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package openshell

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateFetchURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://example.com/policy.json", false},
		{"http rejected", "http://example.com/policy.json", true},
		{"file scheme rejected", "file:///etc/passwd", true},
		{"loopback rejected", "https://127.0.0.1/policy.json", true},
		{"private IP rejected", "https://10.0.0.1/policy.json", true},
		{"link-local rejected", "https://169.254.1.1/policy.json", true},
		{"hostname allowed", "https://policy.example.com/v1/doc", false},
		{"empty string", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFetchURL(tt.url, false)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFetchURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestValidateFetchURLAllowPrivate(t *testing.T) {
	// With allowPrivate, loopback and private IPs should pass
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"loopback allowed", "https://127.0.0.1/policy.json", false},
		{"private IP allowed", "https://10.0.0.1/policy.json", false},
		{"http still rejected", "http://127.0.0.1/policy.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFetchURL(tt.url, true)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateFetchURL(%q, allowPrivate) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestFetcherCaching(t *testing.T) {
	callCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"1.0","agent":"test.example.com","rules":{}}`))
	}))
	defer srv.Close()

	f := NewFetcher(
		WithHTTPClient(srv.Client()),
		WithCacheTTL(1*time.Hour),
		WithAllowPrivate(true),
	)

	ctx := context.Background()

	// First fetch hits the server
	doc, err := f.Fetch(ctx, srv.URL+"/policy.json")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if doc.Agent != "test.example.com" {
		t.Errorf("agent = %q, want %q", doc.Agent, "test.example.com")
	}
	if callCount != 1 {
		t.Errorf("expected 1 server call, got %d", callCount)
	}

	// Second fetch should hit cache
	doc2, err := f.Fetch(ctx, srv.URL+"/policy.json")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if doc2.Agent != "test.example.com" {
		t.Errorf("agent = %q, want %q", doc2.Agent, "test.example.com")
	}
	if callCount != 1 {
		t.Errorf("expected 1 server call after cached fetch, got %d", callCount)
	}
}

func TestFetcherRejectsNonJSON(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	f := NewFetcher(WithHTTPClient(srv.Client()), WithAllowPrivate(true))
	_, err := f.Fetch(context.Background(), srv.URL+"/policy.json")
	if err == nil {
		t.Error("expected error for non-JSON content-type")
	}
}

func TestFetcherRejectsOversized(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write more than maxBytes
		data := make([]byte, 1024)
		for i := range data {
			data[i] = 'x'
		}
		w.Write(data)
	}))
	defer srv.Close()

	f := NewFetcher(
		WithHTTPClient(srv.Client()),
		WithMaxBytes(100), // tiny limit
		WithAllowPrivate(true),
	)
	_, err := f.Fetch(context.Background(), srv.URL+"/policy.json")
	if err == nil {
		t.Error("expected error for oversized document")
	}
}

func TestFetcherSSRFDialBlocksPrivateIP(t *testing.T) {
	// Exercises the post-resolution dial-time SSRF guard. URL validation
	// only catches IP literals; this test points the URL at the hostname
	// "localhost" so URL validation accepts it, then expects the safe
	// transport to look up localhost, see 127.0.0.1, and refuse to dial.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"1.0","agent":"x","rules":{}}`))
	}))
	defer srv.Close()

	// Build a transport with our SSRF guard but trust the httptest CA.
	transport := newSafeTransport(false)
	transport.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig

	f := NewFetcher()
	f.client = &http.Client{Transport: transport, Timeout: 2 * time.Second}

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	// localhost resolves to 127.0.0.1 (loopback) — dial guard must reject.
	policyURL := "https://localhost:" + port + "/policy.json"

	if _, err := f.Fetch(context.Background(), policyURL); err == nil {
		t.Fatal("expected dial-time SSRF rejection, got nil")
	}
}

func TestFetcherRejectsNonHTTPSRedirect(t *testing.T) {
	// Verify CheckRedirect rejects a Location header that downgrades to http.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://example.com/policy.json")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	f := NewFetcher(WithHTTPClient(srv.Client()), WithAllowPrivate(true))
	// httptest.Server.Client() does not install a CheckRedirect; add ours.
	f.client.CheckRedirect = checkRedirect

	if _, err := f.Fetch(context.Background(), srv.URL+"/policy.json"); err == nil {
		t.Fatal("expected CheckRedirect to reject http:// redirect")
	}
}

func TestIsBlockedIP(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.5", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		{"169.254.1.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"fe80::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("could not parse %q", tt.ip)
			}
			if got := isBlockedIP(ip); got != tt.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

func TestFetcherRejectsNon200(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewFetcher(WithHTTPClient(srv.Client()), WithAllowPrivate(true))
	_, err := f.Fetch(context.Background(), srv.URL+"/policy.json")
	if err == nil {
		t.Error("expected error for 404 response")
	}
}
