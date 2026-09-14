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
	stderrors "errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func newProfiledRecipe() *recipe.RecipeResult {
	result := &recipe.RecipeResult{
		Kind:       recipe.RecipeResultKind,
		APIVersion: recipe.RecipeProfileAPIVersion,
		ComponentRefs: []recipe.ComponentRef{{
			Name:    "gpu-operator",
			Type:    recipe.ComponentTypeHelm,
			Source:  "https://helm.ngc.nvidia.com/nvidia",
			Chart:   "gpu-operator",
			Version: "v25.3.0",
			Overrides: map[string]any{
				"driver": map[string]any{"enabled": false},
			},
		}},
	}
	result.Metadata.SelectedProfile = &recipe.SelectedProfile{
		Name:  "gpuStack",
		Value: "driver-installed",
		OwnedPaths: map[string][]string{
			"gpu-operator": {"driver.enabled", "enabled"},
		},
	}
	return result
}

func TestPrepareMirrorCandidate_Canceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := prepareMirrorCandidate(ctx, &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
	}, nil)
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("prepareMirrorCandidate() error = %v, want ErrCodeTimeout", err)
	}
}

func mustParseOverride(t *testing.T, raw string) config.ComponentPath {
	t.Helper()
	var override config.ComponentPath
	if err := override.Parse(raw); err != nil {
		t.Fatalf("ComponentPath.Parse(%q) error = %v", raw, err)
	}
	return override
}

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
			// A source-only ref (no explicit chart) is a deployable shape
			// whose chart name falls back to the component name in the
			// deployers; the mirror inventory must apply the same fallback
			// rather than silently omitting the chart and its images.
			name: "source-only helm ref falls back to component name",
			rec: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{
						Name:    "gpu-operator",
						Type:    recipe.ComponentTypeHelm,
						Source:  "https://helm.ngc.nvidia.com/nvidia",
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
`),
				},
			},
			wantImages: 1,
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

func TestDiscover_ProfileLock(t *testing.T) {
	tests := []struct {
		name             string
		override         string
		wantErr          bool
		wantDriver       bool
		wantEnabledValue bool
	}{
		{
			name:       "divergent owned value",
			override:   "gpuoperator:driver.enabled=true",
			wantErr:    true,
			wantDriver: false,
		},
		{
			name:       "redundant owned value",
			override:   "gpuoperator:driver.enabled=false",
			wantDriver: false,
		},
		{
			name:             "redundant component presence",
			override:         "gpuoperator:enabled=true",
			wantDriver:       false,
			wantEnabledValue: false,
		},
		{
			name:       "divergent component presence",
			override:   "gpuoperator:enabled=false",
			wantErr:    true,
			wantDriver: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderer := &helmtest.MockRenderer{
				Rendered: map[string][]byte{"gpu-operator": {}},
			}
			lister := NewLister(
				WithHelmRenderer(renderer),
				WithValueOverrides([]config.ComponentPath{mustParseOverride(t, tt.override)}),
			)
			result := newProfiledRecipe()
			_, err := lister.Discover(t.Context(), result)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Discover() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if len(renderer.Inputs) != 0 {
					t.Fatalf("renderer called %d times before profile rejection", len(renderer.Inputs))
				}
				return
			}
			if len(renderer.Inputs) != 1 {
				t.Fatalf("renderer calls = %d, want 1", len(renderer.Inputs))
			}
			driver := renderer.Inputs[0].Values["driver"].(map[string]any)
			if driver["enabled"] != tt.wantDriver {
				t.Fatalf("render driver.enabled = %v, want %v", driver["enabled"], tt.wantDriver)
			}
			_, enabledValuePresent := renderer.Inputs[0].Values["enabled"]
			if enabledValuePresent != tt.wantEnabledValue {
				t.Fatalf("render values has enabled=%v, want %v",
					enabledValuePresent, tt.wantEnabledValue)
			}
			inputDriver := result.ComponentRefs[0].Overrides["driver"].(map[string]any)
			if inputDriver["enabled"] != false {
				t.Fatalf("input recipe mutated: driver.enabled=%v", inputDriver["enabled"])
			}
		})
	}
}

func TestDiscover_ProfileValidationRejectsBlockedOverridePath(t *testing.T) {
	result := newProfiledRecipe()

	renderer := &helmtest.MockRenderer{
		Rendered: map[string][]byte{"gpu-operator": {}},
	}
	lister := NewLister(
		WithHelmRenderer(renderer),
		WithValueOverrides([]config.ComponentPath{
			// Even though the canonical child spells the selected value, the
			// alias replaces its parent with a scalar. Bundling rejects that
			// structurally blocked path, so mirror discovery must also fail.
			mustParseOverride(t, "gpuoperator:driver=true"),
			mustParseOverride(t, "gpu-operator:driver.enabled=false"),
		}),
	)
	_, err := lister.Discover(t.Context(), result)
	if err == nil ||
		!strings.Contains(err.Error(), "driver.enabled=false") ||
		!strings.Contains(err.Error(), "exists but is not a map") {

		t.Fatalf("Discover() error = %v, want deterministic blocked-child rejection", err)
	}
	if len(renderer.Inputs) != 0 {
		t.Fatalf("renderer called %d times before override rejection", len(renderer.Inputs))
	}
}

func TestDiscover_RejectsNonMapOverrideAncestor(t *testing.T) {
	renderer := &helmtest.MockRenderer{
		Rendered: map[string][]byte{"gpu-operator": {}},
	}
	result := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{
			Name:    "gpu-operator",
			Type:    recipe.ComponentTypeHelm,
			Source:  "https://helm.ngc.nvidia.com/nvidia",
			Chart:   "gpu-operator",
			Version: "v25.3.0",
			Overrides: map[string]any{
				"driver": "not-a-map",
			},
		}},
	}

	_, err := NewLister(
		WithHelmRenderer(renderer),
		WithValueOverrides([]config.ComponentPath{
			mustParseOverride(t, "gpuoperator:driver.enabled=false"),
		}),
	).Discover(t.Context(), result)
	if err == nil || !strings.Contains(err.Error(), "exists but is not a map") {
		t.Fatalf("Discover() error = %v, want non-map ancestor rejection", err)
	}
	if len(renderer.Inputs) != 0 {
		t.Fatalf("renderer called %d times before override rejection", len(renderer.Inputs))
	}
}

func TestDiscover_SetEnabledOverride(t *testing.T) {
	tests := []struct {
		name     string
		override string
		wantErr  bool
	}{
		{
			name:     "false skips component",
			override: "gpuoperator:enabled=false",
		},
		{
			name:     "non-boolean is rejected",
			override: "gpuoperator:enabled=not-a-bool",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderer := &helmtest.MockRenderer{
				Rendered: map[string][]byte{"gpu-operator": {}},
			}
			result, err := NewLister(
				WithHelmRenderer(renderer),
				WithValueOverrides([]config.ComponentPath{
					mustParseOverride(t, tt.override),
				}),
			).Discover(t.Context(), &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{{
					Name:    "gpu-operator",
					Type:    recipe.ComponentTypeHelm,
					Source:  "https://helm.ngc.nvidia.com/nvidia",
					Chart:   "gpu-operator",
					Version: "v25.3.0",
				}},
			})
			switch {
			case tt.wantErr:
				if err == nil {
					t.Fatal("Discover() error = nil, want invalid enabled override")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Fatalf("Discover() error = %v, want ErrCodeInvalidRequest", err)
				}
				if !strings.Contains(err.Error(), "invalid --set enabled value") {
					t.Fatalf("Discover() error = %v, want invalid enabled override", err)
				}
				if result != nil {
					t.Fatalf("Discover() result = %#v, want nil after invalid override", result)
				}
			case err != nil:
				t.Fatalf("Discover() error = %v", err)
			default:
				if len(result.Components) != 0 || len(result.Charts) != 0 || len(result.Images) != 0 {
					t.Fatalf("component remained in mirror list: %#v", result)
				}
			}
			if len(renderer.Inputs) != 0 {
				t.Fatalf("renderer calls = %d, want 0", len(renderer.Inputs))
			}
		})
	}
}

// TestDiscover_UnloadableValuesFailsClosed covers #2261.
//
// A mirror list is a completeness claim: it is the full set of images an
// operator relocates into a disconnected registry. A component that cannot
// hydrate its values renders nothing and therefore contributes zero images, so
// returning that list successfully means the relocation "succeeds", the bundle
// deploys, and the component then fails to pull in the air-gapped environment
// where diagnosis is hardest.
//
// A values-load failure is deterministic and structural (a missing or
// unreadable values file, or a provider-binding problem), not transient, so it
// is the fail-closed direction per the repo's convention. This test asserts
// Discover errors rather than returning a silently-incomplete list, and covers
// both the profile-owned path (prepareMirrorCandidate, which already failed
// closed) and the unowned path (which did not, and was the bug).
//
// The predecessor of this test asserted the opposite for the unowned case: that
// hydration stayed "best effort" and surfaced only a warning. That expectation
// was the defect #2261 describes, so the assertion is inverted here rather than
// removed, and the profile-lock case it also covered is retained below.
func TestDiscover_UnloadableValuesFailsClosed(t *testing.T) {
	const badValues = "components/does-not-exist/values.yaml"

	// newRecipe builds a two-component recipe where gpu-operator is
	// profile-owned and network-operator is not, so a single shape exercises
	// both hydration paths depending on which values file is broken.
	newRecipe := func(gpuValues, networkValues string) *recipe.RecipeResult {
		result := &recipe.RecipeResult{
			Kind:       recipe.RecipeResultKind,
			APIVersion: recipe.RecipeProfileAPIVersion,
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:       "gpu-operator",
					Type:       recipe.ComponentTypeHelm,
					Source:     "https://helm.ngc.nvidia.com/nvidia",
					Chart:      "gpu-operator",
					Version:    "v25.3.0",
					ValuesFile: gpuValues,
					Overrides: map[string]any{
						"driver": map[string]any{"enabled": false},
					},
				},
				{
					Name:       "network-operator",
					Type:       recipe.ComponentTypeHelm,
					Source:     "https://helm.ngc.nvidia.com/nvidia",
					Chart:      "network-operator",
					Version:    "v25.1.0",
					ValuesFile: networkValues,
				},
			},
		}
		result.Metadata.SelectedProfile = &recipe.SelectedProfile{
			Name:  "gpuStack",
			Value: "driver-installed",
			OwnedPaths: map[string][]string{
				"gpu-operator": {"driver.enabled", "enabled"},
			},
		}
		return result
	}

	tests := []struct {
		name           string
		gpuValues      string
		networkValues  string
		wantErr        bool
		wantComponents int
	}{
		{
			name:          "unowned component values file missing",
			networkValues: badValues,
			wantErr:       true,
		},
		{
			name:      "profile-owned component values file missing",
			gpuValues: badValues,
			wantErr:   true,
		},
		{
			name:           "both components hydrate",
			wantErr:        false,
			wantComponents: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			renderer := &helmtest.MockRenderer{
				Rendered: map[string][]byte{
					"gpu-operator":     {},
					"network-operator": {},
				},
			}

			list, err := NewLister(WithHelmRenderer(renderer)).
				Discover(t.Context(), newRecipe(tt.gpuValues, tt.networkValues))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Discover() error = nil, want error; "+
						"a values-load failure must not yield a successful list: %+v", list)
				}
				// The list must not be returned alongside the error. A
				// non-nil list here is what would let a caller ignore err
				// and relocate an incomplete image set.
				if list != nil {
					t.Errorf("Discover() list = %+v, want nil on error", list)
				}
				return
			}

			if err != nil {
				t.Fatalf("Discover() error = %v, want nil", err)
			}
			if len(list.Components) != tt.wantComponents {
				t.Fatalf("components = %d, want %d", len(list.Components), tt.wantComponents)
			}
			// The control case must be a genuine success, not a success
			// carrying the warning the old behavior produced.
			for _, ci := range list.Components {
				if len(ci.Warnings) != 0 {
					t.Errorf("component %q warnings = %v, want none",
						ci.Component, ci.Warnings)
				}
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
			name: "version below render floor returns default",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
			},
			want: defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "major minor version below render floor returns default",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.25"},
			},
			want: defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "major only version below render floor returns default",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1"},
			},
			want: defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "prerelease below render floor returns default",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.33.0-0"},
			},
			want: defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "version equal to render floor",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: defaults.MirrorDefaultKubeVersion},
			},
			want: defaults.MirrorDefaultKubeVersion,
		},
		{
			name: "version above render floor",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.34.0"},
			},
			want: "1.34.0",
		},
		{
			name: "version above render floor with v prefix",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= v1.34.1"},
			},
			want: "1.34.1",
		},
		{
			name: "invalid version returns default",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: "not-a-version"},
			},
			want: defaults.MirrorDefaultKubeVersion,
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

type kaiConfigRenderer struct {
	inputs []helm.ChartInput
}

func (r *kaiConfigRenderer) Render(_ context.Context, input helm.ChartInput) ([]byte, error) {
	r.inputs = append(r.inputs, input)

	binderName := "binder"
	if binder, ok := input.Values["binder"].(map[string]any); ok {
		if image, ok := binder["image"].(map[string]any); ok {
			if name, ok := image["name"]; ok {
				binderName = fmt.Sprint(name)
			}
		}
	}

	scalingPodImage := ""
	if global, ok := input.Values["global"].(map[string]any); ok {
		if enabled, ok := global["clusterAutoscaling"].(bool); ok && enabled {
			scalingPodImage = `
  nodeScaleAdjuster:
    service:
      enabled: true
    args:
      scalingPodImage:
        name: scalingpod
        repository: ghcr.io/kai-scheduler/kai-scheduler
        tag: v0.14.1`
		}
	}

	return []byte(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: operator
          image: ghcr.io/kai-scheduler/kai-scheduler/operator:v0.14.1
---
apiVersion: kai.scheduler/v1
kind: Config
spec:
  binder:
    service:
      image:
        name: %s
        repository: ghcr.io/kai-scheduler/kai-scheduler
        tag: v0.14.1
  scheduler:
    service:
      image:
        name: scheduler
        repository: ghcr.io/kai-scheduler/kai-scheduler
        tag: v0.14.1%s
`, binderName, scalingPodImage)), nil
}

