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

package aicr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appconfig "github.com/NVIDIA/aicr/pkg/config"
)

// ValidateSettings is a plain value, so most of its coverage lives in the
// external aicr_test package (config_test.go,
// TestConfig_ValidateSettings_FoldsValues). This file covers the two fields
// that test does not (ImagePullSecrets, NodeSelector) plus the
// mutation-critical Cleanup inversion, the pointer-stays-unset guard, the
// unknown-phase rejection path, and nil-Config handling — cases that predate
// the value-shape change and still apply to it.

func writeInternalConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aicr-config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validateSpecConfig = `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  validate:
    agent:
      namespace: aicr-validate
      imagePullSecrets:
        - regcred
      nodeSelector:
        role: gpu
    execution:
      noCluster: true
      noCleanup: true
      failFast: true
      timeout: 15m
      phases:
        - deployment
`

func TestConfig_ValidateSettings_FoldsAgentAndSchedulingValues(t *testing.T) {
	cfg, err := LoadConfig(context.Background(), writeInternalConfig(t, validateSpecConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, _, err := cfg.ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}

	if got.Namespace != "aicr-validate" {
		t.Errorf("Namespace = %q, want aicr-validate", got.Namespace)
	}
	if len(got.ImagePullSecrets) != 1 || got.ImagePullSecrets[0] != "regcred" {
		t.Errorf("ImagePullSecrets = %v, want [regcred]", got.ImagePullSecrets)
	}
	if got.NodeSelector["role"] != "gpu" {
		t.Errorf("NodeSelector[role] = %q, want gpu", got.NodeSelector["role"])
	}
	if !got.NoCluster {
		t.Error("NoCluster = false, want true")
	}
	if got.FailFast == nil || !*got.FailFast {
		t.Errorf("FailFast = %v, want true", got.FailFast)
	}
	if got.Timeout == nil || *got.Timeout != 15*time.Minute {
		t.Errorf("Timeout = %v, want 15m", got.Timeout)
	}
	if len(got.Phases) != 1 || got.Phases[0] != Phase("deployment") {
		t.Errorf("Phases = %v, want [deployment]", got.Phases)
	}
}

// TestConfig_ValidateSettings_CleanupIsInverted is the case most likely to
// ship wrong: spec.validate says "noCleanup", the field says "cleanup". A
// pass-through reverses it, and nothing else in the suite would notice —
// artifacts would be deleted exactly when a post-mortem asked to keep them.
func TestConfig_ValidateSettings_CleanupIsInverted(t *testing.T) {
	tests := []struct {
		name        string
		noCleanup   string
		wantCleanup bool
	}{
		{"noCleanup true means do not clean up", "true", false},
		{"noCleanup false means clean up", "false", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  validate:
    execution:
      noCleanup: ` + tt.noCleanup + "\n"
			cfg, err := LoadConfig(context.Background(), writeInternalConfig(t, body))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			got, _, err := cfg.ValidateSettings()
			if err != nil {
				t.Fatalf("ValidateSettings: %v", err)
			}
			if got.Cleanup != tt.wantCleanup {
				t.Errorf("Cleanup = %v, want %v (noCleanup: %s)", got.Cleanup, tt.wantCleanup, tt.noCleanup)
			}
		})
	}
}

// TestConfig_ValidateSettings_UnsetStaysUnset guards the pointer fields:
// config saying nothing must not become an explicit choice that overrides the
// validator's own default.
func TestConfig_ValidateSettings_UnsetStaysUnset(t *testing.T) {
	body := `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  validate:
    agent:
      namespace: only-this
`
	cfg, err := LoadConfig(context.Background(), writeInternalConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	got, _, err := cfg.ValidateSettings()
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}

	if got.FailFast != nil {
		t.Errorf("FailFast = %v, want nil (config said nothing)", *got.FailFast)
	}
	if got.Timeout != nil {
		t.Errorf("Timeout = %v, want nil (config said nothing)", *got.Timeout)
	}
	if len(got.Phases) != 0 {
		t.Errorf("Phases = %v, want none (config said nothing)", got.Phases)
	}
}

// TestConfig_ValidateSettings_RejectsUnknownPhase pins WHERE an unknown phase
// is caught, which is what lets the derivation cast instead of re-parsing.
//
// The check lives in Validation().Resolve(), not in the loader, so it holds on
// the WrapConfig path too — a hand-built document no loader has seen. If that
// ever moved into LoadConfig, the WrapConfig case here would start returning a
// value built from an unvalidated phase, and the cast would need to become a
// parse again.
func TestConfig_ValidateSettings_RejectsUnknownPhase(t *testing.T) {
	t.Run("LoadConfig rejects it first", func(t *testing.T) {
		body := `apiVersion: aicr.run/v1beta1
kind: AICRConfig
spec:
  validate:
    execution:
      phases:
        - deploymnt
`
		if _, err := LoadConfig(context.Background(), writeInternalConfig(t, body)); err == nil {
			t.Fatal("LoadConfig accepted an unknown phase; it must fail closed")
		}
	})

	t.Run("WrapConfig path is rejected by Resolve", func(t *testing.T) {
		cfg := WrapConfig(&appconfig.AICRConfig{
			Spec: appconfig.Spec{
				Validate: &appconfig.ValidateSpec{
					Execution: &appconfig.ValidateExecutionSpec{
						Phases: []string{"deploymnt"},
					},
				},
			},
		})
		if _, _, err := cfg.ValidateSettings(); err == nil {
			t.Fatal("ValidateSettings accepted an unknown phase from an unvalidated document")
		}
	})
}

func TestConfig_ValidateSettings_Absent(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		var cfg *Config
		got, present, err := cfg.ValidateSettings()
		if err != nil {
			t.Fatalf("ValidateSettings on nil Config: %v", err)
		}
		if present {
			t.Error("present = true, want false for a nil Config")
		}
		if got.Namespace != "" || got.Cleanup || got.NoCluster || len(got.Phases) != 0 {
			t.Errorf("got %+v, want zero value", got)
		}
	})
}
