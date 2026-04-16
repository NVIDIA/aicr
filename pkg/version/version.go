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

package version

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	pkgerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// Error types for version parsing failures
var (
	ErrEmptyVersion      = errors.New("version string is empty")
	ErrTooManyComponents = errors.New("version has more than 3 components")
	ErrNonNumeric        = errors.New("version component is not numeric")
	ErrNegativeComponent = errors.New("version component cannot be negative")
)

// Version represents a semantic version number with Major, Minor, and Patch components.
// It supports flexible precision (1, 2, or 3 components) and preserves additional
// version metadata such as build suffixes (e.g., "-eks-3025e55", "-gke.1337000").
// The Precision field indicates how many components are significant for comparisons.
type Version struct {
	Major int `json:"major,omitempty" yaml:"major,omitempty"`
	Minor int `json:"minor,omitempty" yaml:"minor,omitempty"`
	Patch int `json:"patch,omitempty" yaml:"patch,omitempty"`

	// Precision indicates how many components are significant (1, 2, or 3)
	Precision int `json:"precision,omitempty" yaml:"precision,omitempty"`

	// Extras stores additional version metadata like "-1028-aws" or "-eks-3025e55"
	Extras string `json:"extras,omitempty" yaml:"extras,omitempty"`
}

// NewVersion creates a new Version with the specified major, minor, and patch values.
// The precision is automatically set to 3 (all components are significant).
// Use ParseVersion for parsing version strings or creating versions with different precision.
func NewVersion(major, minor, patch int) Version {
	return Version{
		Major:     major,
		Minor:     minor,
		Patch:     patch,
		Precision: 3,
	}
}

// String returns the string representation of the Version respecting its precision.
// Returns "Major" for precision 1, "Major.Minor" for precision 2,
// and "Major.Minor.Patch" for precision 3. Extras are not included.
func (v Version) String() string {
	switch v.Precision {
	case 1:
		return fmt.Sprintf("%d", v.Major)
	case 2:
		return fmt.Sprintf("%d.%d", v.Major, v.Minor)
	default:
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
}

// ParseVersion parses a version string into a Version struct.
// Supported formats: "1", "1.2", "1.2.3", "v1.2.3", "1.2.3-suffix", "1.2.3+metadata".
// The "v" prefix is optional and stripped if present.
// Additional metadata after '-' or '+' is preserved in the Extras field.
// Returns an error if the version string is empty, has invalid components, or has too many components.
func ParseVersion(s string) (Version, error) {
	// Check for empty string
	if s == "" {
		return Version{}, pkgerrors.Wrap(pkgerrors.ErrCodeInvalidRequest, "empty version string", ErrEmptyVersion)
	}

	// Strip 'v' prefix if present
	s = strings.TrimPrefix(s, "v")
	var v Version

	// First, extract extras if they exist (anything after a dash or plus that comes AFTER digits)
	// This handles cases like "1.28.0-gke.1337000" where the extras contain dots
	// But we need to be careful not to treat "-1" (negative) as having extras
	mainPart := s
	for i, ch := range s {
		if (ch == '-' || ch == '+') && i > 0 {
			// Check if the character before is a digit (not a dot)
			prevCh := s[i-1]
			if prevCh >= '0' && prevCh <= '9' {
				mainPart = s[:i]
				v.Extras = s[i:]
				break
			}
		}
	}

	// Split by dots
	parts := strings.Split(mainPart, ".")
	if len(parts) > 3 {
		return Version{}, pkgerrors.Wrap(pkgerrors.ErrCodeInvalidRequest, "too many version components", ErrTooManyComponents)
	}

	// Parse each component
	for i, part := range parts {
		// Parse the numeric component
		if part == "" {
			return Version{}, pkgerrors.Wrap(pkgerrors.ErrCodeInvalidRequest, "empty component", ErrNonNumeric)
		}
		num, err := strconv.Atoi(part)
		if err != nil {
			return Version{}, pkgerrors.Wrap(pkgerrors.ErrCodeInvalidRequest, fmt.Sprintf("non-numeric component: %q", part), ErrNonNumeric)
		}
		if num < 0 {
			return Version{}, pkgerrors.Wrap(pkgerrors.ErrCodeInvalidRequest, fmt.Sprintf("negative component: %d", num), ErrNegativeComponent)
		}

		switch i {
		case 0:
			v.Major = num
		case 1:
			v.Minor = num
		case 2:
			v.Patch = num
		}
	}

	v.Precision = len(parts)
	return v, nil
}

// MustParseVersion parses a version string and panics if parsing fails.
// This function is useful for initializing package-level constants or test data
// where the version string is known to be valid at compile time.
//
// Only use this for hardcoded strings or in tests. For user input or runtime data,
// always use ParseVersion and handle errors explicitly.
//
// Example usage:
//
//	v := version.MustParseVersion("1.33.0") // OK in init() or tests
//	v, err := version.ParseVersion(userInput) // Required for runtime data
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(fmt.Sprintf("MustParseVersion: %v", err))
	}
	return v
}