func TestDiscoverOperatorManagedImagesWithAutoscalingOverride(t *testing.T) {
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "kai-scheduler",
				Type:    recipe.ComponentTypeHelm,
				Source:  "oci://ghcr.io/kai-scheduler/kai-scheduler",
				Chart:   "kai-scheduler",
				Version: "v0.14.1",
			},
		},
	}
	renderer := &kaiConfigRenderer{}

	overrides, err := config.ParseValueOverrides([]string{
		"kaischeduler:global.clusterAutoscaling=true",
	})
	if err != nil {
		t.Fatalf("ParseValueOverrides() error = %v", err)
	}

	list, err := NewLister(
		WithHelmRenderer(renderer),
		WithValueOverrides(overrides),
	).Discover(context.Background(), rec)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []string{
		"ghcr.io/kai-scheduler/kai-scheduler/binder:v0.14.1",
		"ghcr.io/kai-scheduler/kai-scheduler/operator:v0.14.1",
		"ghcr.io/kai-scheduler/kai-scheduler/scalingpod:v0.14.1",
		"ghcr.io/kai-scheduler/kai-scheduler/scheduler:v0.14.1",
	}
	if !slices.Equal(list.Images, want) {
		t.Errorf("Images = %v, want %v", list.Images, want)
	}

	if len(renderer.inputs) != 1 {
		t.Fatalf("renderer inputs = %d, want 1", len(renderer.inputs))
	}
	global, ok := renderer.inputs[0].Values["global"].(map[string]any)
	if !ok {
		t.Fatalf("global values = %T, want map[string]any", renderer.inputs[0].Values["global"])
	}
	if enabled, ok := global["clusterAutoscaling"].(bool); !ok || !enabled {
		t.Errorf("global.clusterAutoscaling = %#v, want true", global["clusterAutoscaling"])
	}
}

