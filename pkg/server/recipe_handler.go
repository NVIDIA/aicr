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
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strings"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"gopkg.in/yaml.v3"
)

// recipeCacheTTL controls the Cache-Control max-age on successful recipe and
// query responses. Mirrors the value the pkg/recipe handlers emit so the
// facade-backed handlers stay byte-identical.
var recipeCacheTTL = defaults.RecipeCacheTTL

const slurmAccountingModeQueryParameter = "slurmAccountingMode"

var criteriaQueryParameters = map[string]struct{}{
	keyService:                        {},
	"accelerator":                     {},
	"gpu":                             {},
	"intent":                          {},
	"os":                              {},
	"platform":                        {},
	keyNodes:                          {},
	keyProfile:                        {},
	slurmAccountingModeQueryParameter: {},
}

// recipeHandler backs /v1/recipe and /v1/query with an aicr.Client.
//
// There is one contract, and it is the profile-aware one: strict decoding,
// media-type enforcement, and profile selection over the facade's
// ResolveRecipeFromCriteria. The legacy non-profile contract that /v1 carried
// alongside a separate /v2 family was removed in #2112 — before any external
// consumer existed, so it owed no deprecation window.
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

type recipeEnvelope struct {
	Criteria *recipe.Criteria `json:"criteria" yaml:"criteria"`
	Profile  *string          `json:"profile,omitempty" yaml:"profile,omitempty"`
}

type queryEnvelope struct {
	Criteria *recipe.Criteria `json:"criteria" yaml:"criteria"`
	Profile  *string          `json:"profile,omitempty" yaml:"profile,omitempty"`
	Selector *string          `json:"selector" yaml:"selector"`
}

// HandleRecipes processes recipe requests using the criteria-based system.
// It supports GET requests with query parameters and POST requests with JSON/YAML body
// to specify recipe criteria.
// The response returns a RecipeResult with component references and constraints.
// Errors are handled and returned in a structured format.
// HandleRecipes resolves a recipe from criteria. It is profile- and
// Slurm-accounting-aware; the legacy non-profile contract that /v1 carried
// before the collapse is gone.
func (h *recipeHandler) HandleRecipes(w http.ResponseWriter, r *http.Request) {
	// Add request-scoped timeout
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", RequestIDFromContext(r.Context()))

	var criteria *recipe.Criteria
	var profile string
	var err error

	switch r.Method {
	// HEAD resolves exactly as GET does; net/http discards the body on the way
	// out, so the client gets the real status and headers. Rejecting it while
	// accepting GET contradicted both HTTP semantics and /metrics, which has
	// allowed the pair since readOnly() was introduced.
	//
	// It costs the same as GET: the recipe is resolved to produce the headers.
	// Use /health or /ready for a cheap probe.
	case http.MethodGet, http.MethodHead:
		if err = validateStrictQueryParameters(r, criteriaQueryParameters); err != nil {
			break
		}
		if profile, err = singleQueryValue(r, keyProfile); err != nil {
			break
		}
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
		bodyData, readErr := io.ReadAll(bounded)
		if readErr != nil {
			if maxBytesErr, ok := stderrors.AsType[*http.MaxBytesError](readErr); ok {
				logger.Warn("recipe POST body exceeded size limit",
					"limit", defaults.MaxRecipePOSTBytes,
					"received", maxBytesErr.Limit,
				)
				WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
					"Request body exceeds maximum allowed size", false, map[string]any{
						keyLimitBytes: defaults.MaxRecipePOSTBytes,
					})
				return
			}
			err = readErr
			break
		}
		var envelope recipeEnvelope
		if err = decodeStrictEnvelope(bodyData, r.Header.Get("Content-Type"), &envelope); err == nil {
			err = validateEnvelopeProfile(
				bodyData, r.Header.Get("Content-Type"), envelope.Profile)
			if err == nil {
				// Match what GET echoes. GET seeds every dimension from
				// NewCriteria before applying query parameters; a decoded body
				// leaves unspecified ones empty, so the two transports returned
				// criteria objects that differed for identical requests.
				criteria = envelope.Criteria.FillUnsetWithAny()
				profile, err = resolvePOSTProfileSelection(r, envelope.Profile)
			}
			if err == nil {
				err = validateStrictCriteria(criteria, h.client.CriteriaRegistry())
			}
		}
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod:  r.Method,
				keyAllowed: []string{"GET", "HEAD", "POST"},
			})
		return
	}

	if err != nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid recipe criteria", false, map[string]any{
				keyError: err.Error(),
			})
		return
	}

	if !criteriaValid(w, r, criteria) {
		return
	}
	resolveOpts, err := recipeResolveOptions(r, profile)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Invalid Slurm accounting mode", nil)
		return
	}

	logger.Debug("criteria",
		keyService, criteria.Service,
		"accelerator", criteria.Accelerator,
		"intent", criteria.Intent,
		"os", criteria.OS,
		"platform", criteria.Platform,
		keyNodes, criteria.Nodes,
	)

	// Validate criteria against allowlists (if configured). This explicit
	// pre-check preserves the exact user-facing message; the Client's internal
	// enforcement remains a backstop.
	if h.allowLists != nil {
		if validateErr := validateAgainstAllowLists(h.allowLists, criteria); validateErr != nil {
			WriteErrorFromErr(w, r, validateErr, "Criteria value not allowed", nil)
			return
		}
	}

	result, err := h.client.ResolveRecipeFromCriteriaWithOptions(
		ctx, aicr.WrapCriteria(criteria), resolveOpts...)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Failed to build recipe", nil)
		return
	}
	resolved := result.Resolved()

	// Set caching headers
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(recipeCacheTTL.Seconds())))

	// Wire format must remain the upstream pkg/recipe.RecipeResult JSON so
	// CLI/library consumers parse the same shape they always have. Resolved()
	// returns the borrowed upstream pointer the facade wraps; nil would mean
	// the result lacks internal state (only possible from a hand-constructed
	// RecipeResult, which ResolveRecipeFromCriteria never returns).
	serializer.RespondJSON(w, http.StatusOK, resolved)
}

