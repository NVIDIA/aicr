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
	"context"
	"encoding/base64"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// createTestBundle creates a minimal bundle directory with checksums generated
// by the checksum package (same code path as real bundle creation).
func createTestBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create some content files
	files := map[string]string{
		"recipe.yaml":              "apiVersion: v1\nkind: Recipe\n",
		"gpu-operator/values.yaml": "driver:\n  version: 570.86.16\n",
		"deploy.sh":                "#!/bin/bash\nhelm install ...\n",
	}

	filePaths := make([]string, 0, len(files))
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		filePaths = append(filePaths, path)
	}

	// Generate checksums using the same code path as real bundle creation
	if err := checksum.GenerateChecksums(context.Background(), dir, filePaths); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestVerify_ChecksumsOnly(t *testing.T) {
	dir := createTestBundle(t)

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if !result.ChecksumsPassed {
		t.Error("ChecksumsPassed = false, want true")
	}
	if result.TrustLevel != TrustUnverified {
		t.Errorf("TrustLevel = %s, want unverified", result.TrustLevel)
	}
	if result.BundleAttested {
		t.Error("BundleAttested = true, want false")
	}
}

func TestVerify_MissingChecksums(t *testing.T) {
	dir := t.TempDir()

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
}

func TestVerify_TamperedFile(t *testing.T) {
	dir := createTestBundle(t)

	// Tamper with a file after checksums were generated
	if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("tampered content"), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if result.ChecksumsPassed {
		t.Error("ChecksumsPassed = true, want false (file was tampered)")
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors for tampered file")
	}
}

func TestVerify_NonexistentDir(t *testing.T) {
	_, err := Verify(context.Background(), "/nonexistent/path", nil)
	if err == nil {
		t.Error("Verify() with nonexistent dir should return error")
	}
}

func TestVerifyBundle_RejectsEmptyDigest(t *testing.T) {
	// The empty-digest guard now lives in attestation.VerifyStatementWith
	// (the composed core both keyless verifier paths flow through). It must
	// reject empty artifact digests — this prevents accidental fallback to
	// WithoutArtifactUnsafe(). The guard runs before bundle parsing, so the
	// bundle bytes need not be valid here.
	certID, err := verify.NewShortCertificateIdentity("", ".+", "", ".+")
	if err != nil {
		t.Fatal(err)
	}
	id := attestation.NewKeylessVerificationIdentity(certID, nil)
	tlog := attestation.NewRequireTLogPolicy()

	_, err = attestation.VerifyStatementWith(context.Background(), []byte("{}"), id, tlog, nil)
	if err == nil {
		t.Fatal("VerifyStatementWith() with nil digest should return error")
	}
	if !strings.Contains(err.Error(), "artifact digest is required") {
		t.Errorf("error = %v, want message about artifact digest requirement", err)
	}

	_, err = attestation.VerifyStatementWith(context.Background(), []byte("{}"), id, tlog, []byte{})
	if err == nil {
		t.Fatal("VerifyStatementWith() with empty digest should return error")
	}
}

func TestResolveExecutablePath_NotEmpty(t *testing.T) {
	path := resolveExecutablePath()
	if path == "" {
		t.Error("resolveExecutablePath() returned empty string")
	}
}

func TestVerify_NilOptions(t *testing.T) {
	// Verify should handle nil options without panic
	dir := createTestBundle(t)

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() with nil options: %v", err)
	}
	if result == nil {
		t.Fatal("Verify() returned nil result")
	}
}