func TestDiscoverRejectsNullOperatorImageNameOverride(t *testing.T) {
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "kai-scheduler",
				Type:    recipe.ComponentTypeHelm,
				Source:  "oci://ghcr.io/kai-scheduler/kai-scheduler",
				Chart:   "kai-scheduler",
				Version: "v0.14.1",
			},
		},
	}
	renderer := &kaiConfigRenderer{}

	overrides, err := config.ParseValueOverrides([]string{
		"kaischeduler:binder.image.name=null",
	})
	if err != nil {
		t.Fatalf("ParseValueOverrides() error = %v", err)
	}

	_, err = NewLister(
		WithHelmRenderer(renderer),
		WithValueOverrides(overrides),
	).Discover(context.Background(), rec)
	if err == nil {
		t.Fatal("Discover() error = nil, want invalid structured image descriptor error")
	}
	if !bom.IsInvalidStructuredImageDescriptor(err) {
		t.Errorf("IsInvalidStructuredImageDescriptor(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), `component "kai-scheduler"`) {
		t.Errorf("error %q does not identify the component", err.Error())
	}
	if count := strings.Count(err.Error(), "invalid structured image descriptor"); count != 1 {
		t.Errorf("error %q repeats the descriptor summary %d times, want 1", err.Error(), count)
	}

	if len(renderer.inputs) != 1 {
		t.Fatalf("renderer inputs = %d, want 1", len(renderer.inputs))
	}
	binder, ok := renderer.inputs[0].Values["binder"].(map[string]any)
	if !ok {
		t.Fatalf("binder values = %T, want map[string]any", renderer.inputs[0].Values["binder"])
	}
	image, ok := binder["image"].(map[string]any)
	if !ok {
		t.Fatalf("binder.image values = %T, want map[string]any", binder["image"])
	}
	if got := image["name"]; got != "null" {
		t.Errorf("binder.image.name = %#v, want %q", got, "null")
	}
}

