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

// Package constraints parses and evaluates constraint expressions
// (e.g. ">= 1.32.4", "== ubuntu", "1.33.5") against measurement values
// extracted from a snapshot.
//
// Constraints follow a "<operator> <value>" grammar. The supported
// operators are >=, <=, >, <, ==, !=, and the empty operator (exact
// string match). The IsVersionComparison flag selects semver-aware
// ordering via pkg/version; without it comparisons use lexical ordering.
//
// Two entry points are exposed:
//
//   - Parse(expr) returns a ParsedConstraint that callers can evaluate
//     against arbitrary values.
//   - Evaluate(c, snap) walks the snapshot, extracts the named measurement
//     reading, and reports a Result describing whether the constraint
//     passed and why.
//
// One constraint name carries a non-scalar form: the node-set constraint
// GPUNodesLabelConstraintName ("NodeTopology.gpu-nodes.label", issue #1755)
// quantifies a label predicate over the snapshot's GPU-node set instead of
// comparing a single reading. Its value grammar is "key=value" (every GPU
// node carries the label) or "!key" (no GPU node carries the key); it is
// dispatched by exact name before the scalar path and fails closed on
// truncated node lists and on an empty GPU-node universe.
//
// Evaluation is deliberately side-effect free and never performs network
// or cluster I/O; consumers (pkg/validator, pkg/recipe) supply the
// snapshot context.
package constraints
