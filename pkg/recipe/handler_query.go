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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/server"
	"gopkg.in/yaml.v3"
)

// QueryRequest represents a query API request body for POST.
type QueryRequest struct {
	Criteria *Criteria `json:"criteria" yaml:"criteria"`
	Selector string    `json:"selector" yaml:"selector"`
}

// HandleQuery processes query requests. It resolves a recipe from criteria,
// hydrates all component values, and returns the value at the given selector path.
// Supports GET with query parameters (+selector) and POST with JSON/YAML body.
func (b *Builder) HandleQuery(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", server.RequestIDFromContext(r.Context()))

	var criteria *Criteria
	var selector string
	var err error

	switch r.Method {
	case http.MethodGet:
		criteria, err = ParseCriteriaFromRequest(r)
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
		req, parseErr := parseQueryRequestFromBody(bounded, r.Header.Get("Content-Type"))
		if parseErr != nil {
			var maxBytesErr *http.MaxBytesError
			if stderrors.As(parseErr, &maxBytesErr) {
				logger.Warn("query POST body exceeded size limit",
					"limit", defaults.MaxRecipePOSTBytes,
					"received", maxBytesErr.Limit,
				)
				server.WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
					"Request body exceeds maximum allowed size", false, map[string]any{
						"limit_bytes": defaults.MaxRecipePOSTBytes,
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
			if validateErr := req.Criteria.Validate(); validateErr != nil {
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
				"method":   r.Method,
				keyAllowed: []string{"GET", "POST"},
			})
		return
	}

	if err != nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid query criteria", false, map[string]any{
				"error": err.Error(),
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

	hydrated, err := HydrateResult(result)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Failed to hydrate recipe", nil)
		return
	}

	selected, err := Select(hydrated, selector)
	if err != nil {
		server.WriteError(w, r, http.StatusNotFound, aicrerrors.ErrCodeNotFound,
			"Selector path not found", false, map[string]any{
				"selector": selector,
				"error":    err.Error(),
			})
		return
	}

	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(recipeCacheTTL.Seconds())))

	serializer.RespondJSON(w, http.StatusOK, selected)
}

// parseQueryRequestFromBody parses a QueryRequest from the request body.
func parseQueryRequestFromBody(body io.Reader, contentType string) (*QueryRequest, error) {
	if body == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "request body cannot be nil")
	}

	limited := io.LimitReader(body, defaults.MaxRecipePOSTBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read request body", err)
	}
	if int64(len(data)) > defaults.MaxRecipePOSTBytes {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("request body exceeds %d bytes", defaults.MaxRecipePOSTBytes))
	}

	if len(data) == 0 {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "request body cannot be empty")
	}

	var req QueryRequest
	if strings.Contains(contentType, "json") {
		if err := json.Unmarshal(data, &req); err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to parse JSON body", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &req); err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to parse YAML body", err)
		}
	}

	return &req, nil
}
