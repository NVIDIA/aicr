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
	"bytes"
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/deprecation"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

func TestRequestIDMiddleware_GeneratesNewID(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	var capturedRequestID string
	handler := s.requestIDMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedRequestID = r.Context().Value(contextKeyRequestID).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	// Should generate a valid UUID
	if capturedRequestID == "" {
		t.Error("expected request ID to be generated")
	}
	if _, err := uuid.Parse(capturedRequestID); err != nil {
		t.Errorf("expected valid UUID, got: %s", capturedRequestID)
	}

	// Should set the header
	if rec.Header().Get("X-Request-Id") != capturedRequestID {
		t.Errorf("expected X-Request-Id header to be %s, got %s",
			capturedRequestID, rec.Header().Get("X-Request-Id"))
	}
}

func TestRequestIDMiddleware_UsesProvidedID(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	providedID := uuid.New().String()
	var capturedRequestID string
	handler := s.requestIDMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedRequestID = r.Context().Value(contextKeyRequestID).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-Id", providedID)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if capturedRequestID != providedID {
		t.Errorf("expected request ID %s, got %s", providedID, capturedRequestID)
	}
}

func TestRequestIDMiddleware_ReplacesInvalidID(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	var capturedRequestID string
	handler := s.requestIDMiddleware(func(w http.ResponseWriter, r *http.Request) {
		capturedRequestID = r.Context().Value(contextKeyRequestID).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-Id", "invalid-not-a-uuid")
	rec := httptest.NewRecorder()

	handler(rec, req)

	// Should replace with a valid UUID
	if _, err := uuid.Parse(capturedRequestID); err != nil {
		t.Errorf("expected valid UUID, got: %s", capturedRequestID)
	}
	if capturedRequestID == "invalid-not-a-uuid" {
		t.Error("expected invalid UUID to be replaced")
	}
}

func TestVersionMiddleware_SetsHeader(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	handler := s.versionMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Header().Get("X-API-Version") == "" {
		t.Error("expected API version header to be set")
	}
}

func TestVersionMiddleware_StoresInContext(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	var capturedVersion string
	handler := s.versionMiddleware(func(w http.ResponseWriter, r *http.Request) {
		v := r.Context().Value(contextKeyAPIVersion)
		if v != nil {
			capturedVersion = v.(string)
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if capturedVersion == "" {
		t.Error("expected API version to be stored in context")
	}
}

func TestRateLimitMiddleware_AllowsRequests(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	called := false
	handler := s.rateLimitMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if !called {
		t.Error("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Should set rate limit headers
	if rec.Header().Get("X-RateLimit-Limit") == "" {
		t.Error("expected X-RateLimit-Limit header")
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("expected X-RateLimit-Remaining header")
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("expected X-RateLimit-Reset header")
	}
}

func TestRateLimitMiddleware_RejectsWhenExceeded(t *testing.T) {
	// Create a limiter with no capacity
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(0, 0),
	}

	called := false
	handler := s.rateLimitMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if called {
		t.Error("handler should not be called when rate limited")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header when rate limited")
	}
}

func TestPanicRecoveryMiddleware_RecoversPanic(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	handler := s.panicRecoveryMiddleware(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	// Should not panic
	handler(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}
}

func TestPanicRecoveryMiddleware_PassesNormalRequests(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	called := false
	handler := s.panicRecoveryMiddleware(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if !called {
		t.Error("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestLoggingMiddleware_TracksRequestID(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	// First wrap with request ID middleware to populate context
	innerHandler := s.loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := s.requestIDMiddleware(innerHandler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	// Should complete without error
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestLoggingMiddleware_TracksStatusCode(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	tests := []struct {
		name           string
		expectedStatus int
	}{
		{"OK", http.StatusOK},
		{"Created", http.StatusCreated},
		{"BadRequest", http.StatusBadRequest},
		{"NotFound", http.StatusNotFound},
		{"InternalError", http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := s.loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.expectedStatus)
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			rec := httptest.NewRecorder()

			handler(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, rec.Code)
			}
		})
	}
}

func TestMiddlewareChain_PropagatesContext(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	var hasRequestID, hasAPIVersion bool
	handler := s.withMiddleware(func(w http.ResponseWriter, r *http.Request) {
		hasRequestID = r.Context().Value(contextKeyRequestID) != nil
		hasAPIVersion = r.Context().Value(contextKeyAPIVersion) != nil
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if !hasRequestID {
		t.Error("expected request ID in context")
	}
	if !hasAPIVersion {
		t.Error("expected API version in context")
	}
}

func TestMiddlewareChain_SetsAllHeaders(t *testing.T) {
	s := &Server{
		config:      parseConfig(),
		rateLimiter: rate.NewLimiter(100, 200),
	}

	handler := s.withMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler(rec, req)

	expectedHeaders := []string{
		"X-Request-Id",
		"X-RateLimit-Limit",
		"X-RateLimit-Remaining",
		"X-RateLimit-Reset",
		"X-API-Version",
	}

	for _, header := range expectedHeaders {
		if rec.Header().Get(header) == "" {
			t.Errorf("expected header %s to be set", header)
		}
	}
}

func TestTimeoutMiddleware_AppliesDeadline(t *testing.T) {
	t.Parallel()

	s := &Server{config: parseConfig(), rateLimiter: rate.NewLimiter(100, 200)}

	var observedDeadlineSet bool
	handler := s.timeoutMiddleware(func(_ http.ResponseWriter, r *http.Request) {
		_, observedDeadlineSet = r.Context().Deadline()
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if !observedDeadlineSet {
		t.Fatal("timeoutMiddleware should attach a context deadline")
	}
}

func TestTimeoutMiddleware_DeadlineMatchesDefault(t *testing.T) {
	t.Parallel()

	s := &Server{config: parseConfig(), rateLimiter: rate.NewLimiter(100, 200)}

	var remaining time.Duration
	handler := s.timeoutMiddleware(func(_ http.ResponseWriter, r *http.Request) {
		if dl, ok := r.Context().Deadline(); ok {
			remaining = time.Until(dl)
		}
	})

	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	// Deadline should be near ServerHandlerTimeout (allow modest slack for test runtime).
	if remaining <= 0 || remaining > defaults.ServerHandlerTimeout {
		t.Errorf("deadline remaining %v out of expected (0, %v]", remaining, defaults.ServerHandlerTimeout)
	}
	if defaults.ServerHandlerTimeout-remaining > 5*time.Second {
		t.Errorf("deadline drift %v exceeds 5s — middleware not using ServerHandlerTimeout?",
			defaults.ServerHandlerTimeout-remaining)
	}
}

func TestTimeoutMiddleware_PropagatesCancellation(t *testing.T) {
	t.Parallel()

	s := &Server{config: parseConfig(), rateLimiter: rate.NewLimiter(100, 200)}

	var ctxErr error
	handler := s.timeoutMiddleware(func(_ http.ResponseWriter, r *http.Request) {
		// Caller's context is canceled before the handler runs;
		// the middleware-derived context inherits cancellation.
		ctxErr = r.Context().Err()
	})

	parentCtx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parentCtx)
	handler(httptest.NewRecorder(), req)

	if ctxErr == nil {
		t.Fatal("expected canceled context to propagate through timeoutMiddleware")
	}
}

func TestBodyLimitMiddleware_AcceptsSmallBody(t *testing.T) {
	t.Parallel()

	s := &Server{config: parseConfig(), rateLimiter: rate.NewLimiter(100, 200)}

	const sentinel = "small-body-ok"
	body := bytes.NewBufferString(sentinel)

	var read string
	handler := s.bodyLimitMiddleware(func(_ http.ResponseWriter, r *http.Request) {
		buf := make([]byte, len(sentinel)+1)
		n, _ := r.Body.Read(buf)
		read = string(buf[:n])
	})

	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if read != sentinel {
		t.Errorf("expected to read %q, got %q", sentinel, read)
	}
}

func TestBodyLimitMiddleware_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	s := &Server{config: parseConfig(), rateLimiter: rate.NewLimiter(100, 200)}

	// 1 byte over the cap so the read fails on the very last byte.
	body := strings.NewReader(strings.Repeat("x", int(defaults.ServerMaxBodyBytes)+1))

	var readErr error
	handler := s.bodyLimitMiddleware(func(_ http.ResponseWriter, r *http.Request) {
		// Drain — http.MaxBytesReader returns *http.MaxBytesError once cap is exceeded.
		buf := make([]byte, 4096)
		for {
			_, err := r.Body.Read(buf)
			if err != nil {
				readErr = err
				return
			}
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/", body)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if readErr == nil {
		t.Fatal("expected read error for oversized body")
	}
	if _, ok := stderrors.AsType[*http.MaxBytesError](readErr); !ok {
		t.Errorf("expected *http.MaxBytesError, got %T: %v", readErr, readErr)
	}
}

func TestBodyLimitMiddleware_NilBodyIsHandled(t *testing.T) {
	t.Parallel()

	s := &Server{config: parseConfig(), rateLimiter: rate.NewLimiter(100, 200)}

	called := false
	handler := s.bodyLimitMiddleware(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Body = nil
	handler(httptest.NewRecorder(), req)

	if !called {
		t.Fatal("handler should be invoked even when Body is nil")
	}
}

// TestDeprecationMiddleware covers the REST arm of the deprecation channel
// (RELEASING.md). No route is deprecated in the shipped configuration yet, so
// the mechanism is exercised here against a route this test marks itself.
func TestDeprecationMiddleware(t *testing.T) {
	deprecated := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	sunset := time.Date(2026, time.November, 11, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		routes      map[string]deprecation.Notice
		path        string
		wantHeaders bool
	}{
		{
			// The subject/replacement pair is synthetic. Nothing is deprecated
			// in the shipped configuration, so this marks a real route against
			// a stand-in successor purely to drive the middleware.
			name: "marked route carries the headers",
			routes: map[string]deprecation.Notice{
				"/v1/recipe": {
					Subject:     "/v1/recipe",
					Replacement: "/v1/recipe with an explicit profile",
					RemovedIn:   "v0.25",
					Deprecated:  deprecated,
					Sunset:      sunset,
				},
			},
			path:        "/v1/recipe",
			wantHeaders: true,
		},
		{
			// The map is keyed by exact path, so a marked route must not leak
			// onto a sibling. The notice carries a full set of dates: without
			// them the absence of headers would be explained by the zero
			// Deprecated time rather than by the path not matching.
			name: "unmarked route is untouched",
			routes: map[string]deprecation.Notice{
				"/v1/recipe": {
					Subject:    "/v1/recipe",
					RemovedIn:  "v0.25",
					Deprecated: deprecated,
					Sunset:     sunset,
				},
			},
			path:        "/v1/bundle",
			wantHeaders: false,
		},
		{
			name:        "no deprecated routes configured",
			routes:      nil,
			path:        "/v1/recipe",
			wantHeaders: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := parseConfig()
			cfg.DeprecatedRoutes = tt.routes
			s := &Server{config: cfg, rateLimiter: rate.NewLimiter(100, 200)}

			handler := s.deprecationMiddleware(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			got := rec.Header().Get("Deprecation") != ""
			if got != tt.wantHeaders {
				t.Errorf("Deprecation header present = %v, want %v", got, tt.wantHeaders)
			}
			if !tt.wantHeaders {
				return
			}
			if rec.Header().Get("Sunset") == "" {
				t.Error("Sunset header missing on a notice carrying a date")
			}
			if rec.Header().Get("Link") == "" {
				t.Error("Link header missing")
			}
		})
	}
}

// TestDeprecationHeadersSurviveRateLimitRejection is the ordering assertion.
//
// rateLimitMiddleware writes a 429 and returns without calling next, so any
// header-setting layer nested inside it is skipped entirely on a throttled
// request. A client backing off a deprecated endpoint is precisely the one that
// needs to learn the endpoint is going away, and it may never see a 200.
//
// A panicking handler does not test this: the panic happens after the inner
// layers have already run, so the headers are set either way. Rate limiting is
// the short-circuit that actually discriminates.
func TestDeprecationHeadersSurviveRateLimitRejection(t *testing.T) {
	cfg := parseConfig()
	cfg.DeprecatedRoutes = map[string]deprecation.Notice{
		"/v1/recipe": {
			Subject:   "/v1/recipe",
			RemovedIn: "v0.25",
			// A date is required for the Deprecation header to be emitted at
			// all under RFC 9745, so the assertion below needs one to be
			// testing ordering rather than the absence of a date.
			Deprecated: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	// Zero limit and zero burst: every request is rejected.
	s := &Server{config: cfg, rateLimiter: rate.NewLimiter(0, 0)}

	var handlerRan bool
	handler := s.withMiddleware(func(w http.ResponseWriter, _ *http.Request) {
		handlerRan = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/recipe", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d — the rate limiter did not short-circuit, "+
			"so this test is not exercising the ordering it claims to",
			rec.Code, http.StatusTooManyRequests)
	}
	if handlerRan {
		t.Fatal("handler ran despite the 429; the short-circuit did not happen")
	}
	if rec.Header().Get("Deprecation") == "" {
		t.Error("Deprecation header lost on a rate-limited response; the middleware " +
			"is ordered inside rateLimitMiddleware")
	}
}
