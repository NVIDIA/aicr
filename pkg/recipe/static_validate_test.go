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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateStaticFile(t *testing.T) {
	tests := []struct{ name, input, field string }{
		{"unknown component", "apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: test\nspec:\n  componentRefs:\n    - name: no-such-component\n", "spec.componentRefs"},
		{"invalid measurement", "apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: test\nspec:\n  constraints:\n    - name: K8s.server.versionn\n      value: '1'\n", "spec.constraints"},
		{"missing base", "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: test\nspec:\n  base: missing-parent\n", "spec.base"},
		{"missing mixin", "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: test\nspec:\n  mixins: [missing-mixin]\n", "spec.mixins"},
		{"unpinned", "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents:\n  - name: example\n    displayName: Example\n    helm:\n      defaultChart: example/chart\n", "components.helm.defaultVersion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.yaml")
			if err := os.WriteFile(path, []byte(tt.input), 0600); err != nil {
				t.Fatal(err)
			}
			diagnostics, err := ValidateStatic(t.Context(), path, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range diagnostics {
				if d.Field == tt.field && d.File == path && d.Severity == "error" {
					return
				}
			}
			t.Fatalf("missing %s diagnostic: %+v", tt.field, diagnostics)
		})
	}
}

func TestValidateStaticRejectsMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("kind: ["), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ValidateStatic(t.Context(), path, "")
	if err != nil || len(diagnostics) == 0 || !strings.Contains(diagnostics[0].Message, "yaml") {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
}

func TestValidateStaticCatalog(t *testing.T) {
	diagnostics, err := ValidateStatic(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("embedded catalog diagnostics: %+v", diagnostics)
	}
}

