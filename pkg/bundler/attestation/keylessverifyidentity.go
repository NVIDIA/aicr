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

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// keylessVerificationIdentity verifies a keyless (Fulcio) signature against the
// Sigstore public-good trusted root, pinning the signer to a Fulcio
// certificate identity. It is the verification dual of keylessIdentity and the
// VerificationIdentity used by both bundle attestation (any OIDC issuer) and
// binary attestation (NVIDIA-pinned issuer + repository pattern); the caller
// supplies the certificate matcher.
type keylessVerificationIdentity struct{ id verify.CertificateIdentity }

// NewKeylessVerificationIdentity returns a VerificationIdentity that validates
// a keyless Fulcio signature against the Sigstore public-good trusted root and
// requires the signing certificate to match id.
func NewKeylessVerificationIdentity(id verify.CertificateIdentity) VerificationIdentity {
	return &keylessVerificationIdentity{id: id}
}

func (k *keylessVerificationIdentity) TrustedMaterial(context.Context) (root.TrustedMaterial, error) {
	// trust.GetTrustedMaterial is offline (ForceCache) and takes no context, so
	// the ctx is accepted only to satisfy the interface and to keep the
	// key-based identity (which may make a context-bounded KMS call) uniform.
	tm, err := trust.GetTrustedMaterial()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load trusted root", err)
	}
	return tm, nil
}

func (k *keylessVerificationIdentity) PolicyOption() verify.PolicyOption {
	return verify.WithCertificateIdentity(k.id)
}
