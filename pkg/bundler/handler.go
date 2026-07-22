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

package bundler

import (
	"archive/zip"
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

const zipCopyBufferSize = 32 * 1024

var canonicalZipModifiedTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// ParseBundleConfig parses the /v1/bundle query parameters from r and
// returns the bundler *config.Config they describe. It is the exported
// boundary the aicr.Client-backed REST handler (pkg/server) uses to build
// the bundle config from a request without reimplementing the query-param
// parsing or config-building.
//
// The returned error carries an ErrCodeInvalidRequest structured code on a
// bad parameter, suitable for server.WriteErrorFromErr.
func ParseBundleConfig(r *http.Request) (*config.Config, error) {
	params, err := parseQueryParams(r)
	if err != nil {
		return nil, err
	}
	return bundleConfigFromParams(params), nil
}

// bundleConfigFromParams builds the bundler config from already-parsed
// query parameters.
func bundleConfigFromParams(params *bundleParams) *config.Config {
	return config.NewConfig(
		config.WithValueOverridePaths(params.valueOverrides),
		config.WithDynamicValuePaths(params.dynamicValues),
		config.WithSystemNodeSelector(params.systemNodeSelector),
		config.WithSystemNodeTolerations(params.systemNodeTolerations),
		config.WithAcceleratedNodeSelector(params.acceleratedNodeSelector),
		config.WithAcceleratedNodeTolerations(params.acceleratedNodeTolerations),
		config.WithWorkloadGateTaint(params.workloadGateTaint),
		config.WithWorkloadSelector(params.workloadSelector),
		config.WithEstimatedNodeCount(params.estimatedNodeCount),
		config.WithDeployer(params.deployer),
		config.WithRepoURL(params.repoURL),
		config.WithVendorCharts(params.vendorCharts),
		config.WithSerial(params.serial),
		config.WithAppName(params.appName),
		config.WithBundlers(params.bundlers),
	)
}

type streamZipDependencies struct {
	stageVerifiedBundle func(
		context.Context,
		string,
		checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error)
	warn func(string, ...any)
}

func defaultStreamZipDependencies() streamZipDependencies {
	return streamZipDependencies{
		stageVerifiedBundle: checksum.StageVerifiedBundle,
		warn:                slog.Warn,
	}
}

// StreamZipResponseContext verifies and privately stages the complete bundle,
// then streams only the staged inventory to the response. It checks stage
// cleanup before reporting success.
func StreamZipResponseContext(
	ctx context.Context,
	w http.ResponseWriter,
	dir string,
	output *result.Output,
) (err error) {

	return streamZipResponseContextWithDependencies(
		ctx, w, dir, output, defaultStreamZipDependencies())
}

func streamZipResponseContextWithDependencies(
	ctx context.Context,
	w http.ResponseWriter,
	dir string,
	output *result.Output,
	deps streamZipDependencies,
) (retErr error) {

	if output == nil {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "bundle output is required")
	}
	defaults := defaultStreamZipDependencies()
	if deps.stageVerifiedBundle == nil {
		deps.stageVerifiedBundle = defaults.stageVerifiedBundle
	}
	if deps.warn == nil {
		deps.warn = defaults.warn
	}
	options := checksum.InventoryOptions{AllowedMetadataPaths: attestation.BundleMetadataPaths()}
	_, inventory, cleanup, err := deps.stageVerifiedBundle(ctx, dir, options)
	if err != nil {
		return aicrerrors.PropagateOrWrap(
			err, aicrerrors.ErrCodeInternal, "failed to stage verified bundle for archive")
	}
	if cleanup == nil {
		return aicrerrors.New(
			aicrerrors.ErrCodeInternal, "verified bundle stage returned no cleanup owner")
	}
	cleanupPending := true
	cleanupStage := func() error {
		if !cleanupPending {
			return nil
		}
		cleanupPending = false
		return cleanup()
	}
	defer func() {
		cleanupErr := cleanupStage()
		if cleanupErr == nil {
			return
		}
		if retErr != nil {
			deps.warn("failed to clean verified bundle stage after archive failure",
				"error", cleanupErr, "primaryError", retErr)
			return
		}
		retErr = aicrerrors.Wrap(
			aicrerrors.ErrCodeInternal, "failed to clean verified bundle stage", cleanupErr)
	}()
	if inventory == nil {
		return aicrerrors.New(
			aicrerrors.ErrCodeInternal, "verified bundle stage returned no inventory")
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"bundles.zip\"")
	w.Header().Set("X-Bundle-Files", strconv.Itoa(len(inventory.RelativeFiles())))
	w.Header().Set("X-Bundle-Size", strconv.FormatInt(inventory.TotalSize(), 10))
	w.Header().Set("X-Bundle-Duration", output.TotalDuration.String())

	zw := zip.NewWriter(w)
	for _, rel := range inventory.RelativeDirectories() {
		if err := zipContextError(ctx, "bundle archive canceled while writing directories"); err != nil {
			return err
		}
		header := &zip.FileHeader{
			Name:     rel + "/",
			Modified: canonicalZipModifiedTime,
		}
		header.SetMode(os.ModeDir | 0755)
		if _, err := zw.CreateHeader(header); err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ZIP directory entry", err)
		}
	}

	for _, rel := range inventory.RelativeFiles() {
		if err := zipContextError(ctx, "bundle archive canceled while writing files"); err != nil {
			return err
		}
		file, err := inventory.Open(ctx, rel)
		if err != nil {
			return aicrerrors.PropagateOrWrap(
				err, aicrerrors.ErrCodeInternal, "failed to open verified bundle file for archive")
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to inspect verified bundle file", statErr)
		}
		header := &zip.FileHeader{
			Name:     rel,
			Method:   zip.Deflate,
			Modified: canonicalZipModifiedTime,
		}
		header.SetMode(info.Mode())
		entryWriter, createErr := zw.CreateHeader(header)
		if createErr != nil {
			_ = file.Close()
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ZIP file entry", createErr)
		}
		copyErr := copyZipEntryContext(ctx, entryWriter, file)
		closeErr := file.Close()
		if copyErr != nil {
			return aicrerrors.PropagateOrWrap(
				copyErr, aicrerrors.ErrCodeInternal, "failed to copy verified bundle file into archive")
		}
		if closeErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to close verified bundle file", closeErr)
		}
	}

	if cleanupErr := cleanupStage(); cleanupErr != nil {
		return aicrerrors.Wrap(
			aicrerrors.ErrCodeInternal, "failed to clean verified bundle stage", cleanupErr)
	}
	if err := zw.Close(); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to finalize ZIP archive", err)
	}
	return nil
}

