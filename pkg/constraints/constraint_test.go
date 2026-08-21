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

package constraints

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestParseConstraintExpression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		expression  string
		wantOp      Operator
		wantValue   string
		expectError bool
		wantErrCode errors.ErrorCode
	}{
		// Comparison operators
		{name: "greater or equal", expression: ">= 1.32.4", wantOp: OperatorGTE, wantValue: "1.32.4"},
		{name: "less or equal", expression: "<= 1.33", wantOp: OperatorLTE, wantValue: "1.33"},
		{name: "greater than", expression: "> 1.30", wantOp: OperatorGT, wantValue: "1.30"},
		{name: "less than", expression: "< 2.0", wantOp: OperatorLT, wantValue: "2.0"},
		{name: "equal op", expression: "== ubuntu", wantOp: OperatorEQ, wantValue: "ubuntu"},
		{name: "lone = is alias for ==", expression: "= ubuntu", wantOp: OperatorEQ, wantValue: "ubuntu"},
		{name: "lone = no-space is alias for ==", expression: "=ubuntu", wantOp: OperatorEQ, wantValue: "ubuntu"},
		{name: "lone = with empty value errors", expression: "=", expectError: true, wantErrCode: errors.ErrCodeInvalidRequest},
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
		{name: "bare bang label key", expression: "!some-label-key", expectError: true, wantErrCode: errors.ErrCodeInvalidRequest},
		{name: "bare bang scalar", expression: "!ubuntu", expectError: true, wantErrCode: errors.ErrCodeInvalidRequest},
		{name: "empty expression", expression: "", expectError: true},
		{name: "only spaces", expression: "   ", expectError: true},
		{name: "operator without value", expression: ">=", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ParseConstraintExpression(tt.expression)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
					return
				}
				if tt.wantErrCode != "" && !stderrors.Is(err, errors.New(tt.wantErrCode, "")) {
					t.Errorf("error = %v, want code %s", err, tt.wantErrCode)
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
	t.Parallel()

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
			t.Parallel()

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

func TestParseCompoundConstraint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		expr             string
		wantAlternatives int
		wantTermsInFirst int
		wantFirstTermOp  string // operator of first term in first alternative (empty = skip check)
		wantFirstTermVal string // value of first term in first alternative (empty = skip check)
		expectError      bool
	}{
		{
			name:             "single term",
			expr:             ">= 1.32.4",
			wantAlternatives: 1,
			wantTermsInFirst: 1,
		},
		{
			name:             "range AND (two terms in one clause)",
			expr:             ">= 1.34.3-gke.1318000 < 1.35.0",
			wantAlternatives: 1,
			wantTermsInFirst: 2,
			wantFirstTermOp:  ">=",
			wantFirstTermVal: "1.34.3-gke.1318000",
		},
		{
			name:             "OR of two single clauses",
			expr:             ">= 1.32.4 || >= 1.35.0",
			wantAlternatives: 2,
			wantTermsInFirst: 1,
		},
		{
			name:             "full per-track GKE expression",
			expr:             ">= 1.34.3-gke.1318000 < 1.35.0 || >= 1.35.0-gke.2745000",
			wantAlternatives: 2,
			wantTermsInFirst: 2,
		},
		{
			name:             "exact match is a single term",
			expr:             "ubuntu",
			wantAlternatives: 1,
			wantTermsInFirst: 1,
		},
		{
			name:        "empty expression errors",
			expr:        "",
			expectError: true,
		},
		{
			name:        "whitespace-only expression errors",
			expr:        "   ",
			expectError: true,
		},
		{
			name:        "leading || errors (empty first clause)",
			expr:        "|| >= 1.34.3",
			expectError: true,
		},
		{
			name:        "trailing || errors (empty last clause)",
			expr:        ">= 1.34.3 ||",
			expectError: true,
		},
		{
			name:        "consecutive || errors (empty middle clause)",
			expr:        ">= 1.34.3 || || >= 1.35.0",
			expectError: true,
		},
		{
			// Malformed GKE suffix in a term — PropagateOrWrap surfaces the
			// ErrCodeInvalidRequest from ParseConstraintExpression.
			name:        "malformed GKE suffix in compound expr errors",
			expr:        ">= 1.34.3-gke.-1 < 1.35.0 || >= 1.35.0-gke.2745000",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cc, err := ParseCompoundConstraint(tt.expr)
			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cc.Alternatives) != tt.wantAlternatives {
				t.Errorf("Alternatives count = %d, want %d", len(cc.Alternatives), tt.wantAlternatives)
			}
			if len(cc.Alternatives) > 0 && len(cc.Alternatives[0]) != tt.wantTermsInFirst {
				t.Errorf("terms in first alternative = %d, want %d", len(cc.Alternatives[0]), tt.wantTermsInFirst)
			}
			if tt.wantFirstTermOp != "" && len(cc.Alternatives) > 0 && len(cc.Alternatives[0]) > 0 {
				if got := string(cc.Alternatives[0][0].Operator); got != tt.wantFirstTermOp {
					t.Errorf("first term Operator = %q, want %q", got, tt.wantFirstTermOp)
				}
			}
			if tt.wantFirstTermVal != "" && len(cc.Alternatives) > 0 && len(cc.Alternatives[0]) > 0 {
				if got := cc.Alternatives[0][0].Value; got != tt.wantFirstTermVal {
					t.Errorf("first term Value = %q, want %q", got, tt.wantFirstTermVal)
				}
			}
			if cc.String() != strings.TrimSpace(tt.expr) {
				t.Errorf("String() = %q, want %q", cc.String(), strings.TrimSpace(tt.expr))
			}
		})
	}
}

