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

var v2CriteriaQueryParameters = map[string]struct{}{
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

// recipeHandler backs the recipe and query endpoints on both routes —
// /v1/recipe, /v1/query, /v2/recipe, and /v2/query — with an aicr.Client. The
// v1 handlers reproduce the behavior of the pkg/recipe Builder handlers,
// swapping the recipe build for the facade's ResolveRecipeFromCriteria; the
// one v1 addition is rejecting explicit profile input, which legacy clients
// could never send. The v2 handlers add strict decoding, media-type
// enforcement, and profile selection over the same resolution path.
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

type recipeV2Envelope struct {
	Criteria *recipe.Criteria `json:"criteria" yaml:"criteria"`
	Profile  *string          `json:"profile,omitempty" yaml:"profile,omitempty"`
}

type queryV2Envelope struct {
	Criteria *recipe.Criteria `json:"criteria" yaml:"criteria"`
	Profile  *string          `json:"profile,omitempty" yaml:"profile,omitempty"`
	Selector *string          `json:"selector" yaml:"selector"`
}

// HandleRecipes processes recipe requests using the criteria-based system.
// It supports GET requests with query parameters and POST requests with JSON/YAML body
// to specify recipe criteria.
// The response returns a RecipeResult with component references and constraints.
// Errors are handled and returned in a structured format.
func (h *recipeHandler) HandleRecipes(w http.ResponseWriter, r *http.Request) {
	h.handleRecipes(w, r, false)
}

// HandleRecipesV2 is the strict, profile-aware recipe endpoint.
func (h *recipeHandler) HandleRecipesV2(w http.ResponseWriter, r *http.Request) {
	h.handleRecipes(w, r, true)
}

func (h *recipeHandler) handleRecipes(w http.ResponseWriter, r *http.Request, v2 bool) {
	// Add request-scoped timeout
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", RequestIDFromContext(r.Context()))

	var criteria *recipe.Criteria
	var profile string
	var err error

	switch r.Method {
	case http.MethodGet:
		if v2 {
			if err = validateV2QueryParameters(r, v2CriteriaQueryParameters); err != nil {
				break
			}
			profile, err = singleQueryValue(r, keyProfile)
			if err != nil {
				break
			}
		} else if _, supplied := r.URL.Query()[keyProfile]; supplied {
			err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				"profile selection is available only on /v2/recipe")
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
			var maxBytesErr *http.MaxBytesError
			if stderrors.As(readErr, &maxBytesErr) {
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
		if v2 {
			var envelope recipeV2Envelope
			if err = decodeStrictV2Envelope(bodyData, r.Header.Get("Content-Type"), &envelope); err == nil {
				err = validateV2EnvelopeProfile(
					bodyData, r.Header.Get("Content-Type"), envelope.Profile)
				if err == nil {
					criteria = envelope.Criteria
					profile, err = resolvePOSTProfileSelection(r, true, "/v2/recipe", envelope.Profile)
				}
				if err == nil {
					err = validateV2Criteria(criteria, h.client.CriteriaRegistry())
				}
			}
		} else {
			if hasProfile, detectErr := bodyHasTopLevelProfile(
				bodyData,
				recipe.CriteriaBodyFormat(r.Header.Get("Content-Type")),
			); detectErr == nil && hasProfile {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"profile selection is available only on /v2/recipe")
			} else {
				// The v1 pre-detector exists only to reject a successfully
				// decoded profile field. On malformed input, preserve the
				// legacy parser's canonical error contract.
				criteria, err = recipe.ParseCriteriaFromBody(
					bytes.NewReader(bodyData), r.Header.Get("Content-Type"), h.client.CriteriaRegistry(),
				)
				if err == nil {
					profile, err = resolvePOSTProfileSelection(r, false, "/v2/recipe", nil)
				}
			}
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod:  r.Method,
				keyAllowed: []string{"GET", "POST"},
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

	if criteria == nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Recipe criteria cannot be empty", false, nil)
		return
	}
	resolveOpts, err := recipeResolveOptions(r, profile, v2)
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
	if !v2 && result.Resolved().Metadata.SelectedProfile != nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Profiled recipes are available only on /v2/recipe", false, nil)
		return
	}
	resolved := normalizeLegacyRecipeResult(result.Resolved(), v2)

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
	h.handleQuery(w, r, false)
}

// HandleQueryV2 is the strict, profile-aware query endpoint.
func (h *recipeHandler) HandleQueryV2(w http.ResponseWriter, r *http.Request) {
	h.handleQuery(w, r, true)
}

func (h *recipeHandler) parseQueryPOSTBody(
	w http.ResponseWriter,
	r *http.Request,
	logger *slog.Logger,
	v2 bool,
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
		var maxBytesErr *http.MaxBytesError
		if stderrors.As(err, &maxBytesErr) {
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
	if v2 {
		var envelope queryV2Envelope
		err = decodeStrictV2Envelope(bodyData, r.Header.Get("Content-Type"), &envelope)
		if err == nil {
			err = validateV2EnvelopeProfile(
				bodyData, r.Header.Get("Content-Type"), envelope.Profile)
		}
		if err == nil {
			if envelope.Selector == nil {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"selector is required on /v2/query")
			} else {
				req = &recipe.QueryRequest{Criteria: envelope.Criteria, Selector: *envelope.Selector}
				profile = envelope.Profile
			}
		}
	} else {
		hasProfile, detectErr := bodyHasTopLevelProfile(
			bodyData,
			recipe.QueryRequestBodyFormat(r.Header.Get("Content-Type")),
		)
		if detectErr == nil && hasProfile {
			err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				"profile selection is available only on /v2/query")
		} else {
			// As on /v1/recipe, detection failures fall through so the
			// established v1 parser remains the source of parse errors.
			req, err = recipe.ParseQueryRequestFromBody(
				bytes.NewReader(bodyData), r.Header.Get("Content-Type"),
			)
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
		var validateErr error
		if v2 {
			validateErr = validateV2Criteria(req.Criteria, h.client.CriteriaRegistry())
		} else {
			validateErr = req.Criteria.ValidateWithRegistry(h.client.CriteriaRegistry())
		}
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

func (h *recipeHandler) handleQuery(w http.ResponseWriter, r *http.Request, v2 bool) {
	ctx, cancel := context.WithTimeout(r.Context(), defaults.RecipeHandlerTimeout)
	defer cancel()

	logger := slog.With("requestID", RequestIDFromContext(r.Context()))

	var criteria *recipe.Criteria
	var selector string
	var profile string
	var err error

	switch r.Method {
	case http.MethodGet:
		if v2 {
			allowed := maps.Clone(v2CriteriaQueryParameters)
			allowed["selector"] = struct{}{}
			if err = validateV2QueryParameters(r, allowed); err != nil {
				break
			}
			profile, err = singleQueryValue(r, keyProfile)
			if err != nil {
				break
			}
			if _, supplied := r.URL.Query()["selector"]; !supplied {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"selector is required on /v2/query")
				break
			}
			selector, err = singleQueryValue(r, "selector")
			if err != nil {
				break
			}
		} else {
			if _, supplied := r.URL.Query()[keyProfile]; supplied {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"profile selection is available only on /v2/query")
				break
			}
			if !r.URL.Query().Has("selector") {
				err = aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					"selector is required on /v1/query")
				break
			}
		}
		criteria, err = recipe.ParseCriteriaFromRequest(r, h.client.CriteriaRegistry())
		if !v2 {
			selector = r.URL.Query().Get("selector")
		}
	case http.MethodPost:
		req, bodyProfile, handled := h.parseQueryPOSTBody(w, r, logger, v2)
		if handled {
			return
		}
		profile, err = resolvePOSTProfileSelection(r, v2, "/v2/query", bodyProfile)
		criteria = req.Criteria
		selector = req.Selector
	default:
		w.Header().Set("Allow", "GET, POST")
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod:  r.Method,
				keyAllowed: []string{"GET", "POST"},
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

	if criteria == nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Query criteria cannot be empty", false, nil)
		return
	}
	resolveOpts, err := recipeResolveOptions(r, profile, v2)
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
	if !v2 && rec.Resolved().Metadata.SelectedProfile != nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Profiled recipes are available only on /v2/query", false, nil)
		return
	}
	resolved := normalizeLegacyRecipeResult(rec.Resolved(), v2)

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

