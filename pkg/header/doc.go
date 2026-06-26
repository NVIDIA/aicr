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

// Package header provides common header types for AICR data structures.
//
// This package defines the Header type used across recipes, snapshots, and other
// AICR data structures to provide consistent metadata and versioning information.
//
// # Header Structure
//
// The Header contains standard fields for API versioning and metadata:
//
//	type Header struct {
//	    Kind       Kind              `json:"kind,omitempty" yaml:"kind,omitempty"`             // Resource type (Snapshot, Recipe, RecipeResult)
//	    APIVersion string            `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"` // API version (e.g., "aicr.run/v1alpha2")
//	    Metadata   map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`     // Free-form string metadata (timestamp, version, etc.)
//	}
//
// Metadata is a flat map[string]string populated by Init / InitWithTime with
// the unprefixed keys "timestamp" (RFC3339 UTC) and "version" (when supplied).
//
// # Usage
//
// Initialize a header for a recipe via Init:
//
//	var h header.Header
//	h.Init(header.KindRecipe, header.GroupVersion, "v1.0.0")
//	// h.Metadata == map[string]string{"timestamp": "...", "version": "v1.0.0"}
//
// For reproducible-build callers (SLSA, signed artifacts) inject a fixed
// timestamp via InitWithTime instead of Init:
//
//	var h header.Header
//	h.InitWithTime(header.KindSnapshot, header.GroupVersion, "v1.0.0", buildTime)
//
// # Serialization
//
// Headers serialize consistently to JSON and YAML:
//
//	{
//	  "apiVersion": "aicr.run/v1alpha2",
//	  "kind": "Recipe",
//	  "metadata": {
//	    "timestamp": "2025-12-30T10:30:00Z",
//	    "version": "v1.0.0"
//	  }
//	}
//
// # API Versioning
//
// The APIVersion field enables evolution of data formats. The current group and
// version is GroupVersion ("aicr.run/v1alpha2"); APIGroup derives from Domain.
// Per ADR-013 the move from the legacy aicr.nvidia.com/v1alpha1 group was a hard
// break — the old value is rejected, not migrated.
//
// Callers should gate on IsSupportedAPIVersion rather than comparing literals,
// so the single source of truth in this package stays authoritative:
//
//	if h.APIVersion != "" && !header.IsSupportedAPIVersion(h.APIVersion) {
//	    return fmt.Errorf("unsupported apiVersion %q; expected %s", h.APIVersion, header.GroupVersion)
//	}
//
// # Kind Field
//
// The Kind field is a typed constant identifying the resource:
//   - KindSnapshot ("Snapshot"): System configuration capture
//   - KindRecipe ("Recipe"): Configuration recommendations
//   - KindRecipeResult ("RecipeResult"): Resolved recipe with hydrated values
//
// # Custom Metadata
//
// Because Metadata is a flat map[string]string, callers may add their own
// keys alongside the Init-populated "timestamp" and "version":
//
//	h.Metadata["node"] = "gpu-node-1"
//	h.Metadata["cluster"] = "production"
//	h.Metadata["environment"] = "staging"
//
// # Timestamps
//
// Init writes the timestamp using RFC3339 format in UTC:
//
//	h.Init(header.KindRecipe, header.GroupVersion, "v1.0.0")
//	// h.Metadata["timestamp"] == "2025-12-30T10:30:00Z"
//
// # Validation
//
// While Header doesn't enforce validation, consumers should verify:
//   - APIVersion is supported
//   - Kind is recognized
//   - Metadata["timestamp"] is reasonable
//   - Version is a valid semantic version (if present)
package header
