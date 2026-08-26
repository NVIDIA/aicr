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
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// attesterBuilder constructs an Attester from resolve options. Injectable so
// tests can assert signing wiring without a real KMS/Fulcio backend. Defaults
// to attestation.ResolveAttester (eager: the keyless token is already in hand
// from the token-file read, so no deferred resolution is needed).
type attesterBuilder func(context.Context, attestation.ResolveOptions) (attestation.Attester, error)

var bundleZipResponseHeaders = []string{
	"Content-Type",
	"Content-Disposition",
	"X-Bundle-Files",
	"X-Bundle-Size",
	"X-Bundle-Duration",
}

type streamZipFunc func(context.Context, http.ResponseWriter, string, *result.Output) error

// bundleHandler backs the /v1/bundle and /v2/bundle endpoints with an
// aicr.Client. The v1 handler reproduces
// pkg/bundler.(*DefaultBundler).HandleBundles exactly — same method gate, body
// decode, allowlist check, query-param parsing, and zip response — swapping
// the direct bundler.New + Make for the Client facade (AdoptRecipe +
// MakeBundle). Its headers and status codes match the legacy handler;
// error-body detail strings may differ where the facade wraps decode errors. The v2 handler shares that path and additionally accepts
// strict v1alpha3 profile recipes.
type bundleHandler struct {
	client    *aicr.Client
	streamZip streamZipFunc
	// allowLists is held for exact error-message parity on rejection: the
	// handler runs an explicit pre-check (matching the legacy handler's
	// "Recipe criteria value not allowed" message) before bundling. The
	// Client's MakeBundle enforcement remains a backstop.
	allowLists *aicr.AllowLists
	// signing holds the operator-configured server signing identity. When nil
	// or disabled, an attest=true request is rejected with 400. No field here
	// ever comes from a request.
	signing *signingConfig
	// newAttester builds an Attester from resolve options. Injectable for tests.
	newAttester attesterBuilder
	// allowVendorCharts gates the vendor-charts=true query parameter. Off by
	// default; the vendor path performs server-side helm pull against a
	// caller-supplied URL, so exposing it requires operator opt-in via
	// defaults.EnvAllowVendorCharts. See issue #2118.
	allowVendorCharts bool
}

// newBundleHandler constructs a bundleHandler bound to the given client,
// allowlists, and server signing identity. allowVendorCharts opts the server
// into honoring vendor-charts=true — see defaults.EnvAllowVendorCharts.
func newBundleHandler(client *aicr.Client, allowLists *aicr.AllowLists, signing *signingConfig, allowVendorCharts bool) *bundleHandler {
	return &bundleHandler{
		client:            client,
		allowLists:        allowLists,
		streamZip:         bundler.StreamZipResponseContext,
		signing:           signing,
		newAttester:       attestation.ResolveAttester,
		allowVendorCharts: allowVendorCharts,
	}
}

// HandleBundles processes bundle generation requests. It accepts a POST
// request with a JSON body containing the recipe (RecipeResult) and the same
// query parameters as the legacy pkg/bundler handler (bundlers, set, dynamic,
// deployer, node selectors/tolerations, repo, workload-gate, workload-selector,
// nodes, vendor-charts, app-name). The response is a zip archive of the bundle.
func (h *bundleHandler) HandleBundles(w http.ResponseWriter, r *http.Request) {
	h.handleBundles(w, r, false)
}

// HandleBundlesV2 accepts legacy recipes and strict v1alpha3 profile recipes.
func (h *bundleHandler) HandleBundlesV2(w http.ResponseWriter, r *http.Request) {
	h.handleBundles(w, r, true)
}

