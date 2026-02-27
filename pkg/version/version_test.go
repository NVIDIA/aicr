// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expected      Version
		expectedError bool
	}{
		{
			name:  "major only",
			input: "1",
			expected: Version{
				Major:     1,
				Minor:     0,
				Patch:     0,
				Precision: 1,
			},
			expectedError: false,
		},
		{
			name:  "major only with v prefix",
			input: "v2",
			expected: Version{
				Major:     2,
				Minor:     0,
				Patch:     0,
				Precision: 1,
			},
			expectedError: false,
		},
		{
			name:  "major.minor",
			input: "1.2",
			expected: Version{
				Major:     1,
				Minor:     2,
				Patch:     0,
				Precision: 2,
			},
			expectedError: false,
		},
		{
			name:  "major.minor with v prefix",
			input: "v0.1",
			expected: Version{
				Major:     0,
				Minor:     1,
				Patch:     0,
				Precision: 2,
			},
			expectedError: false,
		},
		{
			name:  "full version",
			input: "1.2.3",
			expected: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			expectedError: false,
		},
		{
			name:  "full version with v prefix",
			input: "v1.2.3",
			expected: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			expectedError: false,
		},
		{
			name:  "version with zeros",
			input: "v0.0.0",
			expected: Version{
				Major:     0,
				Minor:     0,
				Patch:     0,
				Precision: 3,
			},
			expectedError: false,
		},
		{
			name:          "invalid - too many components",
			input:         "1.2.3.4",
			expected:      Version{},
			expectedError: true,
		},
		{
			name:  "kernel version with extras",
			input: "6.8.0-1028-aws",
			expected: Version{
				Major:     6,
				Minor:     8,
				Patch:     0,
				Precision: 3,
				Extras:    "-1028-aws",
			},
			expectedError: false,
		},
		{
			name:  "eks version with extras",
			input: "v1.33.5-eks-3025e55",
			expected: Version{
				Major:     1,
				Minor:     33,
				Patch:     5,
				Precision: 3,
				Extras:    "-eks-3025e55",
			},
			expectedError: false,
		},
		{
			name:  "gke version with extras",
			input: "v1.28.0-gke.1337000",
			expected: Version{
				Major:     1,
				Minor:     28,
				Patch:     0,
				Precision: 3,
				Extras:    "-gke.1337000",
			},
			expectedError: false,
		},
		{
			name:  "aks version with extras",
			input: "1.29.2-hotfix.20240322",
			expected: Version{
				Major:     1,
				Minor:     29,
				Patch:     2,
				Precision: 3,
				Extras:    "-hotfix.20240322",
			},
			expectedError: false,
		},
		{
			name:          "invalid - non-numeric",
			input:         "v1.2.a",
			expected:      Version{},
			expectedError: true,
		},
		{
			name:          "invalid - empty string",
			input:         "",
			expected:      Version{},
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseVersion(tt.input)
			if tt.expectedError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if result.Major != tt.expected.Major {
				t.Errorf("Major: got %d, want %d", result.Major, tt.expected.Major)
			}
			if result.Minor != tt.expected.Minor {
				t.Errorf("Minor: got %d, want %d", result.Minor, tt.expected.Minor)
			}
			if result.Patch != tt.expected.Patch {
				t.Errorf("Patch: got %d, want %d", result.Patch, tt.expected.Patch)
			}
			if result.Precision != tt.expected.Precision {
				t.Errorf("Precision: got %d, want %d", result.Precision, tt.expected.Precision)
			}
			if result.Extras != tt.expected.Extras {
				t.Errorf("Extras: got %q, want %q", result.Extras, tt.expected.Extras)
			}
		})
	}
}

