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
	"strconv"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/deprecation"
	"golang.org/x/time/rate"
)

// config holds server configuration
type config struct {
	// Server identity
	Name    string
	Version string

	// Additional Handlers to be added to the server
	Handlers map[string]http.HandlerFunc

	// DeprecatedRoutes marks registered routes as deprecated, keyed by the
	// exact path in Handlers. Matching responses carry the Deprecation,
	// Sunset, and Link headers described in RELEASING.md's deprecation
	// policy. Empty by default: no REST path family has been deprecated yet,
	// and the /v1/* disposition is still open (#2112).
	DeprecatedRoutes map[string]deprecation.Notice

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

	// AllowVendorCharts opts the server into honoring vendor-charts=true on
	// bundle requests. Off by default; the bundle handler rejects the
	// vendor path with 400 when disabled. Set via EnvAllowVendorCharts.
	AllowVendorCharts bool
}

// parseConfig returns sensible defaults
func parseConfig() *config {
	cfg := &config{
		Name:            "server",
		Version:         "undefined",
		Address:         defaults.ServerDefaultBindAddress,
		Port:            defaults.ServerDefaultPort,
		RateLimit:       defaults.ServerDefaultRateLimit,
		RateLimitBurst:  defaults.ServerDefaultRateLimitBurst,
		ReadTimeout:     defaults.ServerReadTimeout,
		WriteTimeout:    defaults.ServerWriteTimeout,
		IdleTimeout:     defaults.ServerIdleTimeout,
		ShutdownTimeout: defaults.ServerShutdownTimeout,
	}

	// Address override — empty string binds all interfaces, so operators
	// must set this explicitly to expose beyond loopback.
	if addr, ok := os.LookupEnv(defaults.EnvServerAddress); ok {
		cfg.Address = addr
	}

	// Override with environment variables if set
	if portStr := os.Getenv(defaults.EnvServerPort); portStr != "" {
		var port int
		if _, err := fmt.Sscanf(portStr, "%d", &port); err == nil {
			cfg.Port = port
		} else {
			slog.Warn("failed to parse port env var, using default",
				"var", defaults.EnvServerPort, "value", portStr, "error", err)
		}
	}

	// Allow customization of shutdown timeout to match K8s eviction grace period
	if shutdownStr := os.Getenv(defaults.EnvServerShutdownTimeoutSeconds); shutdownStr != "" {
		var seconds int
		if _, err := fmt.Sscanf(shutdownStr, "%d", &seconds); err == nil && seconds > 0 {
			cfg.ShutdownTimeout = time.Duration(seconds) * time.Second
		} else {
			slog.Warn("failed to parse shutdown timeout env var, using default",
				"var", defaults.EnvServerShutdownTimeoutSeconds, "value", shutdownStr, "error", err)
		}
	}

	// Opt-in vendor-charts. Same helper used by Serve() when wiring the
	// bundle handler, so both surfaces read exactly one env value.
	cfg.AllowVendorCharts = allowVendorChartsFromEnv()

	return cfg
}

// allowVendorChartsFromEnv parses defaults.EnvAllowVendorCharts. Missing,
// empty, or unparseable values fail closed to false and warn — the safer
// default matches the missing-env case exactly, so a typo in the env value
// never silently enables the vendor path.
func allowVendorChartsFromEnv() bool {
	raw := os.Getenv(defaults.EnvAllowVendorCharts)
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("failed to parse allow-vendor-charts env var, staying off",
			"var", defaults.EnvAllowVendorCharts, "value", raw, "error", err)
		return false
	}
	return v
}