// HandleQuery processes query requests. It resolves a recipe from criteria,
// hydrates all component values, and returns the value at the given selector path.
// Supports GET with query parameters (+selector) and POST with JSON/YAML body.
func (h *recipeHandler) HandleQuery(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r)
}

func (h *recipeHandler) parseQueryPOSTBody(
	w http.ResponseWriter,
	r *http.Request,
	logger *slog.Logger,
) (*recipe.QueryRequest, *string, bool) {

	bounded := http.MaxBytesReader(w, r.Body, defaults.MaxRecipePOSTBytes)
	defer func() {
		if _, drainErr := io.Copy(io.Discard, bounded); drainErr != nil {
			logger.Debug("query request body drain failed", "error", drainErr)
		}
		if closeErr := bounded.Close(); closeErr != nil {
			logger.Debug("query request body close failed", "error", closeErr)
		}
	}()

	bodyData, err := io.ReadAll(bounded)
	if err != nil {
		if maxBytesErr, ok := stderrors.AsType[*http.MaxBytesError](err); ok {
			logger.Warn("query POST body exceeded size limit",
				"limit", defaults.MaxRecipePOSTBytes,
				"received", maxBytesErr.Limit,
			)
			WriteError(w, r, http.StatusRequestEntityTooLarge, aicrerrors.ErrCodeInvalidRequest,
				"Request body exceeds maximum allowed size", false, map[string]any{
					keyLimitBytes: defaults.MaxRecipePOSTBytes,
				})
			return nil, nil, true
		}
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid query request body", false, map[string]any{keyError: err.Error()})
		return nil, nil, true
	}

	var req *recipe.QueryRequest
	var profile *string
	{
		var envelope queryEnvelope
		err = decodeStrictEnvelope(bodyData, r.Header.Get("Content-Type"), &envelope)
		if err == nil {
			err = validateEnvelopeProfile(
				bodyData, r.Header.Get("Content-Type"), envelope.Profile)
		}
		if err == nil {
			if envelope.Selector == nil {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"selector is required on /v1/query")
			} else {
				req = &recipe.QueryRequest{
					Criteria: envelope.Criteria.FillUnsetWithAny(),
					Selector: *envelope.Selector,
				}
				profile = envelope.Profile
			}
		}
	}
	if err != nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid query request body", false, map[string]any{keyError: err.Error()})
		return nil, nil, true
	}
	if req == nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid query request body", false, nil)
		return nil, nil, true
	}
	if req.Criteria != nil {
		validateErr := validateStrictCriteria(req.Criteria, h.client.CriteriaRegistry())
		if validateErr != nil {
			WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
				"Invalid criteria in request body", false, map[string]any{
					keyError: validateErr.Error(),
				})
			return nil, nil, true
		}
	}
	return req, profile, false
}

