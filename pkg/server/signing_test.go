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

package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// TestLoadBinaryAttestation exercises the startup fail-fast loader with an
// injected verifier seam (the real verify pins NVIDIA-CI identity + the running
// binary's digest, unsatisfiable in a `go test` executable).
func TestLoadBinaryAttestation(t *testing.T) {
	t.Run("disabled is a no-op", func(t *testing.T) {
		cfg := &signingConfig{enabled: false}
		called := false
		err := cfg.loadBinaryAttestation(context.Background(), func(context.Context) ([]byte, error) {
			called = true
			return []byte("unexpected"), nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if called {
			t.Error("verifier was called while signing disabled")
		}
		if cfg.binaryAttestation != nil {
			t.Errorf("binaryAttestation = %q, want nil", cfg.binaryAttestation)
		}
	})

	t.Run("enabled caches verified bytes", func(t *testing.T) {
		cfg := &signingConfig{enabled: true}
		fixture := []byte("fixture")
		err := cfg.loadBinaryAttestation(context.Background(), func(context.Context) ([]byte, error) {
			return fixture, nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if string(cfg.binaryAttestation) != "fixture" {
			t.Errorf("binaryAttestation = %q, want %q", cfg.binaryAttestation, fixture)
		}
	})

	t.Run("enabled fails fast when verify errors", func(t *testing.T) {
		cfg := &signingConfig{enabled: true}
		verifyErr := aicrerrors.New(aicrerrors.ErrCodeNotFound, "attestation absent")
		err := cfg.loadBinaryAttestation(context.Background(), func(context.Context) ([]byte, error) {
			return nil, verifyErr
		})
		if err == nil {
			t.Fatal("err = nil, want fail-fast error")
		}
		if cfg.binaryAttestation != nil {
			t.Errorf("binaryAttestation = %q, want nil on failure", cfg.binaryAttestation)
		}
	})
}

// TestResolveBinaryAttestationPath verifies the override/discovery branch:
// AICR_BINARY_ATTESTATION_FILE wins when set, otherwise the path falls back to
// the conventional file next to the running executable via FindBinaryAttestation.
func TestResolveBinaryAttestationPath(t *testing.T) {
	// Build a temp binPath with a sibling attestation so the fallback discovery
	// (suffix "-attestation.sigstore.json" next to the binary) succeeds.
	dir := t.TempDir()
	binPath := dir + "/aicrd"
	sibling := binPath + attestation.AttestationFileSuffix
	if err := os.WriteFile(binPath, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("both env unset falls back to FindBinaryAttestation", func(t *testing.T) {
		t.Setenv(defaults.EnvBinaryAttestationFile, "")
		t.Setenv(defaults.EnvKoDataPath, "")
		want, err := attestation.FindBinaryAttestation(binPath)
		if err != nil {
			t.Fatalf("FindBinaryAttestation setup = %v, want nil", err)
		}
		got, err := resolveBinaryAttestationPath(binPath)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("path = %q, want %q (sibling of binary)", got, want)
		}
	})

	t.Run("explicit file env overrides regardless of binPath", func(t *testing.T) {
		t.Setenv(defaults.EnvKoDataPath, "")
		t.Setenv(defaults.EnvBinaryAttestationFile, "/custom/att.json")
		got, err := resolveBinaryAttestationPath(binPath)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != "/custom/att.json" {
			t.Errorf("path = %q, want /custom/att.json", got)
		}
	})

	t.Run("explicit file env wins over KO_DATA_PATH", func(t *testing.T) {
		t.Setenv(defaults.EnvKoDataPath, "/ko/data")
		t.Setenv(defaults.EnvBinaryAttestationFile, "/custom/att.json")
		got, err := resolveBinaryAttestationPath(binPath)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != "/custom/att.json" {
			t.Errorf("path = %q, want /custom/att.json (explicit override wins)", got)
		}
	})

	t.Run("KO_DATA_PATH resolves per-architecture attestation", func(t *testing.T) {
		t.Setenv(defaults.EnvBinaryAttestationFile, "")
		t.Setenv(defaults.EnvKoDataPath, "/ko/data")
		name := fmt.Sprintf(defaults.BinaryAttestationKoDataNameFormat, runtime.GOARCH)
		want := filepath.Join("/ko/data", name)
		got, err := resolveBinaryAttestationPath(binPath)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	})
}

// TestResolveBinaryAttestationIdentityPattern verifies the
// AICR_BINARY_ATTESTATION_IDENTITY_REGEXP override: unset falls back to the
// release-workflow default; a value still pinned to the NVIDIA repo is accepted;
// a value pointing elsewhere is rejected (fails startup fast).
func TestResolveBinaryAttestationIdentityPattern(t *testing.T) {
	tests := []struct {
		name    string
		env     string // "" means unset (uses default)
		setEnv  bool
		want    string
		wantErr bool
	}{
		{
			name:   "unset returns release-workflow default",
			setEnv: false,
			want:   verifier.TrustedRepositoryPattern,
		},
		{
			name:   "valid override pinned to NVIDIA repo is returned",
			setEnv: true,
			env:    `^https://github\.com/NVIDIA/aicr/\.github/workflows/server-kms-e2e\.yaml@.*`,
			want:   `^https://github\.com/NVIDIA/aicr/\.github/workflows/server-kms-e2e\.yaml@.*`,
		},
		{
			name:    "override not pinned to NVIDIA repo is rejected",
			setEnv:  true,
			env:     `^https://github\.com/evil/repo/.*`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(defaults.EnvBinaryAttestationIdentityRegexp, tt.env)
			} else {
				t.Setenv(defaults.EnvBinaryAttestationIdentityRegexp, "")
			}
			got, err := resolveBinaryAttestationIdentityPattern()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("pattern = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSigningConfig(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		wantEnabled bool
		wantKeyless bool
		wantErr     bool
	}{
		{"none configured", nil, false, false, false},
		{"valid KMS", map[string]string{defaults.EnvSigningKey: "awskms:///alias/aicr"}, true, false, false},
		{"malformed KMS scheme", map[string]string{defaults.EnvSigningKey: "vault:/x"}, false, false, true},
		{"valid keyless (file)", map[string]string{
			defaults.EnvFulcioURL:         "https://fulcio.internal",
			defaults.EnvIdentityTokenFile: "/var/run/sigstore/token",
		}, true, true, false},
		{"valid keyless (ambient)", map[string]string{
			defaults.EnvFulcioURL:                        "https://fulcio.internal",
			defaults.EnvGitHubActionsIDTokenRequestURL:   "https://token.actions.example/req",
			defaults.EnvGitHubActionsIDTokenRequestToken: "ambient-tok",
		}, true, true, false},
		{"keyless missing token source", map[string]string{
			defaults.EnvFulcioURL: "https://fulcio.internal",
		}, false, false, true},
		{"both modes ambiguous", map[string]string{
			defaults.EnvSigningKey: "awskms:///alias/aicr",
			defaults.EnvFulcioURL:  "https://fulcio.internal",
		}, false, false, true},
		{"KMS with invalid tlog upload", map[string]string{
			defaults.EnvSigningKey: "awskms:///alias/aicr",
			defaults.EnvTLogUpload: "garbage",
		}, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ensure GHA ambient env doesn't leak into keyless cases.
			t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "")
			t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := parseSigningConfig()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if cfg.enabled != tt.wantEnabled {
				t.Errorf("enabled = %v, want %v", cfg.enabled, tt.wantEnabled)
			}
			if cfg.keyless != tt.wantKeyless {
				t.Errorf("keyless = %v, want %v", cfg.keyless, tt.wantKeyless)
			}
		})
	}
}

func TestSigningConfigResolveOptions_KeylessReadsTokenFresh(t *testing.T) {
	dir := t.TempDir()
	tokenPath := dir + "/token"
	if err := os.WriteFile(tokenPath, []byte("tok-v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "")
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "")
	t.Setenv(defaults.EnvFulcioURL, "https://fulcio.internal")
	t.Setenv(defaults.EnvIdentityTokenFile, tokenPath)
	cfg, err := parseSigningConfig()
	if err != nil {
		t.Fatal(err)
	}
	o1, err := cfg.resolveOptions()
	if err != nil || o1.IdentityToken != "tok-v1" {
		t.Fatalf("o1 token = %q err %v", o1.IdentityToken, err)
	}
	if err := os.WriteFile(tokenPath, []byte("tok-v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	o2, _ := cfg.resolveOptions()
	if o2.IdentityToken != "tok-v2" {
		t.Errorf("token not re-read: got %q, want tok-v2", o2.IdentityToken)
	}
	if !o2.Attest {
		t.Error("Attest should be true")
	}
}

func TestSigningConfigResolveOptions_KMS(t *testing.T) {
	t.Setenv(defaults.EnvSigningKey, "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k")
	cfg, err := parseSigningConfig()
	if err != nil {
		t.Fatal(err)
	}
	o, err := cfg.resolveOptions()
	if err != nil {
		t.Fatal(err)
	}
	if !o.Attest || o.SigningKey != "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k" {
		t.Errorf("resolve options = %+v", o)
	}
}

// TestSigningConfigResolveOptions_KeylessAmbient verifies that a keyless config
// backed by the GitHub Actions ambient OIDC endpoint (no token file) surfaces
// the ambient URL/token and leaves IdentityToken empty.
func TestSigningConfigResolveOptions_KeylessAmbient(t *testing.T) {
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "https://token.actions.example/req")
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "ambient-tok")
	t.Setenv(defaults.EnvFulcioURL, "https://fulcio.internal")
	cfg, err := parseSigningConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.enabled || !cfg.keyless {
		t.Fatalf("enabled=%v keyless=%v, want both true", cfg.enabled, cfg.keyless)
	}
	o, err := cfg.resolveOptions()
	if err != nil {
		t.Fatal(err)
	}
	if o.AmbientURL != "https://token.actions.example/req" || o.AmbientToken != "ambient-tok" {
		t.Errorf("ambient = %q/%q, want request URL/token", o.AmbientURL, o.AmbientToken)
	}
	if o.IdentityToken != "" {
		t.Errorf("IdentityToken = %q, want empty (ambient source)", o.IdentityToken)
	}
	if o.FulcioURL != "https://fulcio.internal" || !o.Attest {
		t.Errorf("resolve options = %+v", o)
	}
}

// TestSigningConfigResolveOptions_TLogUpload verifies the AICR_TLOG_UPLOAD
// toggle maps to DisableTLogUpload for KMS (Mode A) signing.
func TestSigningConfigResolveOptions_TLogUpload(t *testing.T) {
	tests := []struct {
		name        string
		tlog        string // "" means unset
		wantDisable bool
	}{
		{"default unset uploads", "", false},
		{"explicit false disables", "false", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "")
			t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "")
			t.Setenv(defaults.EnvSigningKey, "awskms:///alias/aicr")
			if tt.tlog != "" {
				t.Setenv(defaults.EnvTLogUpload, tt.tlog)
			}
			cfg, err := parseSigningConfig()
			if err != nil {
				t.Fatal(err)
			}
			o, err := cfg.resolveOptions()
			if err != nil {
				t.Fatal(err)
			}
			if o.DisableTLogUpload != tt.wantDisable {
				t.Errorf("DisableTLogUpload = %v, want %v", o.DisableTLogUpload, tt.wantDisable)
			}
		})
	}
}

// TestParseSigningConfig_InvalidTLogUpload asserts a non-boolean
// AICR_TLOG_UPLOAD fails startup fast rather than silently defaulting.
func TestParseSigningConfig_InvalidTLogUpload(t *testing.T) {
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "")
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "")
	t.Setenv(defaults.EnvSigningKey, "awskms:///alias/aicr")
	t.Setenv(defaults.EnvTLogUpload, "garbage")
	if _, err := parseSigningConfig(); err == nil {
		t.Fatal("expected error for non-boolean AICR_TLOG_UPLOAD, got nil")
	}
}

