// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package config

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// testValueTrue is used as a consistent value string for test assertions.
const testValueTrue = "true"

// testValueModified is used as a consistent value string for test assertions.
const testValueModified = "modified"

// testValueAdded is used as a consistent value string for test assertions.
const testValueAdded = "added"

func TestConfigImmutability(t *testing.T) {
	cfg := NewConfig()

	// Verify getters return expected default values
	if !cfg.IncludeReadme() {
		t.Error("IncludeReadme() = false, want true")
	}

	if !cfg.IncludeChecksums() {
		t.Error("IncludeChecksums() = false, want true")
	}

	if cfg.Verbose() {
		t.Error("Verbose() = true, want false")
	}

	if cfg.Attest() {
		t.Error("Attest() = true, want false (default)")
	}
}

func TestConfigAttest(t *testing.T) {
	cfg := NewConfig(WithAttest(true))

	if !cfg.Attest() {
		t.Error("Attest() = false, want true")
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
	}{
		{
			name:    "valid default config",
			config:  NewConfig(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewConfigWithOptions(t *testing.T) {
	cfg := NewConfig(
		WithIncludeReadme(false),
		WithIncludeChecksums(false),
		WithVerbose(true),
	)

	// Verify all options were applied
	if cfg.IncludeReadme() {
		t.Error("IncludeReadme() = true, want false")
	}
	if cfg.IncludeReadme() {
		t.Error("IncludeReadme() = true, want false")
	}
	if cfg.IncludeChecksums() {
		t.Error("IncludeChecksums() = true, want false")
	}
	if !cfg.Verbose() {
		t.Error("Verbose() = false, want true")
	}
}

func TestAllGetters(t *testing.T) {
	cfg := NewConfig(
		WithIncludeReadme(true),
		WithIncludeChecksums(false),
		WithVerbose(true),
	)

	tests := []struct {
		name     string
		got      any
		want     any
		getterFn string
	}{
		{"IncludeReadme", cfg.IncludeReadme(), true, "IncludeReadme()"},
		{"IncludeChecksums", cfg.IncludeChecksums(), false, "IncludeChecksums()"},
		{"Verbose", cfg.Verbose(), true, "Verbose()"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.getterFn, tt.got, tt.want)
			}
		})
	}
}

func TestVersionOption(t *testing.T) {
	// Test WithVersion sets the version
	cfg := NewConfig(WithVersion("v1.2.3"))
	if cfg.Version() != "v1.2.3" {
		t.Errorf("Version() = %s, want v1.2.3", cfg.Version())
	}

	// Test default version
	cfgDefault := NewConfig()
	if cfgDefault.Version() != "dev" {
		t.Errorf("default Version() = %s, want dev", cfgDefault.Version())
	}
}

func TestValueOverridesOption(t *testing.T) {
	overrides := map[string]map[string]string{
		"gpuoperator": {
			"gds.enabled":    "true",
			"driver.version": "570.86.16",
		},
		"networkoperator": {
			"rdma.enabled": "true",
		},
	}

	cfg := NewConfig(WithValueOverrides(overrides))

	// Verify overrides were set
	got := cfg.ValueOverrides()
	if got == nil {
		t.Fatal("ValueOverrides() returned nil")
	}

	// Verify gpuoperator overrides
	if got["gpuoperator"]["gds.enabled"] != testValueTrue {
		t.Errorf("gpuoperator gds.enabled = %s, want true", got["gpuoperator"]["gds.enabled"])
	}
	if got["gpuoperator"]["driver.version"] != "570.86.16" {
		t.Errorf("gpuoperator driver.version = %s, want 570.86.16", got["gpuoperator"]["driver.version"])
	}

	// Verify networkoperator overrides
	if got["networkoperator"]["rdma.enabled"] != testValueTrue {
		t.Errorf("networkoperator rdma.enabled = %s, want true", got["networkoperator"]["rdma.enabled"])
	}
}

func TestValueOverridesImmutability(t *testing.T) {
	overrides := map[string]map[string]string{
		"gpuoperator": {"key": "value"},
	}

	cfg := NewConfig(WithValueOverrides(overrides))

	// Get and modify returned map
	got := cfg.ValueOverrides()
	got["gpuoperator"]["key"] = testValueModified
	got["gpuoperator"]["new"] = testValueAdded

	// Verify original config unchanged
	fresh := cfg.ValueOverrides()
	if fresh["gpuoperator"]["key"] != "value" {
		t.Error("modifying returned map affected config - not immutable")
	}
	if _, exists := fresh["gpuoperator"]["new"]; exists {
		t.Error("adding key to returned map affected config - not immutable")
	}
}

func TestValueOverridesNil(t *testing.T) {
	// WithValueOverrides with nil should not panic
	cfg := NewConfig(WithValueOverrides(nil))

	// ValueOverrides on empty config should return nil
	got := cfg.ValueOverrides()
	if len(got) > 0 {
		t.Errorf("ValueOverrides() = %v, want nil or empty", got)
	}
}

func TestNodeSelectorOptions(t *testing.T) {
	t.Run("SystemNodeSelector with valid values", func(t *testing.T) {
		selectors := map[string]string{
			"nodeGroup":        "system-pool",
			"kubernetes.io/os": "linux",
		}
		cfg := NewConfig(WithSystemNodeSelector(selectors))

		got := cfg.SystemNodeSelector()
		if got == nil {
			t.Fatal("SystemNodeSelector() returned nil")
		}
		if got["nodeGroup"] != "system-pool" {
			t.Errorf("SystemNodeSelector()[nodeGroup] = %s, want system-pool", got["nodeGroup"])
		}
		if got["kubernetes.io/os"] != "linux" {
			t.Errorf("SystemNodeSelector()[kubernetes.io/os] = %s, want linux", got["kubernetes.io/os"])
		}
	})

	t.Run("SystemNodeSelector with nil input", func(t *testing.T) {
		cfg := NewConfig(WithSystemNodeSelector(nil))
		got := cfg.SystemNodeSelector()
		if got != nil {
			t.Errorf("SystemNodeSelector() = %v, want nil for nil input", got)
		}
	})

	t.Run("SystemNodeSelector returns nil for nil config", func(t *testing.T) {
		cfg := NewConfig()
		got := cfg.SystemNodeSelector()
		if got != nil {
			t.Errorf("SystemNodeSelector() = %v, want nil", got)
		}
	})

	t.Run("SystemNodeSelector immutability", func(t *testing.T) {
		selectors := map[string]string{"key": "value"}
		cfg := NewConfig(WithSystemNodeSelector(selectors))

		got := cfg.SystemNodeSelector()
		got["key"] = testValueModified
		got["new"] = testValueAdded

		fresh := cfg.SystemNodeSelector()
		if fresh["key"] != "value" {
			t.Error("modifying returned map affected config - not immutable")
		}
		if _, exists := fresh["new"]; exists {
			t.Error("adding key to returned map affected config - not immutable")
		}
	})

	t.Run("AcceleratedNodeSelector with valid values", func(t *testing.T) {
		selectors := map[string]string{
			"nvidia.com/gpu.present": "true",
			"accelerator":            "nvidia-gb200",
		}
		cfg := NewConfig(WithAcceleratedNodeSelector(selectors))

		got := cfg.AcceleratedNodeSelector()
		if got == nil {
			t.Fatal("AcceleratedNodeSelector() returned nil")
		}
		if got["nvidia.com/gpu.present"] != testValueTrue {
			t.Errorf("AcceleratedNodeSelector()[nvidia.com/gpu.present] = %s, want true", got["nvidia.com/gpu.present"])
		}
	})

	t.Run("AcceleratedNodeSelector with nil input", func(t *testing.T) {
		cfg := NewConfig(WithAcceleratedNodeSelector(nil))
		got := cfg.AcceleratedNodeSelector()
		if got != nil {
			t.Errorf("AcceleratedNodeSelector() = %v, want nil for nil input", got)
		}
	})

	t.Run("AcceleratedNodeSelector returns nil for nil config", func(t *testing.T) {
		cfg := NewConfig()
		got := cfg.AcceleratedNodeSelector()
		if got != nil {
			t.Errorf("AcceleratedNodeSelector() = %v, want nil", got)
		}
	})

	t.Run("EstimatedNodeCount default zero", func(t *testing.T) {
		cfg := NewConfig()
		if got := cfg.EstimatedNodeCount(); got != 0 {
			t.Errorf("EstimatedNodeCount() = %d, want 0", got)
		}
	})

	t.Run("EstimatedNodeCount with value", func(t *testing.T) {
		cfg := NewConfig(WithEstimatedNodeCount(8))
		if got := cfg.EstimatedNodeCount(); got != 8 {
			t.Errorf("EstimatedNodeCount() = %d, want 8", got)
		}
	})

	t.Run("EstimatedNodeCount negative clamped to zero", func(t *testing.T) {
		cfg := NewConfig(WithEstimatedNodeCount(-1))
		if got := cfg.EstimatedNodeCount(); got != 0 {
			t.Errorf("EstimatedNodeCount() = %d, want 0 (negative clamped)", got)
		}
	})
}

func TestNodeTolerationOptions(t *testing.T) {
	t.Run("SystemNodeTolerations with valid values", func(t *testing.T) {
		tolerations := []corev1.Toleration{
			{Key: "dedicated", Value: "system", Effect: corev1.TaintEffectNoSchedule},
		}
		cfg := NewConfig(WithSystemNodeTolerations(tolerations))

		got := cfg.SystemNodeTolerations()
		if got == nil {
			t.Fatal("SystemNodeTolerations() returned nil")
		}
		if len(got) != 1 {
			t.Fatalf("SystemNodeTolerations() len = %d, want 1", len(got))
		}
		if got[0].Key != "dedicated" {
			t.Errorf("SystemNodeTolerations()[0].Key = %s, want dedicated", got[0].Key)
		}
	})

	t.Run("SystemNodeTolerations with nil input", func(t *testing.T) {
		cfg := NewConfig(WithSystemNodeTolerations(nil))
		got := cfg.SystemNodeTolerations()
		if got != nil {
			t.Errorf("SystemNodeTolerations() = %v, want nil for nil input", got)
		}
	})

	t.Run("SystemNodeTolerations returns nil for nil config", func(t *testing.T) {
		cfg := NewConfig()
		got := cfg.SystemNodeTolerations()
		if got != nil {
			t.Errorf("SystemNodeTolerations() = %v, want nil", got)
		}
	})

	t.Run("AcceleratedNodeTolerations with valid values", func(t *testing.T) {
		tolerations := []corev1.Toleration{
			{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule},
		}
		cfg := NewConfig(WithAcceleratedNodeTolerations(tolerations))

		got := cfg.AcceleratedNodeTolerations()
		if got == nil {
			t.Fatal("AcceleratedNodeTolerations() returned nil")
		}
		if len(got) != 1 {
			t.Fatalf("AcceleratedNodeTolerations() len = %d, want 1", len(got))
		}
		if got[0].Key != "nvidia.com/gpu" {
			t.Errorf("AcceleratedNodeTolerations()[0].Key = %s, want nvidia.com/gpu", got[0].Key)
		}
	})

	t.Run("AcceleratedNodeTolerations with nil input", func(t *testing.T) {
		cfg := NewConfig(WithAcceleratedNodeTolerations(nil))
		got := cfg.AcceleratedNodeTolerations()
		if got != nil {
			t.Errorf("AcceleratedNodeTolerations() = %v, want nil for nil input", got)
		}
	})

	t.Run("AcceleratedNodeTolerations returns nil for nil config", func(t *testing.T) {
		cfg := NewConfig()
		got := cfg.AcceleratedNodeTolerations()
		if got != nil {
			t.Errorf("AcceleratedNodeTolerations() = %v, want nil", got)
		}
	})
}

func TestDeployerOptions(t *testing.T) {
	t.Run("default deployer is helm", func(t *testing.T) {
		cfg := NewConfig()
		if cfg.Deployer() != DeployerHelm {
			t.Errorf("Deployer() = %s, want %s", cfg.Deployer(), DeployerHelm)
		}
	})

	t.Run("WithDeployer sets argocd", func(t *testing.T) {
		cfg := NewConfig(WithDeployer(DeployerArgoCD))
		if cfg.Deployer() != DeployerArgoCD {
			t.Errorf("Deployer() = %s, want %s", cfg.Deployer(), DeployerArgoCD)
		}
	})

	t.Run("WithRepoURL sets repository URL", func(t *testing.T) {
		cfg := NewConfig(WithRepoURL("https://github.com/org/repo.git"))
		if cfg.RepoURL() != "https://github.com/org/repo.git" {
			t.Errorf("RepoURL() = %s, want https://github.com/org/repo.git", cfg.RepoURL())
		}
	})

	t.Run("default RepoURL is empty", func(t *testing.T) {
		cfg := NewConfig()
		if cfg.RepoURL() != "" {
			t.Errorf("RepoURL() = %s, want empty string", cfg.RepoURL())
		}
	})

	t.Run("WithTargetRevision sets target revision", func(t *testing.T) {
		cfg := NewConfig(WithTargetRevision("v1.0.0"))
		if cfg.TargetRevision() != "v1.0.0" {
			t.Errorf("TargetRevision() = %s, want v1.0.0", cfg.TargetRevision())
		}
	})

	t.Run("default TargetRevision is empty", func(t *testing.T) {
		cfg := NewConfig()
		if cfg.TargetRevision() != "" {
			t.Errorf("TargetRevision() = %s, want empty string", cfg.TargetRevision())
		}
	})
}

func TestParseValueOverrides(t *testing.T) {
	t.Run("valid single override", func(t *testing.T) {
		result, err := ParseValueOverrides([]string{"gpuoperator:gds.enabled=true"})
		if err != nil {
			t.Fatalf("ParseValueOverrides() error = %v", err)
		}
		if result["gpuoperator"]["gds.enabled"] != testValueTrue {
			t.Errorf("result[gpuoperator][gds.enabled] = %s, want true", result["gpuoperator"]["gds.enabled"])
		}
	})

	t.Run("valid multiple overrides same bundler", func(t *testing.T) {
		result, err := ParseValueOverrides([]string{
			"gpuoperator:gds.enabled=true",
			"gpuoperator:driver.version=570.86.16",
		})
		if err != nil {
			t.Fatalf("ParseValueOverrides() error = %v", err)
		}
		if result["gpuoperator"]["gds.enabled"] != testValueTrue {
			t.Errorf("result[gpuoperator][gds.enabled] = %s, want true", result["gpuoperator"]["gds.enabled"])
		}
		if result["gpuoperator"]["driver.version"] != "570.86.16" {
			t.Errorf("result[gpuoperator][driver.version] = %s, want 570.86.16", result["gpuoperator"]["driver.version"])
		}
	})

	t.Run("valid multiple bundlers", func(t *testing.T) {
		result, err := ParseValueOverrides([]string{
			"gpuoperator:gds.enabled=true",
			"networkoperator:rdma.enabled=false",
		})
		if err != nil {
			t.Fatalf("ParseValueOverrides() error = %v", err)
		}
		if result["gpuoperator"]["gds.enabled"] != testValueTrue {
			t.Errorf("result[gpuoperator][gds.enabled] = %s, want true", result["gpuoperator"]["gds.enabled"])
		}
		if result["networkoperator"]["rdma.enabled"] != "false" {
			t.Errorf("result[networkoperator][rdma.enabled] = %s, want false", result["networkoperator"]["rdma.enabled"])
		}
	})

	t.Run("empty input", func(t *testing.T) {
		result, err := ParseValueOverrides([]string{})
		if err != nil {
			t.Fatalf("ParseValueOverrides() error = %v", err)
		}
		if len(result) != 0 {
			t.Errorf("ParseValueOverrides([]) len = %d, want 0", len(result))
		}
	})

	t.Run("missing colon separator", func(t *testing.T) {
		_, err := ParseValueOverrides([]string{"invalid-no-colon"})
		if err == nil {
			t.Error("ParseValueOverrides() expected error for missing colon, got nil")
		}
	})

	t.Run("missing equals sign", func(t *testing.T) {
		_, err := ParseValueOverrides([]string{"bundler:path-no-equals"})
		if err == nil {
			t.Error("ParseValueOverrides() expected error for missing equals, got nil")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := ParseValueOverrides([]string{"bundler:=value"})
		if err == nil {
			t.Error("ParseValueOverrides() expected error for empty path, got nil")
		}
	})

	t.Run("empty value", func(t *testing.T) {
		_, err := ParseValueOverrides([]string{"bundler:path="})
		if err == nil {
			t.Error("ParseValueOverrides() expected error for empty value, got nil")
		}
	})

	t.Run("value with equals sign", func(t *testing.T) {
		result, err := ParseValueOverrides([]string{"bundler:path=value=with=equals"})
		if err != nil {
			t.Fatalf("ParseValueOverrides() error = %v", err)
		}
		if result["bundler"]["path"] != "value=with=equals" {
			t.Errorf("result[bundler][path] = %s, want value=with=equals", result["bundler"]["path"])
		}
	})
}

func TestParseDeployerType(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    DeployerType
		wantErr bool
	}{
		{"helm lowercase", "helm", DeployerHelm, false},
		{"helm uppercase", "HELM", DeployerHelm, false},
		{"helm mixed case", "Helm", DeployerHelm, false},
		{"argocd lowercase", "argocd", DeployerArgoCD, false},
		{"argocd uppercase", "ARGOCD", DeployerArgoCD, false},
		{"argocd mixed case", "ArgoCD", DeployerArgoCD, false},
		{"helm with spaces", "  helm  ", DeployerHelm, false},
		{"invalid type", "invalid", "", true},
		{"empty string", "", "", true},
		{"flux not supported", "flux", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDeployerType(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseDeployerType(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseDeployerType(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestGetDeployerTypes(t *testing.T) {
	types := GetDeployerTypes()

	// Verify we get the expected types
	if len(types) != 2 {
		t.Errorf("GetDeployerTypes() returned %d types, want 2", len(types))
	}

	// Verify types are sorted alphabetically
	for i := 1; i < len(types); i++ {
		if types[i-1] > types[i] {
			t.Errorf("GetDeployerTypes() not sorted: %v", types)
			break
		}
	}

	// Verify expected types are present
	found := make(map[string]bool)
	for _, dt := range types {
		found[dt] = true
	}
	if !found[string(DeployerArgoCD)] {
		t.Error("GetDeployerTypes() missing 'argocd'")
	}
	if !found[string(DeployerHelm)] {
		t.Error("GetDeployerTypes() missing 'helm'")
	}
}

func TestDeployerTypeString(t *testing.T) {
	tests := []struct {
		dt   DeployerType
		want string
	}{
		{DeployerHelm, "helm"},
		{DeployerArgoCD, "argocd"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.dt.String(); got != tt.want {
				t.Errorf("DeployerType.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkloadGateTaintOptions(t *testing.T) {
	t.Run("WithWorkloadGateTaint with valid taint", func(t *testing.T) {
		taint := &corev1.Taint{
			Key:    "skyhook.io/runtime-required",
			Value:  "true",
			Effect: corev1.TaintEffectNoSchedule,
		}
		cfg := NewConfig(WithWorkloadGateTaint(taint))

		got := cfg.WorkloadGateTaint()
		if got == nil {
			t.Fatal("WorkloadGateTaint() returned nil")
		}
		if got.Key != "skyhook.io/runtime-required" {
			t.Errorf("WorkloadGateTaint().Key = %s, want skyhook.io/runtime-required", got.Key)
		}
		if got.Value != "true" {
			t.Errorf("WorkloadGateTaint().Value = %s, want true", got.Value)
		}
		if got.Effect != corev1.TaintEffectNoSchedule {
			t.Errorf("WorkloadGateTaint().Effect = %s, want NoSchedule", got.Effect)
		}
	})

	t.Run("WithWorkloadGateTaint with nil input", func(t *testing.T) {
		cfg := NewConfig(WithWorkloadGateTaint(nil))
		got := cfg.WorkloadGateTaint()
		if got != nil {
			t.Errorf("WorkloadGateTaint() = %v, want nil for nil input", got)
		}
	})

	t.Run("WorkloadGateTaint returns nil for default config", func(t *testing.T) {
		cfg := NewConfig()
		got := cfg.WorkloadGateTaint()
		if got != nil {
			t.Errorf("WorkloadGateTaint() = %v, want nil", got)
		}
	})

	t.Run("WorkloadGateTaint with taint without value", func(t *testing.T) {
		taint := &corev1.Taint{
			Key:    "dedicated",
			Effect: corev1.TaintEffectNoSchedule,
		}
		cfg := NewConfig(WithWorkloadGateTaint(taint))

		got := cfg.WorkloadGateTaint()
		if got == nil {
			t.Fatal("WorkloadGateTaint() returned nil")
		}
		if got.Key != "dedicated" {
			t.Errorf("WorkloadGateTaint().Key = %s, want dedicated", got.Key)
		}
		if got.Value != "" {
			t.Errorf("WorkloadGateTaint().Value = %s, want empty", got.Value)
		}
	})
}

func TestWorkloadSelectorOptions(t *testing.T) {
	t.Run("WithWorkloadSelector with valid selector", func(t *testing.T) {
		selector := map[string]string{
			"workload-type": "training",
			"app":           "pytorch",
		}
		cfg := NewConfig(WithWorkloadSelector(selector))

		got := cfg.WorkloadSelector()
		if got == nil {
			t.Fatal("WorkloadSelector() returned nil")
		}
		if got["workload-type"] != "training" {
			t.Errorf("WorkloadSelector()[workload-type] = %s, want training", got["workload-type"])
		}
		if got["app"] != "pytorch" {
			t.Errorf("WorkloadSelector()[app] = %s, want pytorch", got["app"])
		}
	})

	t.Run("WithWorkloadSelector with nil input", func(t *testing.T) {
		cfg := NewConfig(WithWorkloadSelector(nil))
		got := cfg.WorkloadSelector()
		if got != nil {
			t.Errorf("WorkloadSelector() = %v, want nil for nil input", got)
		}
	})

	t.Run("WorkloadSelector returns nil for default config", func(t *testing.T) {
		cfg := NewConfig()
		got := cfg.WorkloadSelector()
		if got != nil {
			t.Errorf("WorkloadSelector() = %v, want nil", got)
		}
	})

	t.Run("WorkloadSelector immutability", func(t *testing.T) {
		selector := map[string]string{"workload-type": "training"}
		cfg := NewConfig(WithWorkloadSelector(selector))

		got := cfg.WorkloadSelector()
		got["workload-type"] = testValueModified
		got["new"] = testValueAdded

		fresh := cfg.WorkloadSelector()
		if fresh["workload-type"] != "training" {
			t.Error("modifying returned map affected config - not immutable")
		}
		if _, exists := fresh["new"]; exists {
			t.Error("adding key to returned map affected config - not immutable")
		}
	})
}
