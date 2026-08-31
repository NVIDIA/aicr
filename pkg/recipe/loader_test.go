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

package recipe

import (
	"context"
	stderrors "errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
)

type unexpectedLoadProvider struct {
	calls atomic.Int64
}

func (p *unexpectedLoadProvider) ReadFile(_ context.Context, _ string) ([]byte, error) {
	p.calls.Add(1)
	return nil, fs.ErrNotExist
}

func (p *unexpectedLoadProvider) WalkDir(
	_ context.Context,
	_ string,
	_ fs.WalkDirFunc,
) error {

	p.calls.Add(1)
	return fs.ErrNotExist
}

func (p *unexpectedLoadProvider) Source(path string) string {
	return path
}

func TestLoadFromFile(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		filePath    string // override file path (skip writing yamlContent)
		wantErr     bool
		errContain  string
		checkResult func(t *testing.T, rec *RecipeResult)
	}{
		{
			name:       "nonexistent file returns error",
			filePath:   "/tmp/does-not-exist-aicr-test.yaml",
			wantErr:    true,
			errContain: "/tmp/does-not-exist-aicr-test.yaml",
		},
		{
			name:        "RecipeResult loads directly",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\ncriteria:\n  service: eks\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Kind != RecipeResultKind {
					t.Errorf("kind = %q, want %q", rec.Kind, RecipeResultKind)
				}
			},
		},
		{
			name:        "Release N target RecipeResult loads directly",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.run/v1\ncriteria:\n  service: eks\n",
		},
		{
			name:        "RecipeMetadata with criteria auto-hydrates",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Kind != RecipeResultKind {
					t.Errorf("kind = %q, want %q", rec.Kind, RecipeResultKind)
				}
				if len(rec.ComponentRefs) == 0 {
					t.Error("expected hydrated recipe with components")
				}
			},
		},
		{
			name:        "Release N target RecipeMetadata auto-hydrates",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
		},
		{
			name: "profile RecipeMetadata outside active catalog fails closed",
			yamlContent: `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha3
metadata:
  name: direct-profile
spec:
  criteria:
    service: eks
    accelerator: h100
    intent: training
  profile:
    name: gpuStack
    default: operator-managed
    values:
      operator-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: true
`,
			wantErr:    true,
			errContain: "was not applied; add a structurally matching declaration to the active catalog",
		},
		{
			name:        "RecipeMetadata without criteria errors",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  "has no criteria",
		},
		{
			name:        "RecipeMixin kind rejected",
			yamlContent: "kind: RecipeMixin\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  `kind "RecipeMixin"`,
		},
		{
			name:        "unknown kind rejected",
			yamlContent: "kind: SomethingElse\napiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  `kind "SomethingElse"`,
		},
		{
			name:        "empty kind allowed",
			yamlContent: "apiVersion: aicr.run/v1alpha2\ncriteria:\n  service: eks\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Kind != "" {
					t.Errorf("kind = %q, want empty", rec.Kind)
				}
			},
		},
		{
			// #2421: a RecipeMetadata is a catalog kind however it arrives, so
			// the direct-input path must fail closed on an empty header exactly
			// as the catalog scanner does. Before the fix this hydrated
			// silently.
			name:        "headerless RecipeMetadata rejected",
			yamlContent: "kind: RecipeMetadata\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     true,
			errContain:  `recipe metadata file has apiVersion ""`,
		},
		{
			// The empty tolerance is narrowed to RecipeResult, not removed.
			// ADR-022 §3 retires this one at Release N+2 (#2417); until then a
			// pre-apiVersion recipe must still load.
			name:        "headerless RecipeResult still accepted",
			yamlContent: "kind: RecipeResult\ncriteria:\n  service: eks\n",
			wantErr:     false,
		},
		{
			name:        "unsupported apiVersion rejected",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\ncriteria:\n  service: eks\n",
			wantErr:     true,
			errContain:  `apiVersion "aicr.nvidia.com/v1alpha1"`,
		},
		{
			name:        "authoring target rejected for RecipeResult",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.run/v1beta1\ncriteria:\n  service: eks\n",
			wantErr:     true,
			errContain:  `apiVersion "aicr.run/v1beta1"`,
		},
		{
			name: "profile RecipeResult loads strictly",
			yamlContent: `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
componentRefs: []
`,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Metadata.SelectedProfile == nil ||
					rec.Metadata.SelectedProfile.Value != "one" {

					t.Fatalf("selectedProfile = %#v", rec.Metadata.SelectedProfile)
				}
			},
		},
		{
			name: "Release N target profile RecipeResult loads strictly",
			yamlContent: `kind: RecipeResult
apiVersion: aicr.run/v1beta2
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
componentRefs: []
`,
		},
		{
			name: "profile RecipeResult rejects unknown field",
			yamlContent: `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
profie: typo
`,
			wantErr:    true,
			errContain: "field profie not found",
		},
		{
			name: "Release N target profile RecipeResult rejects unknown field",
			yamlContent: `kind: RecipeResult
apiVersion: aicr.run/v1beta2
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
profie: typo
`,
			wantErr:    true,
			errContain: "field profie not found",
		},
		{
			name: "profile RecipeResult requires kind",
			yamlContent: `apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
componentRefs: []
`,
			wantErr:    true,
			errContain: `requires kind "RecipeResult"`,
		},
		{
			name: "Release N target profile RecipeResult requires kind",
			yamlContent: `apiVersion: aicr.run/v1beta2
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
componentRefs: []
`,
			wantErr:    true,
			errContain: `requires kind "RecipeResult"`,
		},
		{
			name:        "profile version requires selection",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.run/v1alpha3\ncomponentRefs: []\n",
			wantErr:     true,
			errContain:  "requires metadata.selectedProfile",
		},
		{
			name: "legacy version rejects selection",
			yamlContent: `kind: RecipeResult
apiVersion: aicr.run/v1alpha2
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
`,
			wantErr:    true,
			errContain: "cannot carry metadata.selectedProfile",
		},
		{
			name:        "unsupported apiVersion on RecipeMetadata overlay rejected",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     true,
			errContain:  `apiVersion "aicr.nvidia.com/v1alpha1"`,
		},
		{
			name:        "empty apiVersion allowed for backward compat",
			yamlContent: "kind: RecipeResult\ncriteria:\n  service: eks\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.APIVersion != "" {
					t.Errorf("apiVersion = %q, want empty", rec.APIVersion)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recipeFile := tt.filePath
			if recipeFile == "" {
				dir := t.TempDir()
				recipeFile = filepath.Join(dir, "recipe.yaml")
				if err := os.WriteFile(recipeFile, []byte(tt.yamlContent), 0o600); err != nil {
					t.Fatalf("failed to write test recipe file: %v", err)
				}
			}

			rec, err := LoadFromFileWithProvider(t.Context(), recipeFile, "", "test", nil)

			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
			if tt.checkResult != nil && err == nil {
				tt.checkResult(t, rec)
			}
		})
	}
}

