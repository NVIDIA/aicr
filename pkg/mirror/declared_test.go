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
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/helm/helmtest"
	"aicr.run/pkg/recipe"
)

const (
	declaredPath    = "components/op/images.yaml"
	declaredOpImage = "example.com/op/operator:v1"
	declaredFixture = `component: op
charts:
  - repository: oci://example.com/charts
    chart: op
    sourceVersion: 1.0.0
    images:
      - image: example.com/op/initc:1.0.0
        source: INIT_IMAGE env of the operator
      - image: example.com/op/extra:1.0.0
        enabledBy: extra.enabled
        source: rendered only when extra.enabled is true
manifests:
  - operatorImage: example.com/op/operator:v1
    images:
      - image: docker.io/library/alpine:3.10
        source: compiled-in default
`
)

func TestDiscover_DeclaredImages(t *testing.T) {
	helmRef := func(version string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "op", Type: recipe.ComponentTypeHelm,
			Source: "oci://example.com/charts", Chart: "op", Version: version, Overrides: overrides}
	}
	const manifestPath = "components/op/manifests/operator.yaml"
	manifest := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: op
spec:
  template:
    spec:
      containers:
      - name: op
        image: ` + declaredOpImage + "\n")

	tests := []struct {
		name     string
		ref      recipe.ComponentRef
		files    map[string]string
		want     []string
		wantCode errors.ErrorCode // empty when no error is expected
		wantMsg  string
		wantWarn string // substring of the component's warning, empty when none
	}{
		{
			name:  "chart version entry, gate off",
			ref:   helmRef("1.0.0", nil),
			files: map[string]string{declaredPath: declaredFixture},
			want:  []string{"example.com/op/initc:1.0.0", "example.com/op/rendered:1.0.0"},
		},
		{
			name:  "chart version entry, gate on",
			ref:   helmRef("1.0.0", map[string]any{"extra": map[string]any{"enabled": true}}),
			files: map[string]string{declaredPath: declaredFixture},
			want: []string{"example.com/op/extra:1.0.0", "example.com/op/initc:1.0.0",
				"example.com/op/rendered:1.0.0"},
		},
		{
			name:  "gate set to a non-boolean",
			ref:   helmRef("1.0.0", map[string]any{"extra": map[string]any{"enabled": "true"}}),
			files: map[string]string{declaredPath: declaredFixture},
			want:  []string{"example.com/op/initc:1.0.0", "example.com/op/rendered:1.0.0"},
		},
		{
			name:     "pin with no entry warns and declares nothing",
			ref:      helmRef("2.0.0", nil),
			files:    map[string]string{declaredPath: declaredFixture},
			want:     []string{"example.com/op/rendered:1.0.0"},
			wantWarn: `component "op": no entry for chart oci://example.com/charts op@2.0.0`,
		},
		{
			name: "chart relocated to another repository keeps its entry",
			ref: recipe.ComponentRef{Name: "op", Type: recipe.ComponentTypeHelm,
				Source: "oci://registry.internal/charts", Chart: "op", Version: "1.0.0"},
			files: map[string]string{declaredPath: declaredFixture},
			want:  []string{"example.com/op/initc:1.0.0", "example.com/op/rendered:1.0.0"},
		},
		{
			name: "manifest-only ref matched by its operator image",
			ref: recipe.ComponentRef{Name: "op", Type: recipe.ComponentTypeHelm,
				ManifestFiles: []string{manifestPath}},
			files: map[string]string{declaredPath: declaredFixture, manifestPath: string(manifest)},
			want:  []string{"docker.io/library/alpine:3.10", declaredOpImage},
		},
		{
			name: "manifest-only ref with another operator image",
			ref: recipe.ComponentRef{Name: "op", Type: recipe.ComponentTypeHelm,
				ManifestFiles: []string{manifestPath}},
			files: map[string]string{declaredPath: declaredFixture,
				manifestPath: strings.ReplaceAll(string(manifest), ":v1", ":v2")},
			want:     []string{"example.com/op/operator:v2"},
			wantWarn: `component "op": no entry for an operator image`,
		},
		{
			name: "unreadable operator manifest warns",
			ref: recipe.ComponentRef{Name: "op", Type: recipe.ComponentTypeHelm,
				ManifestFiles: []string{manifestPath}},
			files:    map[string]string{declaredPath: declaredFixture},
			want:     []string{},
			wantWarn: "declared images omitted",
		},
		{
			name:  "no images.yaml declares nothing",
			ref:   helmRef("1.0.0", nil),
			files: map[string]string{},
			want:  []string{"example.com/op/rendered:1.0.0"},
		},
		{
			name:     "file names another component",
			ref:      helmRef("1.0.0", nil),
			files:    map[string]string{declaredPath: strings.Replace(declaredFixture, "component: op", "component: other", 1)},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: `of component "op" name component "other"`,
		},
		{
			name:     "malformed file",
			ref:      helmRef("1.0.0", nil),
			files:    map[string]string{declaredPath: "component: op\nbogus: 1\n"},
			wantCode: errors.ErrCodeInvalidRequest, wantMsg: "parse declared images",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string][]byte{}
			for p, c := range tt.files {
				files[p] = []byte(c)
			}
			rec := &recipe.RecipeResult{
				APIVersion:    recipe.RecipeResultAPIVersion,
				ComponentRefs: []recipe.ComponentRef{tt.ref},
			}
			rec.BindDataProvider(&inMemoryDataProvider{files: files})
			renderer := &helmtest.MockRenderer{Rendered: map[string][]byte{"op": []byte(`apiVersion: v1
kind: Pod
metadata:
  name: op
spec:
  containers:
  - name: op
    image: example.com/op/rendered:1.0.0
`)}}

			got, err := NewLister(WithHelmRenderer(renderer)).Discover(context.Background(), rec)
			if tt.wantCode != "" {
				if err == nil {
					t.Fatalf("Discover() images = %v, want %s error", got.Images, tt.wantCode)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			if !slices.Equal(got.Images, tt.want) {
				t.Errorf("images = %v, want %v", got.Images, tt.want)
			}
			warnings := strings.Join(got.Components[0].Warnings, "\n")
			if tt.wantWarn == "" && warnings != "" {
				t.Errorf("warnings = %q, want none", warnings)
			}
			if !strings.Contains(warnings, tt.wantWarn) {
				t.Errorf("warnings = %q, want one containing %q", warnings, tt.wantWarn)
			}
		})
	}
}

