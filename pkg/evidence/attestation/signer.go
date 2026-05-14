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

// Signer signs the in-toto Statement carrying the v1 recipe-evidence
// predicate. statementJSON is the unsigned bytes from BuildStatement; the
// signer DSSE-wraps it and produces a Sigstore bundle.
//
// The keyless implementation lives in pkg/bundler/attestation as
// SignStatement; callers that need keyless signing invoke it directly
// (the validate-emit path resolves an OIDC token adjacent to the call so
// Fulcio's nonce-binding window is respected). This interface exists for
// the offline NoOpSigner path and for test seams.
type Signer interface {
	Sign(ctx context.Context, statementJSON []byte) (*bundleattest.SignedAttestation, error)
}

// NoOpSigner returns BundleJSON=nil. Leaves the bundle's unsigned
// Statement on disk so a follow-up `cosign attest` can sign it.
type NoOpSigner struct{}

// Sign returns an empty SignedAttestation; WriteSignedAttestation no-ops on it.
func (NoOpSigner) Sign(_ context.Context, _ []byte) (*bundleattest.SignedAttestation, error) {
	return &bundleattest.SignedAttestation{}, nil
}

// SignBundle signs the bundle's StatementJSON (recipe-subject form) and
// writes attestation.intoto.jsonl into the summary directory. A
// NoOpSigner skips the write.
//
//nolint:unparam // *SignedAttestation feeds the pointer file's signer block; tests exercise only error paths.
func SignBundle(ctx context.Context, b *Bundle, s Signer) (*bundleattest.SignedAttestation, error) {
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
