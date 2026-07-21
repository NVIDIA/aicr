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

//go:build integration

// This build-tagged integration test proves the aicrd server produces a REAL
// KMS-signed bundle end-to-end. It is excluded from the default build (and thus
// from `make test`) by the `integration` tag; run it explicitly with
// `go test -tags integration ...` against a live OpenBAO Transit KMS.
//
// Unlike the unit tests in bundle_handler_test.go (which inject a stub
// attesterBuilder), this test leaves h.newAttester at its DEFAULT
// (attestation.ResolveAttester), so the server performs REAL hashivault signing
// via the sigstore KMS provider. Only bundler.New's binary-attestation gate is
// bypassed with fixture bytes: the real NVIDIA-CI binary attestation cannot
// exist inside a `go test` executable, but the KMS signing over the bundle
// checksums is genuine and is verified back with the same key.
//
// Provision the backing infra the way tests/chainsaw/signing/bundle-attestation-vault/run.sh
// does (openbao/openbao:2.6.0 dev mode + Transit ecdsa-p256 key), then:
//
//	export VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root VAULT_KMS_KEY=aicr
//	go test -tags integration -run TestServerSigning ./pkg/server/ -v
package server

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
)

// serverSigningIntegrationTimeout bounds the whole flow: real KMS round-trips
// (sign on the POST, GetPublicKey + verify on the verifier) plus bundle
// generation. Generous so a slow local OpenBAO does not flake the test.
const serverSigningIntegrationTimeout = 2 * time.Minute