func TestDeclaredImages_ReadErrors(t *testing.T) {
	ref := recipe.ComponentRef{Name: "op", Type: recipe.ComponentTypeHelm,
		Source: "oci://example.com/charts", Chart: "op", Version: "1.0.0"}

	t.Run("provider failure warns", func(t *testing.T) {
		ci := &ComponentImages{Component: "op"}
		read := func(context.Context, string) ([]byte, error) {
			return nil, errors.New(errors.ErrCodeUnavailable, "mount stalled")
		}
		got, err := declaredImages(context.Background(), &inMemoryDataProvider{readFile: read}, ref, nil, nil, ci)
		if err != nil || len(got) != 0 {
			t.Fatalf("declaredImages() = %v, %v, want no images and no error", got, err)
		}
		if len(ci.Warnings) != 1 || !strings.Contains(ci.Warnings[0], "mount stalled") {
			t.Errorf("warnings = %q, want one naming the read failure", ci.Warnings)
		}
	})

	tests := []struct {
		name     string
		ctx      func() context.Context
		read     func(context.Context, string) ([]byte, error)
		wantCode errors.ErrorCode
	}{
		{
			name: "canceled context",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			read: func(ctx context.Context, _ string) ([]byte, error) {
				return nil, ctx.Err()
			},
			wantCode: errors.ErrCodeCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := declaredImages(tt.ctx(), &inMemoryDataProvider{readFile: tt.read}, ref, nil, nil, &ComponentImages{})
			if err == nil {
				t.Fatal("declaredImages() error = nil, want a read error")
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}
