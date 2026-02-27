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

// Package trust manages Sigstore trusted root material for offline attestation
// verification.
//
// # Trusted Root Resolution
//
// The trusted root (trusted_root.json) contains Fulcio CA certificates and Rekor
// public keys needed to verify Sigstore attestation bundles. Resolution follows
// three layers in priority order:
//
//  1. Local cache (~/.sigstore/root/) — written by Update(), read by
//     GetTrustedMaterial() with ForceCache. No network access.
//  2. Embedded TUF root — compiled into the binary via sigstore-go's
//     //go:embed directive. Used to bootstrap the TUF update chain when no
//     local cache exists. Updated when the sigstore-go dependency is updated.
//  3. TUF update — Update() contacts the Sigstore TUF CDN
//     (tuf-repo-cdn.sigstore.dev), verifies the update chain cryptographically
//     from the embedded root, and writes the latest trusted_root.json to the
//     local cache.
//
// Verification (GetTrustedMaterial) is always fully offline. Trust material is
// updated only when the user explicitly runs "aicr trust update".
//
// # Key Rotation
//
// Sigstore rotates keys a few times per year. When rotation causes verification
// to fail (signing certificate chains to a CA not in the local root), the
// verifier detects this and surfaces an actionable error directing the user to
// run "aicr trust update".
package trust

import (
	"context"
	"log/slog"

	prototrustroot "github.com/sigstore/protobuf-specs/gen/pb-go/trustroot/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// GetTrustedMaterial returns Sigstore trusted material for offline verification.
// Uses the sigstore-go TUF client with ForceCache to avoid network calls.
// Falls back to the embedded TUF root if no cache exists.
func GetTrustedMaterial() (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions().WithForceCache()

	client, err := tuf.New(opts)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to initialize TUF client", err)
	}

	return trustedMaterialFromClient(client)
}

// Update fetches the latest Sigstore trusted root via TUF CDN
// and updates the local cache.
func Update(ctx context.Context) (root.TrustedMaterial, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.HTTPClientTimeout)
	defer cancel()

	slog.Info("fetching latest Sigstore trusted root via TUF...")

	type updateResult struct {
		material root.TrustedMaterial
		err      error
	}

	// Channel + select because tuf.New and client.Refresh do not accept
	// context — errgroup.Wait would block until they return even after ctx expires.
	ch := make(chan updateResult, 1)
	go func() {
		opts := tuf.DefaultOptions()

		client, err := tuf.New(opts)
		if err != nil {
			ch <- updateResult{err: errors.Wrap(errors.ErrCodeUnavailable, "failed to initialize TUF client for update", err)}
			return
		}

		if refreshErr := client.Refresh(); refreshErr != nil {
			ch <- updateResult{err: errors.Wrap(errors.ErrCodeUnavailable, "TUF refresh failed", refreshErr)}
			return
		}

		material, err := trustedMaterialFromClient(client)
		ch <- updateResult{material: material, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout, "TUF update timed out", ctx.Err())
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		if result.material == nil {
			return nil, errors.New(errors.ErrCodeInternal, "TUF update returned nil trusted material")
		}

		slog.Info("trusted root updated successfully",
			"fulcio_cas", len(result.material.FulcioCertificateAuthorities()),
			"rekor_logs", len(result.material.RekorLogs()),
		)

		return result.material, nil
	}
}

// trustedMaterialFromClient loads the trusted root from a TUF client.
func trustedMaterialFromClient(client *tuf.Client) (root.TrustedMaterial, error) {
	trustedRootJSON, err := client.GetTarget("trusted_root.json")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get trusted root from TUF", err)
	}

	var trustedRootPB prototrustroot.TrustedRoot
	if unmarshalErr := protojson.Unmarshal(trustedRootJSON, &trustedRootPB); unmarshalErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse trusted root", unmarshalErr)
	}

	trustedRoot, err := root.NewTrustedRootFromProtobuf(&trustedRootPB)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "invalid trusted root", err)
	}

	return trustedRoot, nil
}
