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

// Package checksum provides closed-world SHA256 bundle inventories.
//
// Used by component bundlers (GPU Operator, Network Operator, etc.) and deployers
// (Helm, Argo CD) to generate and verify strict checksums.txt manifests. Every
// regular payload file must be listed, and every unlisted filesystem entry is
// rejected. Deployment-bundle callers allow exactly three paths outside the
// manifest: checksums.txt, attestation/bundle-attestation.sigstore.json, and
// attestation/aicr-attestation.sigstore.json. Those paths remain part of the
// verified inventory and are revalidated separately from checksum payloads.
//
// Usage:
//
//	err := checksum.GenerateChecksums(ctx, "/path/to/bundle", fileList)
//	if err != nil {
//	    return err
//	}
//
// The checksums.txt parser accepts valid entries in any order. Generation is
// deterministic and sorts entries by canonical slash-relative path. Reordering
// an already signed manifest changes its bytes and invalidates the existing
// attestation.
//
// The manifest retains the sha256sum-compatible two-space layout. Compatibility
// tools check only listed digests; they cannot reject additional files,
// directories, symlinks, or other non-regular objects and therefore are not a
// replacement for this package's closed-world inventory verification.
//
// StageVerifiedBundle returns a private verified copy plus a checked cleanup
// owner. Callers must install that cleanup immediately, check its error, and
// treat a cleanup-only failure as an unsuccessful operation. Cleanup is safe
// for repeated or concurrent calls and returns one cached result.
package checksum
