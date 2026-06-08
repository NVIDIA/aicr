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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// keylessIdentity signs with a fresh ephemeral keypair and obtains a Fulcio
// certificate via OIDC.
type keylessIdentity struct {
	oidcToken string
	fulcioURL string
}

// NewKeylessIdentity returns the keyless SigningIdentity. Empty fulcioURL
// falls back to defaults.SigstoreFulcioURL. The Rekor endpoint is not a
// SigningIdentity concern; it is supplied separately via the TransparencyPolicy.
func NewKeylessIdentity(oidcToken, fulcioURL string) SigningIdentity {
	if fulcioURL == "" {
		fulcioURL = defaults.SigstoreFulcioURL
	}
	return &keylessIdentity{oidcToken: oidcToken, fulcioURL: fulcioURL}
}

// Keypair returns a fresh ephemeral signing key. Keyless keygen is local, so
// the context is unused (the KMS implementation threads it into remote RPCs).
func (k *keylessIdentity) Keypair(_ context.Context) (sign.Keypair, error) {
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create ephemeral keypair", err)
	}
	return kp, nil
}

func (k *keylessIdentity) CertProvider() (sign.CertificateProvider, *sign.CertificateProviderOptions) {
	return sign.NewFulcio(&sign.FulcioOptions{BaseURL: k.fulcioURL}),
		&sign.CertificateProviderOptions{IDToken: k.oidcToken}
}

func (k *keylessIdentity) FallbackIdentity() string { return "" }
