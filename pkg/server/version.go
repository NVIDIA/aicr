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
	"net/http"
	"strings"
)

const (
	// defaultAPIVersion is the default API version if none is negotiated
	defaultAPIVersion = "v1"
)

// negotiateAPIVersion extracts the API version from the Accept header.
// It supports version negotiation via the vendor MIME type:
//
//	Accept: application/vnd.nvidia.aicr.v2+json
//
// Multiple comma-separated media types and parameters (e.g., "; q=0.5") are
// tolerated. Returns defaultAPIVersion when no recognized vendor type is
// present.
const vendorMediaTypePrefix = "application/vnd.nvidia.aicr."

func negotiateAPIVersion(r *http.Request) string {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return defaultAPIVersion
	}

	for _, raw := range strings.Split(accept, ",") {
		// Strip parameters (e.g., "application/json; q=0.5" → "application/json").
		mediaType := strings.TrimSpace(raw)
		if i := strings.Index(mediaType, ";"); i >= 0 {
			mediaType = strings.TrimSpace(mediaType[:i])
		}
		// RFC 7231 §3.1.1.1: media types are case-insensitive.
		lower := strings.ToLower(mediaType)
		if !strings.HasPrefix(lower, vendorMediaTypePrefix) {
			continue
		}
		// "application/vnd.nvidia.aicr.v2+json" → "v2+json" → "v2"
		rest := strings.TrimPrefix(lower, vendorMediaTypePrefix)
		version := rest
		if i := strings.Index(rest, "+"); i >= 0 {
			version = rest[:i]
		}
		if isValidAPIVersion(version) {
			return version
		}
	}

	return defaultAPIVersion
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

// setAPIVersionHeader sets the API version header in the response.
// This helps clients understand which version of the API is being used.
func setAPIVersionHeader(w http.ResponseWriter, version string) {
	w.Header().Set("X-API-Version", version)
}