// TestParseSigningConfig_InvalidTLogUploadIgnoredWhenUnconfigured asserts a
// malformed AICR_TLOG_UPLOAD does NOT fail startup when no signing mode is
// configured (the toggle is KMS-only and has no effect otherwise).
func TestParseSigningConfig_InvalidTLogUploadIgnoredWhenUnconfigured(t *testing.T) {
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "")
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "")
	t.Setenv(defaults.EnvTLogUpload, "garbage")
	cfg, err := parseSigningConfig()
	if err != nil {
		t.Fatalf("unconfigured server must ignore AICR_TLOG_UPLOAD, got err %v", err)
	}
	if cfg.enabled {
		t.Error("expected signing disabled when no mode configured")
	}
}

// TestSigningConfigResolveOptions_MissingTokenFile verifies resolveOptions
// surfaces the read error when the keyless identity-token file is absent.
func TestSigningConfigResolveOptions_MissingTokenFile(t *testing.T) {
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestURL, "")
	t.Setenv(defaults.EnvGitHubActionsIDTokenRequestToken, "")
	t.Setenv(defaults.EnvFulcioURL, "https://fulcio.internal")
	t.Setenv(defaults.EnvIdentityTokenFile, t.TempDir()+"/does-not-exist")
	cfg, err := parseSigningConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.resolveOptions(); err == nil {
		t.Fatal("expected error for missing identity token file, got nil")
	}
}
