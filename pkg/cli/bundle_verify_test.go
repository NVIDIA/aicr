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

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
)

func TestBundleVerifyCmd_HasExpectedFlags(t *testing.T) {
	cmd := bundleVerifyCmd()

	if cmd.Name != "verify" {
		t.Errorf("Name = %q, want %q", cmd.Name, "verify")
	}

	expectedFlags := []string{"min-trust-level", "require-creator", "cli-version-constraint", "certificate-identity-regexp", "format"}
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
