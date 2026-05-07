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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"golang.org/x/time/rate"
)

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
}

// parseConfig returns sensible defaults
func parseConfig() *config {
	cfg := &config{
		Name:            "server",
		Version:         "undefined",
		Address:         "",
		Port:            8080,
		RateLimit:       defaults.ServerDefaultRateLimit,
		RateLimitBurst:  defaults.ServerDefaultRateLimitBurst,
		ReadTimeout:     defaults.ServerReadTimeout,
		WriteTimeout:    defaults.ServerWriteTimeout,
		IdleTimeout:     defaults.ServerIdleTimeout,
		ShutdownTimeout: defaults.ServerShutdownTimeout,
	}

	// Override with environment variables if set
	if portStr := os.Getenv("PORT"); portStr != "" {
		var port int
		if _, err := fmt.Sscanf(portStr, "%d", &port); err == nil {
			cfg.Port = port
		} else {
			slog.Warn("failed to parse PORT env var, using default", "value", portStr, "error", err)
		}
	}

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
