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

package constraints

import (
	"testing"
)

func TestParseConstraintExpression(t *testing.T) {
	tests := []struct {
		name        string
		expression  string
		wantOp      Operator
		wantValue   string
		expectError bool
	}{
		// Comparison operators
		{name: "greater or equal", expression: ">= 1.32.4", wantOp: OperatorGTE, wantValue: "1.32.4"},
		{name: "less or equal", expression: "<= 1.33", wantOp: OperatorLTE, wantValue: "1.33"},
		{name: "greater than", expression: "> 1.30", wantOp: OperatorGT, wantValue: "1.30"},
		{name: "less than", expression: "< 2.0", wantOp: OperatorLT, wantValue: "2.0"},
		{name: "equal op", expression: "== ubuntu", wantOp: OperatorEQ, wantValue: "ubuntu"},
		{name: "not equal", expression: "!= rhel", wantOp: OperatorNE, wantValue: "rhel"},

		// Exact match (no operator)
		{name: "exact match simple", expression: "ubuntu", wantOp: OperatorExact, wantValue: "ubuntu"},
		{name: "exact match version", expression: "24.04", wantOp: OperatorExact, wantValue: "24.04"},
		{name: "exact match with dots", expression: "v1.33.5", wantOp: OperatorExact, wantValue: "v1.33.5"},

		// Whitespace handling
		{name: "extra spaces", expression: ">=  1.32.4", wantOp: OperatorGTE, wantValue: "1.32.4"},
		{name: "leading space", expression: " >= 1.32.4", wantOp: OperatorGTE, wantValue: "1.32.4"},
		{name: "trailing space", expression: ">= 1.32.4 ", wantOp: OperatorGTE, wantValue: "1.32.4"},
		{name: "no space after operator", expression: ">=6.8", wantOp: OperatorGTE, wantValue: "6.8"},
		{name: "no space with gt", expression: ">1.30", wantOp: OperatorGT, wantValue: "1.30"},
		{name: "no space with lte", expression: "<=1.33", wantOp: OperatorLTE, wantValue: "1.33"},
		{name: "no space with lt", expression: "<2.0", wantOp: OperatorLT, wantValue: "2.0"},
		{name: "no space with eq", expression: "==ubuntu", wantOp: OperatorEQ, wantValue: "ubuntu"},
		{name: "no space with ne", expression: "!=rhel", wantOp: OperatorNE, wantValue: "rhel"},

		// Error cases
		{name: "empty expression", expression: "", expectError: true},
		{name: "only spaces", expression: "   ", expectError: true},
		{name: "operator without value", expression: ">=", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseConstraintExpression(tt.expression)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Operator != tt.wantOp {
				t.Errorf("operator = %v, want %v", result.Operator, tt.wantOp)
			}
			if result.Value != tt.wantValue {
				t.Errorf("value = %q, want %q", result.Value, tt.wantValue)
			}
		})
	}
}