// inMemoryDataProvider is a minimal recipe.DataProvider backed by an
// in-memory map[path]content. Only ReadFile is exercised by extractManifestImages;
// WalkDir and Source are implemented to satisfy the interface.
type inMemoryDataProvider struct {
	files    map[string][]byte
	readFile func(context.Context, string) ([]byte, error)
}

func (p *inMemoryDataProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if p.readFile != nil {
		return p.readFile(ctx, path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := p.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return content, nil
}

func (p *inMemoryDataProvider) WalkDir(ctx context.Context, _ string, _ fs.WalkDirFunc) error {
	return ctx.Err()
}

func (p *inMemoryDataProvider) Source(path string) string { return "inmem:" + path }

// TestDiscover_HonorsRecipeBoundDataProviderForManifests pins the invariant
// that extractManifestImages reads ManifestFiles through the recipe-bound
// DataProvider. Without the binding, mirror would silently fall back to the
// package-global embedded provider — making `aicr mirror --data <dir>`
// inconsistent with `aicr bundle --data <dir>` for overlay-shadowed manifests.
func TestDiscover_HonorsRecipeBoundDataProviderForManifests(t *testing.T) {
	const manifestPath = "components/network-operator/manifests/overlay-only.yaml"
	overlayManifest := []byte(`
apiVersion: v1
kind: Pod
metadata:
  name: overlay-only
spec:
  containers:
    - name: c
      image: overlay/from-provider:v9.9.9
`)

	dp := &inMemoryDataProvider{files: map[string][]byte{
		manifestPath: overlayManifest,
	}}

	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "network-operator",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{manifestPath},
			},
		},
	}
	rec.BindDataProvider(dp)

	lister := NewLister(WithHelmRenderer(&helmtest.MockRenderer{}))
	result, err := lister.Discover(context.Background(), rec)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if !slices.Contains(result.Images, "overlay/from-provider:v9.9.9") {
		t.Errorf("overlay manifest image not extracted: images=%v", result.Images)
	}
	for _, c := range result.Components {
		if c.Component == "network-operator" && len(c.Warnings) > 0 {
			t.Errorf("unexpected warnings on overlay-bound read: %v", c.Warnings)
		}
	}
}

