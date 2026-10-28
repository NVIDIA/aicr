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

package aicr_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe/recipetest"
)

// TestBundleComponents_PromQLScrapeScopeParity pins that the SDK values path
// carries the same scope as MakeBundle and fails on the same declarations.
func TestBundleComponents_PromQLScrapeScopeParity(t *testing.T) {
	t.Parallel()
	missingSource := recipetest.DefaultPromQLFixture()
	missingSource["prometheus-adapter"] = recipetest.FixturePromQLConsumer(
		[]string{"DCGM_FI_DEV_GPU_UTIL"}, "gpu-operator", "dynamo-platform")
	training := aicr.RecipeRequest{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training"}
	dynamo := aicr.RecipeRequest{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "inference", Platform: "dynamo"}

	tests := []struct {
		name    string
		request aicr.RecipeRequest
		absent  bool
		wantErr string
	}{
		{name: "eks h100 training", request: training},
		{name: "eks h100 inference dynamo", request: dynamo},
		{name: "absent monitorsFrom source fails both paths", request: training, absent: true,
			wantErr: `"dynamo-platform" is absent or disabled`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := recipetest.DefaultPromQLFixture()
			if tt.absent {
				fixture = missingSource
			}
			client, err := aicr.NewClient(
				aicr.WithRecipeSource(aicr.FilesystemSource(recipetest.WriteTelemetryRegistry(t, fixture))),
				aicr.WithVersion("v-test"))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
			rec, err := client.ResolveRecipe(t.Context(), tt.request)
			if err != nil {
				t.Fatalf("ResolveRecipe: %v", err)
			}

			bundles, sdkErr := client.BundleComponents(t.Context(), rec)
			out := t.TempDir()
			_, bundleErr := client.MakeBundle(t.Context(), rec, aicr.BundleOptions{
				Config:    config.NewConfig(config.WithVersion("v-test"), config.WithDeployer(config.DeployerHelm)),
				OutputDir: out,
			})
			if tt.wantErr != "" {
				for path, err := range map[string]error{"BundleComponents": sdkErr, "MakeBundle": bundleErr} {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("%s error = %v, want one containing %q", path, err, tt.wantErr)
					}
					errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				}
				return
			}
			if sdkErr != nil || bundleErr != nil {
				t.Fatalf("BundleComponents error = %v, MakeBundle error = %v", sdkErr, bundleErr)
			}

			var sdkValues, bundleValues map[string]any
			for _, b := range bundles {
				if b.Component.Name == recipetest.PromQLServerComponent {
					if unmarshalErr := yaml.Unmarshal(b.HelmValues, &sdkValues); unmarshalErr != nil {
						t.Fatalf("unmarshal SDK values: %v", unmarshalErr)
					}
				}
			}
			matches, _ := filepath.Glob(filepath.Join(out, "*-"+recipetest.PromQLServerComponent, "values.yaml"))
			if len(matches) != 1 {
				t.Fatalf("bundle values = %v, want one file", matches)
			}
			data, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatalf("read bundle values: %v", err)
			}
			if err := yaml.Unmarshal(data, &bundleValues); err != nil {
				t.Fatalf("unmarshal bundle values: %v", err)
			}
			for _, path := range []string{
				recipetest.FixtureSMNamespaceSelectorPath, recipetest.FixturePMNamespaceSelectorPath,
				recipetest.FixtureSMSelectorPath, recipetest.FixturePMSelectorPath, recipetest.FixtureScrapeClassesPath,
			} {
				sdk, sdkOK := component.GetValueByPath(sdkValues, path)
				bundle, bundleOK := component.GetValueByPath(bundleValues, path)
				if !sdkOK || !bundleOK || !reflect.DeepEqual(sdk, bundle) {
					t.Errorf("%s: SDK %v (set %v), bundle %v (set %v); want both set and equal", path, sdk, sdkOK, bundle, bundleOK)
				}
			}
		})
	}
}