func TestProfileDeclarationsEqualNormalizesJSONNumbers(t *testing.T) {
	declaration := func(value any) *ProfileDeclaration {
		return &ProfileDeclaration{
			Name:    "mode",
			Default: "selected",
			Values: map[string]ProfileValue{
				"selected": {
					ComponentRefs: []ProfileComponentRef{{
						Name:      "gpu-operator",
						Overrides: map[string]any{"replicas": value},
					}},
				},
			},
		}
	}

	equal, err := profileDeclarationsEqual(declaration(1), declaration(float64(1)))
	if err != nil {
		t.Fatalf("profileDeclarationsEqual() error: %v", err)
	}
	if !equal {
		t.Fatal("profileDeclarationsEqual() = false, want numerically equivalent declarations")
	}
}

func TestLoadFromFile_UnsupportedOverlayRejectedBeforeHydration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.yaml")
	content := `kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: unsupported
spec:
  criteria:
    service: eks
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	provider := &unexpectedLoadProvider{}
	_, err := LoadFromFileWithProvider(t.Context(), path, "", "test", provider)
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("LoadFromFileWithProvider() error = %v, want ErrCodeInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), `apiVersion "aicr.nvidia.com/v1alpha1"`) {
		t.Fatalf("LoadFromFileWithProvider() error = %v, want apiVersion rejection", err)
	}
	if got := provider.calls.Load(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 before version rejection", got)
	}
}

func TestLoadFromFile_ProfileDecodeErrorIsInvalidRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	content := `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
profie: typo
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write profile recipe: %v", err)
	}

	_, err := LoadFromFileWithProvider(t.Context(), path, "", "test", nil)
	if err == nil {
		t.Fatal("LoadFromFileWithProvider() error = nil, want invalid request")
	}
	if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("error code = %v, want ErrCodeInvalidRequest", err)
	}
}

