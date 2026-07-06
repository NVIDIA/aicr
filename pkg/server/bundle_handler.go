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
	"encoding/json"
	stderrors "errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/NVIDIA/aicr/pkg/bundler"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// bundleHandler backs the /v1/bundle endpoint with an aicr.Client. It
// reproduces pkg/bundler.(*DefaultBundler).HandleBundles exactly — same
// method gate, body decode, allowlist check, query-param parsing, and zip
// response — swapping the direct bundler.New + Make for the Client facade
// (AdoptRecipe + MakeBundle). The body, headers, and status codes are
// byte-identical to the legacy handler.
type bundleHandler struct {
	client *aicr.Client
	// allowLists is held for exact error-message parity on rejection: the
	// handler runs an explicit pre-check (matching the legacy handler's
	// "Recipe criteria value not allowed" message) before bundling. The
	// Client's MakeBundle enforcement remains a backstop.
	allowLists *aicr.AllowLists
}

// newBundleHandler constructs a bundleHandler bound to the given client and
// allowlists.
func newBundleHandler(client *aicr.Client, allowLists *aicr.AllowLists) *bundleHandler {
	return &bundleHandler{client: client, allowLists: allowLists}
}

// HandleBundles processes bundle generation requests. It accepts a POST
// request with a JSON body containing the recipe (RecipeResult) and the same
// query parameters as the legacy pkg/bundler handler (bundlers, set, dynamic,
// deployer, node selectors/tolerations, repo, workload-gate, workload-selector,
// nodes, vendor-charts, app-name). The response is a zip archive of the bundle.
func (h *bundleHandler) HandleBundles(w http.ResponseWriter, r *http.Request) {
	logger := slog.With("requestID", RequestIDFromContext(r.Context()))

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod: r.Method,
			})
		return
	}

	// Add request-scoped timeout (matches the legacy handler's bundle timeout).
	ctx, cancel := context.WithTimeout(r.Context(), defaults.BundleHandlerTimeout)
	defer cancel()

	// Parse all query parameters into a bundler config via the exported
	// boundary so this handler stays byte-identical to the legacy one.
	bundleConfig, err := bundler.ParseBundleConfig(r)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Invalid query parameters", nil)
		return
	}

	// Parse request body directly as RecipeResult. Bound the body to defend
	// against memory exhaustion (same cap as the legacy handler).
	bounded := http.MaxBytesReader(w, r.Body, defaults.MaxBundlePOSTBytes)
	var recipeResult recipe.RecipeResult
	if err = json.NewDecoder(bounded).Decode(&recipeResult); err != nil {
		var maxBytesErr *http.MaxBytesError
		if stderrors.As(err, &maxBytesErr) {
			logger.Warn("bundle POST body exceeded size limit",
				"limit", defaults.MaxBundlePOSTBytes,
				"received", maxBytesErr.Limit,
			)
			WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
				"Request body exceeds maximum allowed size", false, map[string]any{
					keyLimitBytes: defaults.MaxBundlePOSTBytes,
				})
			return
		}
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid request body", false, map[string]any{
				keyError: err.Error(),
			})
		return
	}

	// Validate recipe has component references.
	if len(recipeResult.ComponentRefs) == 0 {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Recipe must contain at least one component reference", false, nil)
		return
	}

	// Validate recipe criteria against allowlists (if configured). Explicit
	// pre-check preserves the legacy user-facing message; the Client's
	// MakeBundle enforcement remains a backstop.
	if h.allowLists != nil && recipeResult.Criteria != nil {
		if validateErr := validateAgainstAllowLists(h.allowLists, recipeResult.Criteria); validateErr != nil {
			WriteErrorFromErr(w, r, validateErr, "Recipe criteria value not allowed", nil)
			return
		}
	}

	logger.Debug("bundle request received",
		"components", len(recipeResult.ComponentRefs),
	)

	// Create temporary directory for bundle output.
	tempDir, err := os.MkdirTemp("", "aicr-bundle-*")
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			"Failed to create temporary directory", true, nil)
		return
	}
	defer os.RemoveAll(tempDir) // Clean up on exit

	// Adopt the decoded recipe onto the Client (binds the Client's provider
	// + owner token) so MakeBundle accepts it and provider-scoped reads route
	// through the Client's recipe source.
	adopted, err := h.client.AdoptRecipe(ctx, &recipeResult)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Failed to prepare recipe for bundling", nil)
		return
	}

	// Generate bundle through the facade. Set Timeout as a belt-and-
	// suspenders cap that matches the ctx deadline above — MakeBundle
	// defaults to uncapped (opt-in), so handlers must supply the REST
	// boundary explicitly. The outer ServerHandlerTimeout middleware is
	// now sized ≥ BundleHandlerTimeout so this 60s deadline runs to
	// completion instead of being silently clamped.
	output, err := h.client.MakeBundle(ctx, adopted, aicr.BundleOptions{
		Config:    bundleConfig,
		OutputDir: tempDir,
		Timeout:   defaults.BundleHandlerTimeout,
	})
	if err != nil {
		WriteErrorFromErr(w, r, err, "Failed to generate bundle", nil)
		return
	}

	// Check for bundle errors. Per-bundler errors may include internal
	// detail (file paths, helm template stacks, network diagnostics).
	// Log the full payload server-side and surface only the failing
	// bundler component names to the client — enough to know *which*
	// component failed without leaking implementation detail on 5xx.
	if output.HasErrors() {
		failedBundlers := make([]string, 0, len(output.Errors))
		for _, be := range output.Errors {
			failedBundlers = append(failedBundlers, string(be.BundlerType))
			logger.Error("bundler reported error",
				"bundler", be.BundlerType,
				"error", be.Error)
		}
		WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			"Bundle generation failed", true, map[string]any{
				"failedBundlers": failedBundlers,
			})
		return
	}

	// Stream zip response using the same exported helper the legacy handler
	// uses, so headers + entry layout are byte-identical.
	if err := bundler.StreamZipResponse(w, tempDir, output); err != nil {
		// Can't write error response if we've already started writing.
		logger.Error("failed to stream zip response", "error", err)
		return
	}
}
