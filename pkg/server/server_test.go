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

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		"/test": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}

	s := New(WithHandler(routes))
	if s == nil {
		t.Fatal("expected server instance, got nil")
		return
	}

	if s.config == nil {
		t.Error("expected config to be initialized")
	}

	if s.httpServer == nil {
		t.Error("expected httpServer to be initialized")
	}

	if s.rateLimiter == nil {
		t.Error("expected rateLimiter to be initialized")
	}
}

func TestHealthEndpoint(t *testing.T) {
	s := New()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", w.Header().Get("Content-Type"))
	}
}

func TestReadyEndpoint(t *testing.T) {
	s := New()

	tests := []struct {
		name           string
		ready          bool
		expectedStatus int
	}{
		{
			name:           "ready state",
			ready:          true,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "not ready state",
			ready:          false,
			expectedStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.setReady(tt.ready)

			req := httptest.NewRequest(http.MethodGet, "/ready", nil)
			w := httptest.NewRecorder()

			s.handleReady(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}
		})
	}
}

func TestReadyEndpoint_MethodNotAllowed(t *testing.T) {
	s := New()

	req := httptest.NewRequest(http.MethodPost, "/ready", nil)
	w := httptest.NewRecorder()

	s.handleReady(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestRateLimiting(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		"/test": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}

	// Create a custom config with very restrictive rate limiting
	cfg := parseConfig()
	cfg.RateLimit = 1      // 1 req/sec
	cfg.RateLimitBurst = 1 // burst of 1
	cfg.Handlers = routes

	s := New(withConfig(cfg))

	handler := s.withMiddleware(s.config.Handlers["/test"])

	// First request should succeed
	req1 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w1 := httptest.NewRecorder()
	handler(w1, req1)

	if w1.Code != http.StatusOK {
		t.Errorf("expected first request to succeed with status 200, got %d", w1.Code)
	}

	// Second request should be rate limited (bucket is empty)
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	w2 := httptest.NewRecorder()
	handler(w2, req2)

	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("expected rate limit error with status 429, got %d", w2.Code)
	}

	if w2.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header to be set")
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		"/test": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}

	s := New(WithHandler(routes))

	t.Run("generates request ID when not provided", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		w := httptest.NewRecorder()

		handler := s.requestIDMiddleware(s.config.Handlers["/test"])
		handler(w, req)

		requestID := w.Header().Get("X-Request-Id")
		if requestID == "" {
			t.Error("expected X-Request-Id header to be set")
		}
	})

	t.Run("uses provided request ID", func(t *testing.T) {
		expectedID := "550e8400-e29b-41d4-a716-446655440000"
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Request-Id", expectedID)
		w := httptest.NewRecorder()

		handler := s.requestIDMiddleware(s.config.Handlers["/test"])
		handler(w, req)

		requestID := w.Header().Get("X-Request-Id")
		if requestID != expectedID {
			t.Errorf("expected request ID %s, got %s", expectedID, requestID)
		}
	})

	t.Run("regenerates invalid UUID", func(t *testing.T) {
		invalidID := "not-a-valid-uuid"
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Request-Id", invalidID)
		w := httptest.NewRecorder()

		handler := s.requestIDMiddleware(s.config.Handlers["/test"])
		handler(w, req)

		requestID := w.Header().Get("X-Request-Id")
		if requestID == invalidID {
			t.Error("expected invalid UUID to be regenerated")
		}
	})
}

func TestPanicRecovery(t *testing.T) {
	panicHandler := func(_ http.ResponseWriter, _ *http.Request) {
		panic("test panic")
	}

	routes := map[string]http.HandlerFunc{
		"/panic": panicHandler,
	}

	s := New(WithHandler(routes))

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	w := httptest.NewRecorder()

	handler := s.panicRecoveryMiddleware(panicHandler)

	// Should not panic, should return 500
	handler(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected status %d after panic recovery, got %d", http.StatusInternalServerError, w.Code)
	}
}

func TestGracefulShutdown(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		"/test": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}

	cfg := parseConfig()
	cfg.Port = 18080 // Use a different port to avoid conflicts
	cfg.ShutdownTimeout = 100 * time.Millisecond
	cfg.Handlers = routes

	s := New(withConfig(cfg))

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- s.Start(ctx)
	}()

	// Wait for server to start by polling the listen address
	addr := fmt.Sprintf(":%d", cfg.Port)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Cancel context to trigger shutdown
	cancel()

	// Wait for shutdown to complete
	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("expected clean shutdown, got error: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("shutdown timed out")
	}
}