func TestDiscover_RejectsInvalidStructuredImageDescriptorInManifest(t *testing.T) {
	const manifestPath = "components/kai-scheduler/manifests/invalid-image.yaml"
	dp := &inMemoryDataProvider{files: map[string][]byte{
		manifestPath: []byte(`
apiVersion: kai.scheduler/v1
kind: Config
spec:
  binder:
    service:
      image:
        name: null
        repository: ghcr.io/kai-scheduler/kai-scheduler
        tag: v0.14.1
`),
	}}

	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "kai-scheduler",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{manifestPath},
			},
		},
	}
	rec.BindDataProvider(dp)

	_, err := NewLister(WithHelmRenderer(&helmtest.MockRenderer{})).
		Discover(context.Background(), rec)
	if err == nil {
		t.Fatal("Discover() error = nil, want invalid structured image descriptor error")
	}
	if !bom.IsInvalidStructuredImageDescriptor(err) {
		t.Errorf("IsInvalidStructuredImageDescriptor(%v) = false, want true", err)
	}
	if !strings.Contains(err.Error(), manifestPath) {
		t.Errorf("error %q does not identify manifest %q", err.Error(), manifestPath)
	}
}

func TestDiscover_PropagatesManifestReadCancellation(t *testing.T) {
	const manifestPath = "components/test/manifests/canceled.yaml"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "test",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{manifestPath},
			},
		},
	}
	rec.BindDataProvider(&inMemoryDataProvider{
		readFile: func(ctx context.Context, _ string) ([]byte, error) {
			cancel()
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	_, err := NewLister(WithHelmRenderer(&helmtest.MockRenderer{})).Discover(ctx, rec)
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("Discover() error = %v, want context.Canceled", err)
	}
}

func TestDiscover_ChecksCancellationBetweenManifestReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const validManifest = `
apiVersion: v1
kind: Pod
spec:
  containers:
    - image: example.com/test:v1
`
	readCount := 0
	dp := &inMemoryDataProvider{
		readFile: func(_ context.Context, _ string) ([]byte, error) {
			readCount++
			if readCount == 1 {
				cancel()
			}
			return []byte(validManifest), nil
		},
	}
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "test",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{"first.yaml", "second.yaml"},
			},
		},
	}
	rec.BindDataProvider(dp)

	_, err := NewLister(WithHelmRenderer(&helmtest.MockRenderer{})).Discover(ctx, rec)
	if !stderrors.Is(err, context.Canceled) {
		t.Fatalf("Discover() error = %v, want context.Canceled", err)
	}
	if readCount != 1 {
		t.Errorf("manifest reads = %d, want 1", readCount)
	}
}