func TestParsedConstraint_Evaluate(t *testing.T) {
	tests := []struct {
		name        string
		constraint  ParsedConstraint
		actual      string
		want        bool
		expectError bool
	}{
		// Version comparisons
		{
			name:       "version gte - pass exact",
			constraint: ParsedConstraint{Operator: OperatorGTE, Value: "1.32.4"},
			actual:     "1.32.4",
			want:       true,
		},
		{
			name:       "version gte - pass higher",
			constraint: ParsedConstraint{Operator: OperatorGTE, Value: "1.32.4"},
			actual:     "v1.33.5-eks-3025e55",
			want:       true,
		},
		{
			name:       "version gte - fail lower",
			constraint: ParsedConstraint{Operator: OperatorGTE, Value: "1.32.4"},
			actual:     "1.30.0",
			want:       false,
		},
		{
			name:       "version lte - pass exact",
			constraint: ParsedConstraint{Operator: OperatorLTE, Value: "1.33"},
			actual:     "1.33.0",
			want:       true,
		},
		{
			name:       "version lte - pass lower",
			constraint: ParsedConstraint{Operator: OperatorLTE, Value: "1.33"},
			actual:     "1.32.0",
			want:       true,
		},
		{
			name:       "version lte - fail higher",
			constraint: ParsedConstraint{Operator: OperatorLTE, Value: "1.33"},
			actual:     "1.34.0",
			want:       false,
		},
		{
			name:       "version gt - pass higher",
			constraint: ParsedConstraint{Operator: OperatorGT, Value: "1.30"},
			actual:     "1.32.0",
			want:       true,
		},
		{
			name:       "version gt - fail equal",
			constraint: ParsedConstraint{Operator: OperatorGT, Value: "1.30"},
			actual:     "1.30.0",
			want:       false,
		},
		{
			name:       "version lt - pass lower",
			constraint: ParsedConstraint{Operator: OperatorLT, Value: "2.0"},
			actual:     "1.30.0",
			want:       true,
		},
		{
			name:       "version lt - fail equal",
			constraint: ParsedConstraint{Operator: OperatorLT, Value: "2.0"},
			actual:     "2.0.0",
			want:       false,
		},

		// Kernel version comparisons
		{
			name:       "kernel version gte - pass",
			constraint: ParsedConstraint{Operator: OperatorGTE, Value: "6.8"},
			actual:     "6.8.0-1028-aws",
			want:       true,
		},
		{
			name:       "kernel version gte - fail",
			constraint: ParsedConstraint{Operator: OperatorGTE, Value: "6.8"},
			actual:     "5.15.0-1050-aws",
			want:       false,
		},

		// String equality
		{
			name:       "equal op - pass",
			constraint: ParsedConstraint{Operator: OperatorEQ, Value: "ubuntu"},
			actual:     "ubuntu",
			want:       true,
		},
		{
			name:       "equal op - fail",
			constraint: ParsedConstraint{Operator: OperatorEQ, Value: "ubuntu"},
			actual:     "rhel",
			want:       false,
		},
		{
			name:       "not equal - pass",
			constraint: ParsedConstraint{Operator: OperatorNE, Value: "rhel"},
			actual:     "ubuntu",
			want:       true,
		},
		{
			name:       "not equal - fail",
			constraint: ParsedConstraint{Operator: OperatorNE, Value: "rhel"},
			actual:     "rhel",
			want:       false,
		},

		// Exact match
		{
			name:       "exact match - pass",
			constraint: ParsedConstraint{Operator: OperatorExact, Value: "24.04"},
			actual:     "24.04",
			want:       true,
		},
		{
			name:       "exact match - fail",
			constraint: ParsedConstraint{Operator: OperatorExact, Value: "24.04"},
			actual:     "22.04",
			want:       false,
		},

		// Case sensitivity
		{
			name:       "exact match case sensitive",
			constraint: ParsedConstraint{Operator: OperatorExact, Value: "Ubuntu"},
			actual:     "ubuntu",
			want:       false,
		},

		// Version comparison with IsVersionComparison flag
		{
			name:       "eq with version comparison - equal versions",
			constraint: ParsedConstraint{Operator: OperatorEQ, Value: "1.2.3", IsVersionComparison: true},
			actual:     "v1.2.3",
			want:       true,
		},
		{
			name:       "eq with version comparison - different versions",
			constraint: ParsedConstraint{Operator: OperatorEQ, Value: "1.2.3", IsVersionComparison: true},
			actual:     "v1.2.4",
			want:       false,
		},
		{
			name:       "ne with version comparison - different versions",
			constraint: ParsedConstraint{Operator: OperatorNE, Value: "1.2.3", IsVersionComparison: true},
			actual:     "v1.2.4",
			want:       true,
		},
		{
			name:       "ne with version comparison - equal versions",
			constraint: ParsedConstraint{Operator: OperatorNE, Value: "1.2.3", IsVersionComparison: true},
			actual:     "v1.2.3",
			want:       false,
		},
		{
			name:       "eq with non-parseable version falls back to string comparison",
			constraint: ParsedConstraint{Operator: OperatorEQ, Value: "not-a-version", IsVersionComparison: true},
			actual:     "not-a-version",
			want:       true,
		},
		{
			name:       "ne with non-parseable version falls back to string comparison",
			constraint: ParsedConstraint{Operator: OperatorNE, Value: "not-a-version", IsVersionComparison: true},
			actual:     "different",
			want:       true,
		},

		// Error cases - invalid version parsing for comparison operators
		{
			name:        "gte with invalid expected version",
			constraint:  ParsedConstraint{Operator: OperatorGTE, Value: "not-a-version"},
			actual:      "1.0.0",
			expectError: true,
		},
		{
			name:        "gte with invalid actual version",
			constraint:  ParsedConstraint{Operator: OperatorGTE, Value: "1.0.0"},
			actual:      "not-a-version",
			expectError: true,
		},
		{
			name:        "lt with invalid expected version",
			constraint:  ParsedConstraint{Operator: OperatorLT, Value: "invalid"},
			actual:      "1.0.0",
			expectError: true,
		},

		// Whitespace in actual value
		{
			name:       "actual value with leading/trailing whitespace",
			constraint: ParsedConstraint{Operator: OperatorExact, Value: "ubuntu"},
			actual:     "  ubuntu  ",
			want:       true,
		},

		// Unknown operator error case
		{
			name:        "unknown operator",
			constraint:  ParsedConstraint{Operator: Operator("unknown"), Value: "test"},
			actual:      "test",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.constraint.Evaluate(tt.actual)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.want {
				t.Errorf("Evaluate(%q) = %v, want %v", tt.actual, result, tt.want)
			}
		})
	}
}

