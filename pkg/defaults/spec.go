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

	// MaxSetFileBytes is the maximum size in bytes for a value file referenced
	// by `aicr bundle --set-file component:path=<file>`. The file holds a
	// single JSON/YAML value override; 1 MiB is far above any legitimate
	// value snippet and bounds the read against a corrupted or hostile path.
	MaxSetFileBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// DRAEvictionNodeLabelKey is the node-label key shared by the NVIDIA
	// DRA kubelet plugin selector and GPU Operator Driver Manager eviction
	// configuration. Keeping the key in one default prevents the two chart
	// values from drifting apart during driver upgrades.
	DRAEvictionNodeLabelKey = "nvidia.com/dra-kubelet-plugin"

	// DRAEvictionNodeLabelValue is the default value nodes must carry for the
	// DRA kubelet plugin and Driver Manager eviction contract.
	DRAEvictionNodeLabelValue = "true"
)
