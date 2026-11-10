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
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

// driverOwnershipRecipe ships both driver-ownership families and an unrelated
// component, so a bundlers subset can omit either family.
func driverOwnershipRecipe() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		APIVersion: recipe.RecipeResultAPIVersion,
		Kind:       recipe.RecipeResultKind,
		ComponentRefs: []recipe.ComponentRef{
			{
				Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator",
				Version: "v26.4.0", Type: recipe.ComponentTypeHelm,
				Source: "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name: "nvidia-dra-driver-gpu", Namespace: "nvidia-dra-driver-gpu", Chart: "nvidia-dra-driver-gpu",
				Version: "25.12.0", Type: recipe.ComponentTypeHelm,
				Source:    "https://helm.ngc.nvidia.com/nvidia",
				Overrides: map[string]any{"nvidiaDriverRoot": "/run/nvidia/driver"},
			},
			{
				Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager",
				Version: "v1.20.2", Type: recipe.ComponentTypeHelm,
				Source: "https://charts.jetstack.io",
			},
		},
		DeploymentOrder: []string{"cert-manager", "gpu-operator", "nvidia-dra-driver-gpu"},
	}
}

func TestMake_DriverOwnershipLockUsesEmittedComponents(t *testing.T) {
	tests := []struct {
		name      string
		bundlers  []string
		overrides map[string]map[string]string
		wantGPU   bool
		wantDRA   bool
	}{
		{name: "both families", wantGPU: true, wantDRA: true},
		{name: "subset without guarded components", bundlers: []string{"cert-manager"}},
		{name: "subset without GPU Operator", bundlers: []string{"nvidia-dra-driver-gpu"}, wantDRA: true},
		{name: "subset without DRA", bundlers: []string{"gpu-operator"}, wantGPU: true},
		{
			name: "disabled by bundle-time override",
			overrides: map[string]map[string]string{
				"gpuoperator": {"enabled": "false"},
				"dradriver":   {"enabled": "false"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDeployer(config.DeployerArgoCDHelm),
				config.WithBundlers(tt.bundlers),
				config.WithValueOverrides(tt.overrides),
				config.WithDeterministic(true),
			)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			outputDir := t.TempDir()
			if _, makeErr := b.Make(t.Context(), driverOwnershipRecipe(), outputDir); makeErr != nil {
				t.Fatalf("Make() error = %v", makeErr)
			}
			guard, err := os.ReadFile(filepath.Join(outputDir, "templates", "aicr-driver-ownership-lock.yaml"))
			if !tt.wantGPU && !tt.wantDRA {
				if !os.IsNotExist(err) {
					t.Fatalf("guard stat error = %v, want not-exist", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read guard: %v", err)
			}
			for _, family := range []struct {
				path string
				want bool
			}{
				{"gpu-operator.driver.enabled", tt.wantGPU},
				{"nvidia-dra-driver-gpu.nvidiaDriverRoot", tt.wantDRA},
			} {
				if got := strings.Contains(string(guard), family.path); got != family.want {
					t.Errorf("guard names %s = %v, want %v\n%s", family.path, got, family.want, guard)
				}
			}
		})
	}
}

// TestMake_DriverOwnershipRejectsDynamicPathsBeforeOutput covers bundles where
// no CheckDriverOwnershipCoherence runs: a dynamic driver root on a bundle
// without gpu-operator would ship a stub the lock rejects on every render.
func TestMake_DriverOwnershipRejectsDynamicPathsBeforeOutput(t *testing.T) {
	tests := []struct {
		name      string
		bundlers  []string
		overrides map[string]map[string]string
		dynamic   map[string][]string
		wantMsg   string
	}{
		{
			name:     "subset without GPU Operator",
			bundlers: []string{"nvidia-dra-driver-gpu"},
			dynamic:  map[string][]string{"dradriver": {"nvidiaDriverRoot"}},
			wantMsg:  "install-time path nvidia-dra-driver-gpu.nvidiaDriverRoot intersects driver-ownership path nvidia-dra-driver-gpu.nvidiaDriverRoot",
		},
		{
			name:      "GPU Operator disabled",
			overrides: map[string]map[string]string{"gpuoperator": {"enabled": "false"}},
			dynamic:   map[string][]string{"dradriver": {"nvidiaDriverRoot"}},
			wantMsg:   "install-time path nvidia-dra-driver-gpu.nvidiaDriverRoot intersects driver-ownership path nvidia-dra-driver-gpu.nvidiaDriverRoot",
		},
		{
			// CheckDriverOwnershipCoherence runs here and reports first.
			name:    "GPU Operator present",
			dynamic: map[string][]string{"dradriver": {"nvidiaDriverRoot"}},
			wantMsg: "--dynamic dradriver:nvidiaDriverRoot targets the driver-ownership path nvidiaDriverRoot",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDeployer(config.DeployerArgoCDHelm),
				config.WithBundlers(tt.bundlers),
				config.WithValueOverrides(tt.overrides),
				config.WithDynamicValues(tt.dynamic),
			)))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			outputDir := filepath.Join(t.TempDir(), "bundle")
			_, err = b.Make(t.Context(), driverOwnershipRecipe(), outputDir)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Make() error = %v, want containing %q", err, tt.wantMsg)
			}
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
			if _, statErr := os.Stat(outputDir); !os.IsNotExist(statErr) {
				t.Fatalf("output directory exists after rejection: %v", statErr)
			}
		})
	}
}

