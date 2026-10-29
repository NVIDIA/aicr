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

package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// KeyFingerprint is the fingerprint of the KMS key that signed AICR's
// evidence, read from the Sigstore bundle AICR attaches to the evidence as a
// referrer. AICR
// v1.1.0 and later record a key signature's public-key hint as the base64
// SHA-256 of the key's PKIX DER encoding, and name the key by the same hash
// as "sha256:<hex>" (`aicr evidence verify --key`). It identifies the key and
// verifies nothing: the operator runs no Sigstore verification.
func KeyFingerprint(attestation []byte) (string, error) {
	line, _, _ := bytes.Cut(bytes.TrimSpace(attestation), []byte("\n"))
	var b struct {
		VerificationMaterial struct {
			PublicKey *struct {
				Hint string `json:"hint"`
			} `json:"publicKey"`
		} `json:"verificationMaterial"`
	}
	if err := json.Unmarshal(line, &b); err != nil {
		return "", operrors.Wrap(operrors.ErrCodeInvalidRequest, "AICR evidence attestation is not a Sigstore bundle", err)
	}
	if b.VerificationMaterial.PublicKey == nil {
		return "", operrors.New(operrors.ErrCodeInvalidRequest, "AICR evidence is not signed with a key")
	}
	sum, err := base64.StdEncoding.DecodeString(b.VerificationMaterial.PublicKey.Hint)
	if err != nil || len(sum) != sha256.Size {
		return "", operrors.New(operrors.ErrCodeInvalidRequest,
			"AICR evidence signature carries no SHA-256 public-key hint: "+b.VerificationMaterial.PublicKey.Hint)
	}
	return "sha256:" + hex.EncodeToString(sum), nil
}
