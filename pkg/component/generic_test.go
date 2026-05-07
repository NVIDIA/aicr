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

package component

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestEnrichConfigFromRegistry(t *testing.T) {
	// Get registry to verify expected values
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}

	t.Run("enriches empty config from registry", func(t *testing.T) {
		// Start with minimal config (only Name)
		cfg := ComponentConfig{
			Name: "gpu-operator",
		}

		enrichConfigFromRegistry(&cfg)

		// Verify values were populated from registry
		comp := registry.Get("gpu-operator")
		if comp == nil {
			t.Fatal("gpu-operator not found in registry")
		}

		if cfg.DisplayName == "" {
			t.Error("DisplayName should be populated from registry")
		}
		if cfg.DisplayName != comp.DisplayName {
			t.Errorf("DisplayName = %q, want %q", cfg.DisplayName, comp.DisplayName)
		}
		if len(cfg.ValueOverrideKeys) == 0 {
			t.Error("ValueOverrideKeys should be populated from registry")
		}
		if cfg.DefaultHelmRepository == "" {
			t.Error("DefaultHelmRepository should be populated from registry")
		}
		if cfg.DefaultHelmChart == "" {
			t.Error("DefaultHelmChart should be populated from registry")
		}
	})

	t.Run("does not override existing values", func(t *testing.T) {
		// Start with fully populated config
		cfg := ComponentConfig{
			Name:                    "gpu-operator",
			DisplayName:             "Custom Display Name",
			ValueOverrideKeys:       []string{"custom-key"},
			DefaultHelmRepository:   "https://custom.repo",
			DefaultHelmChart:        "custom/chart",
			SystemNodeSelectorPaths: []string{"custom.path"},
		}

		enrichConfigFromRegistry(&cfg)

		// Verify existing values were preserved
		if cfg.DisplayName != "Custom Display Name" {
			t.Errorf("DisplayName should be preserved, got %q", cfg.DisplayName)
		}
		if len(cfg.ValueOverrideKeys) != 1 || cfg.ValueOverrideKeys[0] != "custom-key" {
			t.Errorf("ValueOverrideKeys should be preserved, got %v", cfg.ValueOverrideKeys)
		}
		if cfg.DefaultHelmRepository != "https://custom.repo" {
			t.Errorf("DefaultHelmRepository should be preserved, got %q", cfg.DefaultHelmRepository)
		}
		if cfg.DefaultHelmChart != "custom/chart" {
			t.Errorf("DefaultHelmChart should be preserved, got %q", cfg.DefaultHelmChart)
		}
		if len(cfg.SystemNodeSelectorPaths) != 1 || cfg.SystemNodeSelectorPaths[0] != "custom.path" {
			t.Errorf("SystemNodeSelectorPaths should be preserved, got %v", cfg.SystemNodeSelectorPaths)
		}
	})

	t.Run("unknown component is unchanged", func(t *testing.T) {
		cfg := ComponentConfig{
			Name:        "unknown-component",
			DisplayName: "",
		}

		enrichConfigFromRegistry(&cfg)

		// Verify nothing changed
		if cfg.DisplayName != "" {
			t.Errorf("DisplayName should remain empty for unknown component, got %q", cfg.DisplayName)
		}
	})

	t.Run("enriches node scheduling paths", func(t *testing.T) {
		cfg := ComponentConfig{
			Name: "gpu-operator",
		}

		enrichConfigFromRegistry(&cfg)

		// gpu-operator should have scheduling paths
		if len(cfg.SystemNodeSelectorPaths) == 0 {
			t.Error("SystemNodeSelectorPaths should be populated")
		}
		if len(cfg.SystemTolerationPaths) == 0 {
			t.Error("SystemTolerationPaths should be populated")
		}
		if len(cfg.AcceleratedNodeSelectorPaths) == 0 {
			t.Error("AcceleratedNodeSelectorPaths should be populated")
		}
		if len(cfg.AcceleratedTolerationPaths) == 0 {
			t.Error("AcceleratedTolerationPaths should be populated")
		}
	})
}

