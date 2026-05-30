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

package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/logging"
	"github.com/NVIDIA/aicr/pkg/server"
)

const (
	name           = "aicrd"
	versionDefault = "dev"
)

var (
	// overridden during build with ldflags to reflect actual version info
	// e.g., -X "github.com/NVIDIA/aicr/pkg/api.version=1.0.0"
	version = versionDefault
	commit  = "unknown"
	date    = "unknown"
)

// Serve starts the API server and blocks until shutdown.
// It configures logging, sets up routes, and handles graceful shutdown.
// Returns an error if the server fails to start or encounters a fatal error.
func Serve() error {
	// Install signal handling at the entrypoint so SIGTERM/SIGINT cancels
	// the context throughout pre-Run setup (allowlist parsing, bundler
	// creation) as well as during request handling.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logging.SetDefaultStructuredLogger(name, version)
	slog.Debug("starting",
		"name", name,
		"version", version,
		"commit", commit,
		"date", date,
	)

	// Parse allowlists from environment variables
	allowLists, err := aicr.ParseAllowListsFromEnv()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to parse allowlists from environment", err)
	}

	if allowLists != nil {
		slog.Info("criteria allowlists configured",
			"accelerators", len(allowLists.Accelerators),
			"services", len(allowLists.Services),
			"intents", len(allowLists.Intents),
			"os_types", len(allowLists.OSTypes),
		)
		slog.Debug("criteria allowlists loaded",
			"accelerators", allowLists.Accelerators,
			"services", allowLists.Services,
			"intents", allowLists.Intents,
			"os_types", allowLists.OSTypes,
		)
	}

	// Setup recipe/query handlers backed by the aicr.Client facade.
	//
	// This Client is long-lived: constructed once at server startup and
	// reused across every request. EmbeddedSource() routes through the
	// process-embedded data, so the LayeredDataProvider sync.Once caching
	// hazard (see pkg/recipe/provider.go getMergedRegistry / getMergedCatalog)
	// is not reachable here. If a future change wires this Client with an
	// external --data overlay (LayeredDataProvider), the cache must first
	// be made context-aware (drop ctx.Err() results on retry) so a single
	// canceled startup request does not poison the cache for the
	// server's lifetime.
	client, err := aicr.NewClient(
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion(version),
		aicr.WithAllowLists(allowLists),
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to construct aicr client", err)
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			slog.Warn("aicr client close failed", "error", closeErr)
		}
	}()
	h := newRecipeHandler(client, allowLists)

	// Setup bundle handler backed by the same aicr.Client facade. server.go
	// no longer constructs a bundler.Bundler (or a recipe.Builder) directly —
	// the Client owns both, completing #1077 acceptance criterion #2.
	bh := newBundleHandler(client, allowLists)

	r := map[string]http.HandlerFunc{
		"/v1/recipe": h.HandleRecipes,
		"/v1/query":  h.HandleQuery,
		"/v1/bundle": bh.HandleBundles,
	}

	// Create and run server
	s := server.New(
		server.WithName(name),
		server.WithVersion(version),
		server.WithHandler(r),
	)

	if err := s.Run(ctx); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "server exited with error", err)
	}

	return nil
}
