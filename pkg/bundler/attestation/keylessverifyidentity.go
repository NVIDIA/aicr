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
)

// keylessVerificationIdentity verifies a keyless (Fulcio) signature against a
// TrustedRootSource, pinning the signer to a Fulcio certificate identity. It is
// the verification dual of keylessIdentity and the VerificationIdentity used by
// both bundle attestation (any OIDC issuer) and binary attestation
// (NVIDIA-pinned issuer + repository pattern); the caller supplies the
// certificate matcher. The trust anchors come from src, which defaults to the
// public-good root but may be a union source supplied by `verify --trust-root`.
type keylessVerificationIdentity struct {
	id  verify.CertificateIdentity
	src TrustedRootSource
}

// NewKeylessVerificationIdentity returns a VerificationIdentity that validates
// a keyless Fulcio signature against the trust anchors from src and requires
// the signing certificate to match id. A nil src defaults to
// PublicGoodTrustedRoot.
func NewKeylessVerificationIdentity(id verify.CertificateIdentity, src TrustedRootSource) VerificationIdentity {
	if src == nil {
		src = PublicGoodTrustedRoot
	}
	return &keylessVerificationIdentity{id: id, src: src}
}

func (k *keylessVerificationIdentity) TrustedMaterial(ctx context.Context) (root.TrustedMaterial, error) {
	return k.src(ctx)
}

func (k *keylessVerificationIdentity) PolicyOption() verify.PolicyOption {
	return verify.WithCertificateIdentity(k.id)
}
