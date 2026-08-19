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
	"os"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestParseConfig(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		// Ensure env overrides from other tests / the developer shell do not
		// leak into the default-config assertion.
		t.Setenv(defaults.EnvServerAddress, "")
		os.Unsetenv(defaults.EnvServerAddress)
		t.Setenv(defaults.EnvAllowVendorCharts, "")
		os.Unsetenv(defaults.EnvAllowVendorCharts)

		cfg := parseConfig()

		// Default bind is empty (== all interfaces) so kubelet probes and
		// kube-proxy can reach the container on the pod IP; operators
		// tighten via EnvServerAddress.
		if cfg.Address != defaults.ServerDefaultBindAddress {
			t.Errorf("expected default address %q, got %q", defaults.ServerDefaultBindAddress, cfg.Address)
		}
		if cfg.Address != "" {
			t.Errorf("default bind must be all-interfaces to remain reachable by kubelet probes; got %q", cfg.Address)
		}

		if cfg.Port != 8080 {
			t.Errorf("expected port 8080, got %d", cfg.Port)
		}

		if cfg.AllowVendorCharts {
			t.Errorf("expected AllowVendorCharts false by default, got true")
		}

		if cfg.RateLimit != 100 {
			t.Errorf("expected rate limit 100, got %v", cfg.RateLimit)
		}

		if cfg.RateLimitBurst != 200 {
			t.Errorf("expected rate limit burst 200, got %d", cfg.RateLimitBurst)
		}

		if cfg.ReadTimeout != 10*time.Second {
			t.Errorf("expected read timeout 10s, got %v", cfg.ReadTimeout)
		}

		if cfg.WriteTimeout != 90*time.Second {
			t.Errorf("expected write timeout 90s, got %v", cfg.WriteTimeout)
		}

		if cfg.IdleTimeout != 120*time.Second {
			t.Errorf("expected idle timeout 120s, got %v", cfg.IdleTimeout)
		}

		if cfg.ShutdownTimeout != 30*time.Second {
			t.Errorf("expected shutdown timeout 30s, got %v", cfg.ShutdownTimeout)
		}
	})

	t.Run("custom port from environment", func(t *testing.T) {
		os.Setenv("PORT", "9090")
		defer os.Unsetenv("PORT")

		cfg := parseConfig()

		if cfg.Port != 9090 {
			t.Errorf("expected port 9090 from env, got %d", cfg.Port)
		}
	})

	t.Run("invalid port from environment uses default", func(t *testing.T) {
		os.Setenv("PORT", "invalid")
		defer os.Unsetenv("PORT")

		cfg := parseConfig()

		if cfg.Port != 8080 {
			t.Errorf("expected default port 8080 for invalid env, got %d", cfg.Port)
		}
	})

	t.Run("custom shutdown timeout from environment", func(t *testing.T) {
		t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "60")

		cfg := parseConfig()

		if cfg.ShutdownTimeout != 60*time.Second {
			t.Errorf("expected shutdown timeout 60s, got %v", cfg.ShutdownTimeout)
		}
	})

	t.Run("invalid shutdown timeout uses default", func(t *testing.T) {
		t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "invalid")

		cfg := parseConfig()

		if cfg.ShutdownTimeout != 30*time.Second {
			t.Errorf("expected default shutdown timeout 30s for invalid env, got %v", cfg.ShutdownTimeout)
		}
	})

	t.Run("zero shutdown timeout uses default", func(t *testing.T) {
		t.Setenv("SHUTDOWN_TIMEOUT_SECONDS", "0")

		cfg := parseConfig()

		if cfg.ShutdownTimeout != 30*time.Second {
			t.Errorf("expected default shutdown timeout 30s for zero, got %v", cfg.ShutdownTimeout)
		}
	})

	t.Run("bind address override to loopback (sidecar deployment)", func(t *testing.T) {
		t.Setenv(defaults.EnvServerAddress, "127.0.0.1")

		cfg := parseConfig()

		if cfg.Address != "127.0.0.1" {
			t.Errorf("expected loopback address from env, got %q", cfg.Address)
		}
	})

	t.Run("empty env override keeps default all-interfaces bind", func(t *testing.T) {
		// LookupEnv distinguishes "set to empty" from "unset"; both yield
		// the default all-interfaces bind. This locks the contract for the
		// K8s / Cloud Run deployment model.
		t.Setenv(defaults.EnvServerAddress, "")

		cfg := parseConfig()

		if cfg.Address != "" {
			t.Errorf("expected empty address, got %q", cfg.Address)
		}
	})

	// TestAllowVendorChartsFromEnv below covers the full strconv.ParseBool
	// matrix — every accepted truthy and falsy spelling, plus values that
	// look truthy but aren't (yes/on/y) so a doc promise the code doesn't
	// back is caught here.
}

// TestAllowVendorChartsFromEnv pins the boolean parse contract that
// pkg/defaults/timeouts.go and docs/integrator/kubernetes-deployment.md
// document for AICR_ALLOW_VENDOR_CHARTS. The parser is strconv.ParseBool:
// unlisted values (including "yes", "on", "y", "enabled") fail closed to
// false so a typo never silently enables the vendor path. Split off from
// TestParseConfig so the table is a single flat matrix instead of a nested
// t.Run stack.
func TestAllowVendorChartsFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		// Truthy — every spelling strconv.ParseBool accepts.
		{"truthy 1", "1", true, true},
		{"truthy t", "t", true, true},
		{"truthy T", "T", true, true},
		{"truthy TRUE", "TRUE", true, true},
		{"truthy true", "true", true, true},
		{"truthy True", "True", true, true},
		// Falsy — every spelling strconv.ParseBool accepts as false.
		{"falsy 0", "0", true, false},
		{"falsy f", "f", true, false},
		{"falsy F", "F", true, false},
		{"falsy FALSE", "FALSE", true, false},
		{"falsy false", "false", true, false},
		{"falsy False", "False", true, false},
		// Fail-closed on values a human might reasonably type expecting them
		// to work — the docstring and deployment doc explicitly warn these
		// are rejected and stay off, which is the safer default.
		{"reject yes", "yes", true, false},
		{"reject on", "on", true, false},
		{"reject y", "y", true, false},
		{"reject enabled", "enabled", true, false},
		{"reject garbage", "not-a-bool", true, false},
		// Missing env — the safest default.
		{"unset", "", false, false},
		{"empty string set", "", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(defaults.EnvAllowVendorCharts, tt.value)
			} else {
				// t.Setenv with "" still counts as set; use os.Unsetenv to
				// exercise the truly-missing path. Cleanup restores.
				prev, wasSet := os.LookupEnv(defaults.EnvAllowVendorCharts)
				os.Unsetenv(defaults.EnvAllowVendorCharts)
				t.Cleanup(func() {
					if wasSet {
						os.Setenv(defaults.EnvAllowVendorCharts, prev)
					} else {
						os.Unsetenv(defaults.EnvAllowVendorCharts)
					}
				})
			}
			if got := allowVendorChartsFromEnv(); got != tt.want {
				t.Errorf("allowVendorChartsFromEnv() = %v, want %v (env=%q set=%v)",
					got, tt.want, tt.value, tt.set)
			}
		})
	}
}
