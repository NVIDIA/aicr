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

package defaults

import "os"

// Build spec file constants.
const (
	// SpecFileMode is the file permission used when writing build spec files.
	// Restrictive (owner read/write only) since spec files may contain
	// registry credentials in adjacent fields and are managed by a single
	// controller process.
	SpecFileMode os.FileMode = 0o600

	// MaxSpecFileBytes is the maximum size in bytes for a build spec file
	// loaded from disk. Bounds the input read to prevent memory exhaustion
	// from a corrupted or hostile file. A typical spec is well under 64KiB;
	// 1 MiB provides generous headroom for embedded status payloads.
	MaxSpecFileBytes = 1 * 1024 * 1024 // 1 MiB
)
