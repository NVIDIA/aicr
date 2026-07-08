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

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseBundleCmdOptions_RekorV2Default covers the Rekor v2 default and its
// opt-outs for `bundle --attest` keyless signing: with no signing flags the
// command signs to Rekor v2 (useTUFSigningConfig), and --rekor-url,
// --signing-config, or --signing-key each opt out. See #1650.
func TestParseBundleCmdOptions_RekorV2Default(t *testing.T) {
	tmp := t.TempDir()
	recipePath := filepath.Join(tmp, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("kind: Recipe\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}
	scPath := filepath.Join(tmp, "sc.json")
	base := []string{"--recipe", recipePath, "--output", filepath.Join(tmp, "out")}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }

	t.Run("no signing flags default to Rekor v2", func(t *testing.T) {
		opts := captureBundleOpts(t, base)
		if !opts.useTUFSigningConfig {
			t.Error("useTUFSigningConfig = false, want true (v2 default) with no signing flags")
		}
		if opts.rekorURL != "" || opts.signingConfigPath != "" {
			t.Errorf("want empty rekorURL/signingConfigPath, got %q/%q", opts.rekorURL, opts.signingConfigPath)
		}
	})

	t.Run("--rekor-url opts out to Rekor v1", func(t *testing.T) {
		const u = "https://rekor.internal.example.com"
		opts := captureBundleOpts(t, with("--rekor-url", u))
		if opts.useTUFSigningConfig {
			t.Error("useTUFSigningConfig = true, want false when --rekor-url is set")
		}
		if opts.rekorURL != u {
			t.Errorf("rekorURL = %q, want %q", opts.rekorURL, u)
		}
	})

	t.Run("--signing-config opts out of the v2 default", func(t *testing.T) {
		opts := captureBundleOpts(t, with("--signing-config", scPath))
		if opts.useTUFSigningConfig {
			t.Error("useTUFSigningConfig = true, want false when --signing-config is set")
		}
		if opts.signingConfigPath != scPath {
			t.Errorf("signingConfigPath = %q, want %q", opts.signingConfigPath, scPath)
		}
	})

	t.Run("--signing-key (KMS) still defaults to Rekor v2", func(t *testing.T) {
		opts := captureBundleOpts(t, with("--signing-key", "awskms://alias/k"))
		if !opts.useTUFSigningConfig {
			t.Error("useTUFSigningConfig = false, want true (KMS defaults to v2 like keyless)")
		}
	})

	t.Run("--signing-key with --signing-config is allowed (KMS custom v2)", func(t *testing.T) {
		opts := captureBundleOpts(t, with("--signing-key", "awskms://alias/k", "--signing-config", scPath))
		if opts.useTUFSigningConfig {
			t.Error("useTUFSigningConfig = true, want false when --signing-config is set")
		}
		if opts.signingConfigPath != scPath {
			t.Errorf("signingConfigPath = %q, want %q", opts.signingConfigPath, scPath)
		}
	})

	// The parsed bundleCmdOptions are only correct if they propagate into the
	// ResolveOptions actually handed to the attester. Assert that final mapping.
	t.Run("options propagate into ResolveOptions", func(t *testing.T) {
		ro := bundleOIDCResolveOptions(captureBundleOpts(t, base))
		if !ro.UseTUFSigningConfig {
			t.Error("ResolveOptions.UseTUFSigningConfig = false, want true (v2 default)")
		}

		ro = bundleOIDCResolveOptions(captureBundleOpts(t, with("--rekor-url", "https://rekor.example.com")))
		if ro.UseTUFSigningConfig || ro.RekorURL != "https://rekor.example.com" {
			t.Errorf("with --rekor-url: UseTUFSigningConfig=%v RekorURL=%q", ro.UseTUFSigningConfig, ro.RekorURL)
		}

		ro = bundleOIDCResolveOptions(captureBundleOpts(t, with("--signing-config", scPath)))
		if ro.UseTUFSigningConfig || ro.SigningConfigPath != scPath {
			t.Errorf("with --signing-config: UseTUFSigningConfig=%v SigningConfigPath=%q", ro.UseTUFSigningConfig, ro.SigningConfigPath)
		}
	})

	t.Run("--rekor-url and --signing-config are mutually exclusive", func(t *testing.T) {
		_, err := tryCaptureBundleOpts(t, with("--rekor-url", "https://rekor.example.com", "--signing-config", scPath))
		if err == nil {
			t.Fatal("want error for --rekor-url + --signing-config, got nil")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("error should mention mutual exclusivity, got: %v", err)
		}
	})
}