// TestMake_DriverOwnershipRejectsStaleOutputBeforeData proves the stale
// artifact rejection runs before --data files are copied: a rejected
// regeneration leaves the prior bundle byte-identical, and a corrected rerun
// into the same directory then succeeds.
func TestMake_DriverOwnershipRejectsStaleOutputBeforeData(t *testing.T) {
	dataDir := providertest.WriteDir(t, map[string][]byte{
		recipe.RegistryFileName: []byte(providertest.EmptyRegistryYAML),
		"notes/extra.yaml":      []byte("marker: data\n"),
	})
	provider, err := recipe.NewLayeredDataProvider(
		recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: dataDir},
	)
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	recipe.EvictCachedRegistry(provider)
	t.Cleanup(func() { recipe.EvictCachedRegistry(provider) })
	bundle := func(t *testing.T, outputDir string, bundlers []string, withData bool) error {
		t.Helper()
		b, newErr := New(WithConfig(config.NewConfig(
			config.WithDeployer(config.DeployerArgoCDHelm),
			config.WithBundlers(bundlers),
			config.WithDeterministic(true),
		)))
		if newErr != nil {
			t.Fatalf("New() error = %v", newErr)
		}
		rr := driverOwnershipRecipe()
		if withData {
			rr.BindDataProvider(provider)
		}
		_, makeErr := b.Make(t.Context(), rr, outputDir)
		return makeErr
	}

	outputDir := t.TempDir()
	if initErr := bundle(t, outputDir, []string{"gpu-operator", "cert-manager"}, false); initErr != nil {
		t.Fatalf("initial Make() error = %v", initErr)
	}
	before := snapshotTree(t, outputDir)

	err = bundle(t, outputDir, []string{"cert-manager"}, true)
	if err == nil || !strings.Contains(err.Error(), "fresh output directory") {
		t.Fatalf("regeneration without gpu-operator error = %v, want stale-artifact rejection", err)
	}
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
	after := snapshotTree(t, outputDir)
	if _, copied := after["data"]; copied {
		t.Error("rejected regeneration copied --data files into the prior bundle")
	}
	if len(after) != len(before) {
		t.Fatalf("rejected regeneration changed the file set: %d entries, want %d", len(after), len(before))
	}
	for rel, want := range before {
		if got, ok := after[rel]; !ok || !bytes.Equal(got, want) {
			t.Errorf("rejected regeneration changed %s", rel)
		}
	}

	if rerunErr := bundle(t, outputDir, []string{"gpu-operator", "cert-manager"}, true); rerunErr != nil {
		t.Fatalf("corrected rerun into the same directory error = %v", rerunErr)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "data", "notes", "extra.yaml")); statErr != nil {
		t.Fatalf("corrected rerun did not copy data: %v", statErr)
	}
}

// snapshotTree maps every path under root (directories as nil) to its bytes.
func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	tree := map[string][]byte{}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			tree[rel] = nil
			return nil
		}
		data, readErr := os.ReadFile(path)
		tree[rel] = data
		return readErr
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	return tree
}