// TestServerSigning_KMSBundleEndToEnd drives POST /v1/bundle?attest=true against
// a REAL OpenBAO Transit KMS and proves the returned zip carries a genuine
// KMS-signed bundle attestation:
//
//  1. The server builds the KMS attester itself (default newAttester =
//     attestation.ResolveAttester) from AICR_SIGNING_KEY=hashivault://<key>.
//  2. The streamed zip contains a structurally valid Sigstore bundle at
//     attestation/bundle-attestation.sigstore.json.
//  3. That bundle is a REAL signature over the bundle's checksums.txt: the Go
//     verifier (verifier.Verify with the same hashivault Key) re-derives the
//     checksums digest and cryptographically verifies the signature against the
//     KMS public key, yielding BundleAttested=true.
//  4. attestation/aicr-attestation.sigstore.json equals the injected binary
//     attestation fixture bytes (the startup-verified tool provenance the server
//     embeds verbatim).
func TestServerSigning_KMSBundleEndToEnd(t *testing.T) {
	vaultAddr := os.Getenv("VAULT_ADDR")
	kmsKeyName := os.Getenv("VAULT_KMS_KEY")
	if vaultAddr == "" || kmsKeyName == "" {
		t.Skip("integration: set VAULT_ADDR + VAULT_KMS_KEY (run " +
			"tests/chainsaw/signing/bundle-attestation-vault/run.sh or a local OpenBAO) to run")
	}

	// The sigstore hashivault provider reads VAULT_ADDR / VAULT_TOKEN from the
	// process environment; nothing else in this test performs network I/O.
	kmsURI := "hashivault://" + kmsKeyName

	// Fixture stand-in for the server's pre-verified binary attestation. The real
	// NVIDIA-CI provenance cannot exist in a `go test` binary, and bundler.New's
	// Config.Attest() gate only requires the bytes to be present (verified once at
	// startup in production), so arbitrary JSON satisfies the gate. This ONLY
	// bypasses the binary-attestation gate; the bundle-attestation KMS signing is
	// real. These exact bytes must round-trip into the zip unchanged.
	fixtureBinaryAttestation := []byte(`{"pre-verified":"server-binary-attestation"}`)

	ctx, cancel := context.WithTimeout(context.Background(), serverSigningIntegrationTimeout)
	defer cancel()

	body := resolveEmbeddedBundleBody(t)

	// Build the REAL bundle handler with an embedded-recipe Client, then attach
	// an operator-style KMS signing identity. h.newAttester stays at its default
	// (attestation.ResolveAttester), so the server signs via the live KMS.
	h := newTestBundleHandler(t)
	h.signing = &signingConfig{
		enabled:           true,
		signingKey:        kmsURI,
		tlogUpload:        false, // no transparency log against a local OpenBAO
		binaryAttestation: fixtureBinaryAttestation,
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/bundle?attest=true", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleBundles(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. Body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Extract the returned zip to disk so the file-based verifier can re-stage and
	// verify it exactly as an operator's `aicr verify` would.
	bundleDir := t.TempDir()
	extractZip(t, w.Body.Bytes(), bundleDir)

	bundleAttestPath := filepath.Join(bundleDir, filepath.FromSlash(attestation.BundleAttestationFile))
	bundleAttestBytes := readFile(t, bundleAttestPath)

	// (2) Structural proof: the emitted file is a valid Sigstore bundle.
	if err := attestation.ValidateSigstoreBundleData(bundleAttestBytes); err != nil {
		t.Fatalf("bundle attestation is not a structurally valid sigstore bundle: %v", err)
	}
	// Belt-and-suspenders: a real signature is non-empty. A no-op/empty bundle
	// would still parse, so guard against a hollow artifact even before the
	// cryptographic check below.
	if !bytes.Contains(bundleAttestBytes, []byte("messageSignature")) &&
		!bytes.Contains(bundleAttestBytes, []byte("dsseEnvelope")) {

		t.Errorf("bundle attestation carries neither messageSignature nor dsseEnvelope; got: %s", bundleAttestBytes)
	}

	// (3) Cryptographic proof: verify the KMS signature over the bundle checksums
	// with the SAME hashivault key. IgnoreTLog=true because the server signed with
	// tlogUpload=false (no transparency-log entry); IgnoreTLog is only valid with
	// Key set, which it is. A KMS Key URI still makes a live GetPublicKey call, so
	// this exercises the real key material, not a cached PEM.
	res, err := verifier.Verify(ctx, bundleDir, &verifier.VerifyOptions{
		Key:        kmsURI,
		IgnoreTLog: true,
	})
	if err != nil {
		t.Fatalf("verifier.Verify returned error: %v", err)
	}
	if !res.BundleAttested {
		t.Fatalf("bundle attestation did not verify against the KMS key (BundleAttested=false); "+
			"trust=%s errors=%v", res.TrustLevel, res.Errors)
	}
	if res.ChecksumFiles == 0 {
		t.Errorf("expected checksum-verified payload files, got ChecksumFiles=0")
	}
	// The binary attestation is a fixture (not NVIDIA-CI signed), so the FULL
	// chain caps at TrustUnknown after the binary step fails. That does not
	// weaken the KMS proof above: BundleAttested is set only after the real
	// signature-over-checksums verification succeeds. Log for visibility.
	t.Logf("verify result: trust=%s bundleAttested=%t bundleCreator=%q checksumFiles=%d",
		res.TrustLevel, res.BundleAttested, res.BundleCreator, res.ChecksumFiles)

	// (4) The embedded binary attestation is the injected fixture, byte-for-byte.
	binaryAttestPath := filepath.Join(bundleDir, filepath.FromSlash(attestation.BinaryAttestationFile))
	gotBinary := readFile(t, binaryAttestPath)
	if !bytes.Equal(gotBinary, fixtureBinaryAttestation) {
		t.Errorf("embedded binary attestation = %q, want %q", gotBinary, fixtureBinaryAttestation)
	}
}

// extractZip writes every entry of the in-memory zip archive under destDir,
// recreating the directory structure and rejecting zip-slip paths that would
// escape destDir.
func extractZip(t *testing.T, archive []byte, destDir string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("open bundle zip: %v", err)
	}
	for _, f := range zr.File {
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))
		// Guard against zip-slip: the cleaned target must stay within destDir.
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			t.Fatalf("zip entry %q escapes destination directory", f.Name)
		}
		if f.FileInfo().IsDir() {
			if mkErr := os.MkdirAll(target, 0o750); mkErr != nil {
				t.Fatalf("mkdir %s: %v", target, mkErr)
			}
			continue
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o750); mkErr != nil {
			t.Fatalf("mkdir parent of %s: %v", target, mkErr)
		}
		rc, openErr := f.Open()
		if openErr != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, openErr)
		}
		out, createErr := os.Create(target) //nolint:gosec // target validated against destDir above
		if createErr != nil {
			_ = rc.Close()
			t.Fatalf("create %s: %v", target, createErr)
		}
		if _, copyErr := io.Copy(out, rc); copyErr != nil { //nolint:gosec // trusted test-produced archive
			_ = rc.Close()
			_ = out.Close()
			t.Fatalf("write %s: %v", target, copyErr)
		}
		_ = rc.Close()
		if closeErr := out.Close(); closeErr != nil {
			t.Fatalf("close %s: %v", target, closeErr)
		}
	}
}

// readFile reads path or fails the test.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-controlled path under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
