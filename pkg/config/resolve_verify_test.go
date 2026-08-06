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

package config_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/config"
)

func TestVerifyResolve_NilReceiver(t *testing.T) {
	var spec *config.VerifySpec

	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Resolve() returned nil; callers reach into fields")
	}
	if *got != (config.VerifyResolved{}) {
		t.Errorf("Resolve() = %+v, want zero value", *got)
	}
}

func TestVerifyResolve_EmptySpec(t *testing.T) {
	got, err := (&config.VerifySpec{}).Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}
	if *got != (config.VerifyResolved{}) {
		t.Errorf("Resolve() = %+v, want zero value", *got)
	}
}

func TestVerifyResolve_AllFieldsPopulated(t *testing.T) {
	spec := &config.VerifySpec{
		Policy: &config.VerifyPolicySpec{
			MinTrustLevel:        "attested",
			RequireCreator:       "ci@example.com",
			CLIVersionConstraint: ">= 0.16.0",
		},
		Trust: &config.VerifyTrustSpec{
			CertificateIdentityRegexp: "https://github.com/NVIDIA/aicr/.+",
			Key:                       "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
			TrustRoot:                 "./trusted_root.json",
		},
	}

	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil", err)
	}

	want := config.VerifyResolved{
		MinTrustLevel:             "attested",
		RequireCreator:            "ci@example.com",
		VersionConstraint:         ">= 0.16.0",
		CertificateIdentityRegexp: "https://github.com/NVIDIA/aicr/.+",
		Key:                       "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
		TrustRoot:                 "./trusted_root.json",
	}
	if *got != want {
		t.Errorf("Resolve() = %+v, want %+v", *got, want)
	}
}

// TestVerifyResolve_AcceptsMaxMetaValue locks in that "max" survives Resolve.
// It is the --min-trust-level default and a meta-value rather than a real
// level, so verifier.ParseTrustLevel rejects it and it must be special-cased.
func TestVerifyResolve_AcceptsMaxMetaValue(t *testing.T) {
	spec := &config.VerifySpec{
		Policy: &config.VerifyPolicySpec{MinTrustLevel: "max"},
	}

	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil for the \"max\" meta-value", err)
	}
	if got.MinTrustLevel != "max" {
		t.Errorf("MinTrustLevel = %q, want %q", got.MinTrustLevel, "max")
	}
}

// TestVerifyResolve_BareVersionConstraintAccepted covers the bare-version form
// documented on the flag, which ParseVersionConstraint normalizes to ">=".
func TestVerifyResolve_BareVersionConstraintAccepted(t *testing.T) {
	spec := &config.VerifySpec{
		Policy: &config.VerifyPolicySpec{CLIVersionConstraint: "0.16.0"},
	}

	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil for a bare version", err)
	}
	// The raw wire value is preserved; normalization happens at check time.
	if got.VersionConstraint != "0.16.0" {
		t.Errorf("VersionConstraint = %q, want %q", got.VersionConstraint, "0.16.0")
	}
}

