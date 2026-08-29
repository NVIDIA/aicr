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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/deprecation"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

// Server represents the HTTP server for handling API requests.
// It includes rate limiting, health checks, metrics, and graceful shutdown capabilities.
type Server struct {
	config      *config
	httpServer  *http.Server
	rateLimiter *rate.Limiter
	mu          sync.RWMutex
	ready       bool

	// routePaths records every pattern registered on the mux, in registration
	// order. http.ServeMux exposes no way to enumerate its patterns, so without
	// this the OpenAPI conformance tests would have to re-derive the route set
	// from a hand-maintained list -- and a route added directly here, which is
	// exactly where the system endpoints live, would be invisible to them.
	routePaths []string
}

// Option is a functional option for configuring Server instances.
type Option func(*Server)

// withConfig returns an Option that sets a custom configuration for the Server.
func withConfig(cfg *config) Option {
	return func(s *Server) {
		s.config = cfg
	}
}

// WithName returns an Option that sets the server name in the configuration.
func WithName(name string) Option {
	return func(s *Server) {
		s.config.Name = name
	}
}

// WithVersion returns an Option that sets the server version in the configuration.
func WithVersion(version string) Option {
	return func(s *Server) {
		s.config.Version = version
	}
}

// WithHandler returns an Option that adds custom HTTP handlers to the server.
// The map keys are URL paths and values are the corresponding handler functions.
func WithHandler(handlers map[string]http.HandlerFunc) Option {
	return func(s *Server) {
		s.config.Handlers = handlers
	}
}

// WithDeprecatedRoutes returns an Option marking registered routes as
// deprecated. Keys must match the paths passed to WithHandler exactly.
// Responses from a marked route carry the Deprecation, Sunset, and Link headers
// defined by the deprecation policy in RELEASING.md, so a client learns the
// endpoint is going away without having to read a release note.
func WithDeprecatedRoutes(routes map[string]deprecation.Notice) Option {
	return func(s *Server) {
		s.config.DeprecatedRoutes = routes
	}
}

// New creates a new Server instance with the provided functional options.
// It parses environment configuration, sets up rate limiting, and configures
// the HTTP server with health checks, metrics, and custom handlers.
func New(opts ...Option) *Server {
	config := parseConfig()

	s := &Server{
		config:      config,
		rateLimiter: rate.NewLimiter(config.RateLimit, config.RateLimitBurst),
	}

	// Apply options
	for _, opt := range opts {
		opt(s)
	}

	// Re-create rate limiter if config was changed
	s.rateLimiter = rate.NewLimiter(s.config.RateLimit, s.config.RateLimitBurst)

	// Setup HTTP server
	mux := http.NewServeMux()

	// System endpoints (no rate limiting)
	s.handle(mux, "/health", http.HandlerFunc(s.handleHealth))
	s.handle(mux, "/ready", http.HandlerFunc(s.handleReady))
	s.handle(mux, "/metrics", readOnly(promhttp.Handler()))

	// setup root handler
	s.configureRootHandler()

	// setup application routes
	for path, handler := range s.config.Handlers {
		s.handle(mux, path, s.withMiddleware(handler))
	}

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", s.config.Address, s.config.Port),
		Handler:           mux,
		ReadTimeout:       s.config.ReadTimeout,
		WriteTimeout:      s.config.WriteTimeout,
		IdleTimeout:       s.config.IdleTimeout,
		MaxHeaderBytes:    defaults.ServerMaxHeaderBytes,    // 64KB limit to prevent header-based attacks
		ReadHeaderTimeout: defaults.ServerReadHeaderTimeout, // Prevent slow header attacks
	}

	return s
}

// handle registers a pattern on the mux and records it, so the set of served
// routes has one source of truth rather than a list someone must remember to
// update.
func (s *Server) handle(mux *http.ServeMux, pattern string, h http.Handler) {
	mux.Handle(pattern, h)
	s.routePaths = append(s.routePaths, pattern)
}

// SetReady marks the server as ready to serve traffic or not.
func (s *Server) setReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = ready
}

