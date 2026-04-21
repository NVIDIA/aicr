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

// Package checksum provides SHA256 checksum generation for bundle verification.
//
// Used by component bundlers (GPU Operator, Network Operator, etc.) and deployers
// (Helm, Argo CD) to generate checksums.txt files for integrity verification.
//
// Usage:
//
//	err := checksum.GenerateChecksums(ctx, "/path/to/bundle", fileList)
//	if err != nil {
//	    return err
//	}
//
// The checksums.txt file format is compatible with sha256sum:
//
//	sha256sum -c checksums.txt
package checksum
