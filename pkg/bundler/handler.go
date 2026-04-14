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

package bundler

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/server"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// DefaultBundleTimeout is the timeout for bundle generation.
// Exported for backwards compatibility; prefer using defaults.BundleHandlerTimeout.
const DefaultBundleTimeout = defaults.BundleHandlerTimeout

// HandleBundles processes bundle generation requests.
// It accepts a POST request with a JSON body containing the recipe (RecipeResult).
// Supports query parameters:
//   - set: Value overrides in format "bundler:path.to.field=value" (can be repeated)
//   - system-node-selector: Node selectors for system components in format "key=value" (can be repeated)
//   - system-node-toleration: Tolerations for system components in format "key=value:effect" (can be repeated)
//   - accelerated-node-selector: Node selectors for GPU nodes in format "key=value" (can be repeated)
//   - accelerated-node-toleration: Tolerations for GPU nodes in format "key=value:effect" (can be repeated)
//   - workload-gate: Taint for skyhook-operator runtime required in format "key=value:effect" or "key:effect"
//   - workload-selector: Label selector for skyhook-customizations in format "key=value" (can be repeated)
//   - nodes: Estimated number of GPU nodes (sets estimatedNodeCount in skyhook-operator; 0 = unset)
//
// The response is a zip archive containing the Helm per-component bundle:
//   - README.md: Root deployment guide
//   - deploy.sh: Automation script
//   - recipe.yaml: Copy of the input recipe
//   - <component>/values.yaml: Helm values per component
//   - <component>/README.md: Component install/upgrade/uninstall
//   - checksums.txt: SHA256 checksums of generated files
//
// Example:
//
//	POST /v1/bundle?set=gpuoperator:gds.enabled=true
//	Content-Type: application/json
//	Body: { "apiVersion": "aicr.nvidia.com/v1alpha1", "kind": "Recipe", ... }
func (b *DefaultBundler) HandleBundles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		server.WriteError(w, r, http.StatusMethodNotAllowed, aicrerrors.ErrCodeMethodNotAllowed,
			"Method not allowed", false, map[string]any{
				"method": r.Method,
			})
		return
	}

	// Add request-scoped timeout
	ctx, cancel := context.WithTimeout(r.Context(), DefaultBundleTimeout)
	defer cancel()

	// Parse all query parameters
	params, err := parseQueryParams(r)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Invalid query parameters", nil)
		return
	}

	// Parse request body directly as RecipeResult
	var recipeResult recipe.RecipeResult
	err = json.NewDecoder(r.Body).Decode(&recipeResult)
	if err != nil {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Invalid request body", false, map[string]any{
				"error": err.Error(),
			})
		return
	}

	// Validate recipe has component references
	if len(recipeResult.ComponentRefs) == 0 {
		server.WriteError(w, r, http.StatusBadRequest, aicrerrors.ErrCodeInvalidRequest,
			"Recipe must contain at least one component reference", false, nil)
		return
	}

	// Validate recipe criteria against allowlists (if configured)
	if b.AllowLists != nil && recipeResult.Criteria != nil {
		if validateErr := b.AllowLists.ValidateCriteria(recipeResult.Criteria); validateErr != nil {
			server.WriteErrorFromErr(w, r, validateErr, "Recipe criteria value not allowed", nil)
			return
		}
	}

	slog.Debug("bundle request received",
		"components", len(recipeResult.ComponentRefs),
		"value_overrides", len(params.valueOverrides),
		"system_node_selectors", len(params.systemNodeSelector),
		"accelerated_node_selectors", len(params.acceleratedNodeSelector),
	)

	// Create temporary directory for bundle output
	tempDir, err := os.MkdirTemp("", "aicr-bundle-*")
	if err != nil {
		server.WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			"Failed to create temporary directory", true, nil)
		return
	}
	defer os.RemoveAll(tempDir) // Clean up on exit

	// Create a new bundler with configuration
	bundler, err := New(
		WithConfig(config.NewConfig(
			config.WithValueOverrides(params.valueOverrides),
			config.WithSystemNodeSelector(params.systemNodeSelector),
			config.WithSystemNodeTolerations(params.systemNodeTolerations),
			config.WithAcceleratedNodeSelector(params.acceleratedNodeSelector),
			config.WithAcceleratedNodeTolerations(params.acceleratedNodeTolerations),
			config.WithWorkloadGateTaint(params.workloadGateTaint),
			config.WithWorkloadSelector(params.workloadSelector),
			config.WithEstimatedNodeCount(params.estimatedNodeCount),
			config.WithDeployer(params.deployer),
			config.WithRepoURL(params.repoURL),
		)),
	)
	if err != nil {
		server.WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			"Failed to create bundler", true, map[string]any{
				"error": err.Error(),
			})
		return
	}

	// Generate bundle
	output, err := bundler.Make(ctx, &recipeResult, tempDir)
	if err != nil {
		server.WriteErrorFromErr(w, r, err, "Failed to generate bundle", nil)
		return
	}

	// Check for bundle errors
	if output.HasErrors() {
		errorDetails := make([]map[string]any, 0, len(output.Errors))
		for _, be := range output.Errors {
			errorDetails = append(errorDetails, map[string]any{
				"bundler": be.BundlerType,
				"error":   be.Error,
			})
		}
		server.WriteError(w, r, http.StatusInternalServerError, aicrerrors.ErrCodeInternal,
			"Bundle generation failed", true, map[string]any{
				"errors": errorDetails,
			})
		return
	}

	// Stream zip response
	if err := streamZipResponse(w, tempDir, output); err != nil {
		// Can't write error response if we've already started writing
		slog.Error("failed to stream zip response", "error", err)
		return
	}
}