func TestVersionString(t *testing.T) {
	tests := []struct {
		name     string
		version  Version
		expected string
	}{
		{
			name: "major only",
			version: Version{
				Major:     1,
				Minor:     0,
				Patch:     0,
				Precision: 1,
			},
			expected: "1",
		},
		{
			name: "major.minor",
			version: Version{
				Major:     1,
				Minor:     2,
				Patch:     0,
				Precision: 2,
			},
			expected: "1.2",
		},
		{
			name: "full version",
			version: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			expected: "1.2.3",
		},
		{
			name: "zero version with precision 2",
			version: Version{
				Major:     0,
				Minor:     1,
				Patch:     5,
				Precision: 2,
			},
			expected: "0.1",
		},
		{
			name: "version with extras - should not include in String()",
			version: Version{
				Major:     6,
				Minor:     8,
				Patch:     0,
				Precision: 3,
				Extras:    "-1028-aws",
			},
			expected: "6.8.0",
		},
		{
			name: "eks version with extras - should not include in String()",
			version: Version{
				Major:     1,
				Minor:     33,
				Patch:     5,
				Precision: 3,
				Extras:    "-eks-3025e55",
			},
			expected: "1.33.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.version.String()
			if result != tt.expected {
				t.Errorf("got %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestEqualsOrNewer(t *testing.T) {
	tests := []struct {
		name     string
		version  Version
		other    Version
		expected bool
	}{
		{
			name: "major only - equal",
			version: Version{
				Major:     1,
				Minor:     0,
				Patch:     0,
				Precision: 1,
			},
			other: Version{
				Major:     1,
				Minor:     5,
				Patch:     10,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "major only - newer",
			version: Version{
				Major:     2,
				Minor:     0,
				Patch:     0,
				Precision: 1,
			},
			other: Version{
				Major:     1,
				Minor:     9,
				Patch:     9,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "major only - older",
			version: Version{
				Major:     1,
				Minor:     0,
				Patch:     0,
				Precision: 1,
			},
			other: Version{
				Major:     2,
				Minor:     0,
				Patch:     0,
				Precision: 3,
			},
			expected: false,
		},
		{
			name: "major.minor - equal (example from user: v0.1 matches 0.1.1)",
			version: Version{
				Major:     0,
				Minor:     1,
				Patch:     0,
				Precision: 2,
			},
			other: Version{
				Major:     0,
				Minor:     1,
				Patch:     1,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "major.minor - newer minor",
			version: Version{
				Major:     1,
				Minor:     3,
				Patch:     0,
				Precision: 2,
			},
			other: Version{
				Major:     1,
				Minor:     2,
				Patch:     99,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "major.minor - older minor",
			version: Version{
				Major:     1,
				Minor:     1,
				Patch:     0,
				Precision: 2,
			},
			other: Version{
				Major:     1,
				Minor:     2,
				Patch:     0,
				Precision: 3,
			},
			expected: false,
		},
		{
			name: "full version - equal",
			version: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			other: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "full version - newer patch",
			version: Version{
				Major:     1,
				Minor:     2,
				Patch:     4,
				Precision: 3,
			},
			other: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "full version - older patch",
			version: Version{
				Major:     1,
				Minor:     2,
				Patch:     2,
				Precision: 3,
			},
			other: Version{
				Major:     1,
				Minor:     2,
				Patch:     3,
				Precision: 3,
			},
			expected: false,
		},
		{
			name: "full version - newer major",
			version: Version{
				Major:     2,
				Minor:     0,
				Patch:     0,
				Precision: 3,
			},
			other: Version{
				Major:     1,
				Minor:     9,
				Patch:     9,
				Precision: 3,
			},
			expected: true,
		},
		{
			name: "full version - newer minor",
			version: Version{
				Major:     1,
				Minor:     3,
				Patch:     0,
				Precision: 3,
			},
			other: Version{
				Major:     1,
				Minor:     2,
				Patch:     99,
				Precision: 3,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.version.EqualsOrNewer(tt.other)
			if result != tt.expected {
				t.Errorf("got %v, want %v (comparing %s vs %s)", result, tt.expected, tt.version.String(), tt.other.String())
			}
		})
	}
}

func TestNewVersion(t *testing.T) {
	v := NewVersion(1, 2, 3)
	if v.Major != 1 || v.Minor != 2 || v.Patch != 3 || v.Precision != 3 {
		t.Errorf("NewVersion(1,2,3) = %+v, want Major:1 Minor:2 Patch:3 Precision:3", v)
	}
}

func TestMustParseVersion(t *testing.T) {
	// Should not panic on valid input
	v := MustParseVersion("v1.2.3")
	if v.Major != 1 || v.Minor != 2 || v.Patch != 3 {
		t.Errorf("MustParseVersion failed: got %+v", v)
	}

	// Should panic on invalid input
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustParseVersion did not panic on invalid input")
		}
	}()
	MustParseVersion("invalid")
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		name     string
		version  Version
		other    Version
		expected bool
	}{
		{
			name:     "newer major",
			version:  Version{Major: 2, Minor: 0, Patch: 0, Precision: 3},
			other:    Version{Major: 1, Minor: 9, Patch: 9, Precision: 3},
			expected: true,
		},
		{
			name:     "newer minor",
			version:  Version{Major: 1, Minor: 3, Patch: 0, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 99, Precision: 3},
			expected: true,
		},
		{
			name:     "newer patch",
			version:  Version{Major: 1, Minor: 2, Patch: 4, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: true,
		},
		{
			name:     "equal - full version",
			version:  Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: false,
		},
		{
			name:     "equal - precision 1",
			version:  Version{Major: 1, Minor: 0, Patch: 0, Precision: 1},
			other:    Version{Major: 1, Minor: 5, Patch: 10, Precision: 3},
			expected: false,
		},
		{
			name:     "older",
			version:  Version{Major: 1, Minor: 2, Patch: 2, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.version.IsNewer(tt.other)
			if result != tt.expected {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestEquals(t *testing.T) {
	tests := []struct {
		name     string
		version  Version
		other    Version
		expected bool
	}{
		{
			name:     "equal",
			version:  Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: true,
		},
		{
			name:     "equal - different precision",
			version:  Version{Major: 1, Minor: 2, Patch: 3, Precision: 2},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: true,
		},
		{
			name:     "different major",
			version:  Version{Major: 2, Minor: 2, Patch: 3, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: false,
		},
		{
			name:     "different minor",
			version:  Version{Major: 1, Minor: 3, Patch: 3, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: false,
		},
		{
			name:     "different patch",
			version:  Version{Major: 1, Minor: 2, Patch: 4, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.version.Equals(tt.other)
			if result != tt.expected {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name     string
		version  Version
		other    Version
		expected int
	}{
		{
			name:     "less - major",
			version:  Version{Major: 1, Minor: 9, Patch: 9, Precision: 3},
			other:    Version{Major: 2, Minor: 0, Patch: 0, Precision: 3},
			expected: -1,
		},
		{
			name:     "less - minor",
			version:  Version{Major: 1, Minor: 2, Patch: 99, Precision: 3},
			other:    Version{Major: 1, Minor: 3, Patch: 0, Precision: 3},
			expected: -1,
		},
		{
			name:     "less - patch",
			version:  Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 4, Precision: 3},
			expected: -1,
		},
		{
			name:     "equal - full",
			version:  Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: 0,
		},
		{
			name:     "equal - precision 1",
			version:  Version{Major: 1, Minor: 0, Patch: 0, Precision: 1},
			other:    Version{Major: 1, Minor: 5, Patch: 10, Precision: 3},
			expected: 0,
		},
		{
			name:     "equal - precision 2",
			version:  Version{Major: 1, Minor: 2, Patch: 0, Precision: 2},
			other:    Version{Major: 1, Minor: 2, Patch: 5, Precision: 3},
			expected: 0,
		},
		{
			name:     "greater - major",
			version:  Version{Major: 2, Minor: 0, Patch: 0, Precision: 3},
			other:    Version{Major: 1, Minor: 9, Patch: 9, Precision: 3},
			expected: 1,
		},
		{
			name:     "greater - minor",
			version:  Version{Major: 1, Minor: 3, Patch: 0, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 99, Precision: 3},
			expected: 1,
		},
		{
			name:     "greater - patch",
			version:  Version{Major: 1, Minor: 2, Patch: 4, Precision: 3},
			other:    Version{Major: 1, Minor: 2, Patch: 3, Precision: 3},
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.version.Compare(tt.other)
			if result != tt.expected {
				t.Errorf("got %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestParseVersionErrors(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedErr error
	}{
		{
			name:        "empty string",
			input:       "",
			expectedErr: ErrEmptyVersion,
		},
		{
			name:        "too many components",
			input:       "1.2.3.4",
			expectedErr: ErrTooManyComponents,
		},
		{
			name:        "non-numeric major",
			input:       "a.2.3",
			expectedErr: ErrNonNumeric,
		},
		{
			name:        "non-numeric minor",
			input:       "1.b.3",
			expectedErr: ErrNonNumeric,
		},
		{
			name:        "non-numeric patch",
			input:       "1.2.c",
			expectedErr: ErrNonNumeric,
		},
		{
			name:        "negative major",
			input:       "-1.2.3",
			expectedErr: ErrNegativeComponent,
		},
		{
			name:        "negative minor",
			input:       "1.-2.3",
			expectedErr: ErrNegativeComponent,
		},
		{
			name:        "negative patch",
			input:       "1.2.-3",
			expectedErr: ErrNegativeComponent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseVersion(tt.input)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tt.expectedErr) && !strings.Contains(err.Error(), tt.expectedErr.Error()) {
				t.Errorf("expected error containing %v, got %v", tt.expectedErr, err)
			}
		})
	}
}

func TestParseVersionRoundTrip(t *testing.T) {
	tests := []string{
		"1",
		"v2",
		"1.2",
		"v0.1",
		"1.2.3",
		"v1.2.3",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			v, err := ParseVersion(input)
			if err != nil {
				t.Fatalf("ParseVersion failed: %v", err)
			}
			// Parse again from the string representation
			v2, err := ParseVersion(v.String())
			if err != nil {
				t.Fatalf("ParseVersion round-trip failed: %v", err)
			}
			if v.Major != v2.Major || v.Minor != v2.Minor || v.Patch != v2.Patch || v.Precision != v2.Precision {
				t.Errorf("round-trip mismatch: %+v != %+v", v, v2)
			}
		})
	}
}

// ExampleParseVersion demonstrates how to parse version strings
func ExampleParseVersion() {
	// Parse various version formats
	v1, _ := ParseVersion("1")
	v2, _ := ParseVersion("v1.2")
	v3, _ := ParseVersion("1.2.3")

	fmt.Println(v1.String())
	fmt.Println(v2.String())
	fmt.Println(v3.String())
	// Output:
	// 1
	// 1.2
	// 1.2.3
}

// ExampleVersion_EqualsOrNewer demonstrates precision-aware version comparison
func ExampleVersion_EqualsOrNewer() {
	// Parse versions with different precisions
	v1, _ := ParseVersion("v1.2")  // Precision 2: Major.Minor
	v2, _ := ParseVersion("1.2.5") // Precision 3: Full version
	v3, _ := ParseVersion("1.3.0") // Precision 3: Full version

	// v1.2 matches v1.2.5 because v1.2 has precision 2
	fmt.Println(v1.EqualsOrNewer(v2))

	// v1.2 does not match v1.3.0 because minor differs
	fmt.Println(v1.EqualsOrNewer(v3))

	// Output:
	// true
	// false
}

// Example_precision demonstrates how precision affects version matching
func Example_precision() {
	// v1 has precision 1 (Major only)
	major, _ := ParseVersion("v1")

	// These all match because major is the only significant component
	fmt.Println(major.EqualsOrNewer(Version{Major: 1, Minor: 0, Patch: 0, Precision: 3}))
	fmt.Println(major.EqualsOrNewer(Version{Major: 1, Minor: 5, Patch: 0, Precision: 3}))
	fmt.Println(major.EqualsOrNewer(Version{Major: 1, Minor: 99, Patch: 99, Precision: 3}))

	// This doesn't match because major differs
	fmt.Println(major.EqualsOrNewer(Version{Major: 2, Minor: 0, Patch: 0, Precision: 3}))

	// Output:
	// true
	// true
	// true
	// false
}

// ExampleNewVersion demonstrates creating a version programmatically
func ExampleNewVersion() {
	v := NewVersion(1, 2, 3)
	fmt.Println(v.String())
	fmt.Printf("Major: %d, Minor: %d, Patch: %d, Precision: %d\n", v.Major, v.Minor, v.Patch, v.Precision)
	// Output:
	// 1.2.3
	// Major: 1, Minor: 2, Patch: 3, Precision: 3
}

// ExampleVersion_Compare demonstrates sorting versions
func ExampleVersion_Compare() {
	v1, _ := ParseVersion("1.2.0")
	v2, _ := ParseVersion("1.2.3")
	v3, _ := ParseVersion("1.3.0")

	fmt.Println(v1.Compare(v2)) // v1 < v2
	//nolint:gocritic // intentional self-comparison for demonstration
	fmt.Println(v2.Compare(v2)) // v2 == v2
	fmt.Println(v3.Compare(v1)) // v3 > v1

	// Output:
	// -1
	// 0
	// 1
}
