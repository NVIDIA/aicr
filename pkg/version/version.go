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

package version

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	pkgerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// gkeSuffixPrefix is the bare prefix that identifies GKE-specific build number
// extras of the form "-gke.NNNNNN". Unexported: use HasRawGKESuffix or
// ExtractGKEBuild for cross-package consumption.
const gkeSuffixPrefix = "gke."

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

// EqualsOrNewer returns true if v is equal to or newer than other. Comparison
// is performed up to min(v.Precision, other.Precision) so the function is
// symmetric with Compare — a Major.Minor (precision=2) version compares
// equal to any Major.Minor.x (precision=3) sharing the same Major.Minor.
func (v Version) EqualsOrNewer(other Version) bool {
	return v.Compare(other) >= 0
}

// isNewer returns true if v is strictly newer than other (not equal).
// Uses the same min-precision semantics as Compare/EqualsOrNewer.
func (v Version) isNewer(other Version) bool {
	return v.Compare(other) > 0
}

// Equals returns true if v exactly equals other (all components match).
// Unlike EqualsOrNewer, this ignores precision and compares all fields.
func (v Version) Equals(other Version) bool {
	return v.Major == other.Major && v.Minor == other.Minor && v.Patch == other.Patch
}

// ExtractGKEBuild parses a GKE-specific build number from the Extras field of
// a Version. It expects extras in the form "-gke.NNNNNN" (e.g. "-gke.1318000")
// and returns the integer build number and true on success. Non-GKE extras
// (e.g. "-eks-3025e55", "-hotfix.20240322", "") return (0, false).
func ExtractGKEBuild(extras string) (int64, bool) {
	s := strings.TrimPrefix(extras, "-")
	if !strings.HasPrefix(s, gkeSuffixPrefix) {
		return 0, false
	}
	numStr := s[len(gkeSuffixPrefix):]
	if numStr == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// HasRawGKESuffix reports whether the Extras field begins with the "-gke."
// prefix, regardless of whether the trailing build number is valid. Use this
// when you need to distinguish "has the GKE suffix leader but an invalid build
// number" from "no GKE suffix at all" (e.g. to reject malformed constraint
// values at parse time). For full validity checking combine with ExtractGKEBuild.
func HasRawGKESuffix(extras string) bool {
	return strings.HasPrefix(strings.TrimPrefix(extras, "-"), gkeSuffixPrefix)
}

// Compare returns an integer comparing two versions:
// -1 if v < other, 0 if v == other, 1 if v > other.
// This comparison respects precision like EqualsOrNewer.
//
// When the numeric core (Major.Minor.Patch) is equal and both versions carry a
// GKE build suffix in the form "-gke.NNNNNN", the build numbers are compared
// numerically. If only the constraint (other) carries a GKE suffix, v is
// considered older (-1). If only v carries a GKE suffix, v is considered newer
// (1). Non-GKE Extras fields (e.g. "-eks-3025e55") are never compared.
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

	// Numeric cores are equal. Break ties with GKE build numbers when the
	// extras match the "-gke.NNNNNN" pattern.
	//
	// Rationale:
	//  - Both GKE → compare build numbers numerically.
	//  - Only constraint has GKE suffix → actual lacks the required build
	//    floor, so actual is considered older (-1).
	//  - Only actual has GKE suffix → actual is a GKE build of a bare
	//    version that it satisfies, so actual is considered newer (1).
	//  - Neither has GKE suffix (or neither is parseable) → equal (0).
	leftBuild, leftIsGKE := ExtractGKEBuild(v.Extras)
	rightBuild, rightIsGKE := ExtractGKEBuild(other.Extras)
	switch {
	case leftIsGKE && rightIsGKE:
		if leftBuild < rightBuild {
			return -1
		}
		if leftBuild > rightBuild {
			return 1
		}
		return 0
	case rightIsGKE:
		// Actual has no GKE suffix; fails the GKE build floor.
		return -1
	case leftIsGKE:
		// Actual is a GKE build satisfying a bare version constraint.
		// NOTE: a GKE build is treated as strictly newer than its bare numeric
		// core: "1.35.0-gke.N" fails "<= 1.35.0" and passes "> 1.35.0". This
		// is intentional — the exclusive upper bound "< 1.35.0" in per-track
		// compound expressions excludes any 1.35.0-gke.N from the 1.34 track,
		// pushing it to the 1.35 track clause. Note that the 1.35 track clause
		// may still reject it if the build number is below the floor (e.g.
		// 1.35.0-gke.500 fails ">= 1.35.0-gke.2745000"), so exclusion from one
		// track does not guarantee acceptance by another.
		return 1
	default:
		return 0
	}
}
