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
	"os"
	"path/filepath"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Sigstore public-good instance URLs. Re-exported so callers that only
// import pkg/evidence/attestation don't need to pull in the bundler
// package to construct a KeylessSigner.
const (
	DefaultFulcioURL = bundleattest.DefaultFulcioURL
	DefaultRekorURL  = bundleattest.DefaultRekorURL
)

// SignResult describes a successful sign() call.
type SignResult struct {
	// BundleJSON is the serialized Sigstore bundle (.sigstore.json),
	// equivalent to a DSSE-wrapped in-toto Statement plus verification
	// material (Fulcio cert, Rekor inclusion proof).
	BundleJSON []byte

	// Identity is the OIDC subject claim from the Fulcio cert
	// (e.g., contributor email or GitHub Actions workflow URI).
	Identity string

	// Issuer is the OIDC issuer URL recorded in the Fulcio cert.
	Issuer string

	// RekorLogIndex is the Rekor inclusion-proof log index, or 0 when
	// no Rekor entry was created (NoOpSigner).
	RekorLogIndex int64
}

// Signer signs the in-toto Statement carrying the v1 recipe-evidence
// predicate. statementJSON is the unsigned bytes from BuildStatement; the
// signer DSSE-wraps it and produces a Sigstore bundle.
type Signer interface {
	Sign(ctx context.Context, statementJSON []byte) (*SignResult, error)
}

// KeylessSigner signs via Fulcio + Rekor with the supplied OIDC token.
// Token discovery is the caller's responsibility.
type KeylessSigner struct {
	OIDCToken string
	FulcioURL string
	RekorURL  string
}

// NewKeylessSigner returns a signer wired to Sigstore public-good URLs.
func NewKeylessSigner(oidcToken string) *KeylessSigner {
	return &KeylessSigner{
		OIDCToken: oidcToken,
		FulcioURL: DefaultFulcioURL,
		RekorURL:  DefaultRekorURL,
	}
}

// Sign DSSE-wraps and signs the statement by calling the shared
// pkg/bundler/attestation primitive. Errors are propagated as-is so
// the underlying ErrCodeTimeout (504) vs ErrCodeUnavailable (502)
// classification reaches the CLI/API boundary.
//
//nolint:unparam // *SignResult is part of the Signer interface; tests exercise error paths only.
func (k *KeylessSigner) Sign(ctx context.Context, statementJSON []byte) (*SignResult, error) {
	res, err := bundleattest.SignStatement(ctx, statementJSON, bundleattest.SignOptions{
		OIDCToken: k.OIDCToken,
		FulcioURL: k.FulcioURL,
		RekorURL:  k.RekorURL,
	})
	if err != nil {
		return nil, err
	}
	return &SignResult{
		BundleJSON:    res.BundleJSON,
		Identity:      res.Identity,
		Issuer:        res.Issuer,
		RekorLogIndex: res.RekorLogIndex,
	}, nil
}

// NoOpSigner returns BundleJSON=nil. Leaves the bundle's unsigned
// Statement on disk so a follow-up `cosign attest` can sign it.
type NoOpSigner struct{}

// Sign returns an empty SignResult; WriteSignedAttestation no-ops on it.
func (NoOpSigner) Sign(_ context.Context, _ []byte) (*SignResult, error) {
	return &SignResult{}, nil
}

// SignBundle signs the bundle's StatementJSON (recipe-subject form) and
// writes attestation.intoto.jsonl into the summary directory. A
// NoOpSigner skips the write.
//
//nolint:unparam // *SignResult feeds the pointer file's signer block; tests exercise only error paths.
func SignBundle(ctx context.Context, b *Bundle, s Signer) (*SignResult, error) {
	if b == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle is required")
	}
	if s == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "signer is required (use NoOpSigner for unsigned)")
	}

	res, err := s.Sign(ctx, b.StatementJSON)
	if err != nil {
		return nil, err
	}
	if err := WriteSignedAttestation(b, res.BundleJSON); err != nil {
		return nil, err
	}
	return res, nil
}

// WriteSignedAttestation writes the Sigstore Bundle bytes into the
// summary directory as attestation.intoto.jsonl. No-op for empty input.
func WriteSignedAttestation(b *Bundle, bundleJSON []byte) error {
	if b == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "bundle is required")
	}
	if len(bundleJSON) == 0 {
		return nil
	}
	out := filepath.Join(b.SummaryDir, AttestationFilename)
	if err := os.WriteFile(out, bundleJSON, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write signed attestation", err)
	}
	return nil
}
