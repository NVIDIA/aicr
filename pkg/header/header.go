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

package header

import (
	"time"
)

// Kind represents the type of AICR resource.
// All AICR resources should use these constants for consistency.
type Kind string

// Valid Kind constants for all AICR resource types.
const (
	KindSnapshot     Kind = "Snapshot"
	KindRecipe       Kind = "Recipe"
	KindRecipeResult Kind = "RecipeResult"
)

// String returns the string representation of the Kind.
func (k Kind) String() string {
	return string(k)
}

// newHeader creates a new Header instance with an initialized Metadata map.
func newHeader() *Header {
	return &Header{
		Metadata: make(map[string]string),
	}
}

// Header contains metadata and versioning information for AICR resources.
// It follows Kubernetes-style resource conventions with Kind, APIVersion, and Metadata fields.
type Header struct {
	// Kind is the type of the snapshot object.
	Kind Kind `json:"kind,omitempty" yaml:"kind,omitempty"`

	// APIVersion is the API version of the snapshot object.
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`

	// Metadata contains key-value pairs with metadata about the snapshot.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// Init initializes the Header with the specified kind, apiVersion, and version.
// It sets the Kind, APIVersion, and populates Metadata with timestamp and version.
// Uses unprefixed keys (timestamp, version) for all kinds.
func (h *Header) Init(kind Kind, apiVersion string, version string) {
	h.Kind = kind
	h.APIVersion = apiVersion
	h.Metadata = make(map[string]string)

	// Use unprefixed keys for all kinds
	h.Metadata["timestamp"] = time.Now().UTC().Format(time.RFC3339)
	if version != "" {
		h.Metadata["version"] = version
	}
}
