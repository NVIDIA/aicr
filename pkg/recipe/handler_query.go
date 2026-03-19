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

package recipe

import (
	"context"
	"encoding/json"
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

	var criteria *Criteria
	var selector string
	var err error

	switch r.Method {
	case http.MethodGet:
		criteria, err = ParseCriteriaFromRequest(r)
		selector = r.URL.Query().Get("selector")
	case http.MethodPost:
		req, parseErr := parseQueryRequestFromBody(r.Body, r.Header.Get("Content-Type"))
		defer func() {
			if r.Body != nil {
				r.Body.Close()
			}
		}()
		if parseErr != nil {
			server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
				"Invalid query request body", false, map[string]any{
					"error": parseErr.Error(),
				})
			return
		}
		criteria = req.Criteria
		selector = req.Selector
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

	slog.Debug("query",
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

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read request body", err)
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
