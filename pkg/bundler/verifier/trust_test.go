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
		// Valid — contains NVIDIA/aicr
		{TrustedRepositoryPattern, false},
		{`https://github.com/NVIDIA/aicr/.github/workflows/.*`, false},
		{`https://github.com/NVIDIA/aicr/.github/workflows/build-attested\.yaml@.*`, false},

		// Invalid — missing NVIDIA/aicr
		{`https://github.com/attacker/aicr/.*`, true},
		{`https://github.com/NVIDIA/other-repo/.*`, true},
		{`.*`, true},
		{"", true},
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
