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

package bundler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	stderrors "errors"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const validReadinessTestYAML = `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`

func TestGateImage(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{"explicit v-prefixed version", "v1.2.3", "ghcr.io/nvidia/aicr-gate:v1.2.3"},
		{"release version without v prefix", "0.13.0", "ghcr.io/nvidia/aicr-gate:v0.13.0"},
		{"empty falls back to dev", "", "ghcr.io/nvidia/aicr-gate:dev"},
		{"dev stays dev", "dev", "ghcr.io/nvidia/aicr-gate:dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &DefaultBundler{Config: config.NewConfig(config.WithVersion(tt.version))}
			if got := b.gateImage(); got != tt.want {
				t.Errorf("gateImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateReadinessTestYAML(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{"valid", validReadinessTestYAML, false},
		{"wrong kind", "apiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Policy\n", true},
		{"wrong apiVersion", "apiVersion: v1\nkind: Test\n", true},
		{"invalid yaml", ":\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReadinessTestYAML("gpu-operator", []byte(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateReadinessTestYAML() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var se *aicrerrors.StructuredError
				if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
					t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
				}
			}
		})
	}
}

func TestCollectComponentReadiness(t *testing.T) {
	tmpData := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpData, "registry.yaml"), []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: ComponentRegistry\ncomponents: []\n"), 0o600); err != nil {
		t.Fatalf("WriteFile registry.yaml: %v", err)
	}
	compDir := filepath.Join(tmpData, "components", "gpu-operator")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(compDir, readinessFileName), []byte(validReadinessTestYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: tmpData})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator", Namespace: "gpu-operator"}},
	}
	rr.BindDataProvider(layered)

	t.Run("disabled returns empty", func(t *testing.T) {
		b, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), rr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d entries, want 0", len(got))
		}
	})

	t.Run("enabled collects manifest", func(t *testing.T) {
		b, err := New(WithConfig(config.NewConfig(
			config.WithReadinessHooks(true),
			config.WithDeployer(config.DeployerArgoCD),
			config.WithVersion("0.13.0"),
		)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := b.collectComponentReadiness(context.Background(), rr)
		if err != nil {
			t.Fatalf("collectComponentReadiness: %v", err)
		}
		manifests, ok := got["gpu-operator"]
		if !ok {
			t.Fatal("gpu-operator readiness missing")
		}
		body, ok := manifests[readinessManifestKey]
		if !ok {
			t.Fatal("readiness manifest key missing")
		}
		s := string(body)
		if !strings.Contains(s, "argocd.argoproj.io/sync-options: Replace=true") {
			t.Errorf("missing Replace=true:\n%s", s)
		}
		if !strings.Contains(s, "ghcr.io/nvidia/aicr-gate:v0.13.0") {
			t.Errorf("missing normalized gate image tag:\n%s", s)
		}
	})

	t.Run("malformed test rejected", func(t *testing.T) {
		badDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(badDir, "registry.yaml"), []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: ComponentRegistry\ncomponents: []\n"), 0o600); err != nil {
			t.Fatalf("WriteFile registry.yaml: %v", err)
		}
		badComp := filepath.Join(badDir, "components", "gpu-operator")
		if err := os.MkdirAll(badComp, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(badComp, readinessFileName), []byte("kind: ConfigMap\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		badLayered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: badDir})
		if err != nil {
			t.Fatalf("NewLayeredDataProvider: %v", err)
		}
		badRR := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
		}
		badRR.BindDataProvider(badLayered)

		b, err := New(WithConfig(config.NewConfig(config.WithReadinessHooks(true))))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := b.collectComponentReadiness(context.Background(), badRR); err == nil {
			t.Fatal("expected error for malformed readiness.yaml")
		}
	})
}

func TestBuildDeployer_ReadinessHooksUnsupportedDeployer(t *testing.T) {
	b, err := New(WithConfig(config.NewConfig(
		config.WithReadinessHooks(true),
		config.WithDeployer(config.DeployerFlux),
	)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rr := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}
	_, err = b.buildDeployer(context.Background(), rr, map[string]map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected error for flux + readiness-hooks")
	}
	var se *aicrerrors.StructuredError
	if !stderrors.As(err, &se) || se.Code != aicrerrors.ErrCodeInvalidRequest {
		t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
	}
}