// Start starts the HTTP server and listens for incoming requests.
func (s *Server) Start(ctx context.Context) error {
	s.setReady(true)

	slog.Debug("server start", "port", s.httpServer.Addr)

	// Start server in goroutine. Always send on errChan (nil for clean exit)
	// so the consumer below is deterministic even when the server crashes
	// after Shutdown is initiated.
	errChan := make(chan error, 1)
	go func() {
		err := s.httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errChan <- err
	}()

	// Wait for context cancellation or server error
	select {
	case <-ctx.Done():
		// Use fresh context for shutdown - parent context is already canceled
		return s.Shutdown(context.Background()) //nolint:contextcheck // intentional: need fresh context for graceful shutdown
	case err := <-errChan:
		if err == nil {
			return nil
		}
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "http server error", err)
	}
}

// Shutdown gracefully shuts down the server within the given context.
func (s *Server) Shutdown(ctx context.Context) error {
	s.setReady(false)

	shutdownCtx, cancel := context.WithTimeout(ctx, s.config.ShutdownTimeout)
	defer cancel()

	slog.Info("shutting down server")
	return s.httpServer.Shutdown(shutdownCtx)
}

// RunWithConfig starts the server with custom configuration and graceful shutdown handling.
func (s *Server) Run(ctx context.Context) error {
	slog.Debug("server config",
		slog.String("address", s.httpServer.Addr),
		slog.Int("port", s.config.Port),
		slog.Any("rateLimit", s.config.RateLimit),
		slog.Int("rateLimitBurst", s.config.RateLimitBurst),
		slog.Duration("readTimeout", s.config.ReadTimeout),
		slog.Duration("writeTimeout", s.config.WriteTimeout),
		slog.Duration("idleTimeout", s.config.IdleTimeout),
		slog.Duration("shutdownTimeout", s.config.ShutdownTimeout),
	)

	// Setup graceful shutdown
	notifCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Use errgroup for concurrent operations
	g, gctx := errgroup.WithContext(notifCtx)

	// Start HTTP server
	g.Go(func() error {
		return s.Start(gctx)
	})

	// Wait for completion or error
	if err := g.Wait(); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "server error", err)
	}

	slog.Debug("server stopped gracefully")
	return nil
}

// configureRootHandler creates a default handler for the root path that lists available routes
func (s *Server) configureRootHandler() {
	// Initialize handlers map if nil
	if s.config.Handlers == nil {
		s.config.Handlers = make(map[string]http.HandlerFunc)
	}

	if _, exists := s.config.Handlers["/"]; !exists {
		s.config.Handlers["/"] = func(w http.ResponseWriter, r *http.Request) {
			// http.ServeMux routes ANY unmatched path to the "/" handler.
			// Without this guard, GET /garbage would receive the route
			// directory — both confusing for clients (200 instead of 404)
			// and a free reconnaissance surface for endpoint probing.
			if r.URL.Path != "/" {
				WriteError(w, r, http.StatusNotFound, aicrerrors.ErrCodeNotFound,
					"Route not found", false, map[string]any{
						keyPath: r.URL.Path,
					})
				return
			}

			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
					"Method not allowed", false, map[string]any{
						keyMethod: r.Method,
					})
				return
			}

			routes := make([]string, 0)
			for path := range s.config.Handlers {
				if path != "/" { // Don't include self
					routes = append(routes, path)
				}
			}
			// Sort so the response is byte-stable across runs — Go map
			// iteration order is randomized, which otherwise breaks golden
			// tests, response-diff probes, and CDN cache keys.
			sort.Strings(routes)

			response := map[string]any{
				keyService: s.config.Name,
				"version":  s.config.Version,
				"routes":   routes,
			}

			serializer.RespondJSON(w, http.StatusOK, response)
		}
	}
}

// readOnly restricts a handler to GET and HEAD, answering 405 otherwise.
//
// promhttp.Handler does no method filtering, so /metrics answered 200 to
// DELETE, PUT, POST, PATCH, OPTIONS and TRACE alike. That contradicted
// api/aicr/v1/server.yaml and left undocumented operations on a public
// endpoint.
//
// HEAD is allowed, not rejected. RFC 9110 §9.1 makes GET and HEAD mandatory for
// a general-purpose server, and an earlier revision of this guard narrowed
// /metrics to GET alone — a behavior change against a monitoring endpoint some
// probes reach with HEAD. The spec declares head: alongside get: so the
// contract and the server agree.
//
// The 405 body is the structured envelope WriteError produces, matching
// handleHealth and handleReady above. A client parsing the error for one system
// endpoint should not get a bare string from another for the same condition.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
			WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
				"Method not allowed", false, map[string]any{keyMethod: r.Method})
			return
		}
		next.ServeHTTP(w, r)
	})
}
