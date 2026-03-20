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
//	    APIVersion string    `json:"apiVersion" yaml:"apiVersion"` // API version (e.g., "v1")
//	    Kind       string    `json:"kind" yaml:"kind"`             // Resource type (e.g., "Recipe", "Snapshot")
//	    Metadata   *Metadata `json:"metadata,omitempty" yaml:"metadata,omitempty"` // Optional metadata
//	}
//
// Metadata includes timestamps and version information:
//
//	type Metadata struct {
//	    Created time.Time              `json:"created" yaml:"created"`       // Creation timestamp
//	    Version string                 `json:"version,omitempty" yaml:"version,omitempty"` // Tool version
//	    Custom  map[string]any `json:"custom,omitempty" yaml:"custom,omitempty"`   // Custom fields
//	}
//
// # Usage
//
// Create a header for a recipe:
//
//	header := header.Header{
//	    APIVersion: "v1",
//	    Kind:       "Recipe",
//	    Metadata: &header.Metadata{
//	        Created: time.Now(),
//	        Version: "v1.0.0",
//	    },
//	}
//
// Create a header for a snapshot:
//
//	header := header.Header{
//	    APIVersion: "v1",
//	    Kind:       "Snapshot",
//	    Metadata: &header.Metadata{
//	        Created: time.Now(),
//	        Version: "v1.0.0",
//	        Custom: map[string]any{
//	            "node": "gpu-node-1",
//	            "cluster": "production",
//	        },
//	    },
//	}
//
// # Serialization
//
// Headers serialize consistently to JSON and YAML:
//
//	{
//	  "apiVersion": "v1",
//	  "kind": "Recipe",
//	  "metadata": {
//	    "created": "2025-12-30T10:30:00Z",
//	    "version": "v1.0.0"
//	  }
//	}
//
// # API Versioning
//
// The APIVersion field enables evolution of data formats:
//   - v1: Current stable API
//   - Future versions can add fields with backward compatibility
//
// Tools should check APIVersion before parsing:
//
//	if header.APIVersion != "v1" {
//	    return fmt.Errorf("unsupported API version: %s", header.APIVersion)
//	}
//
// # Kind Field
//
// The Kind field identifies the resource type:
//   - Recipe: Configuration recommendations
//   - Snapshot: System configuration capture
//   - Bundle: Deployment artifact metadata
//
// # Custom Metadata
//
// Custom fields enable extensibility without API version changes:
//
//	metadata.Custom = map[string]any{
//	    "node": "gpu-node-1",
//	    "cluster": "production",
//	    "environment": "staging",
//	}
//
// # Timestamps
//
// Timestamps use RFC3339 format for consistency:
//
//	metadata.Created = time.Now().UTC()
//	// Serializes as: "2025-12-30T10:30:00Z"
//
// # Validation
//
// While Header doesn't enforce validation, consumers should verify:
//   - APIVersion is supported
//   - Kind is recognized
//   - Created timestamp is reasonable
//   - Version is a valid semantic version (if present)
package header