// streamZipResponse creates a zip archive from the output directory and streams it to the response.
func streamZipResponse(w http.ResponseWriter, dir string, output *result.Output) (retErr error) {
	// Set response headers before writing body
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"bundles.zip\"")
	w.Header().Set("X-Bundle-Files", strconv.Itoa(output.TotalFiles))
	w.Header().Set("X-Bundle-Size", strconv.FormatInt(output.TotalSize, 10))
	w.Header().Set("X-Bundle-Duration", output.TotalDuration.String())

	// Create zip writer directly to response
	zw := zip.NewWriter(w)
	defer func() {
		closeErr := zw.Close()
		if retErr == nil {
			retErr = closeErr
		}
	}()

	// Walk the directory and add all files to zip
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "walk error", err)
		}

		// Skip the root directory itself
		if path == dir {
			return nil
		}

		// Get relative path for zip entry
		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to get relative path", err)
		}

		// Create zip file header
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create file header", err)
		}
		header.Name = relPath

		// Preserve directory structure
		if info.IsDir() {
			header.Name += "/"
			_, headerErr := zw.CreateHeader(header)
			return headerErr
		}

		// Use deflate compression
		header.Method = zip.Deflate

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create zip entry", err)
		}

		// Open and copy file content
		file, err := os.Open(filepath.Clean(path)) //nolint:gosec // G122: path from internal os.MkdirTemp, not user input
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to open file", err)
		}
		_, copyErr := io.Copy(writer, file)
		file.Close()
		if copyErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to copy file content", copyErr)
		}

		return nil
	})
}

// bundleParams holds parsed query parameters for bundle generation
type bundleParams struct {
	valueOverrides             map[string]map[string]string
	systemNodeSelector         map[string]string
	systemNodeTolerations      []corev1.Toleration
	acceleratedNodeSelector    map[string]string
	acceleratedNodeTolerations []corev1.Toleration
	workloadGateTaint          *corev1.Taint
	workloadSelector           map[string]string
	estimatedNodeCount         int
	deployer                   config.DeployerType
	repoURL                    string
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

	return params, nil
}
