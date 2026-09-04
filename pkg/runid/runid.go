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

// Package runid generates unique run identifiers shared by the validator
// and the snapshot agent so both subsystems use one format and one
// generator.
package runid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Generate creates a unique run identifier.
// Format: {timestamp}-{random-hex} (e.g., "20260514-123045-abc123def456").
// Callers use this to generate runIDs before creating ConfigMaps and
// rendering Jobs.
//
// Panics if the system's random number generator fails. Entropy failures are
// exceptional and we prefer to fail fast rather than generate predictable IDs
// that could collide across concurrent runs.
func Generate() string {
	timestamp := time.Now().Format("20060102-150405")
	randomBytes := make([]byte, 8)
	n, err := rand.Read(randomBytes)
	if err != nil {
		panic(fmt.Sprintf("failed to generate random bytes for runID: %v", err))
	}
	if n != len(randomBytes) {
		panic(fmt.Sprintf("failed to generate runID: read %d bytes, expected %d", n, len(randomBytes)))
	}
	return fmt.Sprintf("%s-%s", timestamp, hex.EncodeToString(randomBytes))
}
