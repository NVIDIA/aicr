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

package trust

import (
	"log/slog"

	prototrustroot "github.com/sigstore/protobuf-specs/gen/pb-go/trustroot/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"google.golang.org/protobuf/encoding/protojson"

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
func Update() (root.TrustedMaterial, error) {
	slog.Info("fetching latest Sigstore trusted root via TUF...")

	opts := tuf.DefaultOptions()

	client, err := tuf.New(opts)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to initialize TUF client for update", err)
	}

	// Force a refresh to get the latest metadata
	if refreshErr := client.Refresh(); refreshErr != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "TUF refresh failed", refreshErr)
	}

	material, err := trustedMaterialFromClient(client)
	if err != nil {
		return nil, err
	}

	slog.Info("trusted root updated successfully",
		"fulcio_cas", len(material.FulcioCertificateAuthorities()),
		"rekor_logs", len(material.RekorLogs()),
	)

	return material, nil
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
