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

package server

import (
	"os"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	t.Run("default config", func(t *testing.T) {
		cfg := parseConfig()

		if cfg.Address != "" {
			t.Errorf("expected empty address, got %s", cfg.Address)
		}

		if cfg.Port != 8080 {
			t.Errorf("expected port 8080, got %d", cfg.Port)
		}

		if cfg.RateLimit != 100 {
			t.Errorf("expected rate limit 100, got %v", cfg.RateLimit)
		}

		if cfg.RateLimitBurst != 200 {
			t.Errorf("expected rate limit burst 200, got %d", cfg.RateLimitBurst)
		}

		if cfg.MaxBulkRequests != 100 {
			t.Errorf("expected max bulk requests 100, got %d", cfg.MaxBulkRequests)
		}

		if cfg.ReadTimeout != 10*time.Second {
			t.Errorf("expected read timeout 10s, got %v", cfg.ReadTimeout)
		}

		if cfg.WriteTimeout != 30*time.Second {
			t.Errorf("expected write timeout 30s, got %v", cfg.WriteTimeout)
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
}