func TestGenerateDefaultBundleMetadata(t *testing.T) {
	tests := []struct {
		name             string
		config           map[string]string
		componentName    string
		defaultHelmRepo  string
		defaultHelmChart string
		wantNamespace    string
		wantHelmRepo     string
		wantHelmChart    string
		wantVersion      string
	}{
		{
			name:             "with all config values",
			config:           map[string]string{"namespace": "custom-ns", "helm_repository": "https://custom.repo", "helm_chart_version": "v1.0.0", "bundler_version": "1.2.3", "recipe_version": "2.0.0"},
			componentName:    "test-component",
			defaultHelmRepo:  "https://default.repo",
			defaultHelmChart: "default/chart",
			wantNamespace:    "custom-ns",
			wantHelmRepo:     "https://custom.repo",
			wantHelmChart:    "default/chart",
			wantVersion:      "1.2.3",
		},
		{
			name:             "with empty config uses defaults",
			config:           map[string]string{},
			componentName:    "my-component",
			defaultHelmRepo:  "https://default.repo",
			defaultHelmChart: "nvidia/operator",
			wantNamespace:    "my-component",
			wantHelmRepo:     "https://default.repo",
			wantHelmChart:    "nvidia/operator",
			wantVersion:      "unknown", // GetBundlerVersion returns "unknown" when not set
		},
		{
			name:             "with nil config",
			config:           nil,
			componentName:    "test",
			defaultHelmRepo:  "https://repo.io",
			defaultHelmChart: "chart",
			wantNamespace:    "test",
			wantHelmRepo:     "https://repo.io",
			wantHelmChart:    "chart",
			wantVersion:      "unknown", // GetBundlerVersion returns "unknown" when not set
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateDefaultBundleMetadata(tt.config, tt.componentName, tt.defaultHelmRepo, tt.defaultHelmChart)

			if got == nil {
				t.Fatal("GenerateDefaultBundleMetadata returned nil")
			}
			if got.Namespace != tt.wantNamespace {
				t.Errorf("Namespace = %q, want %q", got.Namespace, tt.wantNamespace)
			}
			if got.HelmRepository != tt.wantHelmRepo {
				t.Errorf("HelmRepository = %q, want %q", got.HelmRepository, tt.wantHelmRepo)
			}
			if got.HelmChart != tt.wantHelmChart {
				t.Errorf("HelmChart = %q, want %q", got.HelmChart, tt.wantHelmChart)
			}
			if got.HelmReleaseName != tt.componentName {
				t.Errorf("HelmReleaseName = %q, want %q", got.HelmReleaseName, tt.componentName)
			}
			if got.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if got.Extensions == nil {
				t.Error("Extensions map should not be nil")
			}
		})
	}
}