func TestLooksLikeVersion(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "simple version", input: "1.2.3", want: true},
		{name: "version with v prefix", input: "v1.2.3", want: true},
		{name: "two part version", input: "1.0", want: true},
		{name: "no dots", input: "123", want: false},
		{name: "no digits", input: "abc.def", want: false},
		{name: "empty string", input: "", want: false},
		{name: "just v prefix", input: "v", want: false},
		{name: "string with dots but no digits", input: "a.b.c", want: false},
		{name: "string with digits and dots", input: "ubuntu22.04", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := looksLikeVersion(tt.input)
			if got != tt.want {
				t.Errorf("looksLikeVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParsedConstraint_String(t *testing.T) {
	tests := []struct {
		name       string
		constraint ParsedConstraint
		want       string
	}{
		{
			name:       "exact match returns value only",
			constraint: ParsedConstraint{Operator: OperatorExact, Value: "ubuntu"},
			want:       "ubuntu",
		},
		{
			name:       "exact match with version",
			constraint: ParsedConstraint{Operator: OperatorExact, Value: "24.04"},
			want:       "24.04",
		},
		{
			name:       "gte operator",
			constraint: ParsedConstraint{Operator: OperatorGTE, Value: "1.32.4"},
			want:       ">= 1.32.4",
		},
		{
			name:       "lte operator",
			constraint: ParsedConstraint{Operator: OperatorLTE, Value: "1.33"},
			want:       "<= 1.33",
		},
		{
			name:       "gt operator",
			constraint: ParsedConstraint{Operator: OperatorGT, Value: "1.30"},
			want:       "> 1.30",
		},
		{
			name:       "lt operator",
			constraint: ParsedConstraint{Operator: OperatorLT, Value: "2.0"},
			want:       "< 2.0",
		},
		{
			name:       "eq operator",
			constraint: ParsedConstraint{Operator: OperatorEQ, Value: "ubuntu"},
			want:       "== ubuntu",
		},
		{
			name:       "ne operator",
			constraint: ParsedConstraint{Operator: OperatorNE, Value: "rhel"},
			want:       "!= rhel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.constraint.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
