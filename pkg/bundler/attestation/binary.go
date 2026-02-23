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

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// AttestationFileSuffix is the conventional suffix for attestation files.
const AttestationFileSuffix = "-attestation.sigstore.json"

// BundleAttestationFile is the filename for the bundle attestation in the output directory.
const BundleAttestationFile = "bundle-attestation.sigstore.json"

// BinaryAttestationFile is the filename for the binary attestation copied into the bundle.
const BinaryAttestationFile = "aicr-attestation.sigstore.json"

// FindBinaryAttestation locates the attestation file for a binary at the
// conventional path: <binary-path>-attestation.sigstore.json.
// Returns the attestation file path.
func FindBinaryAttestation(binaryPath string) (string, error) {
	// Convention: attestation file is named <binary-name>-attestation.sigstore.json
	// in the same directory as the binary.
	dir := filepath.Dir(binaryPath)
	base := filepath.Base(binaryPath)
	attestPath := filepath.Join(dir, base+AttestationFileSuffix)

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

// ComputeFileDigest reads a file and returns its SHA256 hex digest.
func ComputeFileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read file for digest: %s", path), err)
	}

	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
