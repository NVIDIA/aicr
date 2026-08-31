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

package attestation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The four settings SignCatalog already rejects were bypassable by putting the
// same endpoints in a signing config file: SigningConfigPath passed through
// unvalidated, so a catalog could sign successfully against a private Fulcio or
// Rekor and be unverifiable by its documented counterpart (#2227).

// TestValidateSigningConfigIsPublicGood_AcceptsShippedConfigs is the guard
// against breaking the release.
//
// The goreleaser hook signs with a public-good config on every tagged build, so
// a validator that rejects one of these does not fail a test — it fails the
// release. These are the configs the project actually ships with.
func TestValidateSigningConfigIsPublicGood_AcceptsShippedConfigs(t *testing.T) {
	entries, err := filepath.Glob("testdata/signing_config*.json")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no signing config fixtures found; this test would pass over nothing")
	}

	for _, path := range entries {
		t.Run(filepath.Base(path), func(t *testing.T) {
			sc, loadErr := LoadSigningConfigForValidation(path)
			if loadErr != nil {
				t.Fatalf("load: %v", loadErr)
			}
			if err := ValidateSigningConfigIsPublicGood(sc); err != nil {
				t.Errorf("public-good config rejected: %v\nthis would break the "+
					"release signing path, not just this test", err)
			}
		})
	}
}

// TestValidateSigningConfigIsPublicGood_RejectsPrivateEndpoints covers the
// case the guard exists for, in every service group.
func TestValidateSigningConfigIsPublicGood_RejectsPrivateEndpoints(t *testing.T) {
	base, err := os.ReadFile(filepath.Clean("testdata/signing_config_v2.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	tests := map[string]struct {
		from, to string
	}{
		"private certificate authority": {"https://fulcio.sigstore.dev", "https://fulcio.internal.corp"},
		"private transparency log":      {"rekor.sigstore.dev", "rekor.internal.corp"},
		"private OIDC provider":         {"oauth2.sigstore.dev", "oauth2.internal.corp"},

		// The domain check must match on a label boundary. A bare suffix test
		// would accept this, which is the whole point of checking.
		"lookalike domain": {"fulcio.sigstore.dev", "fulcio.evilsigstore.dev"},

		// A host-only check would accept these: the hostname is genuinely
		// public-good, but the URL is handed to sign.NewRekor /
		// sign.NewTimestampAuthority as-is, so signing traffic would go out in
		// the clear.
		"plaintext transparency log":      {"https://rekor.sigstore.dev", "http://rekor.sigstore.dev"},
		"plaintext timestamp authority":   {"https://timestamp.sigstore.dev", "http://timestamp.sigstore.dev"},
		"plaintext certificate authority": {"https://fulcio.sigstore.dev", "http://fulcio.sigstore.dev"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			mutated := strings.ReplaceAll(string(base), tt.from, tt.to)
			if mutated == string(base) {
				t.Fatalf("fixture does not contain %q; this case would pass "+
					"vacuously", tt.from)
			}

			path := filepath.Join(t.TempDir(), "config.json")
			if writeErr := os.WriteFile(path, []byte(mutated), 0o600); writeErr != nil {
				t.Fatalf("write: %v", writeErr)
			}

			sc, loadErr := LoadSigningConfigForValidation(path)
			if loadErr != nil {
				t.Fatalf("load: %v", loadErr)
			}
			if err := ValidateSigningConfigIsPublicGood(sc); err == nil {
				t.Errorf("accepted a config naming %q; a catalog signed against it "+
					"cannot be verified by VerifyCatalog", tt.to)
			}
		})
	}
}

