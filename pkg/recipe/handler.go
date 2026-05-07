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

package recipe

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/server"
)

var (
	// recipeCacheTTL can be overridden for testing or custom configurations
	recipeCacheTTL = defaults.RecipeCacheTTL
)

// HandleRecipes processes recipe requests using the criteria-based system.
// It supports GET requests with query parameters and POST requests with JSON/YAML body
// to specify recipe criteria.
// The response returns a RecipeResult with component references and constraints.
// Errors are handled and returned in a structured format.
func (b *Builder) HandleRecipes(w http.ResponseWriter, r *http.Request) {
	// Add request-scoped timeout
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", server.RequestIDFromContext(r.Context()))

	var criteria *Criteria
	var err error

	switch r.Method {
	case http.MethodGet:
		criteria, err = ParseCriteriaFromRequest(r)
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
		criteria, err = ParseCriteriaFromBody(bounded, r.Header.Get("Content-Type"))
		var maxBytesErr *http.MaxBytesError
		if err != nil && stderrors.As(err, &maxBytesErr) {
			logger.Warn("recipe POST body exceeded size limit",
				"limit", defaults.MaxRecipePOSTBytes,
				"received", maxBytesErr.Limit,
			)
			server.WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
				"Request body exceeds maximum allowed size", false, map[string]any{
					"limit_bytes": defaults.MaxRecipePOSTBytes,
				})
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		server.WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				"method":  r.Method,
				"allowed": []string{"GET", "POST"},
			})
		return
	}

	if err != nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid recipe criteria", false, map[string]any{
				"error": err.Error(),
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

	// Validate criteria against allowlists (if configured)
	if b.AllowLists != nil {
		if validateErr := b.AllowLists.ValidateCriteria(criteria); validateErr != nil {
			server.WriteErrorFromErr(w, r, validateErr, "Criteria value not allowed", nil)
			return
		}
	}

	result, err := b.BuildFromCriteria(ctx, criteria)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Failed to build recipe", nil)
		return
	}

	// Set caching headers
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(recipeCacheTTL.Seconds())))

	serializer.RespondJSON(w, http.StatusOK, result)
}
