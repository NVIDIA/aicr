// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

import (
	"fmt"
	"strings"
	"time"
)

// buildID constructs a deterministic, monotonically-ordered build identifier.
//
// Format: {unix-seconds}-{8-char-digest-suffix}
// Example: 1749600000-abc12345
//
// Properties:
//   - Deterministic: same AttestedAt + digest always produces same build-id
//   - Monotonic: lexicographic sort equals time sort for timestamps in the
//     range 2001–2286 (all 10 decimal digits); not guaranteed outside that range
//   - Idempotent: re-ingesting the same bundle writes to the same GCS path
//     (last-write-wins, no duplicate columns)
//   - Digest-bound: two bundles with the same timestamp but different content
//     get distinct build-ids
func buildID(attestedAt time.Time, digest string) string {
	ts := attestedAt.Unix()
	// Extract last 8 hex chars of the digest (after "sha256:" prefix).
	shortDigest := shortHex(digest, 8)
	return fmt.Sprintf("%d-%s", ts, shortDigest)
}

// shortHex returns the last n hex characters of a digest string, stripping
// any "sha256:" or "algorithm:" prefix. Falls back to "unknown" if the
// digest is empty or too short.
func shortHex(digest string, n int) string {
	// Strip algorithm prefix (e.g. "sha256:").
	if i := strings.IndexByte(digest, ':'); i >= 0 {
		digest = digest[i+1:]
	}
	if len(digest) >= n {
		return digest[len(digest)-n:]
	}
	if digest != "" {
		return digest
	}
	return "unknown0"
}