func TestCompoundConstraint_Evaluate(t *testing.T) {
	t.Parallel()

	// The core expression under test: per-track GKE version floors.
	// Track 1.34: must be >= 1.34.3-gke.1318000 and < 1.35.0
	// Track 1.35: must be >= 1.35.0-gke.2745000
	// Any 1.36+ version qualifies.
	const perTrackExpr = ">= 1.34.3-gke.1318000 < 1.35.0 || >= 1.35.0-gke.2745000"

	tests := []struct {
		name    string
		expr    string
		actual  string
		want    bool
		wantErr bool
	}{
		// --- Core GKE bug regression ---
		{
			name:   "1.34.3-gke.1000000 fails per-track floor (GKE build too low)",
			expr:   perTrackExpr,
			actual: "1.34.3-gke.1000000",
			want:   false,
		},
		{
			name:   "1.34.3-gke.1318000 passes per-track floor (exact floor match)",
			expr:   perTrackExpr,
			actual: "1.34.3-gke.1318000",
			want:   true,
		},
		{
			name:   "1.35.0-gke.0001 fails per-track floor (1.35 track build too low)",
			expr:   perTrackExpr,
			actual: "1.35.0-gke.0001",
			want:   false,
		},
		{
			name:   "1.35.0-gke.2745000 passes per-track floor (exact 1.35 track match)",
			expr:   perTrackExpr,
			actual: "1.35.0-gke.2745000",
			want:   true,
		},
		{
			name:   "1.35.0-gke.3000000 passes per-track floor (above 1.35 track floor)",
			expr:   perTrackExpr,
			actual: "1.35.0-gke.3000000",
			want:   true,
		},
		{
			name:   "1.34.5-gke.1318000 passes per-track floor (higher patch in 1.34 track)",
			expr:   perTrackExpr,
			actual: "1.34.5-gke.1318000",
			want:   true,
		},
		// --- Bare actual vs GKE compound (tests leftIsGKE=false, rightIsGKE=true path) ---
		{
			name:   "bare 1.34.3 fails GKE compound floor (no GKE suffix)",
			expr:   perTrackExpr,
			actual: "1.34.3",
			want:   false,
		},
		{
			// 1.34.5 > 1.34.3 numerically, so it satisfies ">= 1.34.3-gke.1318000"
			// despite having no GKE suffix — the patch version alone clears the floor.
			// Only at the exact same patch (1.34.3) does the GKE build number matter.
			name:   "bare 1.34.5 passes GKE compound floor (higher patch clears floor numerically)",
			expr:   perTrackExpr,
			actual: "1.34.5",
			want:   true,
		},
		// --- GKE actual vs bare constraint (tests leftIsGKE=true, rightIsGKE=false path) ---
		{
			name:   "GKE actual passes bare >= constraint",
			expr:   ">= 1.34.3",
			actual: "1.34.3-gke.1318000",
			want:   true,
		},
		// --- Simple single-expression backward compat ---
		{
			name:   "simple >= 1.32.4 passes 1.33.0",
			expr:   ">= 1.32.4",
			actual: "1.33.0",
			want:   true,
		},
		{
			name:   "simple >= 1.32.4 fails 1.31.0",
			expr:   ">= 1.32.4",
			actual: "1.31.0",
			want:   false,
		},
		{
			name:   "exact match ubuntu passes",
			expr:   "ubuntu",
			actual: "ubuntu",
			want:   true,
		},
		{
			name:   "exact match ubuntu fails rhel",
			expr:   "ubuntu",
			actual: "rhel",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cc, err := ParseCompoundConstraint(tt.expr)
			if err != nil {
				t.Fatalf("ParseCompoundConstraint(%q) unexpected error: %v", tt.expr, err)
			}
			got, err := cc.Evaluate(tt.actual)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate(%q) unexpected error: %v", tt.actual, err)
			}
			if got != tt.want {
				t.Errorf("Evaluate(%q) = %v, want %v (expr=%q)", tt.actual, got, tt.want, tt.expr)
			}
		})
	}
}

