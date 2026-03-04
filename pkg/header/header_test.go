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

package header

import (
	"testing"
	"time"
)

// Test API version constant - matches aicr.nvidia.com/v1alpha1 used by snapshotter and recipe packages
const testAPIVersion = "aicr.nvidia.com/v1alpha1"

func TestKind_String(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		want string
	}{
		{
			name: "Snapshot kind",
			kind: KindSnapshot,
			want: "Snapshot",
		},
		{
			name: "Recipe kind",
			kind: KindRecipe,
			want: "Recipe",
		},
		{
			name: "Custom kind",
			kind: Kind("CustomKind"),
			want: "CustomKind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.kind.String(); got != tt.want {
				t.Errorf("Kind.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	h := New()
	if h == nil {
		t.Fatal("New() returned nil")
	}
	if h.Metadata == nil {
		t.Error("Metadata should be initialized")
	}
	if len(h.Metadata) != 0 {
		t.Errorf("Metadata should be empty, got %d items", len(h.Metadata))
	}
}

func TestHeader_Init(t *testing.T) {
	tests := []struct {
		name    string
		kind    Kind
		version string
		check   func(*testing.T, *Header)
	}{
		{
			name:    "Init Snapshot with version",
			kind:    KindSnapshot,
			version: "v1.0.0",
			check: func(t *testing.T, h *Header) {
				if h.Kind != KindSnapshot {
					t.Errorf("Kind = %v, want %v", h.Kind, KindSnapshot)
				}
				if h.APIVersion != testAPIVersion {
					t.Errorf("APIVersion = %v, want %s", h.APIVersion, testAPIVersion)
				}
				if h.Metadata == nil {
					t.Fatal("Metadata is nil")
				}
				if _, exists := h.Metadata["timestamp"]; !exists {
					t.Error("timestamp not found in metadata")
				}
				if v := h.Metadata["version"]; v != "v1.0.0" {
					t.Errorf("version = %v, want v1.0.0", v)
				}
			},
		},
		{
			name:    "Init Recipe without version",
			kind:    KindRecipe,
			version: "",
			check: func(t *testing.T, h *Header) {
				if h.Kind != KindRecipe {
					t.Errorf("Kind = %v, want %v", h.Kind, KindRecipe)
				}
				if h.APIVersion != testAPIVersion {
					t.Errorf("APIVersion = %v, want %s", h.APIVersion, testAPIVersion)
				}
				if _, exists := h.Metadata["timestamp"]; !exists {
					t.Error("timestamp not found in metadata")
				}
				if _, exists := h.Metadata["version"]; exists {
					t.Error("version should not exist when version is empty")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Header{}
			h.Init(tt.kind, testAPIVersion, tt.version)
			tt.check(t, h)
		})
	}
}

func TestHeader_Init_TimestampFormat(t *testing.T) {
	h := &Header{}
	h.Init(KindSnapshot, testAPIVersion, "v1.0.0")

	timestamp, exists := h.Metadata["timestamp"]
	if !exists {
		t.Fatal("timestamp not found in metadata")
	}

	// Parse the timestamp to ensure it's valid RFC3339
	parsedTime, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		t.Errorf("Failed to parse timestamp as RFC3339: %v", err)
	}

	// Verify timestamp is recent (within last minute)
	now := time.Now().UTC()
	diff := now.Sub(parsedTime)
	if diff < 0 || diff > time.Minute {
		t.Errorf("Timestamp %v is not recent (diff: %v)", timestamp, diff)
	}
}

func TestHeader_Init_OverwritesExistingData(t *testing.T) {
	h := &Header{
		Kind:       KindRecipe,
		APIVersion: "old.example.com/v1",
		Metadata: map[string]string{
			"existing-key": "existing-value",
		},
	}

	h.Init(KindSnapshot, testAPIVersion, "v2.0.0")

	// Check that old data is replaced
	if h.Kind != KindSnapshot {
		t.Errorf("Kind was not updated, got %v, want %v", h.Kind, KindSnapshot)
	}

	if h.APIVersion != testAPIVersion {
		t.Errorf("APIVersion was not updated, got %v, want %s", h.APIVersion, testAPIVersion)
	}

	// Metadata should be completely replaced
	if _, exists := h.Metadata["existing-key"]; exists {
		t.Error("Old metadata key should have been removed")
	}

	if _, exists := h.Metadata["version"]; !exists {
		t.Error("New metadata should be present")
	}
}

func TestConstants(t *testing.T) {
	// Verify constant values haven't changed
	if KindSnapshot != "Snapshot" {
		t.Errorf("KindSnapshot = %v, want Snapshot", KindSnapshot)
	}
	if KindRecipe != "Recipe" {
		t.Errorf("KindRecipe = %v, want Recipe", KindRecipe)
	}
	if KindRecipeResult != "RecipeResult" {
		t.Errorf("KindRecipeResult = %v, want RecipeResult", KindRecipeResult)
	}
	if KindValidationResult != "ValidationResult" {
		t.Errorf("KindValidationResult = %v, want ValidationResult", KindValidationResult)
	}
	// Note: API version constants moved to resource-specific packages
	// - snapshotter.FullAPIVersion for Snapshot resources
	// - recipe.FullAPIVersion for Recipe resources
	// This allows independent evolution of each resource type's API version
}
