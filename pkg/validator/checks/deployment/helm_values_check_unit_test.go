// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package deployment

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

func TestCheckHelmValues(t *testing.T) {
	tests := []struct {
		name        string
		setup       func() *checks.ValidationContext
		wantErr     bool
		errContains string
	}{
		{
			name: "all values match",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":                 "gpu-operator",
						"gpu-operator.values.driver.version": "570.86.16",
						"gpu-operator.values.driver.enabled": "true",
					}),
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{
							"version": "570.86.16",
							"enabled": true,
						},
					}),
				}
			},
			wantErr: false,
		},
		{
			name: "value mismatch",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":                 "gpu-operator",
						"gpu-operator.values.driver.version": "560.35.03",
					}),
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{
							"version": "570.86.16",
						},
					}),
				}
			},
			wantErr:     true,
			errContains: "driver.version",
		},
		{
			name: "component not in snapshot - skip",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context:  context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{}),
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{
							"version": "570.86.16",
						},
					}),
				}
			},
			wantErr: false,
		},
		{
			name: "component with no recipe values - skip",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":                 "gpu-operator",
						"gpu-operator.values.driver.version": "570.86.16",
					}),
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "kustomize component - skip",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context:  context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{}),
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name:      "my-kustomize",
								Type:      recipe.ComponentTypeKustomize,
								Overrides: map[string]any{"key": "val"},
							},
						},
					},
				}
			},
			wantErr: false,
		},
		{
			name: "nil snapshot",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context:  context.Background(),
					Snapshot: nil,
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{"version": "570.86.16"},
					}),
				}
			},
			wantErr:     true,
			errContains: "snapshot is not available",
		},
		{
			name: "nil recipe",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context:  context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{}),
					Recipe:   nil,
				}
			},
			wantErr:     true,
			errContains: "recipe is not available",
		},
		{
			name: "multiple components some match some mismatch",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":                        "gpu-operator",
						"gpu-operator.values.driver.version":        "570.86.16",
						"network-operator.chart":                    "network-operator",
						"network-operator.values.operator.replicas": "2",
					}),
					Recipe: &recipe.RecipeResult{
						ComponentRefs: []recipe.ComponentRef{
							{
								Name: "gpu-operator",
								Type: recipe.ComponentTypeHelm,
								Overrides: map[string]any{
									"driver": map[string]any{"version": "570.86.16"},
								},
							},
							{
								Name: "network-operator",
								Type: recipe.ComponentTypeHelm,
								Overrides: map[string]any{
									"operator": map[string]any{"replicas": float64(3)},
								},
							},
						},
					},
				}
			},
			wantErr:     true,
			errContains: "operator.replicas",
		},
		{
			name: "nested values flattened correctly",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart": "gpu-operator",
						"gpu-operator.values.operator.resources.limits.cpu":      "500m",
						"gpu-operator.values.operator.resources.limits.memory":   "256Mi",
						"gpu-operator.values.operator.resources.requests.cpu":    "100m",
						"gpu-operator.values.operator.resources.requests.memory": "128Mi",
					}),
					Recipe: recipeWithOverrides(map[string]any{
						"operator": map[string]any{
							"resources": map[string]any{
								"limits": map[string]any{
									"cpu":    "500m",
									"memory": "256Mi",
								},
								"requests": map[string]any{
									"cpu":    "100m",
									"memory": "128Mi",
								},
							},
						},
					}),
				}
			},
			wantErr: false,
		},
		{
			name: "boolean normalization",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":                 "gpu-operator",
						"gpu-operator.values.driver.enabled": "true",
					}),
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{"enabled": true},
					}),
				}
			},
			wantErr: false,
		},
		{
			name: "numeric value normalization",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":           "gpu-operator",
						"gpu-operator.values.replicas": "3",
					}),
					Recipe: recipeWithOverrides(map[string]any{
						"replicas": float64(3),
					}),
				}
			},
			wantErr: false,
		},
		{
			name: "snapshot key not present for recipe key - skip that key",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: snapshotWithHelm(map[string]string{
						"gpu-operator.chart":                 "gpu-operator",
						"gpu-operator.values.driver.version": "570.86.16",
					}),
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{
							"version":  "570.86.16",
							"newField": "some-value",
						},
					}),
				}
			},
			wantErr: false,
		},
		{
			name: "no helm subtype in snapshot - skip",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					Snapshot: &snapshotter.Snapshot{
						Measurements: []*measurement.Measurement{
							{
								Type: measurement.TypeK8s,
								Subtypes: []measurement.Subtype{
									{Name: "version", Data: map[string]measurement.Reading{
										"version": measurement.Str("1.30.0"),
									}},
								},
							},
						},
					},
					Recipe: recipeWithOverrides(map[string]any{
						"driver": map[string]any{"version": "570.86.16"},
					}),
				}
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup()
			err := validateHelmValues(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("validateHelmValues() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("validateHelmValues() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckHelmValuesRegistration(t *testing.T) {
	check, ok := checks.GetCheck("helm-values")
	if !ok {
		t.Fatal("helm-values check not registered")
	}

	if check.Name != "helm-values" {
		t.Errorf("Name = %v, want helm-values", check.Name)
	}

	if check.Phase != "deployment" { //nolint:goconst // test literal
		t.Errorf("Phase = %v, want deployment", check.Phase)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

func TestFlattenValues(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		expected map[string]string
	}{
		{
			name:     "empty map",
			input:    map[string]any{},
			expected: map[string]string{},
		},
		{
			name:     "flat values",
			input:    map[string]any{"key": "val", "enabled": true},
			expected: map[string]string{"key": "val", "enabled": "true"},
		},
		{
			name: "nested values",
			input: map[string]any{
				"driver": map[string]any{
					"version": "570.86.16",
					"enabled": true,
				},
			},
			expected: map[string]string{
				"driver.version": "570.86.16",
				"driver.enabled": "true",
			},
		},
		{
			name: "deeply nested",
			input: map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": "deep",
					},
				},
			},
			expected: map[string]string{"a.b.c": "deep"},
		},
		{
			name:     "numeric values",
			input:    map[string]any{"count": float64(5), "intVal": 42},
			expected: map[string]string{"count": "5", "intVal": "42"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := flattenValues(tt.input)

			if len(result) != len(tt.expected) {
				t.Fatalf("got %d keys, want %d\ngot: %v", len(result), len(tt.expected), result)
			}

			for k, want := range tt.expected {
				got, ok := result[k]
				if !ok {
					t.Errorf("missing key %q", k)
					continue
				}
				if got != want {
					t.Errorf("key %q = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// snapshotWithHelm creates a snapshot with K8s helm subtype data from flat key-value pairs.
func snapshotWithHelm(data map[string]string) *snapshotter.Snapshot {
	helmData := make(map[string]measurement.Reading, len(data))
	for k, v := range data {
		helmData[k] = measurement.Str(v)
	}

	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{Name: "helm", Data: helmData},
				},
			},
		},
	}
}

// recipeWithOverrides creates a RecipeResult with a single gpu-operator Helm component and inline overrides.
func recipeWithOverrides(overrides map[string]any) *recipe.RecipeResult {
	return &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "gpu-operator",
				Type:      recipe.ComponentTypeHelm,
				Overrides: overrides,
			},
		},
	}
}