func TestLooksLikeVersion(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			got := looksLikeVersion(tt.input)
			if got != tt.want {
				t.Errorf("looksLikeVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestParseConstraintExpression_MalformedGKESuffix verifies that constraint
// values with a "-gke." prefix but invalid build number are rejected at parse
// time, preventing Compare() from silently treating them as unordered extras.
func TestParseConstraintExpression_MalformedGKESuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		expr        string
		wantErr     bool
		wantErrCode errors.ErrorCode
	}{
		// Valid GKE suffix — must parse cleanly.
		{name: "valid GKE suffix parses", expr: ">= 1.34.3-gke.1318000", wantErr: false},
		// Negative build number must be rejected with ErrCodeInvalidRequest.
		{name: "negative GKE build rejected", expr: ">= 1.34.3-gke.-1", wantErr: true, wantErrCode: errors.ErrCodeInvalidRequest},
		// Non-numeric GKE suffix must be rejected.
		{name: "non-numeric GKE build rejected", expr: ">= 1.34.3-gke.abc", wantErr: true, wantErrCode: errors.ErrCodeInvalidRequest},
		// Empty build number must be rejected.
		{name: "empty GKE build rejected", expr: ">= 1.34.3-gke.", wantErr: true, wantErrCode: errors.ErrCodeInvalidRequest},
		// Non-GKE suffix is fine — no validation applied.
		{name: "EKS suffix not validated", expr: ">= 1.33.5-eks-3025e55", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseConstraintExpression(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseConstraintExpression(%q) error = %v, wantErr %v", tt.expr, err, tt.wantErr)
			}
			if err != nil && tt.wantErrCode != "" {
				if !stderrors.Is(err, errors.New(tt.wantErrCode, "")) {
					t.Errorf("ParseConstraintExpression(%q) error code = %v, want %v", tt.expr, err, tt.wantErrCode)
				}
			}
		})
	}
}

func TestParsedConstraint_String(t *testing.T) {
	t.Parallel()

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
			t.Parallel()

			got := tt.constraint.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}
