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

import "github.com/NVIDIA/aicr/pkg/constraints/expr"

// The expression grammar lives in the leaf package pkg/constraints/expr so
// pkg/recipe can parse constraint values without importing this package,
// which imports pkg/recipe. These aliases keep the grammar reachable at its
// original import path for every existing caller.

// Operator represents a comparison operator in constraint expressions.
type Operator = expr.Operator

// ParsedConstraint represents a parsed constraint expression.
type ParsedConstraint = expr.ParsedConstraint

// CompoundConstraint represents a constraint expression with OR alternatives
// of AND-joined terms.
type CompoundConstraint = expr.CompoundConstraint

const (
	// OperatorGTE represents ">=" (greater than or equal).
	OperatorGTE = expr.OperatorGTE

	// OperatorLTE represents "<=" (less than or equal).
	OperatorLTE = expr.OperatorLTE

	// OperatorGT represents ">" (greater than).
	OperatorGT = expr.OperatorGT

	// OperatorLT represents "<" (less than).
	OperatorLT = expr.OperatorLT

	// OperatorEQ represents "==" (exact match).
	OperatorEQ = expr.OperatorEQ

	// OperatorNE represents "!=" (not equal).
	OperatorNE = expr.OperatorNE

	// OperatorExact represents no operator (exact string match).
	OperatorExact = expr.OperatorExact
)

// ParseConstraintExpression parses a constraint value expression.
// Examples:
//   - ">= 1.32.4" -> {Operator: ">=", Value: "1.32.4", IsVersionComparison: true}
//   - "ubuntu" -> {Operator: "", Value: "ubuntu", IsVersionComparison: false}
//   - "== 24.04" -> {Operator: "==", Value: "24.04", IsVersionComparison: true}
func ParseConstraintExpression(expression string) (*ParsedConstraint, error) {
	return expr.ParseConstraintExpression(expression)
}

// ParseCompoundConstraint parses a compound constraint expression that may
// contain OR clauses ("||") and AND groups (space-separated sub-expressions).
//
// Example: ">= 1.34.3-gke.1318000 < 1.35.0 || >= 1.35.0-gke.2745000"
func ParseCompoundConstraint(expression string) (*CompoundConstraint, error) {
	return expr.ParseCompoundConstraint(expression)
}