func validateV2QueryParameters(r *http.Request, allowed map[string]struct{}) error {
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
// the recipe and query POST endpoints. V2 accepts profile in the query string,
// the strict body envelope, or both when the values agree. V1 rejects the
// exact profile query parameter; its body rejection remains parser-specific.
func resolvePOSTProfileSelection(
	r *http.Request,
	v2 bool,
	v2Endpoint string,
	bodyProfile *string,
) (string, error) {

	if !v2 {
		if _, supplied := r.URL.Query()[keyProfile]; supplied {
			return "", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile selection is available only on %s", v2Endpoint))
		}
		return "", nil
	}
	if err := validateV2QueryParameters(r, map[string]struct{}{
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

func decodeStrictV2Envelope(data []byte, contentType string, target any) error {
	format, err := v2BodyFormat(contentType)
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

func v2BodyFormat(contentType string) (serializer.Format, error) {
	if strings.TrimSpace(contentType) == "" {
		return serializer.Format(""), aicrerrors.New(
			aicrerrors.ErrCodeInvalidRequest, "Content-Type is required for v2 request bodies")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return serializer.Format(""), aicrerrors.Wrap(
			aicrerrors.ErrCodeInvalidRequest, "invalid Content-Type for v2 request body", err)
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

// validateV2EnvelopeProfile distinguishes an omitted optional profile from an
// explicitly null profile. Both JSON and YAML decode null into a nil *string,
// but the strict v2 contract permits only a string in name=value form when the
// field is present.
func validateV2EnvelopeProfile(data []byte, contentType string, profile *string) error {
	format, err := v2BodyFormat(contentType)
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

// validateV2Criteria applies constraints specific to the strict v2 envelope
// without changing the legacy v1 body contract.
func validateV2Criteria(criteria *recipe.Criteria, registry *recipe.CriteriaRegistry) error {
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

func recipeResolveOptions(r *http.Request, profile string, v2 bool) ([]aicr.RecipeResolveOption, error) {
	var opts []aicr.RecipeResolveOption
	if profile != "" {
		opts = append(opts, aicr.WithProfile(profile))
	}
	values, present := r.URL.Query()[slurmAccountingModeQueryParameter]
	if !present {
		return opts, nil
	}
	if !v2 {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"slurmAccountingMode is available only on the /v2 endpoints")
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

func normalizeLegacyRecipeResult(result *recipe.RecipeResult, v2 bool) *recipe.RecipeResult {
	if result == nil || v2 {
		return result
	}
	if _, configured := result.AccountingMode(); !configured {
		return result
	}
	projected := result.DeepCopy()
	projected.BindDataProvider(result.DataProvider())
	projected.Configuration = nil
	projected.APIVersion = recipe.RecipeAPIVersion
	return projected
}
