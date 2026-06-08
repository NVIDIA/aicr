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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Compile-time assertion that KMSAttester satisfies the Attester interface.
var _ Attester = (*KMSAttester)(nil)

// KMSAttester signs bundle content with a KMS-backed key (no OIDC). Used for
// CI/CD environments that cannot perform the keyless Fulcio flow. Rekor upload
// is on by default, mirroring keyless; #409 adds the offline opt-out.
type KMSAttester struct {
	keyURI   string
	identity SigningIdentity
	tlog     TransparencyPolicy
}

// NewKMSAttester returns a KMSAttester for keyURI. Empty rekorURL falls back to
// the Sigstore public-good Rekor default.
func NewKMSAttester(keyURI, rekorURL string) *KMSAttester {
	return &KMSAttester{
		keyURI:   keyURI,
		identity: NewKMSIdentity(keyURI),
		tlog:     NewRekorPolicy(rekorURL),
	}
}

// Attest creates a DSSE-signed in-toto SLSA provenance statement for the given
// subject using the KMS-held key, returning the Sigstore bundle as serialized
// JSON. The bundle carries public-key verification material (no Fulcio
// certificate) and the key URI as the signer identity.
func (k *KMSAttester) Attest(ctx context.Context, subject AttestSubject) ([]byte, error) {
	metadata := subject.Metadata
	metadata.BuilderID = k.keyURI
	statementJSON, err := BuildStatement(subject, metadata)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to build attestation statement")
	}

	res, err := SignStatementWith(ctx, statementJSON, k.identity, k.tlog)
	if err != nil {
		return nil, err
	}

	// The KMS identity is a key URI, not user PII, so it is safe to surface at
	// INFO (unlike the keyless Fulcio SAN, which stays at Debug).
	slog.Info("bundle attestation signed successfully (KMS)", "identity", res.Identity)
	return res.BundleJSON, nil
}

// Identity returns the KMS key URI used for signing.
func (k *KMSAttester) Identity() string { return k.keyURI }

// HasRekorEntry reports whether produced attestations include a Rekor entry,
// derived from the configured transparency policy. KMS signing uses Rekor by
// default; this returns false only when paired with the no-tlog policy (the
// offline/air-gapped path, #409).
func (k *KMSAttester) HasRekorEntry() bool {
	_, noTLog := k.tlog.(noTLogPolicy)
	return !noTLog
}
