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

package verifier

import (
	"encoding/base64"
	stderrors "errors"
	"testing"
)

func TestLooksLikeJSON(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{`{"a": 1}`, true},
		{`  {"a": 1}`, true},
		{`[1,2,3]`, true},
		{`hello`, false},
		{``, false},
	}
	for _, tt := range tests {
		if got := looksLikeJSON([]byte(tt.in)); got != tt.want {
			t.Errorf("looksLikeJSON(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsCertChainError(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"x509: certificate signed by unknown authority", true},
		{"failed to validate certificate chain", true},
		{"some other error", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isCertChainError(tt.in); got != tt.want {
			t.Errorf("isCertChainError(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestBuildIdentityMatcher(t *testing.T) {
	if _, err := buildIdentityMatcher(VerifyOptions{}); err != nil {
		t.Errorf("default matcher: %v", err)
	}
	if _, err := buildIdentityMatcher(VerifyOptions{
		ExpectedIssuer:         "https://token.actions.githubusercontent.com",
		ExpectedIdentityRegexp: `^https://github\.com/myorg/.*$`,
	}); err != nil {
		t.Errorf("pinned matcher: %v", err)
	}
}

func TestVerifySignature_AbsentFileIsUnsigned(t *testing.T) {
	bundleDir := buildTestBundle(t)
	mat := &MaterializedBundle{BundleDir: summaryDirOf(t, bundleDir)}
	if _, err := VerifySignature(t.Context(), mat, VerifyOptions{}); !stderrors.Is(err, ErrUnsignedBundle) {
		t.Fatalf("err = %v, want ErrUnsignedBundle", err)
	}
}

func TestVerifySignature_NilBundleErrors(t *testing.T) {
	if _, err := VerifySignature(t.Context(), nil, VerifyOptions{}); err == nil {
		t.Errorf("expected error for nil bundle")
	}
}

func TestSanitizeSigstoreError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "tsa threshold artifact",
			in:   "threshold not met for verified signed timestamps: 0 < 1; error: %!w(<nil>)",
			want: "threshold not met for verified signed timestamps: 0 < 1",
		},
		{
			name: "bare artifact",
			in:   "some error: %!w(<nil>)",
			want: "some error",
		},
		{
			name: "no artifact",
			in:   "ordinary error",
			want: "ordinary error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeSigstoreError(errPlain{msg: tt.in})
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// errPlain is a tiny error wrapper that returns msg verbatim from
// Error(). Used to feed exact strings to sanitizeSigstoreError.
type errPlain struct{ msg string }

func (e errPlain) Error() string { return e.msg }

// TestParseStatement covers the JSON parsing path that runs after DSSE
// decode — pure data-plumbing, no sigstore involvement.
func TestParseStatement(t *testing.T) {
	good := []byte(`{
  "subject": [{"digest": {"sha256": "abc123"}}],
  "predicateType": "https://aicr.nvidia.com/recipe-evidence/v1",
  "predicate": {"schemaVersion": "1.0.0", "aicrVersion": "v0.13.0"}
}`)
	hex, pred, err := parseStatement(good)
	if err != nil {
		t.Fatalf("parseStatement: %v", err)
	}
	if hex != "abc123" {
		t.Errorf("subject hex = %q, want abc123", hex)
	}
	if pred == nil || pred.SchemaVersion != "1.0.0" {
		t.Errorf("predicate parse missing or wrong; got %+v", pred)
	}
}

func TestParseStatement_Rejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no subject", `{"predicateType": "https://aicr.nvidia.com/recipe-evidence/v1", "predicate": {}}`},
		{"empty digest", `{"subject": [{"digest": {}}], "predicateType": "https://aicr.nvidia.com/recipe-evidence/v1", "predicate": {}}`},
		{"wrong predicateType", `{"subject": [{"digest": {"sha256": "abc"}}], "predicateType": "wrong", "predicate": {}}`},
		{"invalid JSON", `not json`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseStatement([]byte(tt.in)); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestDSSEPayload_RoundTrip(t *testing.T) {
	raw := []byte(`{"subject":[]}`)
	if !looksLikeJSON(raw) {
		t.Fatal("raw JSON should be detected")
	}
	enc := base64.StdEncoding.EncodeToString(raw)
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != string(raw) {
		t.Fatalf("got %q, want %q", dec, raw)
	}
}
