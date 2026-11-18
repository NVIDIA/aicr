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

package argocd

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"

	"aicr.run/pkg/bundler/config"
	"aicr.run/pkg/bundler/deployer"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/pkg/recipe/providertest"
)

// groveRetry is the retry the embedded registry declares on dynamo-platform
// for recipes that include grove.
var groveRetry = map[string]any{
	"limit": float64(5),
	"backoff": map[string]any{
		"duration":    "30s",
		"factor":      float64(2),
		"maxDuration": "3m",
	},
}

// dynamoRecipe returns dynamo-platform, preceded by grove when withGrove is
// set.
func dynamoRecipe(withGrove bool) *recipe.RecipeResult {
	rr := &recipe.RecipeResult{}
	rr.Metadata.Version = testVersion
	dynamo := recipe.ComponentRef{
		Name: "dynamo-platform", Namespace: "dynamo-system", Chart: "dynamo-platform",
		Version: "1.6.2", Type: recipe.ComponentTypeHelm,
		Source: "https://helm.ngc.nvidia.com/nvidia/ai-dynamo",
	}
	if withGrove {
		dynamo.DependencyRefs = []string{"grove"}
		rr.ComponentRefs = []recipe.ComponentRef{{
			Name: "grove", Namespace: "dynamo-system", Chart: "grove-charts",
			Version: "v0.1.0-alpha.14", Type: recipe.ComponentTypeHelm,
			Source: "oci://ghcr.io/ai-dynamo/grove",
		}}
		rr.DeploymentOrder = []string{"grove"}
	}
	rr.ComponentRefs = append(rr.ComponentRefs, dynamo)
	rr.DeploymentOrder = append(rr.DeploymentOrder, "dynamo-platform")
	return rr
}

// readSyncRetry returns spec.syncPolicy.retry of the Application at path, or
// nil when the retry key is absent. A missing file, or spec, syncPolicy or
// retry present but not a map, fails the test rather than reading as no
// retry.
func readSyncRetry(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var app map[string]any
	if err := sigsyaml.Unmarshal(raw, &app); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	node := app
	for _, key := range []string{"spec", "syncPolicy", "retry"} {
		v, ok := node[key]
		if !ok {
			if key == "retry" {
				return nil
			}
			t.Fatalf("%s: %s is missing", path, key)
		}
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is %T, want a map", path, key, v)
		}
		node = m
	}
	return node
}

// manifestFor returns the Application path Generate reported for release
// name, failing the test when no such release was written.
func manifestFor(t *testing.T, outputDir string, releases []deployer.Release, name string) string {
	t.Helper()
	for _, rel := range releases {
		if rel.Name == name {
			return filepath.Join(outputDir, rel.Manifest)
		}
	}
	t.Fatalf("no %s release in %v", name, releases)
	return ""
}

// TestGenerate_RegistryDefaultSyncRetry pins the dynamo-platform retry the
// embedded registry declares for Grove recipes, and that a bundle-wide retry
// replaces it.
func TestGenerate_RegistryDefaultSyncRetry(t *testing.T) {
	userRetry := map[string]any{"limit": float64(2)}
	disabled := map[string]any{"limit": float64(0)}

	tests := []struct {
		name       string
		withGrove  bool
		retry      *config.SyncRetry
		wantDynamo map[string]any
		wantGrove  map[string]any
	}{
		{
			name:       "grove present applies the registry default to dynamo-platform only",
			withGrove:  true,
			wantDynamo: groveRetry,
		},
		{
			name:      "grove absent leaves dynamo-platform without retry",
			withGrove: false,
		},
		{
			name:       "bundle retry replaces the registry default",
			withGrove:  true,
			retry:      &config.SyncRetry{Limit: 2},
			wantDynamo: userRetry,
			wantGrove:  userRetry,
		},
		{
			name:       "bundle retry limit 0 disables the registry default",
			withGrove:  true,
			retry:      &config.SyncRetry{Limit: 0},
			wantDynamo: disabled,
			wantGrove:  disabled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputDir := t.TempDir()
			g := &Generator{
				RecipeResult: dynamoRecipe(tt.withGrove),
				Version:      "v0.0.0-test",
				SyncRetry:    tt.retry,
			}
			out, err := g.Generate(context.Background(), outputDir)
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			gotDynamo := readSyncRetry(t, manifestFor(t, outputDir, out.Releases, "dynamo-platform"))
			if !reflect.DeepEqual(gotDynamo, tt.wantDynamo) {
				t.Errorf("dynamo-platform retry = %v, want %v", gotDynamo, tt.wantDynamo)
			}
			if tt.withGrove {
				gotGrove := readSyncRetry(t, manifestFor(t, outputDir, out.Releases, "grove"))
				if !reflect.DeepEqual(gotGrove, tt.wantGrove) {
					t.Errorf("grove retry = %v, want %v", gotGrove, tt.wantGrove)
				}
			}
		})
	}
}

// TestGenerate_RejectsInvalidRegistrySyncRetry covers a --data registry whose
// declared retry Argo CD would reject.
func TestGenerate_RejectsInvalidRegistrySyncRetry(t *testing.T) {
	embedded, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("load embedded registry: %v", err)
	}
	entry := *embedded.Get("dynamo-platform")
	entry.ArgoCD.SyncRetry = &recipe.ArgoCDSyncRetryConfig{
		Limit:   5,
		Backoff: recipe.ArgoCDSyncRetryBackoffConfig{Duration: "soon"},
	}
	external := recipe.ComponentRegistry{
		APIVersion: embedded.APIVersion, Kind: embedded.Kind,
		Components: []recipe.ComponentConfig{entry},
	}
	data, err := yaml.Marshal(&external)
	if err != nil {
		t.Fatalf("marshal registry: %v", err)
	}
	provider, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: providertest.WriteDir(t, map[string][]byte{recipe.RegistryFileName: data})})
	if err != nil {
		t.Fatalf("layer registry: %v", err)
	}
	t.Cleanup(func() { recipe.EvictCachedRegistry(provider) })

	rr := dynamoRecipe(false)
	rr.BindDataProvider(provider)
	g := &Generator{RecipeResult: rr, Version: "v0.0.0-test"}
	_, err = g.Generate(context.Background(), t.TempDir())
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}