func (h *recipeHandler) handleQuery(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", RequestIDFromContext(r.Context()))

	var criteria *recipe.Criteria
	var selector string
	var profile string
	var err error

	switch r.Method {
	// HEAD resolves as GET; see the note on HandleRecipes.
	case http.MethodGet, http.MethodHead:
		{
			allowed := maps.Clone(criteriaQueryParameters)
			allowed["selector"] = struct{}{}
			if err = validateStrictQueryParameters(r, allowed); err != nil {
				break
			}
			profile, err = singleQueryValue(r, keyProfile)
			if err != nil {
				break
			}
			if _, supplied := r.URL.Query()["selector"]; !supplied {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"selector is required on /v1/query")
				break
			}
			selector, err = singleQueryValue(r, "selector")
			if err != nil {
				break
			}
		}
		criteria, err = recipe.ParseCriteriaFromRequest(r, h.client.CriteriaRegistry())
	case http.MethodPost:
		req, bodyProfile, handled := h.parseQueryPOSTBody(w, r, logger)
		if handled {
			return
		}
		profile, err = resolvePOSTProfileSelection(r, bodyProfile)
		criteria = req.Criteria
		selector = req.Selector
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod:  r.Method,
				keyAllowed: []string{"GET", "HEAD", "POST"},
			})
		return
	}

	if err != nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid query criteria", false, map[string]any{
				keyError: err.Error(),
			})
		return
	}

	if !criteriaValid(w, r, criteria) {
		return
	}
	resolveOpts, err := recipeResolveOptions(r, profile)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Invalid Slurm accounting mode", nil)
		return
	}

	logger.Debug("query",
		keyService, criteria.Service,
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
		if validateErr := validateAgainstAllowLists(h.allowLists, criteria); validateErr != nil {
			WriteErrorFromErr(w, r, validateErr, "Criteria value not allowed", nil)
			return
		}
	}

	rec, err := h.client.ResolveRecipeFromCriteriaWithOptions(
		ctx, aicr.WrapCriteria(criteria), resolveOpts...)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Failed to build recipe", nil)
		return
	}
	resolved := rec.Resolved()

	// Hydrate + select through the facade, then shape the response here.
	// The legacy projection keeps the resolved result's bound DataProvider,
	// so WrapResolved hydrates against the same source. The handler's
	// distinct error mapping — a missing selector path is 404, a hydrate
	// failure is its own (5xx) code — is a handler-level mapping over the
	// facade's documented outermost-code contract, not a second
	// hydrate+select implementation.
	selected, err := aicr.SelectFromRecipeWithContext(ctx, aicr.WrapResolved(resolved), selector)
	if err != nil {
		// Match on the OUTERMOST structured code: stderrors.Is walks the
		// wrap chain and would misread a hydration failure whose cause
		// happens to carry ErrCodeNotFound (e.g. a missing values file) as
		// a missing selector path.
		var se *aicrerrors.StructuredError
		if stderrors.As(err, &se) && se.Code == aicrerrors.ErrCodeNotFound {
			WriteError(w, r, http.StatusNotFound, aicrerrors.ErrCodeNotFound,
				"Selector path not found", false, map[string]any{
					"selector": selector,
					keyError:   err.Error(),
				})
			return
		}
		WriteErrorFromErr(w, r, err, "Failed to hydrate recipe", nil)
		return
	}

	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(recipeCacheTTL.Seconds())))

	serializer.RespondJSON(w, http.StatusOK, selected)
}

func validateStrictQueryParameters(r *http.Request, allowed map[string]struct{}) error {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return aicrerrors.Wrap(
			aicrerrors.ErrCodeInvalidRequest, "malformed query parameters", err)
	}
	for key := range values {
		if _, ok := allowed[key]; !ok {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown query parameter %q", key))
		}
	}
	return nil
}

func singleQueryValue(r *http.Request, key string) (string, error) {
	values, ok := r.URL.Query()[key]
	if !ok {
		return "", nil
	}
	if len(values) == 0 {
		return "", nil
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("query parameter %q has conflicting values", key))
		}
	}
	return values[0], nil
}

// resolvePOSTProfileSelection applies the ADR-015 transport rules shared by
// the recipe and query POST endpoints: a profile may arrive in the query
// string, in the strict body envelope, or in both when the values agree.
func resolvePOSTProfileSelection(
	r *http.Request,
	bodyProfile *string,
) (string, error) {

	if err := validateStrictQueryParameters(r, map[string]struct{}{
		keyProfile:                        {},
		slurmAccountingModeQueryParameter: {},
	}); err != nil {
		return "", err
	}
	queryProfile, err := singleQueryValue(r, keyProfile)
	if err != nil {
		return "", err
	}
	_, querySupplied := r.URL.Query()[keyProfile]
	switch {
	case querySupplied && bodyProfile != nil && queryProfile != *bodyProfile:
		return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"profile selection conflicts between the query parameter and request body")
	case querySupplied:
		return queryProfile, nil
	case bodyProfile != nil:
		return *bodyProfile, nil
	default:
		return "", nil
	}
}

