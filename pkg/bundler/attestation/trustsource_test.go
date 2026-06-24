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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	stderrors "errors"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// sentinelSource returns a TrustedRootSource that records that it was called
// and always fails with the given sentinel error. Used to prove the injected
// source is the one consulted by TrustedMaterial.
func sentinelSource(called *bool, err error) TrustedRootSource {
	return func(context.Context) (root.TrustedMaterial, error) {
		*called = true
		return nil, err
	}
}

func TestKeylessVerificationIdentity_UsesInjectedSource(t *testing.T) {
	sentinel := errors.New(errors.ErrCodeUnavailable, "sentinel")
	called := false

	certID, err := verify.NewShortCertificateIdentity("", ".+", "", ".+")
	if err != nil {
		t.Fatalf("failed to build certificate identity: %v", err)
	}
	id := NewKeylessVerificationIdentity(certID, sentinelSource(&called, sentinel))

	tm, err := id.TrustedMaterial(context.Background())
	if tm != nil {
		t.Errorf("expected nil TrustedMaterial, got %v", tm)
	}
	if !called {
		t.Error("expected injected source to be called")
	}
	if !stderrors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestKeylessVerificationIdentity_NilSourceDefaultsToPublicGood(t *testing.T) {
	certID, err := verify.NewShortCertificateIdentity("", ".+", "", ".+")
	if err != nil {
		t.Fatalf("failed to build certificate identity: %v", err)
	}
	id := NewKeylessVerificationIdentity(certID, nil)
	if id == nil {
		t.Fatal("expected non-nil identity")
	}
	// A nil source must default to the public-good root, not panic.
	tm, err := id.TrustedMaterial(context.Background())
	if err != nil {
		t.Fatalf("expected public-good root to load, got %v", err)
	}
	if tm == nil {
		t.Fatal("expected non-nil TrustedMaterial from public-good default")
	}
}

func TestKeyVerificationIdentity_UsesInjectedSourceAsBase(t *testing.T) {
	sentinel := errors.New(errors.ErrCodeUnavailable, "sentinel")
	called := false

	// A valid local PEM key so the constructor succeeds; the sentinel source
	// then fails when TrustedMaterial builds the collection base.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	pemPath := writeTempPubPEM(t, &priv.PublicKey)

	id, err := NewKeyVerificationIdentity(context.Background(), pemPath, sentinelSource(&called, sentinel))
	if err != nil {
		t.Fatalf("expected constructor to succeed, got %v", err)
	}

	tm, err := id.TrustedMaterial(context.Background())
	if tm != nil {
		t.Errorf("expected nil TrustedMaterial when base source fails, got %v", tm)
	}
	if !called {
		t.Error("expected injected source to be used as the collection base")
	}
	if !stderrors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error from base source, got %v", err)
	}
}

// TestKeyVerificationIdentity_CollectionRetainsBaseAndKey proves the key
// identity's TrustedMaterial returns a collection that exposes BOTH the
// injected base's anchors AND the key material, not just one. A future
// regression that drops the base (e.g. returning only the key material) would
// fail the RekorLogs assertion; dropping the key would fail PublicKeyVerifier.
// Fully hermetic: the base comes from the checked-in trusted_root.json fixture,
// not ~/.sigstore.
func TestKeyVerificationIdentity_CollectionRetainsBaseAndKey(t *testing.T) {
	fixture, err := trust.LoadTrustedMaterialFromFile("../../trust/testdata/trusted_root.json")
	if err != nil {
		t.Fatalf("failed to load fixture trusted root: %v", err)
	}
	if len(fixture.RekorLogs()) == 0 {
		t.Fatal("fixture trusted root has no Rekor logs; test cannot distinguish base retention")
	}
	fixtureSource := func(context.Context) (root.TrustedMaterial, error) {
		return fixture, nil
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	pemPath := writeTempPubPEM(t, &priv.PublicKey)

	id, err := NewKeyVerificationIdentity(context.Background(), pemPath, fixtureSource)
	if err != nil {
		t.Fatalf("expected constructor to succeed, got %v", err)
	}

	tm, err := id.TrustedMaterial(context.Background())
	if err != nil {
		t.Fatalf("TrustedMaterial returned error: %v", err)
	}

	// (a) Key material present: the always-valid public-key verifier resolves.
	if _, err := tm.PublicKeyVerifier(""); err != nil {
		t.Errorf("expected key material in collection (PublicKeyVerifier), got error: %v", err)
	}
	// (b) Base retained: the injected fixture's Rekor logs survive the union.
	if len(tm.RekorLogs()) == 0 {
		t.Error("expected injected base's Rekor logs to be retained in the collection")
	}
}

func TestKeyVerificationIdentity_NilSourceDefaultsToPublicGood(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	pemPath := writeTempPubPEM(t, &priv.PublicKey)

	id, err := NewKeyVerificationIdentity(context.Background(), pemPath, nil)
	if err != nil {
		t.Fatalf("expected constructor to succeed, got %v", err)
	}
	tm, err := id.TrustedMaterial(context.Background())
	if err != nil {
		t.Fatalf("expected public-good base to load, got %v", err)
	}
	if tm == nil {
		t.Fatal("expected non-nil TrustedMaterial from public-good default base")
	}
}