// StreamZipResponse creates a bounded context for compatibility callers and
// delegates to StreamZipResponseContext.
func StreamZipResponse(w http.ResponseWriter, dir string, output *result.Output) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.BundleHandlerTimeout)
	defer cancel()
	return StreamZipResponseContext(ctx, w, dir, output)
}

func copyZipEntryContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, zipCopyBufferSize)
	for {
		if err := zipContextError(ctx, "bundle archive copy canceled"); err != nil {
			return err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to write ZIP entry", writeErr)
			}
			if written != read {
				return aicrerrors.Wrap(
					aicrerrors.ErrCodeInternal, "failed to write complete ZIP entry", io.ErrShortWrite)
			}
			if err := zipContextError(ctx, "bundle archive copy canceled"); err != nil {
				return err
			}
		}
		if readErr != nil {
			if stderrors.Is(readErr, io.EOF) {
				return nil
			}
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read verified bundle file", readErr)
		}
	}
}

func zipContextError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, message, err)
	}
	return nil
}

// bundleParams holds parsed query parameters for bundle generation
type bundleParams struct {
	valueOverrides             []config.ComponentPath
	dynamicValues              []config.ComponentPath
	systemNodeSelector         map[string]string
	systemNodeTolerations      []corev1.Toleration
	acceleratedNodeSelector    map[string]string
	acceleratedNodeTolerations []corev1.Toleration
	workloadGateTaint          *corev1.Taint
	workloadSelector           map[string]string
	estimatedNodeCount         int
	deployer                   config.DeployerType
	repoURL                    string
	vendorCharts               bool
	serial                     bool
	appName                    string
	bundlers                   []string
}