// TestVerifyResolve_InvalidValues asserts every rejected value names its own
// spec path, so a typo in a committed config points at the offending field.
func TestVerifyResolve_InvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		spec    *config.VerifySpec
		wantSub string
	}{
		{
			name: "unknown trust level",
			spec: &config.VerifySpec{
				Policy: &config.VerifyPolicySpec{MinTrustLevel: "totally-bogus"},
			},
			wantSub: "spec.verify.policy.minTrustLevel",
		},
		{
			// The shared parser rejects an operator with no value. It does
			// NOT check that the value is version-shaped, so a malformed
			// version surfaces at evaluate time rather than at load time;
			// see TestVerifyResolve_VersionConstraintValueNotVersionChecked.
			name: "version constraint with operator but no version",
			spec: &config.VerifySpec{
				Policy: &config.VerifyPolicySpec{CLIVersionConstraint: ">="},
			},
			wantSub: "spec.verify.policy.cliVersionConstraint",
		},
		{
			// Contains the required anchor, so the substring check passes;
			// only compiling the pattern catches it. Without this, an
			// uncompilable regexp reaches the verifier and fails there
			// instead of at load time with its spec path.
			name: "identity regexp that is not a compilable regexp",
			spec: &config.VerifySpec{
				Trust: &config.VerifyTrustSpec{CertificateIdentityRegexp: "https://github.com/NVIDIA/aicr/["},
			},
			wantSub: "spec.verify.trust.certificateIdentityRegexp",
		},
		{
			name: "identity regexp missing the required NVIDIA/aicr anchor",
			spec: &config.VerifySpec{
				Trust: &config.VerifyTrustSpec{CertificateIdentityRegexp: "https://example.com/.+"},
			},
			wantSub: "spec.verify.trust.certificateIdentityRegexp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.spec.Resolve()
			if err == nil {
				t.Fatalf("Resolve() error = nil, want an error mentioning %q", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Resolve() error = %q, want it to mention %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestVerifyResolve_VersionConstraintValueNotVersionChecked pins a real limit
// of load-time validation: the shared parser splits off the operator but never
// checks the remainder is version-shaped, so a malformed version is accepted
// here and only fails when the constraint is evaluated against a real bundle.
//
// This is deliberate. The config layer validates exactly what the consuming
// parser validates rather than maintaining a parallel, stricter grammar that
// could reject expressions `aicr verify` would otherwise accept.
func TestVerifyResolve_VersionConstraintValueNotVersionChecked(t *testing.T) {
	spec := &config.VerifySpec{
		Policy: &config.VerifyPolicySpec{CLIVersionConstraint: ">= not-a-version"},
	}

	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v; load-time validation is operator-level only", err)
	}
	if got.VersionConstraint != ">= not-a-version" {
		t.Errorf("VersionConstraint = %q, want the value passed through verbatim", got.VersionConstraint)
	}
}

// TestVerifyResolve_KeyAndTrustRootUnvalidated documents the deliberate
// non-validation of these two: they are references whose resolution (KMS
// reachability, file contents) belongs to the verifier, which reports far
// better errors than a syntactic check here could.
func TestVerifyResolve_KeyAndTrustRootUnvalidated(t *testing.T) {
	spec := &config.VerifySpec{
		Trust: &config.VerifyTrustSpec{
			Key:       "not-a-real-scheme://whatever",
			TrustRoot: "/nonexistent/trusted_root.json",
		},
	}

	got, err := spec.Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil (references are not validated here)", err)
	}
	if got.Key != "not-a-real-scheme://whatever" || got.TrustRoot != "/nonexistent/trusted_root.json" {
		t.Errorf("Resolve() = %+v, want both references passed through verbatim", *got)
	}
}

// TestVerifyOnlyConfigIsValid proves spec.verify alone satisfies the
// at-least-one-section requirement, which is the whole point of a
// consumer-side config file that carries no producer settings.
func TestVerifyOnlyConfigIsValid(t *testing.T) {
	cfg := &config.AICRConfig{
		Kind:       config.Kind,
		APIVersion: config.APIVersion,
		Spec: config.Spec{
			Verify: &config.VerifySpec{
				Policy: &config.VerifyPolicySpec{MinTrustLevel: "verified"},
			},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil for a verify-only config", err)
	}
}

// TestVerifyConfigValidationRejectsBadValue proves Validate reaches into the
// verify section rather than only checking that it is present.
func TestVerifyConfigValidationRejectsBadValue(t *testing.T) {
	cfg := &config.AICRConfig{
		Kind:       config.Kind,
		APIVersion: config.APIVersion,
		Spec: config.Spec{
			Verify: &config.VerifySpec{
				Policy: &config.VerifyPolicySpec{MinTrustLevel: "totally-bogus"},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want rejection of an unknown trust level")
	}
	if !strings.Contains(err.Error(), "spec.verify.policy.minTrustLevel") {
		t.Errorf("Validate() error = %q, want it to name the offending field", err.Error())
	}
}

func TestVerificationAccessor(t *testing.T) {
	var nilCfg *config.AICRConfig
	if got := nilCfg.Verification(); got != nil {
		t.Errorf("Verification() on nil config = %+v, want nil", got)
	}

	if got := (&config.AICRConfig{}).Verification(); got != nil {
		t.Errorf("Verification() with unset section = %+v, want nil", got)
	}

	spec := &config.VerifySpec{Policy: &config.VerifyPolicySpec{RequireCreator: "ci@example.com"}}
	cfg := &config.AICRConfig{Spec: config.Spec{Verify: spec}}
	if got := cfg.Verification(); got != spec {
		t.Errorf("Verification() = %+v, want the populated section", got)
	}
}

// TestVerifyTrustRejectsIgnoreTlogField locks the flag-only design of
// --insecure-ignore-tlog into a named test. The guarantee that a committed
// config can never disable transparency-log verification rests on strict
// decoding rejecting the unknown field; a future schema addition that
// accidentally introduced it would otherwise silently weaken that promise.
func TestVerifyTrustRejectsIgnoreTlogField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	doc := "kind: AICRConfig\napiVersion: aicr.run/v1alpha2\n" +
		"spec:\n  verify:\n    trust:\n      ignoreTlog: true\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	_, err := config.Load(context.Background(), path)
	if err == nil {
		t.Fatal("Load() error = nil, want rejection of spec.verify.trust.ignoreTlog")
	}
	if !strings.Contains(err.Error(), "ignoreTlog") {
		t.Errorf("Load() error = %q, want it to name the unknown field", err.Error())
	}
}
