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

package recipe

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/NVIDIA/eidos/pkg/defaults"
	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/server"
)

// DefaultRecipeCacheTTL is the default cache duration for recipe responses.
// Exported for backwards compatibility; prefer using defaults.RecipeCacheTTL.
const DefaultRecipeCacheTTL = defaults.RecipeCacheTTL

var (
	// recipeCacheTTL can be overridden for testing or custom configurations
	recipeCacheTTL = DefaultRecipeCacheTTL
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

	var criteria *Criteria
	var err error

	switch r.Method {
	case http.MethodGet:
		criteria, err = ParseCriteriaFromRequest(r)
	case http.MethodPost:
		criteria, err = ParseCriteriaFromBody(r.Body, r.Header.Get("Content-Type"))
		defer func() {
			if r.Body != nil {
				r.Body.Close()
			}
		}()
	default:
		w.Header().Set("Allow", "GET, POST")
		server.WriteError(w, r, http.StatusMethodNotAllowed, eidoserrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				"method":  r.Method,
				"allowed": []string{"GET", "POST"},
			})
		return
	}

	if err != nil {
		server.WriteError(w, r, http.StatusBadRequest, eidoserrors.ErrCodeInvalidRequest,
			"Invalid recipe criteria", false, map[string]any{
				"error": err.Error(),
			})
		return
	}

	if criteria == nil {
		server.WriteError(w, r, http.StatusBadRequest, eidoserrors.ErrCodeInvalidRequest,
			"Recipe criteria cannot be empty", false, nil)
		return
	}

	slog.Debug("criteria",
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
