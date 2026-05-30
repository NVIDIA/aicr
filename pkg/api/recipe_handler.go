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
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/server"
)

// recipeCacheTTL controls the Cache-Control max-age on successful recipe and
// query responses. Mirrors the value the pkg/recipe handlers emit so the
// facade-backed handlers stay byte-identical.
var recipeCacheTTL = defaults.RecipeCacheTTL

// Detail keys for structured error responses. These mirror the values the
// pkg/recipe handlers emit so the facade-backed responses stay byte-identical.
const (
	keyError      = "error"
	keyAllowed    = "allowed"
	keyMethod     = "method"
	keyLimitBytes = "limit_bytes"
)

// recipeHandler backs the /v1/recipe and /v1/query endpoints with an
// aicr.Client. It reproduces the behavior of the pkg/recipe Builder handlers
// exactly, swapping the recipe build for the facade's
// ResolveRecipeFromCriteria.
type recipeHandler struct {
	client *aicr.Client
	// allowLists is held for exact error-message parity on rejection: the
	// handler runs an explicit pre-check before resolving so the user-facing
	// "Criteria value not allowed" message is preserved. The Client's internal
	// enforcement remains a backstop.
	allowLists *aicr.AllowLists
}

// newRecipeHandler constructs a recipeHandler bound to the given client and
// allowlists.
func newRecipeHandler(client *aicr.Client, allowLists *aicr.AllowLists) *recipeHandler {
	return &recipeHandler{client: client, allowLists: allowLists}
}

// HandleRecipes processes recipe requests using the criteria-based system.
// It supports GET requests with query parameters and POST requests with JSON/YAML body
// to specify recipe criteria.
// The response returns a RecipeResult with component references and constraints.
// Errors are handled and returned in a structured format.
func (h *recipeHandler) HandleRecipes(w http.ResponseWriter, r *http.Request) {
	// Add request-scoped timeout
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", server.RequestIDFromContext(r.Context()))

	var criteria *recipe.Criteria
	var err error

	switch r.Method {
	case http.MethodGet:
		criteria, err = recipe.ParseCriteriaFromRequest(r, h.client.CriteriaRegistry())
	case http.MethodPost:
		// Bound request body to defend against memory exhaustion.
		bounded := http.MaxBytesReader(w, r.Body, defaults.MaxRecipePOSTBytes)
		defer func() {
			// Drain via the bounded reader so any remaining bytes still
			// count against MaxBytesReader (draining r.Body directly would
			// bypass the cap). Errors here are debug-only.
			if _, drainErr := io.Copy(io.Discard, bounded); drainErr != nil {
				logger.Debug("request body drain failed", "error", drainErr)
			}
			if closeErr := bounded.Close(); closeErr != nil {
				logger.Debug("request body close failed", "error", closeErr)
			}
		}()
		criteria, err = recipe.ParseCriteriaFromBody(bounded, r.Header.Get("Content-Type"), h.client.CriteriaRegistry())
		var maxBytesErr *http.MaxBytesError
		if err != nil && stderrors.As(err, &maxBytesErr) {
			logger.Warn("recipe POST body exceeded size limit",
				"limit", defaults.MaxRecipePOSTBytes,
				"received", maxBytesErr.Limit,
			)
			server.WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
				"Request body exceeds maximum allowed size", false, map[string]any{
					keyLimitBytes: defaults.MaxRecipePOSTBytes,
				})
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		server.WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod:  r.Method,
				keyAllowed: []string{"GET", "POST"},
			})
		return
	}

	if err != nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid recipe criteria", false, map[string]any{
				keyError: err.Error(),
			})
		return
	}

	if criteria == nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Recipe criteria cannot be empty", false, nil)
		return
	}

	logger.Debug("criteria",
		"service", criteria.Service,
		"accelerator", criteria.Accelerator,
		"intent", criteria.Intent,
		"os", criteria.OS,
		"platform", criteria.Platform,
		"nodes", criteria.Nodes,
	)

	// Validate criteria against allowlists (if configured). This explicit
	// pre-check preserves the exact user-facing message; the Client's internal
	// enforcement remains a backstop.
	if h.allowLists != nil {
		if validateErr := h.allowLists.ValidateCriteria(criteria); validateErr != nil {
			server.WriteErrorFromErr(w, r, validateErr, "Criteria value not allowed", nil)
			return
		}
	}

	result, err := h.client.ResolveRecipeFromCriteria(ctx, criteria)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Failed to build recipe", nil)
		return
	}

	// Set caching headers
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(recipeCacheTTL.Seconds())))

	serializer.RespondJSON(w, http.StatusOK, result)
}

