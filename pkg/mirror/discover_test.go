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

package mirror

import (
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestDiscover(t *testing.T) {
	tests := []struct {
		name         string
		rec          *recipe.RecipeResult
		helmRenderer helm.Renderer
		ctxFunc      func() (context.Context, context.CancelFunc)
		wantErr      bool
		wantImages   int
		wantCharts   int
		wantComps    int
		wantWarnings bool
	}{
		{
			name:    "nil recipe",
			rec:     nil,
			wantErr: true,
		},
		{
			name: "empty recipe",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{},
			},
			helmRenderer: &helmtest.MockRenderer{},
			wantImages:   0,
			wantCharts:   0,
			wantComps:    0,
		},
		{
			name: "helm component with images",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "gpu-operator",
						Type:    recipe.ComponentTypeHelm,
						Source:  "oci://ghcr.io/nvidia",
						Chart:   "gpu-operator",
						Version: "v25.3.0",
					},
				},
			},
			helmRenderer: &helmtest.MockRenderer{
				Rendered: map[string][]byte{
					"gpu-operator": []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
spec:
  template:
    spec:
      containers:
      - name: gpu-operator
        image: nvcr.io/nvidia/gpu-operator:v25.3.0
      - name: validator
        image: nvcr.io/nvidia/cloud-native/gpu-operator-validator:v25.3.0
`),
				},
			},
			wantImages: 2,
			wantCharts: 1,
			wantComps:  1,
		},
		{
			name: "helm render failure produces warning",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "broken-chart",
						Type:    recipe.ComponentTypeHelm,
						Source:  "oci://example.com",
						Chart:   "broken",
						Version: "v1.0.0",
					},
				},
			},
			helmRenderer: &helmtest.MockRenderer{
				Errs: map[string]error{
					"broken-chart": errors.New(errors.ErrCodeInternal, "chart not found"),
				},
			},
			wantImages:   0,
			wantCharts:   1,
			wantComps:    1,
			wantWarnings: true,
		},
		{
			name: "multiple components with deduplication",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "comp-a",
						Type:    recipe.ComponentTypeHelm,
						Source:  "oci://example.com",
						Chart:   "comp-a",
						Version: "v1.0",
					},
					{
						Name:    "comp-b",
						Type:    recipe.ComponentTypeHelm,
						Source:  "oci://example.com",
						Chart:   "comp-b",
						Version: "v2.0",
					},
				},
			},
			helmRenderer: &helmtest.MockRenderer{
				Rendered: map[string][]byte{
					"comp-a": []byte(`
apiVersion: v1
kind: Pod
spec:
  containers:
  - image: shared/image:v1
  - image: a-only/image:v1
`),
					"comp-b": []byte(`
apiVersion: v1
kind: Pod
spec:
  containers:
  - image: shared/image:v1
  - image: b-only/image:v1
`),
				},
			},
			wantImages: 3, // shared deduped
			wantCharts: 2,
			wantComps:  2,
		},
		{
			name: "disabled component skipped",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:      "disabled-comp",
						Type:      recipe.ComponentTypeHelm,
						Source:    "oci://example.com",
						Chart:     "disabled",
						Version:   "v1.0",
						Overrides: map[string]any{"enabled": false},
					},
				},
			},
			helmRenderer: &helmtest.MockRenderer{},
			wantImages:   0,
			wantCharts:   0,
			wantComps:    0,
		},
		{
			name: "context cancellation returns error",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "slow-comp",
						Type:    recipe.ComponentTypeHelm,
						Source:  "oci://example.com",
						Chart:   "slow",
						Version: "v1.0",
					},
				},
			},
			helmRenderer: &helmtest.BlockingRenderer{},
			ctxFunc: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), time.Millisecond)
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []Option
			if tt.helmRenderer != nil {
				opts = append(opts, WithHelmRenderer(tt.helmRenderer))
			}

			ctx := context.Background()
			cancel := func() {}
			if tt.ctxFunc != nil {
				ctx, cancel = tt.ctxFunc()
			}
			defer cancel()

			lister := NewLister(opts...)
			result, err := lister.Discover(ctx, tt.rec)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Discover() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			if got := len(result.Images); got != tt.wantImages {
				t.Errorf("Images count = %d, want %d (images: %v)", got, tt.wantImages, result.Images)
			}
			if got := len(result.Charts); got != tt.wantCharts {
				t.Errorf("Charts count = %d, want %d", got, tt.wantCharts)
			}
			if got := len(result.Components); got != tt.wantComps {
				t.Errorf("Components count = %d, want %d", got, tt.wantComps)
			}

			if tt.wantWarnings {
				hasWarnings := false
				for _, comp := range result.Components {
					if len(comp.Warnings) > 0 {
						hasWarnings = true
						break
					}
				}
				if !hasWarnings {
					t.Error("expected warnings but none found")
				}
			}
		})
	}
}

func TestSetNestedValue(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		value    string
		initial  map[string]any
		expected any
	}{
		{
			name:     "simple key",
			path:     "key",
			value:    "val",
			initial:  map[string]any{},
			expected: "val",
		},
		{
			name:     "nested key",
			path:     "a.b.c",
			value:    "deep",
			initial:  map[string]any{},
			expected: "deep",
		},
		{
			name:     "override existing",
			path:     "driver.version",
			value:    "new",
			initial:  map[string]any{"driver": map[string]any{"version": "old"}},
			expected: "new",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setNestedValue(tt.initial, tt.path, tt.value)

			// Walk to the value.
			var current any = tt.initial
			for _, part := range splitPath(tt.path) {
				m, ok := current.(map[string]any)
				if !ok {
					t.Fatalf("expected map at %q, got %T", part, current)
				}
				current = m[part]
			}

			if current != tt.expected {
				t.Errorf("got %v, want %v", current, tt.expected)
			}
		})
	}
}

func TestKubeVersionFromConstraints(t *testing.T) {
	tests := []struct {
		name        string
		constraints []recipe.Constraint
		want        string
	}{
		{
			name:        "no constraints returns default",
			constraints: nil,
			want:        defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "no k8s constraint returns default",
			constraints: []recipe.Constraint{
				{Name: "worker-os", Value: "ubuntu"},
			},
			want: defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "semver range >= 1.32.4",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
			},
			want: "1.32.4",
		},
		{
			name: "semver range >= 1.25",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.25"},
			},
			want: "1.25",
		},
		{
			name: "exact version",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: "1.34.0"},
			},
			want: "1.34.0",
		},
		{
			name: "version with v prefix",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= v1.32.0"},
			},
			want: "1.32.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := KubeVersionFromConstraints(tt.constraints)
			if got != tt.want {
				t.Errorf("KubeVersionFromConstraints() = %q, want %q", got, tt.want)
			}
		})
	}
}

func splitPath(path string) []string {
	var parts []string
	for _, p := range []byte(path) {
		switch {
		case p == '.':
			parts = append(parts, "")
		case len(parts) == 0:
			parts = append(parts, string(p))
		default:
			parts[len(parts)-1] += string(p)
		}
	}
	return parts
}
