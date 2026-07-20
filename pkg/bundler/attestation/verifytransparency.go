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
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// requireTLogPolicy requires at least one transparency-log inclusion proof and
// one observer timestamp. It is the verification dual of rekorPolicy and the
// current default for all aicr verification flows: every bundle aicr produces
// records a Rekor entry.
//
// The offline relaxation (no transparency log, key-based verification of an
// air-gapped signature) is #1154, now implemented as noTLogVerifyPolicy below,
// mirroring how noTLogPolicy complements rekorPolicy on the signing side.
type requireTLogPolicy struct{}

// NewRequireTLogPolicy returns a VerifyTransparencyPolicy that requires a
// transparency-log inclusion proof and an observer timestamp.
func NewRequireTLogPolicy() VerifyTransparencyPolicy { return requireTLogPolicy{} }

func (requireTLogPolicy) VerifierOptions() []verify.VerifierOption {
	return []verify.VerifierOption{
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	}
}

// noTLogVerifyPolicy is the offline/air-gapped verification dual of noTLogPolicy
// (the signing side, #409): it requires no transparency-log inclusion proof and
// no observer timestamp, so a key-signed bundle with neither can verify with no
// transparency-log network calls. This policy governs only the transparency/
// timestamp axis; whether verification is fully offline also depends on the
// verification identity and trusted-material source (a local PEM key + offline
// trusted root is fully offline, whereas a KMS key URI still resolves remotely).
// INSECURE relative to the default: without a tlog/timestamp there is no trusted
// proof of WHEN the signature was made. Only valid for key-based (--key)
// verification of an air-gapped signature.
//
// verify.WithNoObserverTimestamps() sets the sigstore-go verifier's
// allowNoTimestamp flag, which the verifier's config validation requires to be
// specified exclusively (no WithTransparencyLog / WithObserverTimestamps
// alongside it); it is only usable for key-based, not certificate-based,
// verification, which is exactly the air-gapped KMS/PEM path here.
type noTLogVerifyPolicy struct{}

// NewNoTLogVerifyPolicy returns a VerifyTransparencyPolicy that requires no
// transparency-log inclusion proof and no observer timestamp, so an air-gapped
// key-signed bundle verifies with no transparency-log network calls (fully
// offline use additionally requires an offline key and trusted-material source).
func NewNoTLogVerifyPolicy() VerifyTransparencyPolicy { return noTLogVerifyPolicy{} }

func (noTLogVerifyPolicy) VerifierOptions() []verify.VerifierOption {
	return []verify.VerifierOption{
		verify.WithNoObserverTimestamps(),
	}
}
