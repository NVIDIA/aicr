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
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/urfave/cli/v3"
)

func TestBundleVerifyCmd_HasExpectedFlags(t *testing.T) {
	cmd := bundleVerifyCmd()

	if cmd.Name != "verify" {
		t.Errorf("Name = %q, want %q", cmd.Name, "verify")
	}

	expectedFlags := []string{"min-trust-level", "require-creator", "cli-version-constraint", "certificate-identity-regexp", "key", "trust-root", "format"}
	for _, name := range expectedFlags {
		found := false
		for _, f := range cmd.Flags {
			if f.Names()[0] == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing expected flag: --%s", name)
		}
	}
}

func TestBundleVerifyCmd_MinTrustLevelDefault(t *testing.T) {
	cmd := bundleVerifyCmd()

	for _, f := range cmd.Flags {
		if f.Names()[0] == "min-trust-level" {
			// Check it's a completable StringFlag with default "max"
			cf, ok := f.(*completableStringFlag)
			if !ok {
				t.Fatal("min-trust-level should be a completableStringFlag")
			}
			sf := cf.StringFlag
			if sf.Value != "max" {
				t.Errorf("min-trust-level default = %q, want %q", sf.Value, "max")
			}
			return
		}
	}
	t.Error("min-trust-level flag not found")
}

