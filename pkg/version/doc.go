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

// Package version provides semantic version parsing and comparison with flexible precision support.
//
// # Overview
//
// This package implements a subset of semantic versioning (semver.org) with a focus on
// precision-aware version comparison. It supports three precision levels:
//
//   - Major only (e.g., "1" or "v1")
//   - Major.Minor (e.g., "1.2" or "v1.2")
//   - Major.Minor.Patch (e.g., "1.2.3" or "v1.2.3")
//
// The key feature is precision-aware comparison: a version with lower precision acts as a
// wildcard for missing components. For example:
//
//   - v1 matches v1.0.0, v1.5.0, v1.99.99 (any minor/patch)
//   - v1.2 matches v1.2.0, v1.2.1, v1.2.99 (any patch)
//   - v1.2.3 matches only v1.2.3 exactly
//
// # Usage
//
// Parse a version string:
//
//	v, err := version.ParseVersion("v1.2.3")
//	if err != nil {
//	    // Handle error
//	}
//	fmt.Println(v.String()) // Output: 1.2.3
//
// Compare versions:
//
//	current, _ := version.ParseVersion("v1.2")
//	required, _ := version.ParseVersion("1.2.5")
//	if current.EqualsOrNewer(required) {
//	    fmt.Println("Version requirement met")
//	}
//
// Create versions programmatically:
//
//	v := version.NewVersion(1, 2, 3)
//	fmt.Println(v.String()) // Output: 1.2.3
//
// # Precision Semantics
//
// The Precision field determines how many components are significant:
//
//   - Precision 1: Only Major is significant (Minor/Patch ignored in comparisons)
//   - Precision 2: Major and Minor are significant (Patch ignored)
//   - Precision 3: All components are significant
//
// When comparing versions, the comparison uses the lower precision of the two versions.
// This allows a version like "1.2" to match "1.2.0", "1.2.1", etc.
//
// # Semantic Versioning Compatibility
//
// This package implements a subset of semantic versioning:
//
// Supported:
//   - Major.Minor.Patch version components
//   - Optional "v" prefix
//   - Flexible precision (1-3 components)
//   - Numeric version components
//
// Not Supported (may be added in future):
//   - Prerelease identifiers (e.g., "1.2.3-alpha")
//   - Build metadata (e.g., "1.2.3+build.123")
//   - Version ranges or constraints
//
// # Error Handling
//
// The ParseVersion function returns specific errors for different failure modes:
//
//   - ErrEmptyVersion: Input string is empty
//   - ErrTooManyComponents: More than 3 version components
//   - ErrNonNumeric: Component contains non-numeric characters
//   - ErrNegativeComponent: Component is a negative number
//
// For constant initialization, use MustParseVersion which panics on error:
//
//	var MinVersion = version.MustParseVersion("1.0.0")
package version
