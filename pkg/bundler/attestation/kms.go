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

	// target carries the transparency-target fields (RekorURL, SigningConfigPath,
	// UseTUFSigningConfig; OIDC/Fulcio fields are unused for KMS). It is built by
	// the shared SignOptionsFromResolve mapper — the same single mapping the
	// keyless and evidence paths use — so a new signing-target field cannot be
	// silently dropped on the KMS path (guarded by TestSignOptionsFromResolve).
	target SignOptions

	// tlog, when non-nil, overrides the transparency policy that Attest would
	// otherwise compute from target. Used to inject a no-tlog policy for offline
	// testing, and wired to the --tlog-upload=false opt-out via
	// WithoutTransparencyLog for offline / air-gapped signing (#409).
	tlog TransparencyPolicy
}

// KMSAttesterOption customizes a KMSAttester at construction time.
type KMSAttesterOption func(*KMSAttester)

// WithoutTransparencyLog makes the attester skip the Rekor upload (offline /
// air-gapped signing, #409). The resulting attester's HasRekorEntry reports
// false and Attest writes no transparency-log entry.
func WithoutTransparencyLog() KMSAttesterOption {
	return func(k *KMSAttester) { k.tlog = NewNoTLogPolicy() }
}

// NewKMSAttester returns a KMSAttester for keyURI. Like keyless signing, KMS
// signs to Rekor v2 by default (via the TUF-distributed signing config, which
// also supplies the timestamp authority v2 requires) unless target opts out to a
// Rekor v1 URL or a custom signing config. target is produced by
// SignOptionsFromResolve so the KMS path shares the keyless path's signing-target
// mapping. The transparency-log entry carries public-key verification material
// (no Fulcio certificate). See #1650.
//
// opts customize the attester; WithoutTransparencyLog opts out of the Rekor
// upload for offline / air-gapped signing (#409). With no options the Rekor
// upload behavior is unchanged.
func NewKMSAttester(keyURI string, target SignOptions, opts ...KMSAttesterOption) *KMSAttester {
	k := &KMSAttester{
		keyURI:   keyURI,
		identity: NewKMSIdentity(keyURI),
		target:   target,
	}
	for _, opt := range opts {
		opt(k)
	}
	return k
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

	tlog := k.tlog
	if tlog == nil {
		tlog, err = transparencyForOptions(ctx, k.target)
		if err != nil {
			return nil, err
		}
	}

	res, err := SignStatementWith(ctx, statementJSON, k.identity, tlog)
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

// HasRekorEntry reports whether produced attestations include a Rekor entry.
// KMS signing records one by default (Rekor v2, or v1 via --rekor-url); it is
// false only when an explicit no-tlog policy override is set (the offline path,
// #409). A nil override (the production default) computes a real Rekor policy at
// Attest time, so this correctly reports true.
func (k *KMSAttester) HasRekorEntry() bool {
	_, noTLog := k.tlog.(noTLogPolicy)
	return !noTLog
}
