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

package measurement

import "strings"

// FilterOut returns a new map with keys filtered out based on the provided patterns.
// Supports wildcard patterns:
//   - "prefix*" matches keys starting with "prefix"
//   - "*suffix" matches keys ending with "suffix"
//   - "*contains*" matches keys containing "contains"
//   - "exact" matches keys exactly
func FilterOut(readings map[string]Reading, keys []string) map[string]Reading {
	result := make(map[string]Reading)

	for key, value := range readings {
		omit := false
		for _, pattern := range keys {
			if matchesPattern(key, pattern) {
				omit = true
				break
			}
		}
		if !omit {
			result[key] = value
		}
	}

	return result
}

// filterIn returns a new map with only keys that match the provided patterns.
// This is the complement of FilterOut.
// Supports the same wildcard patterns as FilterOut:
//   - "prefix*" matches keys starting with "prefix"
//   - "*suffix" matches keys ending with "suffix"
//   - "*contains*" matches keys containing "contains"
//   - "exact" matches keys exactly
func filterIn(readings map[string]Reading, keys []string) map[string]Reading {
	result := make(map[string]Reading)

	for key, value := range readings {
		include := false
		for _, pattern := range keys {
			if matchesPattern(key, pattern) {
				include = true
				break
			}
		}
		if include {
			result[key] = value
		}
	}

	return result
}

// matchesPattern checks if a key matches a wildcard pattern.
// Supports multiple wildcard segments, e.g., "a*b*c" matches "aXbYc".
func matchesPattern(key, pattern string) bool {
	// No wildcard - exact match
	if !strings.Contains(pattern, "*") {
		return key == pattern
	}

	// Split pattern by wildcards to get required segments
	segments := strings.Split(pattern, "*")

	// Empty pattern or just wildcards - matches everything
	if len(segments) == 0 {
		return true
	}

	pos := 0
	for i, segment := range segments {
		if segment == "" {
			continue // Skip empty segments from consecutive wildcards
		}

		// First segment must be at the start (unless pattern starts with *)
		if i == 0 && pattern[0] != '*' {
			if !strings.HasPrefix(key, segment) {
				return false
			}
			pos = len(segment)
			continue
		}

		// Last segment must be at the end (unless pattern ends with *)
		if i == len(segments)-1 && pattern[len(pattern)-1] != '*' {
			return strings.HasSuffix(key[pos:], segment)
		}

		// Middle segments must appear in order
		idx := strings.Index(key[pos:], segment)
		if idx == -1 {
			return false
		}
		pos += idx + len(segment)
	}

	return true
}