func TestValidateStaticNewLeaf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leaf.yaml")
	content := `kind: RecipeMetadata
apiVersion: aicr.run/v1beta1
metadata:
  name: new-h100-eks-ubuntu-training
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
  mixins: [os-ubuntu]
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ValidateStatic(t.Context(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("new leaf diagnostics: %+v", diagnostics)
	}
}

func TestValidateStaticRejectsUnsafeAssertFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	content := `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: example
    displayName: Example
    healthCheck:
      assertFile: ../checks/nfd/health-check.yaml
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ValidateStatic(t.Context(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diagnostics {
		if d.Field == "components.healthCheck.assertFile" {
			return
		}
	}
	t.Fatalf("unsafe path was accepted: %+v", diagnostics)
}

func TestValidateStaticChecksNondefaultProfileRefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	content := `apiVersion: aicr.run/v1beta2
kind: RecipeMetadata
metadata:
  name: profile-test
spec:
  profile:
    name: stack
    default: one
    values:
      one: {}
      two:
        componentRefs:
          - name: no-such-component
            overrides: {enabled: true}
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ValidateStatic(t.Context(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diagnostics {
		if d.Field == "spec.profile.values.two.componentRefs" {
			return
		}
	}
	t.Fatalf("unknown nondefault profile ref was accepted: %+v", diagnostics)
}

func TestValidateStaticRejectsMutatingHealthCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	registry := `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: example
    displayName: Example
    healthCheck:
      assertFile: check.yaml
`
	check := `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: example
spec:
  steps:
    - try:
        - delete:
            ref:
              apiVersion: v1
              kind: Pod
              name: example
`
	if err := os.WriteFile(path, []byte(registry), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "check.yaml"), []byte(check), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ValidateStatic(t.Context(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diagnostics {
		if d.Field == "components.healthCheck.assertFile" {
			return
		}
	}
	t.Fatalf("mutating health check was accepted: %+v", diagnostics)
}

func TestValidateStaticCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	diagnostics, err := ValidateStatic(ctx, "", "")
	if !stderrors.Is(err, context.Canceled) || len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
}

func TestValidateStaticCatalogReportsOverlayDiagnostics(t *testing.T) {
	dir := t.TempDir()
	registry, readErr := defaultEmbeddedProvider.ReadFile(t.Context(), RegistryFileName)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := os.WriteFile(filepath.Join(dir, RegistryFileName), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "overlays"), 0o700); err != nil {
		t.Fatal(err)
	}
	overlay := `apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata:
  name: invalid-static-validation-leaf
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
  mixins: [os-ubuntu]
  componentRefs:
    - name: no-such-component
`
	if err := os.WriteFile(filepath.Join(dir, "overlays", "invalid.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := ValidateStatic(t.Context(), "", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Field == "spec.componentRefs" && strings.Contains(diagnostic.Message, "no-such-component") {
			return
		}
	}
	t.Fatalf("missing overlay component diagnostic: %+v", diagnostics)
}

func TestValidateStaticRejectsUnknownOverlayFields(t *testing.T) {
	for _, version := range []string{"aicr.run/v1beta1", "aicr.run/v1beta2"} {
		for _, catalog := range []bool{false, true} {
			name := version + "/file"
			if catalog {
				name = version + "/catalog"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.CopyFS(dir, GetEmbeddedFS()); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "overlays", "typo.yaml")
				content := "apiVersion: " + version + "\nkind: RecipeMetadata\nmetadata:\n  name: typo\nspec:\n  mixin: [missing-mixin]\n"
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				var diagnostics []StaticDiagnostic
				var err error
				if catalog {
					diagnostics, err = ValidateStaticDirectory(t.Context(), dir)
				} else {
					diagnostics, err = ValidateStatic(t.Context(), path, "")
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range diagnostics {
					if strings.Contains(d.Message, "field mixin not found") {
						return
					}
				}
				t.Fatalf("unknown field accepted: %+v", diagnostics)
			})
		}
	}
}

func TestValidateStaticDirectoryDoesNotMaskDeletions(t *testing.T) {
	tests := []struct {
		name            string
		removeFile      string
		removeComponent string
		wantDiagnostic  string
	}{
		{name: "complete catalog"},
		{name: "deleted parent", removeFile: "overlays/h100-eks-training.yaml", wantDiagnostic: "h100-eks-training"},
		{name: "deleted mixin", removeFile: "mixins/os-ubuntu.yaml", wantDiagnostic: "os-ubuntu"},
		{name: "deleted component", removeComponent: "gpu-operator", wantDiagnostic: "gpu-operator"},
		{name: "deleted assertion", removeFile: "checks/nfd/health-check.yaml", wantDiagnostic: "checks/nfd/health-check.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.CopyFS(dir, GetEmbeddedFS()); err != nil {
				t.Fatal(err)
			}
			if tt.removeFile != "" {
				if err := os.Remove(filepath.Join(dir, tt.removeFile)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.removeComponent != "" {
				path := filepath.Join(dir, RegistryFileName)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var registry ComponentRegistry
				if decodeErr := yaml.Unmarshal(data, &registry); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				var kept []ComponentConfig
				for _, comp := range registry.Components {
					if comp.Name != tt.removeComponent {
						kept = append(kept, comp)
					}
				}
				registry.Components = kept
				data, err = yaml.Marshal(registry)
				if err != nil {
					t.Fatal(err)
				}
				if writeErr := os.WriteFile(path, data, 0o600); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			diagnostics, err := ValidateStaticDirectory(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantDiagnostic == "" {
				if len(diagnostics) != 0 {
					t.Fatalf("complete catalog rejected: %+v", diagnostics)
				}
			} else {
				found := false
				for _, d := range diagnostics {
					found = found || strings.Contains(d.Message, tt.wantDiagnostic)
				}
				if !found {
					t.Fatalf("missing deletion diagnostic for %s: %+v", tt.wantDiagnostic, diagnostics)
				}
			}
			diagnostics, err = ValidateStatic(t.Context(), "", dir)
			if err != nil || len(diagnostics) != 0 {
				t.Fatalf("layered catalog changed behavior: diagnostics=%+v err=%v", diagnostics, err)
			}
		})
	}
}