// EqualsOrNewer returns true if v is equal to or newer than other.
// Comparison is performed up to the precision of v.
// For example, Version{Major:1, Minor:2, Precision:2} matches any 1.2.x version.
func (v Version) EqualsOrNewer(other Version) bool {
	// Always compare Major
	if v.Major > other.Major {
		return true
	}
	if v.Major < other.Major {
		return false
	}

	// If precision is 1 (Major only), we're equal
	if v.Precision == 1 {
		return true
	}

	// Major versions are equal, compare Minor
	if v.Minor > other.Minor {
		return true
	}
	if v.Minor < other.Minor {
		return false
	}

	// If precision is 2 (Major.Minor), we're equal
	if v.Precision == 2 {
		return true
	}

	// Minor versions are equal, compare Patch
	return v.Patch >= other.Patch
}

// isNewer returns true if v is strictly newer than other (not equal).
// Respects precision like EqualsOrNewer.
func (v Version) isNewer(other Version) bool {
	// Always compare Major
	if v.Major > other.Major {
		return true
	}
	if v.Major < other.Major {
		return false
	}

	// If precision is 1 (Major only), they're equal
	if v.Precision == 1 {
		return false
	}

	// Major versions are equal, compare Minor
	if v.Minor > other.Minor {
		return true
	}
	if v.Minor < other.Minor {
		return false
	}

	// If precision is 2 (Major.Minor), they're equal
	if v.Precision == 2 {
		return false
	}

	// Minor versions are equal, compare Patch
	return v.Patch > other.Patch
}

// Equals returns true if v exactly equals other (all components match).
// Unlike EqualsOrNewer, this ignores precision and compares all fields.
func (v Version) Equals(other Version) bool {
	return v.Major == other.Major && v.Minor == other.Minor && v.Patch == other.Patch
}

// Compare returns an integer comparing two versions:
// -1 if v < other, 0 if v == other, 1 if v > other.
// This comparison respects precision like EqualsOrNewer.
// Useful for sorting versions.
func (v Version) Compare(other Version) int {
	// Use lower precision for comparison
	precision := min(v.Precision, other.Precision)

	// Compare Major
	if v.Major < other.Major {
		return -1
	}
	if v.Major > other.Major {
		return 1
	}

	// Major equal, check if we should compare Minor
	if precision == 1 {
		return 0
	}

	// Compare Minor
	if v.Minor < other.Minor {
		return -1
	}
	if v.Minor > other.Minor {
		return 1
	}

	// Minor equal, check if we should compare Patch
	if precision == 2 {
		return 0
	}

	// Compare Patch
	if v.Patch < other.Patch {
		return -1
	}
	if v.Patch > other.Patch {
		return 1
	}

	return 0
}