// TestDiscover_NilDataProviderFallsBackToEmbedded confirms the back-compat
// path: when a RecipeResult has no bound provider, extractManifestImages must
// still resolve manifest paths via the package-global embedded provider
// (recipe.GetManifestContentWithContext treats a nil dp as embedded fallback).

func TestDiscover_OverrideAliasRegistryFailureIsFatal(t *testing.T) {
	dp := &inMemoryDataProvider{files: map[string][]byte{
		"registry.yaml": []byte("components: ["),
	}}
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{
			Name:    "network-operator",
			Type:    recipe.ComponentTypeHelm,
			Source:  "https://helm.ngc.nvidia.com/nvidia",
			Chart:   "network-operator",
			Version: "v25.7.0",
		}},
	}
	rec.BindDataProvider(dp)

	_, err := NewLister(
		WithHelmRenderer(&helmtest.MockRenderer{}),
		WithValueOverrides([]config.ComponentPath{
			mustParseOverride(t, "networkoperator:feature.enabled=true"),
		}),
	).Discover(t.Context(), rec)
	if err == nil {
		t.Fatal("Discover() error = nil, want registry failure")
	}
	if !strings.Contains(err.Error(), "failed to parse registry.yaml") {
		t.Fatalf("Discover() error = %v, want registry parse failure", err)
	}
}

