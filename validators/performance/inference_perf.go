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

package main

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
)

// checkInferencePerf wraps the inference performance pipeline as a CheckFunc.
// Finds the `inference-throughput` and `inference-ttft-p99` constraints from
// the recipe (one per metric the pipeline produces) and runs AIPerf against
// the discovered inference endpoint.
func checkInferencePerf(ctx *validators.Context) error {
	throughputConstraint, hasThroughput := findPerformanceConstraint(ctx, "inference-throughput")
	ttftConstraint, hasTTFT := findPerformanceConstraint(ctx, "inference-ttft-p99")

	if !hasThroughput && !hasTTFT {
		return validators.Skip("no inference-throughput or inference-ttft-p99 constraint in recipe")
	}

	// validateInferencePerf returns pkg/errors StructuredError values with the
	// right codes already (ErrCodeTimeout for deadline exhaustion inside the
	// pipeline, ErrCodeInternal for infra failures, ErrCodeInvalidRequest for
	// bad recipe thresholds, etc.). Propagate as-is so the container exit code
	// mapping in validators/runner.go picks up the intended classification.
	result, err := validateInferencePerf(ctx)
	if err != nil {
		return err
	}

	if strings.HasPrefix(result.status, "skipped") {
		return validators.Skip(result.status)
	}

	// Prefix key metric lines with "RESULT: " so the validator runtime surfaces
	// them to the CLI's own output (not only the CTRF stdout array). See the
	// summary-line loop in pkg/validator/validator.go — any other check that
	// wants the same treatment adopts the same prefix.
	fmt.Printf("RESULT: Inference throughput: %.2f tokens/sec\n", result.throughput)
	fmt.Printf("RESULT: Inference TTFT p99: %.2f ms\n", result.ttftP99Ms)

	if hasThroughput {
		// The inference-perf evaluator fixes the direction and enforces
		// `throughput >= threshold * 0.9`. Accept only `>=` (what the
		// evaluator actually honors). Reject everything else — strict-greater
		// (`>`), non-directional (`==`, `!=`), bare value, and inverted (`<`,
		// `<=`) — because parseThreshold strips the operator and collapses
		// those forms to the same `>= threshold*0.9` check, silently
		// reinterpreting their written meaning. This is a narrower subset
		// than the general operator contract in data-flow.md; the narrowing
		// is intentional and documented there.
		if err := requireComparatorPrefix(throughputConstraint.Value, ">=", "inference-throughput"); err != nil {
			return err
		}
		threshold, parseErr := parseThreshold(throughputConstraint.Value)
		if parseErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid throughput threshold", parseErr)
		}
		// 10% tolerance, same as NCCL validator
		if result.throughput < threshold*0.9 {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("inference throughput %.2f tokens/sec does not satisfy constraint %q (with 10%% tolerance)",
					result.throughput, throughputConstraint.Value))
		}
		fmt.Printf("Throughput constraint: %s → PASS\n", throughputConstraint.Value)
	}

	if hasTTFT {
		// TTFT p99 evaluator enforces `<= threshold * 1.1`. Same tightening
		// rule as throughput: accept only `<=`, reject everything else.
		if err := requireComparatorPrefix(ttftConstraint.Value, "<=", "inference-ttft-p99"); err != nil {
			return err
		}
		threshold, parseErr := parseThreshold(ttftConstraint.Value)
		if parseErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid TTFT threshold", parseErr)
		}
		// For latency, "<= threshold" means actual must be at or below threshold (+ 10% tolerance)
		if result.ttftP99Ms > threshold*1.1 {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("inference TTFT p99 %.2f ms does not satisfy constraint %q (with 10%% tolerance)",
					result.ttftP99Ms, ttftConstraint.Value))
		}
		fmt.Printf("TTFT p99 constraint: %s → PASS\n", ttftConstraint.Value)
	}

	return nil
}

// requireComparatorPrefix fails fast unless the trimmed constraint value
// starts with exactly `want` (`>=` for at-least metrics, `<=` for at-most)
// AND the character immediately after `want` is not another operator
// character. The boundary check rejects malformed inputs like `>== 5000` or
// `<== 200`: HasPrefix alone would accept them, and `parseThreshold` later
// strips the full `><=!` run and silently coerces the value to a valid
// threshold, which would be exactly the silent-reinterpretation bug the
// exact-prefix tightening is meant to prevent.
//
// The inference-perf evaluator implements a narrower subset of the general
// cluster-constraint operator contract (see data-flow.md "Supported
// Operators"). `parseThreshold` strips any leading operator and the
// evaluator always applies one fixed inclusive check per metric —
// `throughput >= threshold*0.9` or `ttftP99 <= threshold*1.1`. Accepting
// other operators (strict `>` / `<`, non-directional `==` / `!=`, or a
// bare number) would silently reinterpret their written meaning as the
// same inclusive form, which is a correctness trap for authors of custom
// recipes. Enforcing the exact prefix here is the safe, documented subset.
func requireComparatorPrefix(value, want, constraintName string) error {
	v := strings.TrimSpace(value)
	if !strings.HasPrefix(v, want) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s constraint must start with %q (got %q); inference-perf uses a narrower operator subset than the general contract — only the operator the evaluator actually honors is accepted",
				constraintName, want, value))
	}
	// parseThreshold strips a whole leading run of `><=! ` (including spaces)
	// from the numeric part, which means a typo like `>= =5000` or
	// `<=   !200` would be silently coerced to `5000` / `200`. Step past any
	// intervening spaces after the required operator prefix and reject if
	// the next non-space character is another comparator character.
	rest := strings.TrimLeft(v[len(want):], " ")
	if len(rest) > 0 && isComparatorChar(rest[0]) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s constraint must start with exactly %q (got %q); extra operator characters after the expected prefix would be silently stripped by the threshold parser and misread",
				constraintName, want, value))
	}
	return nil
}

// isComparatorChar reports whether b is one of the characters parseThreshold
// treats as a leading-operator-run (`><=!`). Used by the exact-prefix guard
// to detect `>==`, `<==`, `<>`, etc. that would otherwise slip through a
// simple HasPrefix check.
func isComparatorChar(b byte) bool {
	switch b {
	case '>', '<', '=', '!':
		return true
	}
	return false
}
