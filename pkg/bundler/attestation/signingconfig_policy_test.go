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

// TestValidateSigningConfigIsPublicGood_NilIsAccepted pins the default path.
//
// "No config supplied" is not a departure from the public-good defaults, so it
// must not be treated as one.
func TestValidateSigningConfigIsPublicGood_NilIsAccepted(t *testing.T) {
	if err := ValidateSigningConfigIsPublicGood(nil); err != nil {
		t.Errorf("nil config rejected: %v", err)
	}
}
