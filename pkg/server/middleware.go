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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/google/uuid"
)

// withMiddleware wraps handlers with common middleware
func (s *Server) withMiddleware(handler http.HandlerFunc) http.HandlerFunc {
	return s.metricsMiddleware(
		s.versionMiddleware(
			s.requestIDMiddleware(
				s.panicRecoveryMiddleware( // Recover first to prevent token waste on panics
					s.rateLimitMiddleware(
						s.bodyLimitMiddleware(
							s.timeoutMiddleware(
								s.loggingMiddleware(handler),
							),
						),
					),
				),
			),
		),
	)
}

// timeoutMiddleware applies a per-request context timeout per CLAUDE.md.
// Without this, a slow upstream call would only be bounded by the server's
// WriteTimeout, which kills the connection but leaves the goroutine running.
func (s *Server) timeoutMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), defaults.ServerHandlerTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// bodyLimitMiddleware bounds request body size as defense against
// unbounded JSON payloads. Handlers may apply a tighter cap themselves.
func (s *Server) bodyLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, defaults.ServerMaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	}
}

// Middleware implementations

// versionMiddleware handles API version negotiation and sets version header
func (s *Server) versionMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version := negotiateAPIVersion(r)
		setAPIVersionHeader(w, version)

		// Store version in context for handlers to access if needed
		ctx := context.WithValue(r.Context(), contextKeyAPIVersion, version)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// requestIDMiddleware extracts or generates request IDs
func (s *Server) requestIDMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = uuid.New().String()
		}

		// Validate UUID format if provided
		if _, err := uuid.Parse(requestID); err != nil {
			requestID = uuid.New().String()
		}

		// Store in context and response header
		ctx := context.WithValue(r.Context(), contextKeyRequestID, requestID)
		w.Header().Set("X-Request-Id", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// rateLimitMiddleware implements rate limiting
func (s *Server) rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.rateLimiter.Allow() {
			rateLimitRejects.Inc()
			w.Header().Set("Retry-After", defaults.ServerRetryAfterSeconds)
			WriteError(w, r, http.StatusTooManyRequests, aicrerrors.ErrCodeRateLimitExceeded,
				"Rate limit exceeded", true, map[string]any{
					"limit": s.config.RateLimit,
					"burst": s.config.RateLimitBurst,
				})
			return
		}

		// Add rate limit headers
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", int(s.config.RateLimit)))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", int(s.rateLimiter.Tokens())))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(defaults.ServerRateLimitWindow).Unix()))

		next.ServeHTTP(w, r)
	}
}

// panicRecoveryMiddleware recovers from panics
func (s *Server) panicRecoveryMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				panicRecoveries.Inc()
				var errMsg string
				switch v := err.(type) {
				case error:
					errMsg = v.Error()
				default:
					errMsg = fmt.Sprintf("%v", v)
				}
				slog.Error("panic recovered",
					"error", errMsg,
					"requestID", r.Context().Value(contextKeyRequestID),
					"path", r.URL.Path,
					"method", r.Method,
				)
				WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
					"Internal server error", true, nil)
			}
		}()
		next.ServeHTTP(w, r)
	}
}

// loggingMiddleware logs requests
func (s *Server) loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Context().Value(contextKeyRequestID)

		// Wrap response writer to track status code
		rw := newResponseWriter(w)

		slog.Debug("request started",
			"requestID", requestID,
			"method", r.Method,
			"path", r.URL.Path,
		)

		next.ServeHTTP(rw, r)

		duration := time.Since(start)
		slog.Info("request completed",
			"requestID", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.Status(),
			"duration", duration.String(),
		)
	}
}