func decodeBundleRecipe(
	input io.Reader,
	contentType string,
	v2 bool,
) (recipe.RecipeResult, error) {

	var result recipe.RecipeResult
	if !v2 {
		if err := decodeRecipeResultRequest(input, &result); err != nil {
			return result, aicrerrors.PropagateOrWrap(
				err, aicrerrors.ErrCodeInvalidRequest, "failed to decode bundle recipe")
		}
		return result, nil
	}
	bodyData, err := io.ReadAll(input)
	if err != nil {
		return result, aicrerrors.PropagateOrWrap(
			err, aicrerrors.ErrCodeInvalidRequest, "failed to read bundle recipe")
	}
	format, err := v2BodyFormat(contentType)
	if err != nil {
		return result, err
	}
	decoded, err := recipe.DecodeRecipeResult(bodyData, format)
	if err != nil {
		return result, aicrerrors.PropagateOrWrap(
			err, aicrerrors.ErrCodeInvalidRequest, "failed to decode bundle recipe")
	}
	return *decoded, nil
}

func (h *bundleHandler) handleBundles(w http.ResponseWriter, r *http.Request, v2 bool) {
	logger := slog.With("requestID", RequestIDFromContext(r.Context()))

	if bundleRequestRejected(w, r, v2) {
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

	if h.vendorChartsRejected(w, r, bundleConfig) {
		return
	}

	// Opt-in signing (?attest=true), parsed and validated up front so a bad
	// value or an unconfigured server is rejected before any bundle work.
	attestRequested, handled := h.resolveAttestRequest(w, r)
	if handled {
		return
	}

	// Parse request body directly as RecipeResult. Bound the body to defend
	// against memory exhaustion (same cap as the legacy handler).
	recipeResult, err := decodeBundleRecipe(
		http.MaxBytesReader(w, r.Body, defaults.MaxBundlePOSTBytes),
		r.Header.Get("Content-Type"),
		v2,
	)

	if err != nil {
		if maxBytesErr, ok := stderrors.AsType[*http.MaxBytesError](err); ok {
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
	if !v2 && recipeResult.Metadata.SelectedProfile != nil {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Profiled recipes are available only on /v2/bundle", false, nil)
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

	// Adopt the decoded recipe onto the Client (binds the Client's provider
	// + owner token) so MakeBundle accepts it and provider-scoped reads route
	// through the Client's recipe source.
	adopted, err := h.client.AdoptRecipe(ctx, &recipeResult)
	if err != nil {
		WriteErrorFromErr(w, r, err, "Failed to prepare recipe for bundling", nil)
		return
	}

	// Create the output directory only after the raw artifact gate succeeds.
	tempDir, err := os.MkdirTemp("", "aicr-bundle-*")
	if err != nil {
		WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			"Failed to create temporary directory", true, nil)
		return
	}
	defer os.RemoveAll(tempDir) // Clean up on exit

	// Generate bundle through the facade. Set Timeout as a belt-and-
	// suspenders cap that matches the ctx deadline above — MakeBundle
	// defaults to uncapped (opt-in), so handlers must supply the REST
	// boundary explicitly. The outer ServerHandlerTimeout middleware is
	// now sized ≥ BundleHandlerTimeout so this 60s deadline runs to
	// completion instead of being silently clamped.
	bundleOpts := aicr.BundleOptions{
		Config:    bundleConfig,
		OutputDir: tempDir,
		Timeout:   defaults.BundleHandlerTimeout,
	}

	// Opt-in signing via ?attest=true. The server signs as itself using the
	// operator-configured identity (KMS or private-Sigstore keyless); no
	// identity material comes from the request. The "not configured" rejection
	// and the attest parse already ran above; here we build only the attester,
	// which reads the identity token fresh (SA tokens rotate) and so cannot be
	// hoisted out of the request path.
	if attestRequested {
		resolveOpts, buildErr := h.signing.resolveOptions()
		if buildErr != nil {
			logger.Error("failed to prepare signing options", "error", buildErr)
			WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
				"Failed to prepare signing", false, nil)
			return
		}
		attester, attErr := h.newAttester(ctx, resolveOpts)
		if attErr != nil {
			logger.Error("failed to construct attester", "error", attErr)
			WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
				"Failed to initialize signing", false, nil)
			return
		}

		// Enable attestation on this request's config (mirrors the CLI's
		// config.WithAttest) and embed the server's pre-verified binary
		// attestation as tool provenance. The bytes were verified ONCE at
		// startup and cached on the signing config.
		bundlercfg.WithAttest(true)(bundleConfig)
		bundleOpts.Attester = attester
		bundleOpts.BinaryAttestation = h.signing.binaryAttestation
	}

	output, err := h.client.MakeBundle(ctx, adopted, bundleOpts)
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

	h.writeZipResponse(ctx, w, r, tempDir, output)
}

// vendorChartsRejected fails-fast on vendor-charts=true when the server has
// not opted into that egress-triggering path. Kept as a small helper so the
// gate stays visible next to the other request-time gates and handleBundles
// remains readable. See issue #2118 and defaults.EnvAllowVendorCharts.
func (h *bundleHandler) vendorChartsRejected(w http.ResponseWriter, r *http.Request, cfg *bundlercfg.Config) bool {
	if !cfg.VendorCharts() || h.allowVendorCharts {
		return false
	}
	WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
		"vendor-charts is not enabled on this server", false, map[string]any{
			keyParam: "vendor-charts",
		})
	return true
}