func TestDefaultRootHandler(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		"/api/v1/test": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}

	s := New(WithHandler(routes))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	// Get the root handler
	handler := s.config.Handlers["/"]
	if handler == nil {
		t.Fatal("expected default root handler to be created")
	}

	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	// Check that response contains routes
	body := w.Body.String()
	if body == "" {
		t.Error("expected non-empty response body")
	}

	// Should contain the test route
	if !strings.Contains(body, "/api/v1/test") {
		t.Error("expected response to contain /api/v1/test route")
	}
}

func TestDefaultRootHandlerMethodNotAllowed(t *testing.T) {
	s := New()

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()

	handler := s.config.Handlers["/"]
	handler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestCustomRootHandlerNotOverridden(t *testing.T) {
	customCalled := false
	routes := map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, _ *http.Request) {
			customCalled = true
			w.WriteHeader(http.StatusOK)
		},
	}

	s := New(WithHandler(routes))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	handler := s.config.Handlers["/"]
	handler(w, req)

	if !customCalled {
		t.Error("expected custom root handler to be called, not default")
	}
}

func TestHealthEndpoint_MethodNotAllowed(t *testing.T) {
	s := New()

	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()

	s.handleHealth(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
	}
}

func TestWithVersion(t *testing.T) {
	s := New(WithVersion("v1.2.3"))

	if s.config.Version != "v1.2.3" {
		t.Errorf("expected version v1.2.3, got %s", s.config.Version)
	}
}

func TestWithName(t *testing.T) {
	customName := "custom-api-server"
	s := New(WithName(customName))

	if s.config.Name != customName {
		t.Errorf("expected server name %s, got %s", customName, s.config.Name)
	}
}

func TestWithHandler(t *testing.T) {
	routes := map[string]http.HandlerFunc{
		"/api/test": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	}

	s := New(WithHandler(routes))

	if len(s.config.Handlers) < 1 {
		t.Error("expected handlers to be set")
	}

	if _, exists := s.config.Handlers["/api/test"]; !exists {
		t.Error("expected /api/test handler to exist")
	}

	// Should also have root handler added by default
	if _, exists := s.config.Handlers["/"]; !exists {
		t.Error("expected default root handler to be created")
	}
}

func TestWithConfigOption(t *testing.T) {
	cfg := parseConfig()
	cfg.Name = "test-server"
	cfg.Port = 9090
	cfg.RateLimit = 500

	s := New(withConfig(cfg))

	if s.config.Name != "test-server" {
		t.Errorf("expected name test-server, got %s", s.config.Name)
	}

	if s.config.Port != 9090 {
		t.Errorf("expected port 9090, got %d", s.config.Port)
	}

	if s.config.RateLimit != 500 {
		t.Errorf("expected rate limit 500, got %v", s.config.RateLimit)
	}
}

func TestDefaultServerName(t *testing.T) {
	s := New()

	if s.config.Name != "server" {
		t.Errorf("expected default name 'server', got %s", s.config.Name)
	}
}

func TestResponseWriter(t *testing.T) {
	t.Run("WriteHeader deduplication", func(t *testing.T) {
		w := httptest.NewRecorder()
		rw := newResponseWriter(w)

		rw.WriteHeader(http.StatusCreated)
		if rw.Status() != http.StatusCreated {
			t.Errorf("status = %d, want %d", rw.Status(), http.StatusCreated)
		}

		// Second WriteHeader should be ignored
		rw.WriteHeader(http.StatusBadRequest)
		if rw.Status() != http.StatusCreated {
			t.Errorf("status changed to %d after duplicate write", rw.Status())
		}
	})

	t.Run("Write auto-writes header", func(t *testing.T) {
		w := httptest.NewRecorder()
		rw := newResponseWriter(w)

		n, err := rw.Write([]byte("hello"))
		if err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if n != 5 {
			t.Errorf("Write() = %d, want 5", n)
		}
		if rw.Status() != http.StatusOK {
			t.Errorf("auto status = %d, want %d", rw.Status(), http.StatusOK)
		}
	})

	t.Run("default status is OK", func(t *testing.T) {
		w := httptest.NewRecorder()
		rw := newResponseWriter(w)

		if rw.Status() != http.StatusOK {
			t.Errorf("default status = %d, want %d", rw.Status(), http.StatusOK)
		}
	})
}
