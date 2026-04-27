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
	"os"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"golang.org/x/time/rate"
)

// LifecycleHook is a function executed at server lifecycle events (start/shutdown).
// OnStart hooks receive the parent server context.
// OnShutdown hooks receive a context bounded by the server's shutdown timeout.
type LifecycleHook func(ctx context.Context) error

// ResolvePort returns the port to bind to, honoring the PORT environment
// variable when set and falling back to defaultPort otherwise. A malformed
// PORT value is logged and ignored. This is the single source of truth for
// the server port across the API server and any sidecars (e.g. discovery
// publishers) that need to register the same port.
func ResolvePort(defaultPort int) int {
	portStr := os.Getenv("PORT")
	if portStr == "" {
		return defaultPort
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 || port > 65535 {
		slog.Warn("invalid PORT env var, using default",
			"value", portStr, "default", defaultPort, "error", err)
		return defaultPort
	}
	return port
}

// config holds server configuration
type config struct {
	// Server identity
	Name    string
	Version string

	// Additional Handlers to be added to the server
	Handlers map[string]http.HandlerFunc

	// Server configuration
	Address string
	Port    int

	// Rate limiting configuration
	RateLimit      rate.Limit // requests per second
	RateLimitBurst int        // burst size

	// Timeouts
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration

	// Lifecycle hooks
	OnStart    []LifecycleHook // Executed after the server is ready to accept traffic
	OnShutdown []LifecycleHook // Executed before the HTTP server shuts down
}

// parseConfig returns sensible defaults
func parseConfig() *config {
	cfg := &config{
		Name:            "server",
		Version:         "undefined",
		Address:         "",
		Port:            defaults.ServerDefaultPort,
		RateLimit:       defaults.ServerDefaultRateLimit,
		RateLimitBurst:  defaults.ServerDefaultRateLimitBurst,
		ReadTimeout:     defaults.ServerReadTimeout,
		WriteTimeout:    defaults.ServerWriteTimeout,
		IdleTimeout:     defaults.ServerIdleTimeout,
		ShutdownTimeout: defaults.ServerShutdownTimeout,
	}

	// Override with environment variables if set
	cfg.Port = ResolvePort(cfg.Port)

	// Allow customization of shutdown timeout to match K8s eviction grace period
	if shutdownStr := os.Getenv("SHUTDOWN_TIMEOUT_SECONDS"); shutdownStr != "" {
		var seconds int
		if _, err := fmt.Sscanf(shutdownStr, "%d", &seconds); err == nil && seconds > 0 {
			cfg.ShutdownTimeout = time.Duration(seconds) * time.Second
		} else {
			slog.Warn("failed to parse SHUTDOWN_TIMEOUT_SECONDS env var, using default", "value", shutdownStr, "error", err)
		}
	}

	return cfg
}
