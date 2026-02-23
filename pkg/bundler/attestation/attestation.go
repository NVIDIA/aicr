// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

import "context"

// Attester signs bundle content and returns a Sigstore bundle.
type Attester interface {
	// Attest creates a DSSE-signed in-toto SLSA provenance statement for the
	// given subject, returning a serialized Sigstore bundle (.sigstore.json).
	// Returns nil bytes when attestation is not performed (e.g., NoOpAttester).
	Attest(ctx context.Context, subject AttestSubject) ([]byte, error)

	// Identity returns the attester's identity as it appears in the signing
	// certificate or key reference (e.g., OIDC email, KMS key URI).
	// Returns empty string when no identity is available.
	Identity() string

	// HasRekorEntry reports whether produced attestations include a Rekor
	// transparency log inclusion proof.
	HasRekorEntry() bool
}

// AttestSubject describes what is being attested.
type AttestSubject struct {
	// Name is the artifact name (e.g., "checksums.txt").
	Name string

	// Digest maps algorithm to hex-encoded digest (e.g., {"sha256": "abc123..."}).
	Digest map[string]string

	// ResolvedDependencies records build inputs in SLSA resolvedDependencies format.
	ResolvedDependencies []Dependency
}

// Dependency records an input artifact in SLSA resolvedDependencies.
type Dependency struct {
	// URI identifies the dependency (e.g., GitHub release URL or file:// URI).
	URI string

	// Digest maps algorithm to hex-encoded digest.
	Digest map[string]string
}
