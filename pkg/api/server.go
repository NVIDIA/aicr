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

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/logging"
	"github.com/NVIDIA/aicr/pkg/recipe"
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
	ctx := context.Background()

	logging.SetDefaultStructuredLogger(name, version)
	slog.Debug("starting",
		"name", name,
		"version", version,
		"commit", commit,
		"date", date,
	)

	// Parse allowlists from environment variables
	allowLists, err := recipe.ParseAllowListsFromEnv()
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
			"accelerators", allowLists.AcceleratorStrings(),
			"services", allowLists.ServiceStrings(),
			"intents", allowLists.IntentStrings(),
			"os_types", allowLists.OSTypeStrings(),
		)
	}

	// Setup recipe handler
	rb := recipe.NewBuilder(
		recipe.WithVersion(version),
		recipe.WithAllowLists(allowLists),
	)

	// Setup bundle handler
	bb, err := bundler.New(
		bundler.WithAllowLists(allowLists),
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create bundler", err)
	}

	r := map[string]http.HandlerFunc{
		"/v1/recipe": rb.HandleRecipes,
		"/v1/bundle": bb.HandleBundles,
	}

	// Create and run server
	s := server.New(
		server.WithName(name),
		server.WithVersion(version),
		server.WithHandler(r),
	)

	if err := s.Run(ctx); err != nil {
		slog.Error("server exited with error", "error", err)
		return errors.Wrap(errors.ErrCodeInternal, "server exited with error", err)
	}

	return nil
}
