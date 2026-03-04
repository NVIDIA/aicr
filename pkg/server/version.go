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

package server

import (
	"net/http"
	"strings"
)

const (
	// DefaultAPIVersion is the default API version if none is negotiated
	DefaultAPIVersion = "v1"
)

// negotiateAPIVersion extracts the API version from the Accept header.
// It supports version negotiation via Accept header like:
// Accept: application/vnd.nvidia.aicr.v2+json
// If no version is specified, it returns the default version (v1).
func negotiateAPIVersion(r *http.Request) string {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return DefaultAPIVersion
	}

	// Parse Accept header for custom vendor MIME type
	// Format: application/vnd.nvidia.aicr.v2+json
	if strings.Contains(accept, "application/vnd.nvidia.aicr.v") {
		parts := strings.Split(accept, ".")
		for i, part := range parts {
			if strings.HasPrefix(part, "v") && i < len(parts) {
				// Extract version (e.g., "v2+json" -> "v2")
				version := strings.Split(part, "+")[0]
				if isValidAPIVersion(version) {
					return version
				}
			}
		}
	}

	return DefaultAPIVersion
}

// isValidAPIVersion checks if the provided version string is a valid API version.
// Currently supports: v1
func isValidAPIVersion(version string) bool {
	validVersions := map[string]bool{
		"v1": true,
		// Add future versions here as they become available
		// "v2": true,
	}
	return validVersions[version]
}

// SetAPIVersionHeader sets the API version header in the response.
// This helps clients understand which version of the API is being used.
func SetAPIVersionHeader(w http.ResponseWriter, version string) {
	w.Header().Set("X-API-Version", version)
}