func TestExtractToolVersion(t *testing.T) {
	t.Run("valid bundle with tool version", func(t *testing.T) {
		// Build a minimal sigstore bundle JSON with a DSSE envelope
		statement := `{"predicate":{"buildDefinition":{"internalParameters":{"toolVersion":"v1.2.3"}}}}`
		payload := base64.StdEncoding.EncodeToString([]byte(statement))
		bundleJSON := fmt.Sprintf(`{"dsseEnvelope":{"payload":"%s"}}`, payload)

		path := filepath.Join(t.TempDir(), "test.sigstore.json")
		if err := os.WriteFile(path, []byte(bundleJSON), 0600); err != nil {
			t.Fatal(err)
		}

		got := extractToolVersion(path)
		if got != "v1.2.3" {
			t.Errorf("extractToolVersion() = %q, want %q", got, "v1.2.3")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		got := extractToolVersion("/nonexistent/path")
		if got != "" {
			t.Errorf("extractToolVersion() = %q, want empty", got)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}
		got := extractToolVersion(path)
		if got != "" {
			t.Errorf("extractToolVersion() = %q, want empty", got)
		}
	})

	t.Run("no dsse envelope", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-dsse.json")
		if err := os.WriteFile(path, []byte(`{"other":"field"}`), 0600); err != nil {
			t.Fatal(err)
		}
		got := extractToolVersion(path)
		if got != "" {
			t.Errorf("extractToolVersion() = %q, want empty", got)
		}
	})

	t.Run("invalid base64 payload", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad-payload.json")
		if err := os.WriteFile(path, []byte(`{"dsseEnvelope":{"payload":"!!!not-base64!!!"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		got := extractToolVersion(path)
		if got != "" {
			t.Errorf("extractToolVersion() = %q, want empty", got)
		}
	})

	t.Run("no tool version in predicate", func(t *testing.T) {
		statement := `{"predicate":{"buildDefinition":{"internalParameters":{}}}}`
		payload := base64.StdEncoding.EncodeToString([]byte(statement))
		bundleJSON := fmt.Sprintf(`{"dsseEnvelope":{"payload":"%s"}}`, payload)

		path := filepath.Join(t.TempDir(), "no-version.json")
		if err := os.WriteFile(path, []byte(bundleJSON), 0600); err != nil {
			t.Fatal(err)
		}
		got := extractToolVersion(path)
		if got != "" {
			t.Errorf("extractToolVersion() = %q, want empty", got)
		}
	})
}

// writeBundleWithStatement writes a sigstore bundle JSON with the given in-toto
// statement as the DSSE payload. Returns the file path.
func writeBundleWithStatement(t *testing.T, statement string) string {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(statement))
	bundleJSON := fmt.Sprintf(`{"dsseEnvelope":{"payload":"%s"}}`, payload)
	path := filepath.Join(t.TempDir(), "test.sigstore.json")
	if err := os.WriteFile(path, []byte(bundleJSON), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractBinaryDigest(t *testing.T) {
	t.Run("valid resolvedDependencies", func(t *testing.T) {
		statement := `{"predicate":{"buildDefinition":{"resolvedDependencies":[{"uri":"file:///usr/local/bin/aicr","digest":{"sha256":"afa80429badccee47ca11075328a0d337af1786223bdae6e32076d042dc26996"}}]}}}`
		path := writeBundleWithStatement(t, statement)

		digest, err := extractBinaryDigest(path)
		if err != nil {
			t.Fatalf("extractBinaryDigest() error: %v", err)
		}
		if len(digest) != 32 {
			t.Errorf("digest length = %d, want 32", len(digest))
		}
	})

	t.Run("no resolvedDependencies", func(t *testing.T) {
		statement := `{"predicate":{"buildDefinition":{"resolvedDependencies":[]}}}`
		path := writeBundleWithStatement(t, statement)

		_, err := extractBinaryDigest(path)
		if err == nil {
			t.Error("extractBinaryDigest() with no deps should return error")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := extractBinaryDigest("/nonexistent/path")
		if err == nil {
			t.Error("extractBinaryDigest() with missing file should return error")
		}
	})

	t.Run("multiple deps returns first sha256", func(t *testing.T) {
		statement := `{"predicate":{"buildDefinition":{"resolvedDependencies":[{"uri":"file:///bin/aicr","digest":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},{"uri":"file://data.yaml","digest":{"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}]}}}`
		path := writeBundleWithStatement(t, statement)

		digest, err := extractBinaryDigest(path)
		if err != nil {
			t.Fatalf("extractBinaryDigest() error: %v", err)
		}
		expected := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		got := hex.EncodeToString(digest)
		if got != expected {
			t.Errorf("digest = %s, want %s (first dep)", got, expected)
		}
	})

	t.Run("invalid hex digest skipped", func(t *testing.T) {
		statement := `{"predicate":{"buildDefinition":{"resolvedDependencies":[{"uri":"file:///bin/aicr","digest":{"sha256":"not-hex"}},{"uri":"file:///bin/aicr2","digest":{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}]}}}`
		path := writeBundleWithStatement(t, statement)

		digest, err := extractBinaryDigest(path)
		if err != nil {
			t.Fatalf("extractBinaryDigest() error: %v", err)
		}
		expected := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		got := hex.EncodeToString(digest)
		if got != expected {
			t.Errorf("digest = %s, want %s (skipped invalid hex)", got, expected)
		}
	})
}

func TestParseDSSEPayload(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		path := writeBundleWithStatement(t, `{"test":"value"}`)
		payload, err := parseDSSEPayload(path)
		if err != nil {
			t.Fatalf("parseDSSEPayload() error: %v", err)
		}
		if string(payload) != `{"test":"value"}` {
			t.Errorf("payload = %s, want {\"test\":\"value\"}", payload)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := parseDSSEPayload("/nonexistent")
		if err == nil {
			t.Error("parseDSSEPayload() with missing file should return error")
		}
	})

	t.Run("no dsse envelope", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-dsse.json")
		if err := os.WriteFile(path, []byte(`{"other":"field"}`), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := parseDSSEPayload(path)
		if err == nil {
			t.Error("parseDSSEPayload() with no envelope should return error")
		}
	})
}

func TestReadBoundedFile_MissingFile(t *testing.T) {
	_, err := readBoundedFile("/nonexistent/path", defaults.MaxSigstoreBundleSize)
	if err == nil {
		t.Error("readBoundedFile() with missing file should return error")
	}
}

func TestReadBoundedFile_OversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.json")
	// Create a file just over the limit
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Write 11 MiB (over the 10 MiB limit)
	buf := make([]byte, 1024*1024)
	for i := 0; i < 11; i++ {
		if _, writeErr := f.Write(buf); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	f.Close()

	_, err = readBoundedFile(path, defaults.MaxSigstoreBundleSize)
	if err == nil {
		t.Error("readBoundedFile() with oversized file should return error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("error = %v, want message about maximum size", err)
	}
}

func TestVerifyChecksumStep_Valid(t *testing.T) {
	dir := createTestBundle(t)
	result := &VerifyResult{}

	data, done := verifyChecksumStep(dir, result)
	if done {
		t.Fatalf("verifyChecksumStep() returned done=true, errors: %v", result.Errors)
	}
	if len(data) == 0 {
		t.Error("verifyChecksumStep() returned empty data")
	}
	if !result.ChecksumsPassed {
		t.Error("ChecksumsPassed = false, want true")
	}
	if result.ChecksumFiles == 0 {
		t.Error("ChecksumFiles = 0, want > 0")
	}
}

func TestVerifyChecksumStep_MissingFile(t *testing.T) {
	dir := t.TempDir()
	result := &VerifyResult{}

	data, done := verifyChecksumStep(dir, result)
	if !done {
		t.Error("verifyChecksumStep() should return done=true for missing checksums")
	}
	if data != nil {
		t.Error("verifyChecksumStep() should return nil data for missing checksums")
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
}

func TestVerifyChecksumStep_TamperedFile(t *testing.T) {
	dir := createTestBundle(t)

	// Tamper with a file after checksums were generated
	if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}

	result := &VerifyResult{}
	_, done := verifyChecksumStep(dir, result)
	if !done {
		t.Error("verifyChecksumStep() should return done=true for tampered files")
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors for tampered file")
	}
}

func TestVerify_ContextCancelled(t *testing.T) {
	dir := createTestBundle(t)

	// Create a fake attestation file so we reach the ctx check in step 3
	attestDir := filepath.Join(dir, "attestation")
	if err := os.MkdirAll(attestDir, 0755); err != nil {
		t.Fatal(err)
	}
	attestPath := filepath.Join(dir, "attestation", "bundle-attestation.sigstore.json")
	if err := os.WriteFile(attestPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Verify(ctx, dir, nil)
	if err == nil {
		t.Error("Verify() with cancelled context should return error")
	}
}

func TestVerify_WithDataDir(t *testing.T) {
	// Bundle with checksums + data dir → trust level capped at attested (but
	// without real attestation, we test the checksum + no-attestation path)
	dir := createTestBundle(t)

	// Create data directory
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "overrides.yaml"), []byte("key: val"), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	// No attestation files → unverified, regardless of data dir
	if result.TrustLevel != TrustUnverified {
		t.Errorf("TrustLevel = %s, want unverified", result.TrustLevel)
	}
}

func TestVerify_EmptyBundleDir(t *testing.T) {
	dir := t.TempDir()
	// Empty dir, no checksums.txt
	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
}

func TestVerify_ChecksumsWithFakeAttestation(t *testing.T) {
	// Bundle with valid checksums + invalid attestation file
	dir := createTestBundle(t)

	// Write a fake attestation file (invalid sigstore bundle)
	attestDir := filepath.Join(dir, "attestation")
	if err := os.MkdirAll(attestDir, 0755); err != nil {
		t.Fatal(err)
	}
	attestPath := filepath.Join(dir, "attestation", "bundle-attestation.sigstore.json")
	if err := os.WriteFile(attestPath, []byte(`{"not":"a-valid-sigstore-bundle"}`), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	// Checksums pass but attestation verification fails → trust unknown
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors for invalid attestation")
	}
}

// writeFakeBundleAttestation creates the standard bundle attestation file path
// under dir with the given JSON content, mirroring TestVerify_ContextCancelled.
func writeFakeBundleAttestation(t *testing.T, dir, content string) {
	t.Helper()
	attestDir := filepath.Join(dir, "attestation")
	if err := os.MkdirAll(attestDir, 0755); err != nil {
		t.Fatal(err)
	}
	attestPath := filepath.Join(attestDir, "bundle-attestation.sigstore.json")
	if err := os.WriteFile(attestPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// writeMinimalBundleAttestation writes a bundle attestation that is just valid
// enough to parse through sigstore-go's bundle.NewBundle (a v0.3 bundle with a
// public-key verification material, a DSSE envelope, and no tlog entries) so
// that VerifyStatementWith reaches id.TrustedMaterial(ctx). It carries no real
// signature, so verification fails; the point is that the failure occurs at
// trust-root resolution, not bundle parsing, which is exactly what the
// --trust-root flow test needs to exercise.
func writeMinimalBundleAttestation(t *testing.T, dir string) {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte(`{"_type":"https://in-toto.io/Statement/v1"}`))
	sig := base64.StdEncoding.EncodeToString([]byte("not-a-real-signature"))
	pub := base64.StdEncoding.EncodeToString([]byte("fake-public-key-bytes"))
	content := fmt.Sprintf(`{
"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json",
"verificationMaterial":{"publicKey":{"hint":"%s"}},
"dsseEnvelope":{"payload":"%s","payloadType":"application/vnd.in-toto+json","signatures":[{"sig":"%s"}]}
}`, pub, payload, sig)
	writeFakeBundleAttestation(t, dir, content)
}

// TestVerify_TrustRootOption_LoaderFailure proves that opts.TrustRoot is
// resolved up front and that a bad --trust-root file fails fast with the
// loader's coded error rather than being folded into a verification-failure
// result. A missing trusted_root.json must surface as a hard
// ErrCodeInvalidRequest whose message names the trust root file (proving the
// trust-root branch ran, not a generic attestation failure).
func TestVerify_TrustRootOption_LoaderFailure(t *testing.T) {
	dir := createTestBundle(t)
	writeMinimalBundleAttestation(t, dir)

	_, err := Verify(context.Background(), dir, &VerifyOptions{
		TrustRoot: "/no/such/trusted_root.json",
	})
	if err == nil {
		t.Fatal("expected a hard error for a missing --trust-root file, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("want ErrCodeInvalidRequest, got %v", err)
	}
	if !strings.Contains(err.Error(), "trust root file") {
		t.Errorf("error does not name the trust root file (trust-root branch may not have run); got: %v", err)
	}
}

func TestVerify_KeyOption_UnknownScheme(t *testing.T) {
	// "bogus://x" is not a recognized KMS scheme, so NewKeyVerificationIdentity
	// treats it as a PEM path, fails to open the file, and returns
	// ErrCodeInvalidRequest. Verify() records this on result.Errors and sets
	// TrustUnknown without hard-failing (returns nil error) — same contract as
	// a keyless verifySigstoreBundle failure.
	dir := createTestBundle(t)
	writeFakeBundleAttestation(t, dir, `{"not":"a-valid-sigstore-bundle"}`)

	result, err := Verify(context.Background(), dir, &VerifyOptions{Key: "bogus://x"})
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors for key-resolution failure")
	}
	if result.BundleAttested {
		t.Error("BundleAttested = true, want false on key-resolution failure")
	}
}

func TestVerify_KeyOption_MissingPEMFile(t *testing.T) {
	// A --key pointing at a nonexistent local PEM file is a user error
	// (ErrCodeInvalidRequest from loadPEMPublicKey). Verify() surfaces it on
	// result.Errors and sets TrustUnknown, returning nil error.
	dir := createTestBundle(t)
	writeFakeBundleAttestation(t, dir, `{"not":"a-valid-sigstore-bundle"}`)

	keyPath := filepath.Join(t.TempDir(), "nonexistent-key.pem")
	result, err := Verify(context.Background(), dir, &VerifyOptions{Key: keyPath})
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors for missing PEM key file")
	}
}

func TestVerify_InvalidIdentityPattern(t *testing.T) {
	dir := createTestBundle(t)

	_, err := Verify(context.Background(), dir, &VerifyOptions{
		CertificateIdentityRegexp: "no-nvidia-repo-here",
	})
	if err == nil {
		t.Error("Verify() with invalid identity pattern should return error")
	}
}

// TestNewUnionTrustedRoot_LoaderErrorPropagates confirms newUnionTrustedRoot
// loads the private root eagerly and returns the loader's classified error: a
// missing trusted_root.json is a user-file failure (ErrCodeInvalidRequest), not
// a server fault. Returning the error here (rather than deferring it into the
// source closure) is what lets Verify fail fast on a bad --trust-root.
func TestNewUnionTrustedRoot_LoaderErrorPropagates(t *testing.T) {
	src, err := newUnionTrustedRoot("/no/such/file.json")
	if src != nil {
		t.Error("expected nil source on loader failure, got non-nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
}
