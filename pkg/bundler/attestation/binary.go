// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// AttestationFileSuffix is the conventional suffix for attestation files.
const AttestationFileSuffix = "-attestation.sigstore.json"

// AttestationDir is the subdirectory within the bundle where attestation files are stored.
const AttestationDir = "attestation"

// BundleAttestationFile is the path for the bundle attestation within the output directory.
const BundleAttestationFile = AttestationDir + "/bundle-attestation.sigstore.json"

// BinaryAttestationFile is the path for the binary attestation copied into the bundle.
const BinaryAttestationFile = AttestationDir + "/aicr-attestation.sigstore.json"

// FindBinaryAttestation locates the attestation file for a binary at the
// conventional path: <binary-path>-attestation.sigstore.json.
// Returns the attestation file path.
func FindBinaryAttestation(binaryPath string) (string, error) {
	// Convention: attestation file is named <binary-name>-attestation.sigstore.json
	// in the same directory as the binary.
	dir := filepath.Dir(binaryPath)
	base := filepath.Base(binaryPath)
	attestPath, joinErr := deployer.SafeJoin(dir, base+AttestationFileSuffix)
	if joinErr != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, "unsafe attestation path", joinErr)
	}

	if _, err := os.Stat(attestPath); err != nil {
		if os.IsNotExist(err) {
			return "", errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("binary attestation not found: %s", attestPath))
		}
		return "", errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("cannot access binary attestation: %s", attestPath), err)
	}

	return attestPath, nil
}

// ValidateSigstoreBundleData checks that raw bytes are a structurally valid
// Sigstore bundle (valid JSON, valid protobuf). Does not verify signatures.
func ValidateSigstoreBundleData(data []byte) error {
	var pb protobundle.Bundle
	if err := protojson.Unmarshal(data, &pb); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid sigstore bundle structure", err)
	}
	return nil
}

// ComputeFileDigest reads a file and returns its SHA256 hex digest.
func ComputeFileDigest(path string) (string, error) {
	raw, err := checksum.SHA256Raw(path)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