func bundleRequestRejected(w http.ResponseWriter, r *http.Request, v2 bool) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				keyMethod: r.Method,
			})
		return true
	}

	if v2 {
		if err := validateV2BundleQueryParameters(r); err != nil {
			WriteErrorFromErr(w, r, err, "Invalid query parameters", nil)
			return true
		}
	}
	return false
}

func validateV2BundleQueryParameters(r *http.Request) error {
	allowed := bundler.SupportedBundleQueryParameters()
	allowed["attest"] = struct{}{}
	return validateV2QueryParameters(r, allowed)
}

func decodeRecipeResultRequest(body io.Reader, result *recipe.RecipeResult) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
			"failed to read request body", err)
	}
	var artifactHeader struct {
		APIVersion string `json:"apiVersion"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&artifactHeader); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
			"failed to inspect request apiVersion", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	if header.IsSupportedProfileAPIVersion(artifactHeader.APIVersion) {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(result); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
			"failed to decode RecipeResult", err)
	}
	if err := decoder.Decode(&struct{}{}); !stderrors.Is(err, io.EOF) {
		if err == nil {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				"request body must contain exactly one RecipeResult")
		}
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
			"failed to validate trailing request content", err)
	}
	return nil
}

// resolveAttestRequest parses the ?attest query parameter and validates it
// against the server's static signing config. It returns attestRequested and a
// handled flag: when handled is true the function has already written the HTTP
// response (a 400) and the caller must return without further processing.
//
// Absent/empty attest means no signing. A present-but-unparseable value is a
// client error (a typo must not silently ship an unsigned bundle) — mirrors
// parseTLogUpload / getSetEnabledOverride. The "not configured" rejection is
// hoisted here so it fails fast: it depends only on the query param and static
// server config, never on request-body or bundle work.
func (h *bundleHandler) resolveAttestRequest(w http.ResponseWriter, r *http.Request) (attestRequested, handled bool) {
	if v := r.URL.Query().Get("attest"); v != "" {
		parsed, perr := strconv.ParseBool(v)
		if perr != nil {
			WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
				"Invalid attest parameter (want true or false)", false, map[string]any{
					keyError: v,
				})
			return false, true
		}
		attestRequested = parsed
	}

	if attestRequested && (h.signing == nil || !h.signing.enabled) {
		WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Server is not configured for attestation", false, nil)
		return false, true
	}

	return attestRequested, false
}

func (h *bundleHandler) writeZipResponse(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	dir string,
	output *result.Output,
) {

	response := newResponseWriter(w)
	if err := h.streamZip(ctx, response, dir, output); err != nil {
		slog.ErrorContext(ctx, "failed to stream zip response",
			"requestID", RequestIDFromContext(ctx),
			"error", err,
		)
		if response.written {
			return
		}
		for _, header := range bundleZipResponseHeaders {
			w.Header().Del(header)
		}

		publicErr := aicrerrors.New(
			aicrerrors.ErrCodeInternal, "Failed to archive generated bundle")
		if stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeTimeout, "")) {
			publicErr = aicrerrors.New(aicrerrors.ErrCodeTimeout, "Bundle archive timed out")
		}
		WriteErrorFromErr(w, r, publicErr, "Failed to archive generated bundle", nil)
	}
}
