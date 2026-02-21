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

package validator

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/version"
)

// Operator represents a comparison operator in constraint expressions.
type Operator string

const (
	// OperatorGTE represents ">=" (greater than or equal).
	OperatorGTE Operator = ">="

	// OperatorLTE represents "<=" (less than or equal).
	OperatorLTE Operator = "<="

	// OperatorGT represents ">" (greater than).
	OperatorGT Operator = ">"

	// OperatorLT represents "<" (less than).
	OperatorLT Operator = "<"

	// OperatorEQ represents "==" (exact match).
	OperatorEQ Operator = "=="

	// OperatorNE represents "!=" (not equal).
	OperatorNE Operator = "!="

	// OperatorExact represents no operator (exact string match).
	OperatorExact Operator = ""
)

// ParsedConstraint represents a parsed constraint expression.
type ParsedConstraint struct {
	// Operator is the comparison operator (or empty for exact match).
	Operator Operator

	// Value is the expected value after the operator.
	Value string

	// IsVersionComparison indicates if this should be treated as a version comparison.
	IsVersionComparison bool
}

// ParseConstraintExpression parses a constraint value expression.
// Examples:
//   - ">= 1.32.4" -> {Operator: ">=", Value: "1.32.4", IsVersionComparison: true}
//   - "ubuntu" -> {Operator: "", Value: "ubuntu", IsVersionComparison: false}
//   - "== 24.04" -> {Operator: "==", Value: "24.04", IsVersionComparison: false}
func ParseConstraintExpression(expr string) (*ParsedConstraint, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "constraint expression cannot be empty")
	}

	pc := &ParsedConstraint{}

	// Check for operators (longest first to avoid matching ">" when ">=" is intended)
	operators := []Operator{OperatorGTE, OperatorLTE, OperatorNE, OperatorEQ, OperatorGT, OperatorLT}
	for _, op := range operators {
		if strings.HasPrefix(expr, string(op)) {
			pc.Operator = op
			pc.Value = strings.TrimSpace(strings.TrimPrefix(expr, string(op)))
			break
		}
	}

	// If no operator found, treat as exact match
	if pc.Operator == "" {
		pc.Operator = OperatorExact
		pc.Value = expr
	}

	if pc.Value == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "constraint value cannot be empty after operator")
	}

	// Determine if this is a version comparison (operators other than exact match with version-like value)
	if pc.Operator != OperatorExact && pc.Operator != OperatorEQ && pc.Operator != OperatorNE {
		pc.IsVersionComparison = true
	} else if looksLikeVersion(pc.Value) {
		pc.IsVersionComparison = true
	}

	return pc, nil
}

// looksLikeVersion returns true if the value appears to be a version string.
func looksLikeVersion(s string) bool {
	// Simple heuristic: contains digits and dots, possibly with 'v' prefix
	s = strings.TrimPrefix(s, "v")
	if len(s) == 0 {
		return false
	}
	// Must start with a digit and contain at least one dot
	hasDigit := false
	hasDot := false
	for _, c := range s {
		if c >= '0' && c <= '9' {
			hasDigit = true
		}
		if c == '.' {
			hasDot = true
		}
	}
	return hasDigit && hasDot
}

// Evaluate evaluates the constraint against an actual value.
// Returns true if the constraint is satisfied, false otherwise.
func (pc *ParsedConstraint) Evaluate(actual string) (bool, error) {
	actual = strings.TrimSpace(actual)

	switch pc.Operator {
	case OperatorExact:
		// Exact string match (case-sensitive)
		return actual == pc.Value, nil

	case OperatorEQ:
		// Explicit equality - try version comparison first, fall back to string
		if pc.IsVersionComparison {
			expectedVer, err := version.ParseVersion(pc.Value)
			if err == nil {
				actualVer, err := version.ParseVersion(actual)
				if err == nil {
					return expectedVer.Equals(actualVer), nil
				}
			}
		}
		return actual == pc.Value, nil

	case OperatorNE:
		// Not equal - try version comparison first, fall back to string
		if pc.IsVersionComparison {
			expectedVer, err := version.ParseVersion(pc.Value)
			if err == nil {
				actualVer, err := version.ParseVersion(actual)
				if err == nil {
					return !expectedVer.Equals(actualVer), nil
				}
			}
		}
		return actual != pc.Value, nil

	case OperatorGTE, OperatorGT, OperatorLTE, OperatorLT:
		// Version comparison required
		expectedVer, err := version.ParseVersion(pc.Value)
		if err != nil {
			return false, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
				"cannot parse expected version", err, map[string]any{"version": pc.Value})
		}

		actualVer, err := version.ParseVersion(actual)
		if err != nil {
			return false, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
				"cannot parse actual version", err, map[string]any{"version": actual})
		}

		cmp := actualVer.Compare(expectedVer)

		//nolint:exhaustive // Only comparison operators reach this point; EQ, NE, Exact are handled above
		switch pc.Operator {
		case OperatorGTE:
			return cmp >= 0, nil
		case OperatorGT:
			return cmp > 0, nil
		case OperatorLTE:
			return cmp <= 0, nil
		case OperatorLT:
			return cmp < 0, nil
		default:
			// This shouldn't happen as this case only handles comparison operators
			return false, errors.NewWithContext(errors.ErrCodeInternal,
				"unexpected operator in version comparison", map[string]any{"operator": pc.Operator})
		}
	default:
		return false, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"unknown operator", map[string]any{"operator": pc.Operator})
	}
}

// String returns a string representation of the parsed constraint.
func (pc *ParsedConstraint) String() string {
	if pc.Operator == OperatorExact {
		return pc.Value
	}
	return fmt.Sprintf("%s %s", pc.Operator, pc.Value)
}
