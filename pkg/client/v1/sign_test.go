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

package aicr_test

import (
	"context"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// TestPublishEvidence_Guards covers every rejection that happens before any
// signing or registry work runs. The success path needs a live registry and a
// Fulcio round trip, which is exercised in pkg/evidence/attestation.
func TestPublishEvidence_Guards(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)
	closed := newClosedClient(t)
	valid := aicr.EvidencePublishOptions{BundleDir: "./out", Push: "ghcr.io/example/aicr-evidence"}

	tests := []struct {
		name   string
		client *aicr.Client
		ctx    context.Context //nolint:containedctx // table-driven guard inputs
		opts   aicr.EvidencePublishOptions
	}{
		{name: "nil client", client: nil, ctx: context.Background(), opts: valid},
		{name: "nil context", client: client, ctx: nil, opts: valid},
		{
			name: "empty bundle dir", client: client, ctx: context.Background(),
			opts: aicr.EvidencePublishOptions{Push: valid.Push},
		},
		{
			name: "empty push reference", client: client, ctx: context.Background(),
			opts: aicr.EvidencePublishOptions{BundleDir: valid.BundleDir},
		},
		{name: "closed client", client: closed, ctx: context.Background(), opts: valid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wantInvalidRequest(t, tt.client.PublishEvidence(tt.ctx, tt.opts))
		})
	}
}

// TestSignCatalog_Guards covers the pre-work rejections. A successful sign
// needs a real OIDC token, which unit tests cannot obtain.
func TestSignCatalog_Guards(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)
	closed := newClosedClient(t)

	tests := []struct {
		name   string
		client *aicr.Client
		ctx    context.Context //nolint:containedctx // table-driven guard inputs
	}{
		{name: "nil client", client: nil, ctx: context.Background()},
		{name: "nil context", client: client, ctx: nil},
		{name: "closed client", client: closed, ctx: context.Background()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := tt.client.SignCatalog(tt.ctx, aicr.CatalogSignOptions{})
			wantInvalidRequest(t, err)
		})
	}
}

// TestSignCatalog_RejectsModesVerifyCatalogCannotVerify pins the sign/verify
// symmetry invariant. VerifyCatalog checks against the public-good Sigstore
// root, requires a transparency-log entry, and accepts keyless GitHub OIDC
// certificates only — so every signing mode outside that envelope must be
// rejected up front rather than producing an artifact the documented
// counterpart cannot verify.
//
// The rejection has to happen before the attester is resolved, which is what
// makes it testable here: no OIDC token is needed to observe it.
func TestSignCatalog_RejectsModesVerifyCatalogCannotVerify(t *testing.T) {
	t.Parallel()

	client := newVerifyClient(t)

	tests := []struct {
		name    string
		resolve aicr.OIDCResolveOptions
	}{
		{
			name:    "KMS key has no catalog verification path",
			resolve: aicr.OIDCResolveOptions{SigningKey: "awskms://alias/aicr-catalog"},
		},
		{
			name:    "local PEM key has no catalog verification path",
			resolve: aicr.OIDCResolveOptions{SigningKey: "./catalog-signer.key"},
		},
		{
			name:    "private Fulcio does not chain to the public-good root",
			resolve: aicr.OIDCResolveOptions{FulcioURL: "https://fulcio.internal.example"},
		},
		{
			name:    "no transparency-log entry to verify",
			resolve: aicr.OIDCResolveOptions{DisableTLogUpload: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := client.SignCatalog(context.Background(), aicr.CatalogSignOptions{OIDCResolve: tt.resolve})
			wantInvalidRequest(t, err)
			if got != nil {
				t.Errorf("result = %+v, want nil alongside the error", got)
			}
		})
	}
}

// The "symmetric settings are NOT rejected" direction is asserted against
// rejectUnverifiableCatalogSigning directly, in the internal test file. It
// cannot be asserted through SignCatalog: an accepted setting by definition
// reaches the attester, and with no OIDC token available that falls through to
// the interactive browser flow, which hangs a headless CI run.
//
// SignCatalog's success path is deliberately not unit tested for the same
// reason. The "SignCatalog sets Attest itself" contract and the nil-bundle
// rejection are covered by the goreleaser release hook that calls
// `aicr recipe sign-catalog` on every tagged build.
