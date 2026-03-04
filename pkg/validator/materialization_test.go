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

package validator

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func TestCompareComponentsAgainstSnapshot(t *testing.T) {
	tests := []struct {
		name           string
		recipeResult   *recipe.RecipeResult
		snap           *snapshotter.Snapshot
		wantCount      int
		wantStatuses   []ValidationStatus
		wantMsgSubstrs []string
	}{
		{
			name: "nil snapshot returns empty",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
				},
			},
			snap:      nil,
			wantCount: 0,
		},
		{
			name: "no helm data skips helm components",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("v1.33.0"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusSkipped},
			wantMsgSubstrs: []string{"no helm data in snapshot"},
		},
		{
			name: "helm component found with matching chart and version",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "gpu-operator",
						Type:      recipe.ComponentTypeHelm,
						Chart:     "gpu-operator",
						Version:   "v24.9.0",
						Namespace: "gpu-operator",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "helm",
								Data: map[string]measurement.Reading{
									"gpu-operator.chart":     measurement.Str("gpu-operator"),
									"gpu-operator.version":   measurement.Str("v24.9.0"),
									"gpu-operator.namespace": measurement.Str("gpu-operator"),
								},
							},
						},
					},
				},
			},
			wantCount:    1,
			wantStatuses: []ValidationStatus{ValidationStatusPass},
		},
		{
			name: "helm component not found in snapshot",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:  "gpu-operator",
						Type:  recipe.ComponentTypeHelm,
						Chart: "gpu-operator",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "helm",
								Data: map[string]measurement.Reading{
									"other-release.chart": measurement.Str("other-chart"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusFail},
			wantMsgSubstrs: []string{"not found in snapshot"},
		},
		{
			name: "helm component version mismatch",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "gpu-operator",
						Type:    recipe.ComponentTypeHelm,
						Chart:   "gpu-operator",
						Version: "v24.9.0",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "helm",
								Data: map[string]measurement.Reading{
									"gpu-operator.chart":   measurement.Str("gpu-operator"),
									"gpu-operator.version": measurement.Str("v24.6.0"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusFail},
			wantMsgSubstrs: []string{"version"},
		},
		{
			name: "helm component chart mismatch",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:  "gpu-operator",
						Type:  recipe.ComponentTypeHelm,
						Chart: "gpu-operator",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "helm",
								Data: map[string]measurement.Reading{
									"gpu-operator.chart": measurement.Str("wrong-chart"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusFail},
			wantMsgSubstrs: []string{"chart"},
		},
		{
			name: "multiple components mixed results",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "gpu-operator",
						Type:    recipe.ComponentTypeHelm,
						Chart:   "gpu-operator",
						Version: "v24.9.0",
					},
					{
						Name:  "network-operator",
						Type:  recipe.ComponentTypeHelm,
						Chart: "network-operator",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "helm",
								Data: map[string]measurement.Reading{
									"gpu-operator.chart":   measurement.Str("gpu-operator"),
									"gpu-operator.version": measurement.Str("v24.9.0"),
								},
							},
						},
					},
				},
			},
			wantCount:    2,
			wantStatuses: []ValidationStatus{ValidationStatusPass, ValidationStatusFail},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := compareComponentsAgainstSnapshot(tt.recipeResult, tt.snap)
			if len(results) != tt.wantCount {
				t.Fatalf("got %d results, want %d", len(results), tt.wantCount)
			}
			for i, wantStatus := range tt.wantStatuses {
				if results[i].Status != wantStatus {
					t.Errorf("result[%d].Status = %q, want %q", i, results[i].Status, wantStatus)
				}
			}
			for i, substr := range tt.wantMsgSubstrs {
				if !strings.Contains(results[i].Message, substr) {
					t.Errorf("result[%d].Message = %q, want substring %q", i, results[i].Message, substr)
				}
			}
		})
	}
}

func TestCompareComponentsAgainstSnapshot_ArgoCD(t *testing.T) {
	tests := []struct {
		name           string
		recipeResult   *recipe.RecipeResult
		snap           *snapshotter.Snapshot
		wantCount      int
		wantStatuses   []ValidationStatus
		wantMsgSubstrs []string
	}{
		{
			name: "kustomize component found via argocd single source",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "my-app",
						Type:    recipe.ComponentTypeKustomize,
						Source:  "https://github.com/example/my-app",
						Version: "v1.0.0",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "argocd",
								Data: map[string]measurement.Reading{
									"my-app.source.repoURL":        measurement.Str("https://github.com/example/my-app"),
									"my-app.source.targetRevision": measurement.Str("v1.0.0"),
									"my-app.namespace":             measurement.Str("default"),
								},
							},
						},
					},
				},
			},
			wantCount:    1,
			wantStatuses: []ValidationStatus{ValidationStatusPass},
		},
		{
			name: "kustomize component found via argocd multi-source",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:   "my-app",
						Type:   recipe.ComponentTypeKustomize,
						Source: "https://github.com/example/my-app",
						Tag:    "v2.0.0",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "argocd",
								Data: map[string]measurement.Reading{
									"my-app.sources.0.repoURL":        measurement.Str("https://github.com/example/my-app"),
									"my-app.sources.0.targetRevision": measurement.Str("v2.0.0"),
									"my-app.namespace":                measurement.Str("argocd"),
								},
							},
						},
					},
				},
			},
			wantCount:    1,
			wantStatuses: []ValidationStatus{ValidationStatusPass},
		},
		{
			name: "kustomize component not found",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:   "my-app",
						Type:   recipe.ComponentTypeKustomize,
						Source: "https://github.com/example/my-app",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "argocd",
								Data: map[string]measurement.Reading{
									"other-app.source.repoURL": measurement.Str("https://github.com/example/other"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusFail},
			wantMsgSubstrs: []string{"not found in snapshot"},
		},
		{
			name: "kustomize component source mismatch",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:   "my-app",
						Type:   recipe.ComponentTypeKustomize,
						Source: "https://github.com/example/my-app",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "argocd",
								Data: map[string]measurement.Reading{
									"my-app.source.repoURL": measurement.Str("https://github.com/example/wrong-app"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusFail},
			wantMsgSubstrs: []string{"source"},
		},
		{
			name: "no argocd data skips kustomize components",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:   "my-app",
						Type:   recipe.ComponentTypeKustomize,
						Source: "https://github.com/example/my-app",
					},
				},
			},
			snap: &snapshotter.Snapshot{
				Measurements: []*measurement.Measurement{
					{
						Type: measurement.TypeK8s,
						Subtypes: []measurement.Subtype{
							{
								Name: "server",
								Data: map[string]measurement.Reading{
									"version": measurement.Str("v1.33.0"),
								},
							},
						},
					},
				},
			},
			wantCount:      1,
			wantStatuses:   []ValidationStatus{ValidationStatusSkipped},
			wantMsgSubstrs: []string{"no argocd data"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := compareComponentsAgainstSnapshot(tt.recipeResult, tt.snap)
			if len(results) != tt.wantCount {
				t.Fatalf("got %d results, want %d", len(results), tt.wantCount)
			}
			for i, wantStatus := range tt.wantStatuses {
				if results[i].Status != wantStatus {
					t.Errorf("result[%d].Status = %q, want %q", i, results[i].Status, wantStatus)
				}
			}
			for i, substr := range tt.wantMsgSubstrs {
				if !strings.Contains(results[i].Message, substr) {
					t.Errorf("result[%d].Message = %q, want substring %q", i, results[i].Message, substr)
				}
			}
		})
	}
}
