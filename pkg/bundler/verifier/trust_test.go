// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package verifier

import "testing"

func TestTrustLevel_String(t *testing.T) {
	tests := []struct {
		level TrustLevel
		want  string
	}{
		{TrustUnknown, "unknown"},
		{TrustUnverified, "unverified"},
		{TrustAttested, "attested"},
		{TrustVerified, "verified"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.level.String(); got != tt.want {
				t.Errorf("TrustLevel(%s).String() = %q, want %q", tt.level, got, tt.want)
			}
		})
	}
}

func TestTrustLevel_MeetsMinimum(t *testing.T) {
	tests := []struct {
		name    string
		level   TrustLevel
		minimum TrustLevel
		want    bool
	}{
		{"verified meets verified", TrustVerified, TrustVerified, true},
		{"verified meets attested", TrustVerified, TrustAttested, true},
		{"attested does not meet verified", TrustAttested, TrustVerified, false},
		{"unverified meets unverified", TrustUnverified, TrustUnverified, true},
		{"unknown does not meet unverified", TrustUnknown, TrustUnverified, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.level.MeetsMinimum(tt.minimum); got != tt.want {
				t.Errorf("%s.MeetsMinimum(%s) = %v, want %v", tt.level, tt.minimum, got, tt.want)
			}
		})
	}
}

func TestValidateIdentityPattern(t *testing.T) {
	tests := []struct {
		pattern string
		wantErr bool
	}{
		// Valid — contains ://github.com/NVIDIA/aicr/
		{TrustedRepositoryPattern, false},
		{`https://github.com/NVIDIA/aicr/.github/workflows/.*`, false},
		{`https://github.com/NVIDIA/aicr/.github/workflows/build-attested\.yaml@.*`, false},

		// Invalid — missing ://github.com/NVIDIA/aicr/
		{`https://github.com/attacker/aicr/.*`, true},
		{`https://github.com/NVIDIA/other-repo/.*`, true},
		{`.*`, true},
		{"", true},

		// Bypass attempts
		{`https://github.com/attacker/NVIDIA/aicr/.*`, true},           // prefix under wrong owner
		{`NVIDIA/aicr`, true},                                          // missing scheme and domain
		{`NVIDIA/aicr/`, true},                                         // missing scheme and domain
		{`github.com/NVIDIA/aicr/.*`, true},                            // missing scheme ://
		{`https://evil.com/github.com/NVIDIA/aicr/.*`, true},           // github.com as path, not domain
		{`https://evil.com/redirect?to=github.com/NVIDIA/aicr/`, true}, // github.com in query string
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			err := ValidateIdentityPattern(tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateIdentityPattern(%q) error = %v, wantErr %v", tt.pattern, err, tt.wantErr)
			}
		})
	}
}

func TestParseTrustLevel(t *testing.T) {
	tests := []struct {
		input   string
		want    TrustLevel
		wantErr bool
	}{
		{"unknown", TrustUnknown, false},
		{"unverified", TrustUnverified, false},
		{"attested", TrustAttested, false},
		{"verified", TrustVerified, false},
		{"VERIFIED", TrustVerified, false},
		{"invalid", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseTrustLevel(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseTrustLevel(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseTrustLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCheckPolicy_ToolVersionConstraint(t *testing.T) {
	tests := []struct {
		name        string
		toolVersion string
		constraint  string
		wantFail    bool
	}{
		// Bare version defaults to >= semantics
		{"bare version equal", "0.8.0", "0.8.0", false},
		{"bare version with v prefix in constraint", "0.8.0", "v0.8.0", false},
		{"bare version with v prefix in result", "v0.8.0", "0.8.0", false},
		{"bare version newer than required", "0.9.0", "0.8.0", false},
		{"bare version older than required", "0.7.0", "0.8.0", true},

		// Explicit >= operator
		{">= equal", "0.8.0", ">= 0.8.0", false},
		{">= newer", "0.9.0", ">= 0.8.0", false},
		{">= older", "0.7.0", ">= 0.8.0", true},
		{">= with v prefix", "0.8.0", ">= v0.8.0", false},

		// Explicit > operator
		{"> strictly newer", "0.8.1", "> 0.8.0", false},
		{"> equal fails", "0.8.0", "> 0.8.0", true},
		{"> older fails", "0.7.0", "> 0.8.0", true},

		// Explicit == operator
		{"== exact match", "0.8.0", "== 0.8.0", false},
		{"== with v in result", "v0.8.0", "== 0.8.0", false},
		{"== mismatch", "0.9.0", "== 0.8.0", true},

		// Explicit < operator
		{"< older passes", "0.7.0", "< 0.8.0", false},
		{"< equal fails", "0.8.0", "< 0.8.0", true},

		// Explicit <= operator
		{"<= equal passes", "0.8.0", "<= 0.8.0", false},
		{"<= older passes", "0.7.0", "<= 0.8.0", false},
		{"<= newer fails", "0.9.0", "<= 0.8.0", true},

		// Explicit != operator
		{"!= different passes", "0.9.0", "!= 0.8.0", false},
		{"!= same fails", "0.8.0", "!= 0.8.0", true},

		// Empty tool version in result (unattested bundle)
		{"empty tool version fails", "", "0.8.0", true},

		// Empty constraint (no check)
		{"empty constraint skips check", "0.8.0", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &VerifyResult{
				TrustLevel:      TrustVerified,
				ChecksumsPassed: true,
				BundleAttested:  true,
				ToolVersion:     tt.toolVersion,
			}
			policy := Policy{VersionConstraint: tt.constraint}
			failure, err := result.CheckPolicy(policy)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (failure != "") != tt.wantFail {
				t.Errorf("CheckPolicy() failure = %q, wantFail %v", failure, tt.wantFail)
			}
		})
	}
}

func TestMaxAchievableTrustLevel(t *testing.T) {
	tests := []struct {
		name   string
		result *VerifyResult
		want   TrustLevel
	}{
		{
			"nil result",
			nil,
			TrustUnknown,
		},
		{
			"checksums failed",
			&VerifyResult{ChecksumsPassed: false},
			TrustUnknown,
		},
		{
			"checksums only (no --attest flag)",
			&VerifyResult{ChecksumsPassed: true, BundleAttested: false},
			TrustUnverified,
		},
		{
			"attested with external data",
			&VerifyResult{ChecksumsPassed: true, BundleAttested: true, HasExternalData: true},
			TrustAttested,
		},
		{
			"full chain no external data",
			&VerifyResult{ChecksumsPassed: true, BundleAttested: true, HasExternalData: false},
			TrustVerified,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.result.MaxAchievableTrustLevel()
			if got != tt.want {
				t.Errorf("MaxAchievableTrustLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}