// writeVerifiableKeyFixture builds a bundle directory that passes the checksum
// step and carries a bundle-attestation file, so verification reaches the
// bundle-attestation step (where --key takes effect) instead of stopping at
// missing checksums. The attestation bytes only need to exist: with --key, key
// resolution runs before they are parsed.
func writeVerifiableKeyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesPath, []byte("replicas: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// GenerateChecksums takes absolute file paths and records them relative to dir.
	if err := checksum.GenerateChecksums(context.Background(), dir, []string{valuesPath}); err != nil {
		t.Fatalf("GenerateChecksums: %v", err)
	}
	attestPath := filepath.Join(dir, attestation.BundleAttestationFile)
	if err := os.MkdirAll(filepath.Dir(attestPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attestPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBundleVerifyCmd_KeyFlagPlumbing runs the real verify command with --key
// against a fixture that passes checksums and has a bundle attestation, so
// verification reaches the bundle-attestation step where --key takes effect. A
// nonexistent PEM path drives the key-verification branch to a public-key
// resolution error, which the keyless path would not produce (it would report a
// sigstore-bundle parse error). Asserting that error proves verifyOpts.Key is
// plumbed and routed to the key branch, and that --key coexists with
// --certificate-identity-regexp (no mutual exclusivity).
func TestBundleVerifyCmd_KeyFlagPlumbing(t *testing.T) {
	tests := []struct {
		name string
		args func(dir string) []string
	}{
		{
			name: "key alone reaches key verification",
			args: func(dir string) []string { return []string{"verify", dir, "--key", "/nonexistent/key.pem"} },
		},
		{
			name: "key coexists with certificate-identity-regexp",
			args: func(dir string) []string {
				return []string{"verify", dir, "--key", "/nonexistent/key.pem", "--certificate-identity-regexp", "https://github.com/NVIDIA/aicr/.+"}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeVerifiableKeyFixture(t)
			cmd := bundleVerifyCmd()
			var buf bytes.Buffer
			cmd.Writer = &buf

			err := cmd.Run(context.Background(), tt.args(dir))

			// Must not be a flag-parse/usage error: the flags were accepted and
			// the action ran (proves --key coexists with the identity regexp).
			if err != nil {
				for _, bad := range []string{"flag provided but not defined", "no such flag", "flag needs an argument"} {
					if strings.Contains(err.Error(), bad) {
						t.Fatalf("got flag-level error %q for args %v", err.Error(), tt.args(dir))
					}
				}
			}

			// Prove the key-verification branch ran: the public-key resolution
			// error surfaces in the output. The keyless path would instead
			// report a sigstore-bundle parse failure.
			combined := buf.String()
			if err != nil {
				combined += " " + err.Error()
			}
			if !strings.Contains(combined, "public key") {
				t.Errorf("output does not show the key-verification path was taken; got:\n%s", combined)
			}
		})
	}
}

// writeParseableTrustRootFixture builds a bundle directory that passes the
// checksum step and carries a bundle attestation parseable as a sigstore v0.3
// bundle (public-key material + DSSE envelope, no real signature). Because it
// parses, verification reaches the trust-root resolution step rather than
// stopping at bundle parsing, so pointing --trust-root at a missing
// trusted_root.json makes the union loader fail there. That isolates and proves
// the --trust-root plumbing into the bundle-attestation step; it does not assert
// how the default public-good path treats this unsigned fixture (that path also
// fails here, just at a different point).
func writeParseableTrustRootFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesPath, []byte("replicas: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checksum.GenerateChecksums(context.Background(), dir, []string{valuesPath}); err != nil {
		t.Fatalf("GenerateChecksums: %v", err)
	}
	payload := base64.StdEncoding.EncodeToString([]byte(`{"_type":"https://in-toto.io/Statement/v1"}`))
	sig := base64.StdEncoding.EncodeToString([]byte("not-a-real-signature"))
	pub := base64.StdEncoding.EncodeToString([]byte("fake-public-key-bytes"))
	content := fmt.Sprintf(`{
"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json",
"verificationMaterial":{"publicKey":{"hint":"%s"}},
"dsseEnvelope":{"payload":"%s","payloadType":"application/vnd.in-toto+json","signatures":[{"sig":"%s"}]}
}`, pub, payload, sig)
	attestPath := filepath.Join(dir, attestation.BundleAttestationFile)
	if err := os.MkdirAll(filepath.Dir(attestPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attestPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestBundleVerifyCmd_TrustRootFlagPlumbing runs the real verify command with
// --trust-root against a fixture that passes checksums and has a parseable
// bundle attestation. A nonexistent trusted_root.json path drives the union
// trust-root loader to a load error which Verify returns up front as a coded
// ErrCodeInvalidRequest naming the "trust root file". Asserting that
// loader-specific detail proves verifyOpts.TrustRoot is plumbed and reaches
// trust-root resolution (not a generic attestation failure), and that
// --trust-root coexists with --key and --certificate-identity-regexp (no mutual
// exclusivity, no flag-parse error).
func TestBundleVerifyCmd_TrustRootFlagPlumbing(t *testing.T) {
	tests := []struct {
		name string
		args func(dir string) []string
	}{
		{
			name: "trust-root alone reaches trust-root resolution",
			args: func(dir string) []string {
				return []string{"verify", dir, "--trust-root", "/no/such/trusted_root.json"}
			},
		},
		{
			name: "trust-root coexists with key and certificate-identity-regexp",
			args: func(dir string) []string {
				return []string{
					"verify", dir,
					"--trust-root", "/no/such/trusted_root.json",
					"--key", "/nonexistent/key.pem",
					"--certificate-identity-regexp", "https://github.com/NVIDIA/aicr/.+",
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeParseableTrustRootFixture(t)
			cmd := bundleVerifyCmd()
			var buf bytes.Buffer
			cmd.Writer = &buf

			err := cmd.Run(context.Background(), tt.args(dir))

			// Must not be a flag-parse/usage error: the flags were accepted and
			// the action ran (proves --trust-root coexists with the other flags).
			if err != nil {
				for _, bad := range []string{"flag provided but not defined", "no such flag", "flag needs an argument"} {
					if strings.Contains(err.Error(), bad) {
						t.Fatalf("got flag-level error %q for args %v", err.Error(), tt.args(dir))
					}
				}
			}

			// Prove the trust-root path was consulted: with a missing
			// trusted_root.json the union loader fails fast and Verify returns
			// the loader's "trust root file" error (ErrCodeInvalidRequest)
			// before any attestation work. The loader-specific detail (not the
			// generic verification banner) proves --trust-root reached
			// trust-root resolution.
			combined := buf.String()
			if err != nil {
				combined += " " + err.Error()
			}
			if !strings.Contains(combined, "trust root file") {
				t.Errorf("output does not show the trust-root loader failing; got:\n%s", combined)
			}
		})
	}
}

func TestBundleVerifyCmd_TrustRootFlagDefinition(t *testing.T) {
	cmd := bundleVerifyCmd()
	for _, f := range cmd.Flags {
		if f.Names()[0] != "trust-root" {
			continue
		}
		// --trust-root is a plain StringFlag (not completion-wrapped): there is
		// no natural completion source for a filesystem path.
		if _, ok := f.(*cli.StringFlag); !ok {
			t.Fatalf("--trust-root should be a plain *cli.StringFlag, got %T", f)
		}
		return
	}
	t.Fatal("--trust-root flag not found")
}

func TestBundleVerifyCmd_KeyFlagDefinition(t *testing.T) {
	cmd := bundleVerifyCmd()
	for _, f := range cmd.Flags {
		if f.Names()[0] != "key" {
			continue
		}
		// --key is a plain StringFlag (not completion-wrapped): there is no
		// natural completion source for a KMS URI or PEM path.
		if _, ok := f.(*cli.StringFlag); !ok {
			t.Fatalf("--key should be a plain *cli.StringFlag, got %T", f)
		}
		return
	}
	t.Fatal("--key flag not found")
}

func TestOutputText_Verdict(t *testing.T) {
	tests := []struct {
		name          string
		result        *verifier.VerifyResult
		policyFailure string
		wantContains  string
		wantAbsent    string
	}{
		{
			name: "clean bundle shows PASSED",
			result: &verifier.VerifyResult{
				ChecksumsPassed: true,
				ChecksumFiles:   12,
				TrustLevel:      verifier.TrustUnverified,
			},
			wantContains: "PASSED",
			wantAbsent:   "FAILED",
		},
		{
			name: "checksum mismatch shows FAILED",
			result: &verifier.VerifyResult{
				TrustLevel: verifier.TrustUnknown,
				Errors:     []string{"checksum mismatch: deploy.sh"},
			},
			wantContains: "FAILED",
			wantAbsent:   "PASSED",
		},
		{
			name: "policy failure shows FAILED",
			result: &verifier.VerifyResult{
				ChecksumsPassed: true,
				TrustLevel:      verifier.TrustUnverified,
			},
			policyFailure: "trust level unverified does not meet minimum attested",
			wantContains:  "FAILED",
			wantAbsent:    "PASSED",
		},
		{
			name: "verified bundle shows PASSED",
			result: &verifier.VerifyResult{
				ChecksumsPassed: true,
				ChecksumFiles:   12,
				BundleAttested:  true,
				BinaryAttested:  true,
				IdentityPinned:  true,
				TrustLevel:      verifier.TrustVerified,
			},
			wantContains: "PASSED",
			wantAbsent:   "FAILED",
		},
		{
			name: "trust reason displayed when set",
			result: &verifier.VerifyResult{
				ChecksumsPassed: true,
				ChecksumFiles:   5,
				BundleAttested:  true,
				HasExternalData: true,
				TrustLevel:      verifier.TrustAttested,
				TrustReason:     "external --data files included; verified requires only embedded recipe data",
			},
			wantContains: "external --data files included",
			wantAbsent:   "FAILED",
		},
		{
			name: "attestation verification error shows FAILED",
			result: &verifier.VerifyResult{
				ChecksumsPassed: true,
				TrustLevel:      verifier.TrustUnknown,
				Errors:          []string{"bundle attestation verification failed: certificate chain error"},
			},
			wantContains: "FAILED",
			wantAbsent:   "PASSED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			outputText(&buf, tt.result, tt.policyFailure)
			output := buf.String()

			if !strings.Contains(output, tt.wantContains) {
				t.Errorf("output missing %q:\n%s", tt.wantContains, output)
			}
			if tt.wantAbsent != "" && strings.Contains(output, tt.wantAbsent) {
				t.Errorf("output should not contain %q:\n%s", tt.wantAbsent, output)
			}
		})
	}
}
