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
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

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
	if strings.HasPrefix(expr, "!") && !strings.HasPrefix(expr, string(OperatorNE)) {
		next, size := utf8.DecodeRuneInString(expr[1:])
		if size > 0 && !unicode.IsSpace(next) {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "bare '!' prefix is invalid; use '!=' or the separate node-set form")
		}
	}

	pc := &ParsedConstraint{}

	// Check for operators (longest first to avoid matching ">" when ">=" is intended).
	// A lone "=" is treated as an alias for "==" so the parse-time operator set
	// agrees with isOperatorStart (which must include "=" to split "==" in AND clauses).
	operators := []Operator{OperatorGTE, OperatorLTE, OperatorNE, OperatorEQ, OperatorGT, OperatorLT, "="}
	for _, op := range operators {
		if strings.HasPrefix(expr, string(op)) {
			matchedPrefix := string(op) // preserve the matched length before aliasing
			if op == "=" {
				op = OperatorEQ // lone "=" is an alias for "=="
			}
			pc.Operator = op
			pc.Value = strings.TrimSpace(strings.TrimPrefix(expr, matchedPrefix))
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

	// Reject a version constraint value whose Extras carry a "-gke." prefix but
	// an invalid (e.g. negative or non-numeric) build number. ExtractGKEBuild
	// returns (0, false) for such values, so Compare() treats the constraint
	// as an unordered extra and fall through to return 0 — allowing a bare
	// version to satisfy ">= 1.34.3-gke.-1" incorrectly.
	// Scoped to GTE/GT/LTE/LT only: EQ and NE call Equals() (ignores Extras)
	// and Exact does a raw string compare, so the GKE build number is never
	// consulted for those operators.
	isComparisonOp := pc.Operator == OperatorGTE || pc.Operator == OperatorGT ||
		pc.Operator == OperatorLTE || pc.Operator == OperatorLT
	if isComparisonOp {
		if parsed, err := version.ParseVersion(pc.Value); err == nil {
			if version.HasRawGKESuffix(parsed.Extras) {
				if _, ok := version.ExtractGKEBuild(parsed.Extras); !ok {
					return nil, errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("constraint value %q has a malformed GKE build suffix (must be -gke.N with N >= 0)", pc.Value))
				}
			}
		}
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

// CompoundConstraint is a disjunction (OR) of conjunction (AND) groups of
// ParsedConstraints. It evaluates as true when at least one AND group is fully
// satisfied. This supports per-track GKE version floors such as:
//
//	>= 1.34.3-gke.1318000 < 1.35.0 || >= 1.35.0-gke.2745000
//
// which the single-expression ParseConstraintExpression cannot represent.
//
// No overlay or mixin emits compound expressions yet; this is groundwork for
// per-track GB300 GKE version floors. See #1985.
type CompoundConstraint struct {
	// Alternatives holds the OR clauses; each inner slice is an AND group.
	Alternatives [][]ParsedConstraint

	// raw is the original expression string, preserved for String().
	raw string
}

// splitOrClauses splits an expression on "||" and returns the trimmed clauses.
// It fails closed: any empty clause (produced by leading, trailing, or
// consecutive "||") is treated as a malformed expression and an error is
// returned so recipe authors are not silently left with a weaker constraint.
func splitOrClauses(expr string) ([]string, error) {
	parts := strings.Split(expr, "||")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"constraint expression contains an empty OR clause: check for leading, trailing, or consecutive pipes")
		}
		result = append(result, p)
	}
	return result, nil
}

// isOperatorStart reports whether the byte is the opening character of any
// comparison operator (">", "<", "!", "=").
func isOperatorStart(c byte) bool {
	return c == '>' || c == '<' || c == '!' || c == '='
}

// splitAndTerms splits a single OR clause into individual constraint terms.
// A new term begins wherever whitespace is immediately followed by an operator
// character, allowing "range AND" expressions such as:
//
//	>= 1.34.3-gke.1318000 < 1.35.0   →   [">= 1.34.3-gke.1318000", "< 1.35.0"]
//
// Exact-match terms with no operator prefix are returned as a single element.
func splitAndTerms(clause string) ([]string, error) {
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "AND clause cannot be empty")
	}

	var terms []string
	start := 0
	i := 0
	for i < len(clause) {
		if clause[i] == ' ' || clause[i] == '\t' {
			// Scan past whitespace to see if an operator follows.
			j := i
			for j < len(clause) && (clause[j] == ' ' || clause[j] == '\t') {
				j++
			}
			if j < len(clause) && isOperatorStart(clause[j]) {
				// Term boundary: emit what we have so far.
				if term := strings.TrimSpace(clause[start:i]); term != "" {
					terms = append(terms, term)
				}
				start = j
				i = j
				continue
			}
		}
		i++
	}
	// Emit the last (or only) term.
	if term := strings.TrimSpace(clause[start:]); term != "" {
		terms = append(terms, term)
	}
	return terms, nil
}

// ParseCompoundConstraint parses a compound constraint expression that may
// contain OR clauses ("||") and AND groups (space-separated sub-expressions).
// Simple single-term expressions ("ubuntu", ">= 1.32.4") are handled as a
// degenerate case with one alternative containing one term.
//
// Examples:
//
//	">= 1.32.4"
//	">= 1.34.3-gke.1318000 < 1.35.0 || >= 1.35.0-gke.2745000"
func ParseCompoundConstraint(expr string) (*CompoundConstraint, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "constraint expression cannot be empty")
	}

	orClauses, err := splitOrClauses(expr)
	if err != nil {
		return nil, err
	}

	cc := &CompoundConstraint{
		Alternatives: make([][]ParsedConstraint, 0, len(orClauses)),
		raw:          expr,
	}

	for _, clause := range orClauses {
		terms, err := splitAndTerms(clause)
		if err != nil {
			// PropagateOrWrap preserves the inner ErrCodeInvalidRequest code;
			// the fallback message is unreachable since splitAndTerms always
			// returns a *StructuredError, but documents intent for future callers.
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid AND clause in constraint")
		}
		andGroup := make([]ParsedConstraint, 0, len(terms))
		for _, term := range terms {
			pc, err := ParseConstraintExpression(term)
			if err != nil {
				// Same: fallback message unreachable; preserves ErrCodeInvalidRequest.
				return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid constraint term")
			}
			andGroup = append(andGroup, *pc)
		}
		cc.Alternatives = append(cc.Alternatives, andGroup)
	}

	return cc, nil
}

// Evaluate evaluates the compound constraint against an actual value.
// Returns true if at least one OR alternative is fully satisfied (all AND
// terms in that group pass). If any individual term evaluation returns an
// error, the error is propagated immediately (fail-closed).
func (cc *CompoundConstraint) Evaluate(actual string) (bool, error) {
	for _, andGroup := range cc.Alternatives {
		// An empty AND group (unreachable via ParseCompoundConstraint but possible
		// under direct struct construction) must not fail open.
		if len(andGroup) == 0 {
			continue
		}
		groupPassed := true
		for i := range andGroup {
			passed, err := andGroup[i].Evaluate(actual)
			if err != nil {
				return false, err
			}
			if !passed {
				groupPassed = false
				break
			}
		}
		if groupPassed {
			return true, nil
		}
	}
	return false, nil
}

// String returns the original expression string.
func (cc *CompoundConstraint) String() string {
	return cc.raw
}