// HandleQuery processes query requests. It resolves a recipe from criteria,
// hydrates all component values, and returns the value at the given selector path.
// Supports GET with query parameters (+selector) and POST with JSON/YAML body.
func (h *recipeHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", server.RequestIDFromContext(r.Context()))

	var criteria *recipe.Criteria
	var selector string
	var err error

	switch r.Method {
	case http.MethodGet:
		criteria, err = recipe.ParseCriteriaFromRequest(r, h.client.CriteriaRegistry())
		selector = r.URL.Query().Get("selector")
	case http.MethodPost:
		// Bound request body to defend against memory exhaustion.
		bounded := http.MaxBytesReader(w, r.Body, defaults.MaxRecipePOSTBytes)
		defer func() {
			// Drain via the bounded reader so any remaining bytes still
			// count against MaxBytesReader. Errors are debug-only.
			if _, drainErr := io.Copy(io.Discard, bounded); drainErr != nil {
				logger.Debug("query request body drain failed", "error", drainErr)
			}
			if closeErr := bounded.Close(); closeErr != nil {
				logger.Debug("query request body close failed", "error", closeErr)
			}
		}()
		req, parseErr := recipe.ParseQueryRequestFromBody(bounded, r.Header.Get("Content-Type"))
		if parseErr != nil {
			var maxBytesErr *http.MaxBytesError
			if stderrors.As(parseErr, &maxBytesErr) {
				logger.Warn("query POST body exceeded size limit",
					"limit", defaults.MaxRecipePOSTBytes,
					"received", maxBytesErr.Limit,
				)
				server.WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
					"Request body exceeds maximum allowed size", false, map[string]any{
						keyLimitBytes: defaults.MaxRecipePOSTBytes,
					})
				return
			}
			server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
				"Invalid query request body", false, map[string]any{
					keyError: parseErr.Error(),
				})
			return
		}
		if req.Criteria != nil {
			if validateErr := req.Criteria.ValidateWithRegistry(h.client.CriteriaRegistry()); validateErr != nil {
				server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
					"Invalid criteria in request body", false, map[string]any{
						keyError: validateErr.Error(),
					})
				return
			}
		}
		criteria = req.Criteria
		selector = req.Selector
	default:
		w.Header().Set("Allow", "GET, POST")
		server.WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod:  r.Method,
				keyAllowed: []string{"GET", "POST"},
			})
		return
	}

	if err != nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid query criteria", false, map[string]any{
				keyError: err.Error(),
			})
		return
	}

	if criteria == nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Query criteria cannot be empty", false, nil)
		return
	}

	logger.Debug("query",
		"service", criteria.Service,
		"accelerator", criteria.Accelerator,
		"intent", criteria.Intent,
		"os", criteria.OS,
		"platform", criteria.Platform,
		"selector", selector,
	)

	// Validate criteria against allowlists (if configured). This explicit
	// pre-check preserves the exact user-facing message; the Client's internal
	// enforcement remains a backstop.
	if h.allowLists != nil {
		if validateErr := h.allowLists.ValidateCriteria(criteria); validateErr != nil {
			server.WriteErrorFromErr(w, r, validateErr, "Criteria value not allowed", nil)
			return
		}
	}

	rec, err := h.client.ResolveRecipeFromCriteria(ctx, criteria)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Failed to build recipe", nil)
		return
	}

	// Hydrate and select as two steps (rather than the combined
	// aicr.SelectFromRecipe) to preserve the legacy handler's distinct error
	// mapping: a hydrate failure surfaces via its own error code (5xx), while a
	// missing selector path is a 404. rec is *aicr.Recipe (= *recipe.RecipeResult),
	// so HydrateResult accepts it directly.
	hydrated, err := recipe.HydrateResult(rec)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Failed to hydrate recipe", nil)
		return
	}

	selected, err := recipe.Select(hydrated, selector)
	if err != nil {
		server.WriteError(w, r, http.StatusNotFound, aicrerrors.ErrCodeNotFound,
			"Selector path not found", false, map[string]any{
				"selector": selector,
				keyError:   err.Error(),
			})
		return
	}

	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(recipeCacheTTL.Seconds())))

	serializer.RespondJSON(w, http.StatusOK, selected)
}