func TestGenerateBundleMetadataWithExtensions(t *testing.T) {
	tests := []struct {
		name                 string
		config               map[string]string
		componentConfig      ComponentConfig
		wantHelmChartVersion string
		wantExtensionCount   int
		wantExtensionKey     string
		wantExtensionValue   any
	}{
		{
			name:   "with extensions and default version",
			config: map[string]string{},
			componentConfig: ComponentConfig{
				Name:                    "test-component",
				DefaultHelmRepository:   "https://repo.io",
				DefaultHelmChart:        "chart",
				DefaultHelmChartVersion: "v1.2.3",
				MetadataExtensions: map[string]any{
					"CustomField": "custom-value",
					"Enabled":     true,
				},
			},
			wantHelmChartVersion: "v1.2.3",
			wantExtensionCount:   2,
			wantExtensionKey:     "CustomField",
			wantExtensionValue:   "custom-value",
		},
		{
			name:   "config version overrides default",
			config: map[string]string{"helm_chart_version": "v2.0.0"},
			componentConfig: ComponentConfig{
				Name:                    "test",
				DefaultHelmRepository:   "https://repo.io",
				DefaultHelmChart:        "chart",
				DefaultHelmChartVersion: "v1.0.0",
			},
			wantHelmChartVersion: "v2.0.0",
			wantExtensionCount:   0,
		},
		{
			name:   "no extensions",
			config: map[string]string{},
			componentConfig: ComponentConfig{
				Name:                  "test",
				DefaultHelmRepository: "https://repo.io",
				DefaultHelmChart:      "chart",
			},
			wantHelmChartVersion: "",
			wantExtensionCount:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GenerateBundleMetadataWithExtensions(tt.config, tt.componentConfig)

			if got == nil {
				t.Fatal("GenerateBundleMetadataWithExtensions returned nil")
			}
			if got.HelmChartVersion != tt.wantHelmChartVersion {
				t.Errorf("HelmChartVersion = %q, want %q", got.HelmChartVersion, tt.wantHelmChartVersion)
			}
			if len(got.Extensions) != tt.wantExtensionCount {
				t.Errorf("Extensions count = %d, want %d", len(got.Extensions), tt.wantExtensionCount)
			}
			if tt.wantExtensionKey != "" {
				if val, ok := got.Extensions[tt.wantExtensionKey]; !ok {
					t.Errorf("Extension %q not found", tt.wantExtensionKey)
				} else if val != tt.wantExtensionValue {
					t.Errorf("Extension[%q] = %v, want %v", tt.wantExtensionKey, val, tt.wantExtensionValue)
				}
			}
		})
	}
}

func TestGetValueOverridesForComponent(t *testing.T) {
	tests := []struct {
		name            string
		configOverrides map[string]map[string]string
		componentConfig ComponentConfig
		wantOverrides   map[string]string
	}{
		{
			name: "finds by component name",
			configOverrides: map[string]map[string]string{
				"gpu-operator": {"driver.version": "550.0.0"},
			},
			componentConfig: ComponentConfig{
				Name:              "gpu-operator",
				ValueOverrideKeys: []string{"gpuoperator"},
			},
			wantOverrides: map[string]string{"driver.version": "550.0.0"},
		},
		{
			name: "finds by alternative key",
			configOverrides: map[string]map[string]string{
				"gpuoperator": {"mig.strategy": "mixed"},
			},
			componentConfig: ComponentConfig{
				Name:              "gpu-operator",
				ValueOverrideKeys: []string{"gpuoperator", "gpu"},
			},
			wantOverrides: map[string]string{"mig.strategy": "mixed"},
		},
		{
			name: "component name takes priority over alternative keys",
			configOverrides: map[string]map[string]string{
				"gpu-operator": {"version": "1.0.0"},
				"gpuoperator":  {"version": "2.0.0"},
			},
			componentConfig: ComponentConfig{
				Name:              "gpu-operator",
				ValueOverrideKeys: []string{"gpuoperator"},
			},
			wantOverrides: map[string]string{"version": "1.0.0"},
		},
		{
			name:            "nil overrides returns nil",
			configOverrides: nil,
			componentConfig: ComponentConfig{
				Name: "test",
			},
			wantOverrides: nil,
		},
		{
			name: "no matching key returns nil",
			configOverrides: map[string]map[string]string{
				"other-component": {"key": "value"},
			},
			componentConfig: ComponentConfig{
				Name:              "test-component",
				ValueOverrideKeys: []string{"test"},
			},
			wantOverrides: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.NewConfig(config.WithValueOverrides(tt.configOverrides))

			b := NewBaseBundler(cfg, "test")
			got := getValueOverridesForComponent(b, tt.componentConfig)

			if tt.wantOverrides == nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}

			if got == nil {
				t.Fatal("got nil, want non-nil")
			}

			for k, v := range tt.wantOverrides {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