func TestDiscover_RegistryFailureWithoutOverridesIsNotFatal(t *testing.T) {
	dp := &inMemoryDataProvider{files: map[string][]byte{
		"registry.yaml": []byte("components: ["),
	}}
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{
			Name:    "network-operator",
			Type:    recipe.ComponentTypeHelm,
			Source:  "https://helm.ngc.nvidia.com/nvidia",
			Chart:   "network-operator",
			Version: "v25.7.0",
		}},
	}
	rec.BindDataProvider(dp)

	_, err := NewLister(
		WithHelmRenderer(&helmtest.MockRenderer{
			Rendered: map[string][]byte{"network-operator": {}},
		}),
	).Discover(t.Context(), rec)
	if err != nil {
		t.Fatalf("Discover() error = %v, want malformed registry ignored without overrides", err)
	}
}

// TestDiscover_NilDataProviderFallsBackToEmbedded confirms the back-compat
// path: when a RecipeResult has no bound provider, extractManifestImages must
// still resolve manifest paths via the package-global embedded provider
// (recipe.GetManifestContentWithContext treats a nil dp as embedded fallback).
// We use a real embedded manifest path to keep the test hermetic.
func TestDiscover_NilDataProviderFallsBackToEmbedded(t *testing.T) {
	const embeddedManifest = "components/network-operator/manifests/nfd-network-rule.yaml"

	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:          "network-operator",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{embeddedManifest},
			},
		},
	}
	// Intentionally do NOT call BindDataProvider — rec.DataProvider() returns nil.

	lister := NewLister(WithHelmRenderer(&helmtest.MockRenderer{}))
	result, err := lister.Discover(context.Background(), rec)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	for _, c := range result.Components {
		if c.Component == "network-operator" && len(c.Warnings) > 0 {
			t.Errorf("unexpected warnings reading embedded manifest: %v", c.Warnings)
		}
	}
}

// TestDiscover_SourceOnlyFallbackShape pins the source-only fallback
// precisely: the renderer must receive the component name as the chart, the
// resulting ChartRef must carry the fallback name, and a manifest-only ref
// must not invoke the renderer at all (its images come from the manifest
// scan; a fabricated chart render would only produce a spurious warning).
func TestDiscover_SourceOnlyFallbackShape(t *testing.T) {
	renderer := &helmtest.MockRenderer{
		Rendered: map[string][]byte{"gpu-operator": []byte("kind: ConfigMap\n")},
	}
	rec := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Type:    recipe.ComponentTypeHelm,
				Source:  "https://helm.ngc.nvidia.com/nvidia",
				Version: "v25.3.0",
			},
			{
				Name:          "nodewright-customizations",
				Type:          recipe.ComponentTypeHelm,
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning.yaml"},
			},
		},
	}

	lister := NewLister(WithHelmRenderer(renderer))
	list, err := lister.Discover(context.Background(), rec)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	if len(renderer.Inputs) != 1 {
		t.Fatalf("renderer calls = %d, want 1 (manifest-only refs must not render)", len(renderer.Inputs))
	}
	if got := renderer.Inputs[0].Chart; got != "gpu-operator" {
		t.Errorf("renderer ChartInput.Chart = %q, want fallback to component name %q", got, "gpu-operator")
	}
	if len(list.Charts) != 1 {
		t.Fatalf("charts = %d, want 1 (source-only mirrored, manifest-only chartless)", len(list.Charts))
	}
	if got := list.Charts[0].Chart; got != "gpu-operator" {
		t.Errorf("ChartRef.Chart = %q, want fallback %q", got, "gpu-operator")
	}
}