func decodeStrictEnvelope(data []byte, contentType string, target any) error {
	format, err := strictBodyFormat(contentType)
	if err != nil {
		return err
	}
	reader, err := serializer.NewReader(
		format, bytes.NewReader(data), serializer.WithStrict(),
	)
	if err != nil {
		return aicrerrors.PropagateOrWrap(
			err, aicrerrors.ErrCodeInvalidRequest, "failed to create request envelope reader")
	}
	if err := reader.Deserialize(target); err != nil {
		return aicrerrors.PropagateOrWrap(
			err, aicrerrors.ErrCodeInvalidRequest, "failed to decode request envelope")
	}
	return nil
}

func strictBodyFormat(contentType string) (serializer.Format, error) {
	if strings.TrimSpace(contentType) == "" {
		return serializer.Format(""), aicrerrors.New(
			aicrerrors.ErrCodeInvalidRequest, "Content-Type is required for request bodies")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return serializer.Format(""), aicrerrors.Wrap(
			aicrerrors.ErrCodeInvalidRequest, "invalid Content-Type for request body", err)
	}
	switch strings.ToLower(mediaType) {
	case "application/json":
		return serializer.FormatJSON, nil
	case "application/x-yaml":
		return serializer.FormatYAML, nil
	default:
		return serializer.Format(""), aicrerrors.New(
			aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported Content-Type %q for v2 request body", mediaType))
	}
}

// validateEnvelopeProfile distinguishes an omitted optional profile from an
// explicitly null profile. Both JSON and YAML decode null into a nil *string,
// but the strict v2 contract permits only a string in name=value form when the
// field is present.
func validateEnvelopeProfile(data []byte, contentType string, profile *string) error {
	format, err := strictBodyFormat(contentType)
	if err != nil {
		return err
	}
	present, err := bodyHasTopLevelProfile(data, format)
	if err != nil {
		return aicrerrors.PropagateOrWrap(
			err, aicrerrors.ErrCodeInvalidRequest, "failed to inspect profile field")
	}
	if present && profile == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"profile must be a string in name=value form; null is not allowed")
	}
	return nil
}

// validateStrictCriteria applies constraints specific to the strict v2 envelope
// without changing the legacy v1 body contract.
func validateStrictCriteria(criteria *recipe.Criteria, registry *recipe.CriteriaRegistry) error {
	if criteria == nil {
		return nil
	}
	if criteria.Nodes < 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"criteria nodes must be a non-negative integer")
	}
	return criteria.ValidateWithRegistry(registry)
}

func bodyHasTopLevelProfile(data []byte, format serializer.Format) (bool, error) {
	switch format {
	case serializer.FormatJSON:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return false, aicrerrors.Wrap(
				aicrerrors.ErrCodeInvalidRequest, "failed to decode profile field", err)
		}
		_, exists := fields[keyProfile]
		return exists, nil
	case serializer.FormatYAML:
		var fields map[string]yaml.Node
		if err := yaml.Unmarshal(data, &fields); err != nil {
			return false, aicrerrors.Wrap(
				aicrerrors.ErrCodeInvalidRequest, "failed to decode profile field", err)
		}
		_, exists := fields[keyProfile]
		return exists, nil
	case serializer.FormatTable:
		return false, aicrerrors.New(
			aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported profile field format %q", format))
	default:
		return false, aicrerrors.New(
			aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported profile field format %q", format))
	}
}

// criteriaValid rejects nil criteria or requests with no effective criteria,
// matching the CLI guard in pkg/cli/query.go. Applied to both /recipe and
// /query so that empty-criteria resolution is consistently rejected. Returns
// true when the request may proceed.
func criteriaValid(w http.ResponseWriter, r *http.Request, criteria *recipe.Criteria) bool {
	if criteria == nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Recipe criteria cannot be empty", false, nil)
		return false
	}
	if criteria.Specificity() == 0 {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"no criteria provided: specify at least one of service, accelerator, intent, os, platform, nodes", false, nil)
		return false
	}
	return true
}

func recipeResolveOptions(r *http.Request, profile string) ([]aicr.RecipeResolveOption, error) {
	var opts []aicr.RecipeResolveOption
	if profile != "" {
		opts = append(opts, aicr.WithProfile(profile))
	}
	values, present := r.URL.Query()[slurmAccountingModeQueryParameter]
	if !present {
		return opts, nil
	}
	if len(values) != 1 || values[0] == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"slurmAccountingMode must be provided exactly once with a non-empty value")
	}
	if _, err := recipe.ParseAccountingMode(values[0]); err != nil {
		return nil, err
	}
	return append(opts, aicr.WithAccountingMode(values[0])), nil
}
