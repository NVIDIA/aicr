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

	"github.com/sigstore/sigstore-go/pkg/sign"
)

// SigningIdentity supplies the signing keypair and, for keyless signing, the
// Fulcio certificate provider. It is one of the two composable axes of
// SignStatementWith; pair it with a TransparencyPolicy.
//
// The keyless implementation lives in keylessidentity.go; the KMS
// implementation in kmsidentity.go. KMS implementations return (nil, nil)
// from CertProvider, which drives sign.Bundle down its public-key path (no
// Fulcio certificate).
type SigningIdentity interface {
	// Keypair returns the sigstore-go keypair: an ephemeral key for keyless,
	// a KMS-backed adapter for key signing. Resolved lazily so provider
	// auth / a bad key URI surfaces at sign time, not construction.
	Keypair(ctx context.Context) (sign.Keypair, error)

	// CertProvider returns the Fulcio certificate provider and its options
	// for keyless signing, or (nil, nil) for key-based signing.
	CertProvider() (sign.CertificateProvider, *sign.CertificateProviderOptions)

	// FallbackIdentity is the audit identity known before signing: the KMS
	// key URI for KMS, "" for keyless (whose identity is the post-sign
	// Fulcio SAN). SignStatement prefers the cert-extracted SAN and falls
	// back to this when the cert path produced none.
	FallbackIdentity() string
}