// parseQueryParams extracts and validates all query parameters from the request
func parseQueryParams(r *http.Request) (*bundleParams, error) {
	query := r.URL.Query()
	params := &bundleParams{}

	var err error

	// Parse value overrides
	params.valueOverrides, err = config.ParseValueOverrides(query["set"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid set parameter", err)
	}

	// Parse dynamic value declarations
	params.dynamicValues, err = config.ParseDynamicValues(query["dynamic"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid dynamic parameter", err)
	}

	// Parse system node selectors
	params.systemNodeSelector, err = snapshotter.ParseNodeSelectors(query["system-node-selector"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid system-node-selector", err)
	}

	// Parse accelerated node selectors
	params.acceleratedNodeSelector, err = snapshotter.ParseNodeSelectors(query["accelerated-node-selector"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid accelerated-node-selector", err)
	}

	// Parse system node tolerations
	params.systemNodeTolerations, err = snapshotter.ParseTolerations(query["system-node-toleration"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid system-node-toleration", err)
	}

	// Parse accelerated node tolerations
	params.acceleratedNodeTolerations, err = snapshotter.ParseTolerations(query["accelerated-node-toleration"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid accelerated-node-toleration", err)
	}

	// Parse deployer type (helm, argocd)
	deployerStr := query.Get("deployer")
	if deployerStr == "" {
		params.deployer = config.DeployerHelm // default
	} else {
		params.deployer, err = config.ParseDeployerType(deployerStr)
		if err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid deployer parameter", err)
		}
	}

	// Parse repo URL (for Argo CD deployer)
	params.repoURL = query.Get("repo")

	// Parse workload-gate taint
	workloadGateStr := query.Get("workload-gate")
	if workloadGateStr != "" {
		params.workloadGateTaint, err = snapshotter.ParseTaint(workloadGateStr)
		if err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid workload-gate parameter", err)
		}
	}

	// Parse workload-selector
	params.workloadSelector, err = snapshotter.ParseNodeSelectors(query["workload-selector"])
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "Invalid workload-selector parameter", err)
	}

	// Parse nodes (estimated node count; 0 = unset)
	if nodesStr := query.Get("nodes"); nodesStr != "" {
		n, parseErr := strconv.Atoi(nodesStr)
		if parseErr != nil || n < 0 {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "nodes must be a non-negative integer")
		}
		params.estimatedNodeCount = n
	}

	// Parse vendor-charts (opt-in air-gap vendoring)
	if v := query.Get("vendor-charts"); v != "" {
		b, parseErr := strconv.ParseBool(v)
		if parseErr != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
				"vendor-charts must be a boolean (true/false)", parseErr)
		}
		params.vendorCharts = b
	}

	// Parse serial (deploy components one at a time instead of parallelizing
	// independent ones; the API counterpart of the --serial CLI flag).
	if v := query.Get("serial"); v != "" {
		b, parseErr := strconv.ParseBool(v)
		if parseErr != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
				"serial must be a boolean (true/false)", parseErr)
		}
		params.serial = b
	}

	// Parse app-name (parent Argo Application name for argocd / argocd-helm).
	// Reject on other deployers so a typo on a helm-deployer request fails
	// loudly rather than being silently ignored.
	if v := query.Get("app-name"); v != "" {
		if params.deployer != config.DeployerArgoCD && params.deployer != config.DeployerArgoCDHelm {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				"app-name is only valid with deployer=argocd or deployer=argocd-helm")
		}
		if validateErr := config.ValidateAppName(v); validateErr != nil {
			return nil, validateErr
		}
		params.appName = v
	}

	// Parse bundlers (positive filter on recipe component names). Comma
	// delimited and repeatable; whitespace around names is trimmed and empty
	// segments are dropped so "a, b," parses as ["a", "b"]. Presence is
	// keyed on the query map, not query.Get, so an explicit empty value
	// ("bundlers=" or "bundlers=,,") is rejected rather than silently
	// bundling everything — only an ABSENT parameter means no filter. See #1531.
	if values, ok := query["bundlers"]; ok {
		for _, v := range values {
			for _, name := range strings.Split(v, ",") {
				if name = strings.TrimSpace(name); name != "" {
					params.bundlers = append(params.bundlers, name)
				}
			}
		}
		if len(params.bundlers) == 0 {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				"bundlers must contain at least one component name")
		}
	}

	return params, nil
}
