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

// ValidateOptions returns opaque functional options, so asserting what it
// folded needs the internal capture struct — hence this file rather than the
// external aicr_test package. Counting the returned options would pass while
// a value was inverted or two fields were swapped, which is the whole failure
// mode this derivation has.

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

func TestConfig_ValidateOptions_FoldsValues(t *testing.T) {
	cfg, err := LoadConfig(context.Background(), writeInternalConfig(t, validateSpecConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	opts, err := cfg.ValidateOptions()
	if err != nil {
		t.Fatalf("ValidateOptions: %v", err)
	}
	vc := buildValidateConfig(opts)

	if vc.namespace == nil || *vc.namespace != "aicr-validate" {
		t.Errorf("namespace = %v, want aicr-validate", vc.namespace)
	}
	if len(vc.imagePullSecrets) != 1 || vc.imagePullSecrets[0] != "regcred" {
		t.Errorf("imagePullSecrets = %v, want [regcred]", vc.imagePullSecrets)
	}
	if vc.nodeSelector["role"] != "gpu" {
		t.Errorf("nodeSelector[role] = %q, want gpu", vc.nodeSelector["role"])
	}
	if vc.noCluster == nil || !*vc.noCluster {
		t.Errorf("noCluster = %v, want true", vc.noCluster)
	}
	if vc.failFast == nil || !*vc.failFast {
		t.Errorf("failFast = %v, want true", vc.failFast)
	}
	if vc.timeout == nil || *vc.timeout != 15*time.Minute {
		t.Errorf("timeout = %v, want 15m", vc.timeout)
	}
	if len(vc.phases) != 1 || vc.phases[0] != Phase("deployment") {
		t.Errorf("phases = %v, want [deployment]", vc.phases)
	}
}

// TestConfig_ValidateOptions_CleanupIsInverted is the case most likely to ship
// wrong: spec.validate says "noCleanup", the option says "cleanup". A
// pass-through reverses it, and nothing else in the suite would notice —
// artifacts would be deleted exactly when a post-mortem asked to keep them.
func TestConfig_ValidateOptions_CleanupIsInverted(t *testing.T) {
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
			opts, err := cfg.ValidateOptions()
			if err != nil {
				t.Fatalf("ValidateOptions: %v", err)
			}
			vc := buildValidateConfig(opts)
			if vc.cleanup == nil {
				t.Fatal("cleanup was not set; the derivation must always emit it")
			}
			if *vc.cleanup != tt.wantCleanup {
				t.Errorf("cleanup = %v, want %v (noCleanup: %s)", *vc.cleanup, tt.wantCleanup, tt.noCleanup)
			}
		})
	}
}

// TestConfig_ValidateOptions_UnsetStaysUnset guards the pointer fields: config
// saying nothing must not become an explicit choice that overrides the
// validator's own default.
func TestConfig_ValidateOptions_UnsetStaysUnset(t *testing.T) {
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
	opts, err := cfg.ValidateOptions()
	if err != nil {
		t.Fatalf("ValidateOptions: %v", err)
	}
	vc := buildValidateConfig(opts)

	if vc.failFast != nil {
		t.Errorf("failFast = %v, want nil (config said nothing)", *vc.failFast)
	}
	if vc.timeout != nil {
		t.Errorf("timeout = %v, want nil (config said nothing)", *vc.timeout)
	}
	if len(vc.phases) != 0 {
		t.Errorf("phases = %v, want none (config said nothing)", vc.phases)
	}
}

// TestConfig_ValidateOptions_RejectsUnknownPhase pins WHERE an unknown phase
// is caught, which is what lets the derivation cast instead of re-parsing.
//
// The check lives in Validation().Resolve(), not in the loader, so it holds on
// the WrapConfig path too — a hand-built document no loader has seen. If that
// ever moved into LoadConfig, the WrapConfig case here would start returning
// options built from an unvalidated phase, and the cast would need to become a
// parse again.
func TestConfig_ValidateOptions_RejectsUnknownPhase(t *testing.T) {
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
		if _, err := cfg.ValidateOptions(); err == nil {
			t.Fatal("ValidateOptions accepted an unknown phase from an unvalidated document")
		}
	})
}

func TestConfig_ValidateOptions_Absent(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		var cfg *Config
		opts, err := cfg.ValidateOptions()
		if err != nil {
			t.Fatalf("ValidateOptions on nil Config: %v", err)
		}
		if len(opts) != 0 {
			t.Errorf("got %d options, want none", len(opts))
		}
	})
}
