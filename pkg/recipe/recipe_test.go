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

// recipe_test.go tests the Recipe struct and its validation methods.
//
// Area of Concern: Runtime recipe validation
// - Recipe.Validate() - ensures recipe has measurements
// - Recipe.ValidateStructure() - validates measurement structure integrity
// - Recipe.ValidateMeasurementExists() - checks specific measurements exist
// - Recipe.ValidateSubtypeExists() - checks specific subtypes exist
// - ValidateRequiredKeys() - validates required keys in readings
//
// These tests use synthesized Go structs to verify the Recipe type behavior
// after a recipe has been built from metadata.
//
// Related test files:
// - metadata_test.go: Tests RecipeMetadata types, Merge(), TopologicalSort(),
//   ValidateDependencies(), and MetadataStore inheritance chain resolution
// - yaml_test.go: Tests embedded YAML data files for schema conformance,
//   valid references, enum values, and constraint syntax

package recipe

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

func TestRecipe_Validate(t *testing.T) {
	tests := []struct {
		name    string
		recipe  *Recipe
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil recipe",
			recipe:  nil,
			wantErr: true,
			errMsg:  "recipe cannot be nil",
		},
		{
			name:    "recipe with no measurements",
			recipe:  &Recipe{Measurements: []*measurement.Measurement{}},
			wantErr: true,
			errMsg:  "recipe has no measurements",
		},
		{
			name: "valid recipe with measurements",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("1.28.0"),
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.recipe.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Recipe.Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("Recipe.Validate() error = %v, want to contain %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestRecipe_ValidateStructure(t *testing.T) {
	tests := []struct {
		name    string
		recipe  *Recipe
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil recipe",
			recipe:  nil,
			wantErr: true,
			errMsg:  "recipe cannot be nil",
		},
		{
			name: "nil measurement",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{nil},
			},
			wantErr: true,
			errMsg:  "measurement at index 0 is nil",
		},
		{
			name: "measurement with empty type",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type: "",
						Subtypes: []measurement.Subtype{
							{Name: "test"},
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "measurement at index 0 has empty type",
		},
		{
			name: "measurement with no subtypes",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type:     measurement.TypeK8s,
						Subtypes: []measurement.Subtype{},
					},
				},
			},
			wantErr: true,
			errMsg:  "measurement type K8s has no subtypes",
		},
		{
			name: "subtype with empty name",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "",
								Data: map[string]measurement.Reading{},
							},
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "subtype at index 0 in measurement K8s has empty name",
		},
		{
			name: "subtype with nil data",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: nil,
							},
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "subtype server in measurement K8s has nil data",
		},
		{
			name: "valid recipe structure",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("1.28.0"),
								},
							},
						},
					},
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{
								Name: "device",
								Data: map[string]measurement.Reading{
									"model": measurement.Str("H100"),
								},
							},
						},
					},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.recipe.ValidateStructure()
			if (err != nil) != tt.wantErr {
				t.Errorf("Recipe.ValidateStructure() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("Recipe.ValidateStructure() error = %v, want to contain %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestRecipe_ValidateMeasurementExists(t *testing.T) {
	validRecipe := &Recipe{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("1.28.0"),
						},
					},
				},
			},
			{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{
					{
						Name: "device",
						Data: map[string]measurement.Reading{
							"model": measurement.Str("H100"),
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name            string
		recipe          *Recipe
		measurementType measurement.Type
		wantErr         bool
		errContains     string
	}{
		{
			name:            "nil recipe",
			recipe:          nil,
			measurementType: measurement.TypeK8s,
			wantErr:         true,
			errContains:     "recipe cannot be nil",
		},
		{
			name:            "measurement exists",
			recipe:          validRecipe,
			measurementType: measurement.TypeK8s,
			wantErr:         false,
		},
		{
			name:            "measurement does not exist",
			recipe:          validRecipe,
			measurementType: measurement.TypeOS,
			wantErr:         true,
			errContains:     "measurement type OS not found in recipe",
		},
		{
			name: "multiple measurements - find first",
			recipe: &Recipe{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{Name: "server", Data: map[string]measurement.Reading{}},
						},
					},
					{
						Type: measurement.TypeGPU,
						Subtypes: []measurement.Subtype{
							{Name: "device", Data: map[string]measurement.Reading{}},
						},
					},
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{Name: "image", Data: map[string]measurement.Reading{}},
						},
					},
				},
			},
			measurementType: measurement.TypeK8s,
			wantErr:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.recipe.ValidateMeasurementExists(tt.measurementType)
			if (err != nil) != tt.wantErr {
				t.Errorf("Recipe.ValidateMeasurementExists() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil {
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Recipe.ValidateMeasurementExists() error = %v, want to contain %v", err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestRecipe_ValidateSubtypeExists(t *testing.T) {
	validRecipe := &Recipe{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("1.28.0"),
						},
					},
					{
						Name: "image",
						Data: map[string]measurement.Reading{
							"name": measurement.Str("nginx"),
						},
					},
				},
			},
			{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{
					{
						Name: "device",
						Data: map[string]measurement.Reading{
							"model": measurement.Str("H100"),
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name            string
		recipe          *Recipe
		measurementType measurement.Type
		subtypeName     string
		wantErr         bool
		errContains     string
	}{
		{
			name:            "nil recipe",
			recipe:          nil,
			measurementType: measurement.TypeK8s,
			subtypeName:     "server",
			wantErr:         true,
			errContains:     "recipe cannot be nil",
		},
		{
			name:            "measurement does not exist",
			recipe:          validRecipe,
			measurementType: measurement.TypeOS,
			subtypeName:     "server",
			wantErr:         true,
			errContains:     "measurement type OS not found in recipe",
		},
		{
			name:            "subtype exists",
			recipe:          validRecipe,
			measurementType: measurement.TypeK8s,
			subtypeName:     "server",
			wantErr:         false,
		},
		{
			name:            "subtype does not exist in measurement",
			recipe:          validRecipe,
			measurementType: measurement.TypeK8s,
			subtypeName:     "config",
			wantErr:         true,
			errContains:     "subtype config not found in measurement type K8s",
		},
		{
			name:            "subtype exists in different measurement",
			recipe:          validRecipe,
			measurementType: measurement.TypeGPU,
			subtypeName:     "device",
			wantErr:         false,
		},
		{
			name:            "multiple subtypes - find correct one",
			recipe:          validRecipe,
			measurementType: measurement.TypeK8s,
			subtypeName:     "image",
			wantErr:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.recipe.ValidateSubtypeExists(tt.measurementType, tt.subtypeName)
			if (err != nil) != tt.wantErr {
				t.Errorf("Recipe.ValidateSubtypeExists() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil {
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("Recipe.ValidateSubtypeExists() error = %v, want to contain %v", err.Error(), tt.errContains)
				}
			}
		})
	}
}

func TestValidateRequiredKeys(t *testing.T) {
	tests := []struct {
		name         string
		subtype      *measurement.Subtype
		requiredKeys []string
		wantErr      bool
		errContains  string
	}{
		{
			name:         "nil subtype",
			subtype:      nil,
			requiredKeys: []string{"key1"},
			wantErr:      true,
			errContains:  "subtype is nil",
		},
		{
			name: "all required keys present",
			subtype: &measurement.Subtype{
				Name: "test",
				Data: map[string]measurement.Reading{
					"key1": measurement.Str("value1"),
					"key2": measurement.Str("value2"),
					"key3": measurement.Str("value3"),
				},
			},
			requiredKeys: []string{"key1", "key2"},
			wantErr:      false,
		},
		{
			name: "missing required key",
			subtype: &measurement.Subtype{
				Name: "test",
				Data: map[string]measurement.Reading{
					"key1": measurement.Str("value1"),
				},
			},
			requiredKeys: []string{"key1", "key2"},
			wantErr:      true,
			errContains:  "required key key2 not found in subtype test",
		},
		{
			name: "empty required keys list",
			subtype: &measurement.Subtype{
				Name: "test",
				Data: map[string]measurement.Reading{
					"key1": measurement.Str("value1"),
				},
			},
			requiredKeys: []string{},
			wantErr:      false,
		},
		{
			name: "empty data map with required keys",
			subtype: &measurement.Subtype{
				Name: "test",
				Data: map[string]measurement.Reading{},
			},
			requiredKeys: []string{"key1"},
			wantErr:      true,
			errContains:  "required key key1 not found in subtype test",
		},
		{
			name: "case sensitive key check",
			subtype: &measurement.Subtype{
				Name: "test",
				Data: map[string]measurement.Reading{
					"Key1": measurement.Str("value1"),
				},
			},
			requiredKeys: []string{"key1"},
			wantErr:      true,
			errContains:  "required key key1 not found in subtype test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRequiredKeys(tt.subtype, tt.requiredKeys)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateRequiredKeys() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err != nil {
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ValidateRequiredKeys() error = %v, want to contain %v", err.Error(), tt.errContains)
				}
			}
		})
	}
}
