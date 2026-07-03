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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

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
		config.WithAppName(params.appName),
		config.WithBundlers(params.bundlers),
	)
}

// StreamZipResponse creates a zip archive from the output directory and
// streams it to the response. Exported so the aicr.Client-backed REST
// handler (pkg/server) emits a consistent zip stream — same headers, same
// entry layout, same compression.
func StreamZipResponse(w http.ResponseWriter, dir string, output *result.Output) (retErr error) {
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
