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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/verify"
	"golang.org/x/sys/unix"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
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
	if result.TrustReason != "checksums.txt not found" {
		t.Errorf("TrustReason = %q, want checksums.txt not found", result.TrustReason)
	}
	if len(result.Errors) != 1 || result.Errors[0] != "checksums.txt not found" {
		t.Errorf("Errors = %v, want [checksums.txt not found]", result.Errors)
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

func TestExtractToolVersionContext_CancellationPropagates(t *testing.T) {
	path := writeBundleWithStatement(t,
		`{"predicate":{"buildDefinition":{"internalParameters":{"toolVersion":"v1.2.3"}}}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	version, err := extractToolVersionContext(ctx, path)
	if version != "" {
		t.Errorf("extractToolVersionContext() version = %q, want empty", version)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("extractToolVersionContext() error = %v, want ErrCodeTimeout", err)
	}
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
	_, err := readBoundedFile("/nonexistent/path")
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
	if closeErr := f.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	_, err = readBoundedFile(path)
	if err == nil {
		t.Error("readBoundedFile() with oversized file should return error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("error = %v, want message about maximum size", err)
	}
}

func TestReadBoundedFile_RejectsFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "bundle.sigstore.json")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := readBoundedFile(fifo)
		result <- err
	}()

	select {
	case err := <-result:
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("readBoundedFile(FIFO) error = %v, want ErrCodeInvalidRequest", err)
		}
	case <-time.After(time.Second):
		t.Error("readBoundedFile(FIFO) blocked instead of rejecting a special file")
		unblockVerifierFIFO(fifo)
		select {
		case <-result:
		case <-time.After(time.Second):
		}
	}
}

func TestReadBoundedFile_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.sigstore.json")
	if err := os.WriteFile(target, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bundle.sigstore.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := readBoundedFile(link)
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("readBoundedFile(symlink) error = %v, want ErrCodeInvalidRequest", err)
	}
}

func TestVerifyBinaryAttestation_ReadCancellationIsBounded(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "binary-attestation.sigstore.json")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}
	ctx := &cancelAfterVerifierErrContext{Context: context.Background(), cancelAfter: 1}
	result := make(chan error, 1)
	go func() {
		_, err := VerifyBinaryAttestation(ctx, fifo, TrustedRepositoryPattern, make([]byte, 32))
		result <- err
	}()

	select {
	case err := <-result:
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("VerifyBinaryAttestation() error = %v, want ErrCodeTimeout", err)
		}
	case <-time.After(time.Second):
		t.Error("VerifyBinaryAttestation() remained blocked after read cancellation")
		unblockVerifierFIFO(fifo)
		select {
		case <-result:
		case <-time.After(time.Second):
		}
	}
}

type cancelAfterVerifierErrContext struct {
	context.Context
	cancelAfter int64
	calls       atomic.Int64
}

func (c *cancelAfterVerifierErrContext) Err() error {
	if c.calls.Add(1) > c.cancelAfter {
		return context.Canceled
	}
	return nil
}

func unblockVerifierFIFO(path string) {
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			_ = writer.Close()
		}
	}()
}

func TestVerifyChecksumStep_Valid(t *testing.T) {
	dir := createTestBundle(t)
	result := &VerifyResult{}

	snapshot, done, err := verifyChecksumStep(context.Background(), dir, result)
	if err != nil {
		t.Fatalf("verifyChecksumStep() error: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := snapshot.cleanup(); cleanupErr != nil {
			t.Errorf("snapshot cleanup error = %v", cleanupErr)
		}
	})
	if done {
		t.Fatalf("verifyChecksumStep() returned done=true, errors: %v", result.Errors)
	}
	if snapshot.checksumDigest == ([sha256.Size]byte{}) {
		t.Error("verifyChecksumStep() returned empty checksum digest")
	}
	if snapshot.stagedDir == dir {
		t.Error("verifyChecksumStep() reused the caller-controlled source directory")
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

	snapshot, done, err := verifyChecksumStep(context.Background(), dir, result)
	if err != nil {
		t.Fatalf("verifyChecksumStep() error: %v", err)
	}
	if !done {
		t.Error("verifyChecksumStep() should return done=true for missing checksums")
	}
	if snapshot != nil {
		t.Error("verifyChecksumStep() should return nil data for missing checksums")
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
}

func TestChecksumStepFailure_MissingManifestUsesSentinel(t *testing.T) {
	dir := t.TempDir()
	manifest, inventory, data, missingErr := checksum.ReadAndVerifyBundle(
		context.Background(), dir, checksum.InventoryOptions{})
	if missingErr == nil {
		t.Fatal("ReadAndVerifyBundle() error = nil, want missing-manifest failure")
	}
	if manifest != nil || inventory != nil || data != nil {
		t.Errorf("ReadAndVerifyBundle() = (%#v, %#v, %v), want nil outputs", manifest, inventory, data)
	}
	reworded := errors.Wrap(
		errors.ErrCodeInvalidRequest, "human-readable wording changed", missingErr)
	result := &VerifyResult{}

	snapshot, done, err := checksumStepFailure(reworded, result)
	if err != nil {
		t.Fatalf("checksumStepFailure() error = %v", err)
	}
	if snapshot != nil || !done {
		t.Errorf("checksumStepFailure() = (%#v, %v), want nil, true", snapshot, done)
	}
	if result.TrustReason != "checksums.txt not found" {
		t.Errorf("TrustReason = %q, want checksums.txt not found", result.TrustReason)
	}
}

func TestVerifyChecksumStep_UsesStagedInventoryMetadata(t *testing.T) {
	dir := createTestBundle(t)
	deps := defaultVerifierDependencies()
	stage := deps.stageVerifiedBundle
	deps.stageVerifiedBundle = func(
		ctx context.Context,
		bundleDir string,
		opts checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		stagedDir, inventory, cleanup, err := stage(ctx, bundleDir, opts)
		if err != nil {
			return "", nil, nil, err
		}
		checksumPath := filepath.Join(stagedDir, checksum.ChecksumFileName)
		movedPath := checksumPath + ".moved"
		if err := os.Rename(checksumPath, movedPath); err != nil {
			_ = cleanup()
			return "", nil, nil, err
		}
		return stagedDir, inventory, func() error {
			renameErr := os.Rename(movedPath, checksumPath)
			cleanupErr := cleanup()
			return stderrors.Join(renameErr, cleanupErr)
		}, nil
	}
	result := &VerifyResult{}

	snapshot, done, err := verifyChecksumStepWithDependencies(
		context.Background(), dir, result, deps)
	if err != nil {
		t.Fatalf("verifyChecksumStepWithDependencies() error = %v", err)
	}
	if done || snapshot == nil {
		t.Fatalf("verifyChecksumStepWithDependencies() = (%#v, %v), want snapshot, false", snapshot, done)
	}
	if result.ChecksumFiles == 0 {
		t.Error("ChecksumFiles = 0, want staged manifest count")
	}
	if snapshot.checksumDigest == ([sha256.Size]byte{}) {
		t.Error("checksumDigest is empty")
	}
	if cleanupErr := snapshot.cleanup(); cleanupErr != nil {
		t.Fatalf("snapshot cleanup error = %v", cleanupErr)
	}
}

func TestVerifyChecksumStep_TamperedFile(t *testing.T) {
	dir := createTestBundle(t)

	// Tamper with a file after checksums were generated
	if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}

	result := &VerifyResult{}
	_, done, err := verifyChecksumStep(context.Background(), dir, result)
	if err != nil {
		t.Fatalf("verifyChecksumStep() error: %v", err)
	}
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

func TestVerifyChecksumStep_AllowsAttestationMetadata(t *testing.T) {
	for _, rel := range attestation.BundleMetadataPaths() {
		t.Run(rel, func(t *testing.T) {
			dir := createTestBundle(t)
			metadataPath := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(metadataPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(metadataPath, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}

			result := &VerifyResult{}
			snapshot, done, err := verifyChecksumStep(context.Background(), dir, result)
			if err != nil {
				t.Fatalf("verifyChecksumStep() error: %v", err)
			}
			if snapshot == nil {
				t.Fatal("verifyChecksumStep() returned nil snapshot")
			}
			t.Cleanup(func() {
				if cleanupErr := snapshot.cleanup(); cleanupErr != nil {
					t.Errorf("snapshot cleanup error = %v", cleanupErr)
				}
			})
			if done {
				t.Fatalf("verifyChecksumStep() rejected allowed metadata %q: %v", rel, result.Errors)
			}
			if !result.ChecksumsPassed {
				t.Errorf("ChecksumsPassed = false for allowed metadata %q", rel)
			}
		})
	}
}

func TestVerifyChecksumStep_ContextCancelled(t *testing.T) {
	dir := createTestBundle(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := &VerifyResult{}
	snapshot, done, err := verifyChecksumStep(ctx, dir, result)
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("verifyChecksumStep() error = %v, want ErrCodeTimeout", err)
	}
	if snapshot != nil {
		t.Error("verifyChecksumStep() returned data after cancellation")
	}
	if done {
		t.Error("verifyChecksumStep() done = true, want false for propagated timeout")
	}
}

func TestVerifyChecksumStep_PropagatesStagingFailure(t *testing.T) {
	const helperBundleEnv = "AICR_TEST_STAGING_FAILURE_BUNDLE"
	if dir := os.Getenv(helperBundleEnv); dir != "" {
		result := &VerifyResult{}
		snapshot, done, err := verifyChecksumStep(context.Background(), dir, result)
		tempRoot := os.Getenv("TMPDIR")
		if removeErr := os.Remove(tempRoot); removeErr != nil {
			t.Fatalf("remove non-directory TMPDIR fixture: %v", removeErr)
		}
		if mkdirErr := os.Mkdir(tempRoot, 0700); mkdirErr != nil {
			t.Fatalf("restore TMPDIR for coverage flush: %v", mkdirErr)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("verifyChecksumStep() error = %v, want ErrCodeInternal", err)
		}
		if snapshot != nil {
			if cleanupErr := snapshot.cleanup(); cleanupErr != nil {
				t.Errorf("unexpected snapshot cleanup error = %v", cleanupErr)
			}
			t.Error("verifyChecksumStep() returned a snapshot after staging failure")
		}
		if done {
			t.Error("verifyChecksumStep() done = true, want false for propagated staging failure")
		}
		if result.TrustLevel != "" {
			t.Errorf("TrustLevel = %s, want unset for operational staging failure", result.TrustLevel)
		}
		return
	}

	dir := createTestBundle(t)
	nonDirectoryTempRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(nonDirectoryTempRoot, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVerifyChecksumStep_PropagatesStagingFailure$")
	cmd.Env = append(os.Environ(), helperBundleEnv+"="+dir, "TMPDIR="+nonDirectoryTempRoot)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("staging-failure helper failed: %v\n%s", err, output)
	}
}

func TestVerifyChecksumStep_SourceMutationCannotChangeSnapshot(t *testing.T) {
	const originalDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const replacementDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	originalStatement := fmt.Sprintf(
		`{"predicate":{"buildDefinition":{"internalParameters":{"toolVersion":"v1.2.3"},"resolvedDependencies":[{"digest":{"sha256":"%s"}}]}}}`,
		originalDigest,
	)
	replacementStatement := fmt.Sprintf(
		`{"predicate":{"buildDefinition":{"internalParameters":{"toolVersion":"v9.9.9"},"resolvedDependencies":[{"digest":{"sha256":"%s"}}]}}}`,
		replacementDigest,
	)

	dir := createTestBundle(t)
	sourceAttestation := filepath.Join(dir, filepath.FromSlash(attestation.BundleAttestationFile))
	if err := os.MkdirAll(filepath.Dir(sourceAttestation), 0755); err != nil {
		t.Fatal(err)
	}
	originalBytes, err := os.ReadFile(writeBundleWithStatement(t, originalStatement))
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(sourceAttestation, originalBytes, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}

	result := &VerifyResult{}
	snapshot, done, err := verifyChecksumStep(context.Background(), dir, result)
	if err != nil {
		t.Fatalf("verifyChecksumStep() error: %v", err)
	}
	if done {
		t.Fatalf("verifyChecksumStep() done = true, errors: %v", result.Errors)
	}
	t.Cleanup(func() {
		if cleanupErr := snapshot.cleanup(); cleanupErr != nil {
			t.Errorf("snapshot cleanup error = %v", cleanupErr)
		}
	})

	stagedAttestation := filepath.Join(snapshot.stagedDir, filepath.FromSlash(attestation.BundleAttestationFile))
	digest, err := extractBinaryDigest(stagedAttestation)
	if err != nil {
		t.Fatalf("extractBinaryDigest() before source mutation: %v", err)
	}
	_, signatureErrBefore := verifySigstoreBundle(
		context.Background(), stagedAttestation, make([]byte, 32), attestation.PublicGoodTrustedRoot)
	if signatureErrBefore == nil {
		t.Fatal("incomplete test attestation unexpectedly passed signature verification")
	}

	replacementBytes, err := os.ReadFile(writeBundleWithStatement(t, replacementStatement))
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(sourceAttestation, replacementBytes, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}

	if got := extractToolVersion(stagedAttestation); got != "v1.2.3" {
		t.Errorf("staged tool version = %q, want v1.2.3", got)
	}
	if got := hex.EncodeToString(digest); got != originalDigest {
		t.Errorf("staged binary digest = %q, want %q", got, originalDigest)
	}
	digestAfter, err := extractBinaryDigest(stagedAttestation)
	if err != nil {
		t.Fatalf("extractBinaryDigest() after source mutation: %v", err)
	}
	if got := hex.EncodeToString(digestAfter); got != originalDigest {
		t.Errorf("staged binary digest after source mutation = %q, want %q", got, originalDigest)
	}
	_, signatureErrAfter := verifySigstoreBundle(
		context.Background(), stagedAttestation, make([]byte, 32), attestation.PublicGoodTrustedRoot)
	if signatureErrAfter == nil || signatureErrAfter.Error() != signatureErrBefore.Error() {
		t.Errorf("staged signature result changed after source mutation: before=%v after=%v",
			signatureErrBefore, signatureErrAfter)
	}
}

func TestVerificationSnapshot_Cleanup(t *testing.T) {
	removeFailure := stderrors.New("injected snapshot removal failure")
	var removeCalls atomic.Int64
	snapshot := &verificationSnapshot{
		remove: func() error {
			removeCalls.Add(1)
			return removeFailure
		},
	}

	const callers = 16
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for index := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = snapshot.cleanup()
		}()
	}
	wg.Wait()
	for index, cleanupErr := range errs {
		if !stderrors.Is(cleanupErr, removeFailure) {
			t.Errorf("cleanup[%d] error = %v, want removal failure", index, cleanupErr)
		}
		if !stderrors.Is(cleanupErr, errs[0]) || !sameErrorInstance(cleanupErr, errs[0]) {
			t.Errorf("cleanup[%d] returned a different cached error instance", index)
		}
	}
	if got := removeCalls.Load(); got != 1 {
		t.Errorf("snapshot removal calls = %d, want 1", got)
	}
}

func TestVerify_CleanupFailure(t *testing.T) {
	dir := createTestBundle(t)
	cleanupFailure := stderrors.New("injected verifier cleanup failure")
	deps := defaultVerifierDependencies()
	stage := deps.stageVerifiedBundle
	var cleanupCalls atomic.Int64
	deps.stageVerifiedBundle = func(
		ctx context.Context,
		bundleDir string,
		opts checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		stagedDir, inventory, cleanup, err := stage(ctx, bundleDir, opts)
		if err != nil {
			return "", nil, nil, err
		}
		return stagedDir, inventory, func() error {
			cleanupCalls.Add(1)
			return stderrors.Join(cleanup(), cleanupFailure)
		}, nil
	}
	warnCalls := 0
	deps.warn = func(string, ...any) { warnCalls++ }

	result, err := verifyWithDependencies(context.Background(), dir, nil, deps)
	if result != nil {
		t.Errorf("Verify() result = %#v, want nil after cleanup failure", result)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("Verify() error = %v, want ErrCodeInternal", err)
	}
	if !stderrors.Is(err, cleanupFailure) {
		t.Errorf("Verify() error = %v, want injected cleanup cause", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("stage cleanup calls = %d, want 1", got)
	}
	if warnCalls != 0 {
		t.Errorf("warning calls = %d, want 0 without a primary error", warnCalls)
	}
}

func TestVerifyChecksumStep_MissingInventoryCleanupFailure(t *testing.T) {
	dir := createTestBundle(t)
	cleanupFailure := stderrors.New("injected missing-inventory cleanup failure")
	deps := defaultVerifierDependencies()
	stage := deps.stageVerifiedBundle
	var cleanupCalls atomic.Int64
	deps.stageVerifiedBundle = func(
		ctx context.Context,
		bundleDir string,
		opts checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		stagedDir, _, cleanup, err := stage(ctx, bundleDir, opts)
		if err != nil {
			return "", nil, nil, err
		}
		return stagedDir, nil, func() error {
			cleanupCalls.Add(1)
			return stderrors.Join(cleanup(), cleanupFailure)
		}, nil
	}
	warnCalls := 0
	deps.warn = func(string, ...any) { warnCalls++ }

	result := &VerifyResult{}
	snapshot, done, err := verifyChecksumStepWithDependencies(
		context.Background(), dir, result, deps)
	if snapshot != nil || done {
		t.Errorf("verifyChecksumStep() = (%#v, %v), want nil, false", snapshot, done)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Fatalf("verifyChecksumStep() error = %v, want ErrCodeInternal", err)
	}
	if stderrors.Is(err, cleanupFailure) {
		t.Errorf("verifyChecksumStep() error joined cleanup failure: %v", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("stage cleanup calls = %d, want 1", got)
	}
	if warnCalls != 1 {
		t.Errorf("warning calls = %d, want 1 with an outward primary error", warnCalls)
	}
}

func TestVerifyChecksumStep_JoinedStagingCleanupFailureIsHardError(t *testing.T) {
	stagingFailure := errors.New(
		errors.ErrCodeInvalidRequest, "injected staging verification failure")
	cleanupFailure := errors.New(
		errors.ErrCodeInternal, "injected staging cleanup failure")
	joined := stderrors.Join(stagingFailure, cleanupFailure)
	deps := defaultVerifierDependencies()
	deps.stageVerifiedBundle = func(
		context.Context,
		string,
		checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		return "", nil, nil, joined
	}
	result := &VerifyResult{}

	snapshot, done, err := verifyChecksumStepWithDependencies(
		context.Background(), t.TempDir(), result, deps)
	if snapshot != nil || done {
		t.Errorf("verifyChecksumStepWithDependencies() = (%#v, %v), want nil, false", snapshot, done)
	}
	if !stderrors.Is(err, stagingFailure) {
		t.Errorf("verifyChecksumStepWithDependencies() error = %v, want staging failure", err)
	}
	if !stderrors.Is(err, cleanupFailure) {
		t.Errorf("verifyChecksumStepWithDependencies() error = %v, want cleanup failure", err)
	}
	if result.TrustLevel != "" || len(result.Errors) != 0 {
		t.Errorf("verification result = %#v, want no normalized trust result", result)
	}
}

func TestVerify_PostSnapshotPrimaryErrorCleanupFailure(t *testing.T) {
	dir := createTestBundle(t)
	primary := errors.New(errors.ErrCodeTimeout, "injected post-snapshot verification failure")
	cleanupFailure := stderrors.New("injected post-snapshot cleanup failure")
	deps := defaultVerifierDependencies()
	stage := deps.stageVerifiedBundle
	var cleanupCalls atomic.Int64
	deps.stageVerifiedBundle = func(
		ctx context.Context,
		bundleDir string,
		opts checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		stagedDir, inventory, cleanup, err := stage(ctx, bundleDir, opts)
		if err != nil {
			return "", nil, nil, err
		}
		return stagedDir, inventory, func() error {
			cleanupCalls.Add(1)
			return stderrors.Join(cleanup(), cleanupFailure)
		}, nil
	}
	deps.afterSnapshot = func() error { return primary }
	warnCalls := 0
	deps.warn = func(string, ...any) { warnCalls++ }

	result, err := verifyWithDependencies(context.Background(), dir, nil, deps)
	if result != nil {
		t.Errorf("Verify() result = %#v, want nil", result)
	}
	if !stderrors.Is(err, primary) || !sameErrorInstance(err, primary) {
		t.Errorf("Verify() error = %v, want exact primary %v", err, primary)
	}
	if stderrors.Is(err, cleanupFailure) {
		t.Errorf("Verify() primary error joined cleanup failure: %v", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("stage cleanup calls = %d, want 1", got)
	}
	if warnCalls != 1 {
		t.Errorf("warning calls = %d, want 1", warnCalls)
	}
}

func sameErrorInstance(got, want error) bool {
	return reflect.ValueOf(got).Pointer() == reflect.ValueOf(want).Pointer()
}

func TestVerifySigstoreBundle_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := verifySigstoreBundle(ctx, "unused", make([]byte, 32), attestation.PublicGoodTrustedRoot)
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("verifySigstoreBundle() error = %v, want ErrCodeTimeout", err)
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
		t.Fatal("Verify() with cancelled context should return error")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("Verify() error = %v, want ErrCodeTimeout", err)
	}
}

func TestVerify_WithDataDir(t *testing.T) {
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
	if result.ChecksumsPassed {
		t.Error("ChecksumsPassed = true, want false for unchecksummed data file")
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if !strings.Contains(strings.Join(result.Errors, "\n"), "unexpected") {
		t.Errorf("Errors = %v, want unmanaged-path error", result.Errors)
	}
}

func TestVerify_RejectsUnmanaged(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, dir string)
		wantMessage string
	}{
		{
			name: "injected root file",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "injected.txt"), []byte("unexpected"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "unexpected file",
		},
		{
			name: "injected nested file",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				extraDir := filepath.Join(dir, "999-extra")
				if err := os.MkdirAll(extraDir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(extraDir, "install.sh"), []byte("#!/bin/sh\n"), 0700); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "unexpected directory",
		},
		{
			name: "empty extra directory",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, "empty-extra"), 0755); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "unexpected directory",
		},
		{
			name: "symlink",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(dir, "recipe.yaml"), filepath.Join(dir, "recipe-link")); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "symlink",
		},
		{
			name: "fifo",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := unix.Mkfifo(filepath.Join(dir, "extra.pipe"), 0600); err != nil {
					t.Skipf("FIFO unsupported: %v", err)
				}
			},
			wantMessage: "special object",
		},
		{
			name: "malformed manifest",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, checksum.ChecksumFileName), []byte("not a checksum manifest\n"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "separator",
		},
		{
			name: "duplicate manifest entry",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				checksumPath := filepath.Join(dir, checksum.ChecksumFileName)
				data, err := os.ReadFile(checksumPath)
				if err != nil {
					t.Fatal(err)
				}
				first, _, ok := strings.Cut(string(data), "\n")
				if !ok {
					t.Fatal("checksums.txt has no complete entry")
				}
				if err := os.WriteFile(checksumPath, append(data, []byte(first+"\n")...), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "duplicate path",
		},
		{
			name: "reserved metadata entry",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				checksumPath := filepath.Join(dir, checksum.ChecksumFileName)
				file, err := os.OpenFile(checksumPath, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := fmt.Fprintf(file, "%064d  %s\n", 0, attestation.BundleAttestationFile)
				closeErr := file.Close()
				if writeErr != nil {
					t.Fatal(writeErr)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
			},
			wantMessage: "reserved metadata namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := createTestBundle(t)
			tt.setup(t, dir)

			result, err := Verify(context.Background(), dir, nil)
			if err != nil {
				t.Fatalf("Verify() error: %v", err)
			}
			if result.ChecksumsPassed {
				t.Error("ChecksumsPassed = true, want false")
			}
			if result.TrustLevel != TrustUnknown {
				t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
			}
			if !strings.Contains(strings.Join(result.Errors, "\n"), tt.wantMessage) {
				t.Errorf("Errors = %v, want message containing %q", result.Errors, tt.wantMessage)
			}
		})
	}
}

func TestVerify_RejectsLegacyUnlistedRecipe(t *testing.T) {
	dir := createTestBundle(t)
	checksumPath := filepath.Join(dir, checksum.ChecksumFileName)
	data, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if line != "" && !strings.HasSuffix(line, "  recipe.yaml") {
			filtered = append(filtered, line)
		}
	}
	if writeErr := os.WriteFile(checksumPath, []byte(strings.Join(filtered, "\n")+"\n"), 0600); writeErr != nil {
		t.Fatal(writeErr)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}
	if result.ChecksumsPassed {
		t.Error("ChecksumsPassed = true, want false")
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if !strings.Contains(strings.Join(result.Errors, "\n"), "unexpected file: \"recipe.yaml\"") {
		t.Errorf("Errors = %v, want omitted recipe.yaml to be rejected", result.Errors)
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

// writeFakeBinaryAttestation writes a garbage (unparseable) binary attestation
// at the standard bundle-relative path. Parsing fails locally in
// loadSigstoreBundleBytes, before any trusted-root (network) resolution.
func writeFakeBinaryAttestation(t *testing.T, dir string) {
	t.Helper()
	attestPath := filepath.Join(dir, attestation.BinaryAttestationFile)
	if err := os.MkdirAll(filepath.Dir(attestPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attestPath, []byte(`{"not":"a-valid-sigstore-bundle"}`), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyBinaryStep is the #1550 regression suite: a binary attestation
// that is PRESENT but fails verification (or whose binary digest cannot be
// extracted from the bundle attestation) must degrade to unknown, not
// attested. Only a MISSING binary attestation is the softer attested
// (incomplete chain).
func TestVerifyBinaryStep(t *testing.T) {
	const validDigestStatement = `{"predicate":{"buildDefinition":{"resolvedDependencies":[{"uri":"file:///usr/local/bin/aicr","digest":{"sha256":"afa80429badccee47ca11075328a0d337af1786223bdae6e32076d042dc26996"}}]}}}`
	const noDigestStatement = `{"predicate":{"buildDefinition":{"resolvedDependencies":[]}}}`

	tests := []struct {
		name            string
		bundleStatement string
		binaryPresent   bool
		wantTrust       TrustLevel
		wantErrSubstr   string
	}{
		{
			name:            "missing binary attestation degrades to attested",
			bundleStatement: validDigestStatement,
			binaryPresent:   false,
			wantTrust:       TrustAttested,
		},
		{
			name:            "present but invalid binary attestation is unknown",
			bundleStatement: validDigestStatement,
			binaryPresent:   true,
			wantTrust:       TrustUnknown,
			wantErrSubstr:   "binary attestation verification failed",
		},
		{
			name:            "digest extraction failure with binary attestation present is unknown",
			bundleStatement: noDigestStatement,
			binaryPresent:   true,
			wantTrust:       TrustUnknown,
			wantErrSubstr:   "could not extract binary digest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bundleAttestPath := writeBundleWithStatement(t, tt.bundleStatement)
			if tt.binaryPresent {
				writeFakeBinaryAttestation(t, dir)
			}

			result := &VerifyResult{ChecksumsPassed: true, BundleAttested: true}
			done, err := verifyBinaryStep(context.Background(), dir, bundleAttestPath, TrustedRepositoryPattern, result)
			if err != nil {
				t.Fatalf("verifyBinaryStep() error: %v", err)
			}
			if !done {
				t.Fatal("verifyBinaryStep() done = false, want true (Verify must return the outcome)")
			}
			if result.TrustLevel != tt.wantTrust {
				t.Errorf("TrustLevel = %s, want %s (reason: %s)", result.TrustLevel, tt.wantTrust, result.TrustReason)
			}
			if result.BinaryAttested {
				t.Error("BinaryAttested = true, want false")
			}
			if tt.wantErrSubstr == "" {
				if len(result.Errors) != 0 {
					t.Errorf("Errors = %v, want none", result.Errors)
				}
				return
			}
			found := false
			for _, e := range result.Errors {
				if strings.Contains(e, tt.wantErrSubstr) {
					found = true
				}
			}
			if !found {
				t.Errorf("Errors = %v, want one containing %q", result.Errors, tt.wantErrSubstr)
			}
		})
	}
}

// TestVerifyBinaryStep_FailedVerifyMaxStaysVerified pins the policy
// interaction: a failed-verify bundle reports unknown while its max
// achievable stays verified, so --min-trust-level max fails it.
func TestVerifyBinaryStep_FailedVerifyMaxStaysVerified(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinaryAttestation(t, dir)
	bundleAttestPath := writeBundleWithStatement(t,
		`{"predicate":{"buildDefinition":{"resolvedDependencies":[{"digest":{"sha256":"afa80429badccee47ca11075328a0d337af1786223bdae6e32076d042dc26996"}}]}}}`)

	result := &VerifyResult{ChecksumsPassed: true, BundleAttested: true}
	if _, err := verifyBinaryStep(context.Background(), dir, bundleAttestPath, TrustedRepositoryPattern, result); err != nil {
		t.Fatalf("verifyBinaryStep() error: %v", err)
	}
	if result.TrustLevel != TrustUnknown {
		t.Fatalf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
	if got := result.MaxAchievableTrustLevel(); got != TrustVerified {
		t.Fatalf("MaxAchievableTrustLevel() = %s, want verified", got)
	}
	failure, err := result.CheckPolicy(Policy{MinTrustLevel: "max"})
	if err != nil {
		t.Fatalf("CheckPolicy() error: %v", err)
	}
	if failure == "" {
		t.Error("CheckPolicy(max) passed; a failed binary attestation must fail --min-trust-level max")
	}
}

func TestVerifyBinaryStep_PropagatesTimeout(t *testing.T) {
	dir := t.TempDir()
	writeFakeBinaryAttestation(t, dir)
	bundleAttestPath := writeBundleWithStatement(t,
		`{"predicate":{"buildDefinition":{"resolvedDependencies":[{"digest":{"sha256":"afa80429badccee47ca11075328a0d337af1786223bdae6e32076d042dc26996"}}]}}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := &VerifyResult{ChecksumsPassed: true, BundleAttested: true}
	done, err := verifyBinaryStep(ctx, dir, bundleAttestPath, TrustedRepositoryPattern, result)
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Fatalf("verifyBinaryStep() error = %v, want ErrCodeTimeout", err)
	}
	if done {
		t.Error("verifyBinaryStep() done = true, want false for propagated timeout")
	}
	if result.TrustLevel != "" {
		t.Errorf("TrustLevel = %s, want unset for propagated timeout", result.TrustLevel)
	}
}