// TestValidateSigningConfigIsPublicGood_AcceptsCaseVariantHosts covers the
// other direction from the rejection table: DNS is case-insensitive, so an
// uppercase spelling of a public-good host names the same service and must not
// be rejected. A case-sensitive comparison would fail a legitimate config.
func TestValidateSigningConfigIsPublicGood_AcceptsCaseVariantHosts(t *testing.T) {
	base, err := os.ReadFile(filepath.Clean("testdata/signing_config_v2.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	mutated := strings.ReplaceAll(string(base), "fulcio.sigstore.dev", "FULCIO.SIGSTORE.DEV")
	if mutated == string(base) {
		t.Fatal("fixture does not contain the host under test; this would pass vacuously")
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if writeErr := os.WriteFile(path, []byte(mutated), 0o600); writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}

	sc, err := LoadSigningConfigForValidation(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := ValidateSigningConfigIsPublicGood(sc); err != nil {
		t.Errorf("rejected an uppercase spelling of a public-good host: %v", err)
	}
}

// TestValidateSigningConfigIsPublicGood_NilIsAccepted pins the default path.
//
// "No config supplied" is not a departure from the public-good defaults, so it
// must not be treated as one.
func TestValidateSigningConfigIsPublicGood_NilIsAccepted(t *testing.T) {
	if err := ValidateSigningConfigIsPublicGood(nil); err != nil {
		t.Errorf("nil config rejected: %v", err)
	}
}

// TestTransparencyPrecedence_ValidatedConfigWinsOverPath is the regression test
// for the check-then-reload gap.
//
// Client.SignCatalog validates a signing config before signing with it. If the
// signing path re-read SigningConfigPath afterwards, it would sign against
// whatever the file held at that later moment -- the validated read and the used
// read would be different reads. SignOptions.SigningConfig closes that, and this
// pins the precedence: given BOTH a parsed config and a path, the parsed config
// must win.
//
// The path here points at a file that does not exist, so a policy built from the
// path could not succeed. Success therefore proves the parsed config was used.
func TestTransparencyPrecedence_ValidatedConfigWinsOverPath(t *testing.T) {
	sc, err := LoadSigningConfigForValidation("testdata/signing_config_v2.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	policy, err := transparencyForOptions(context.Background(), SignOptions{
		SigningConfig:     sc,
		SigningConfigPath: filepath.Join(t.TempDir(), "does-not-exist.json"),
	})
	if err != nil {
		t.Fatalf("transparencyForOptions fell back to the path instead of using "+
			"the validated config: %v", err)
	}
	if policy == nil {
		t.Fatal("no transparency policy returned")
	}
}

// TestTransparencyPrecedence_PathStillWorks guards the other direction: the
// precedence change must not break the release path, which supplies only a path.
func TestTransparencyPrecedence_PathStillWorks(t *testing.T) {
	policy, err := transparencyForOptions(context.Background(), SignOptions{
		SigningConfigPath: "testdata/signing_config_v2.json",
	})
	if err != nil {
		t.Fatalf("path-only signing config rejected: %v", err)
	}
	if policy == nil {
		t.Fatal("no transparency policy returned")
	}
}

// TestKeylessAttesterCarriesValidatedSigningConfig covers the resolve -> sign
// hop, which is the one place the validated config can be silently dropped.
//
// Client.SignCatalog sets ResolveOptions.SigningConfig, the resolver builds a
// KeylessAttester from those options, and the attester rebuilds a ResolveOptions
// to sign with. A field missed at either step reverts to re-reading
// SigningConfigPath with no compile error and no test failure anywhere else --
// signing would just quietly go back to using bytes nobody validated.
func TestKeylessAttesterCarriesValidatedSigningConfig(t *testing.T) {
	sc, err := LoadSigningConfigForValidation("testdata/signing_config_v2.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	attester := NewKeylessAttesterFromOptions("token", ResolveOptions{
		SigningConfigPath: "testdata/signing_config_v2.json",
		SigningConfig:     sc,
	})

	// Step 1: options -> attester.
	if attester.signingConfig != sc {
		t.Fatal("NewKeylessAttesterFromOptions dropped SigningConfig; signing would " +
			"re-read SigningConfigPath instead of using the validated config")
	}

	// Step 2: attester -> ResolveOptions -> SignOptions, the exact chain Attest
	// runs.
	signOpts := SignOptionsFromResolve("token", attester.resolveOptions())
	if signOpts.SigningConfig != sc {
		t.Error("the validated SigningConfig did not survive the resolve -> sign " +
			"projection; transparencyForOptions would fall back to the path")
	}
}