func TestLoadFromFile_ProfileSourceReadOnce(t *testing.T) {
	const content = `kind: RecipeResult
apiVersion: aicr.run/v1alpha3
metadata:
  selectedProfile:
    name: mode
    value: one
    ownedPaths: {}
componentRefs: []
`
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if _, err := w.Write([]byte(content)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	_, err := LoadFromFileWithProvider(
		t.Context(), server.URL+"/recipe.yaml", "", "test", nil,
	)
	if err != nil {
		t.Fatalf("LoadFromFileWithProvider() error = %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("source requests = %d, want 1", got)
	}
}

// TestRecipeMetadataHeaderGatesAgree pins the #2421 invariant: the same
// RecipeMetadata header is accepted or rejected identically whether the
// document reaches AICR through the catalog scanner (`--data`) or through the
// direct recipe input path (`aicr bundle -r`, `aicr validate -r`).
//
// The two gates live in different files and were written at different times.
// They diverged on the empty string for the whole life of the direct loader:
// validateRecipeInputAPIVersion short-circuited on empty before it ever looked
// at the kind, so a headerless overlay was rejected from a --data tree and
// silently hydrated when passed with -r. Comparing the verdicts rather than
// re-asserting one of them is what keeps them from drifting apart again.
func TestRecipeMetadataHeaderGatesAgree(t *testing.T) {
	t.Parallel()

	versions := []struct {
		name       string
		apiVersion string
	}{
		{"empty", ""},
		{"alpha authoring", header.AuthoringGroupVersion},
		{"alpha profile", header.ProfileGroupVersion},
		{"target authoring", header.GroupVersionV1Beta1},
		{"target profile", header.GroupVersionV1Beta2},
		{"stable target belongs to another track", header.GroupVersionV1},
		{"unknown", "aicr.run/v1alpha9"},
	}

	for _, tc := range versions {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			directErr := validateRecipeInputAPIVersion(RecipeMetadataKind, tc.apiVersion)

			hdr := &RecipeMetadataHeader{
				Kind:       RecipeMetadataKind,
				APIVersion: tc.apiVersion,
			}
			_, _, catalogErr := classifyRecipeMetadataCatalogHeader(hdr, "overlay.yaml")

			if (directErr != nil) != (catalogErr != nil) {
				t.Fatalf("gates disagree on apiVersion %q: direct err = %v, catalog err = %v",
					tc.apiVersion, directErr, catalogErr)
			}

			// Both gates must fail closed with the same code, or a caller
			// translating one into an HTTP status gets a different answer per
			// entry point for the same bytes.
			if directErr != nil {
				want := aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")
				if !stderrors.Is(directErr, want) {
					t.Errorf("direct gate error code = %v, want ErrCodeInvalidRequest", directErr)
				}
				if !stderrors.Is(catalogErr, want) {
					t.Errorf("catalog gate error code = %v, want ErrCodeInvalidRequest", catalogErr)
				}
			}
		})
	}
}
