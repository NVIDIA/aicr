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

package fingerprint

import "strconv"

// criteriaAnyValue mirrors the wildcard literal that pkg/recipe uses
// for criteria fields. Kept as a local constant so this package does
// not depend on an unexported symbol from pkg/recipe.
const criteriaAnyValue = "any"

// Match compares the fingerprint against a recipe's criteria and
// returns a per-dimension diff plus an overall Matched flag.
//
// Per-dimension semantics:
//   - Recipe value is empty / "any"  → matched (recipe is generic).
//   - Recipe value is specific, fingerprint did not capture →
//     unknown (the fingerprint cannot prove a match, but cannot
//     disprove one either).
//   - Recipe value is specific, fingerprint captured the same value
//     → matched.
//   - Recipe value is specific, fingerprint captured a different
//     value → mismatched.
//
// Overall Matched is true when no dimension is mismatched. Unknown
// dimensions surface in PerDimension for human review without
// flipping the overall outcome.
//
// Criteria fields that the cluster cannot reveal — Intent and
// Platform — are reported as unknown when the recipe declares a
// specific value, and matched when the recipe is generic. The
// fingerprint deliberately does not attempt to fabricate them.
func (f *Fingerprint) Match(c CriteriaInput) MatchResult {
	if f == nil {
		f = &Fingerprint{}
	}

	diffs := make(map[string]DimensionDiff, 6)
	diffs["service"] = matchDim(c.Service, f.Service.Value, !isAny(f.Service.Value))
	diffs["accelerator"] = matchDim(c.Accelerator, f.Accelerator.Value, !isAny(f.Accelerator.Value))
	diffs["os"] = matchDim(c.OS, f.OS.Value, !isAny(f.OS.Value))
	diffs["intent"] = matchDim(c.Intent, "", false)
	diffs["platform"] = matchDim(c.Platform, "", false)
	// Nodes uses 0 as the "any" sentinel; remap to "" so isAny in
	// matchDim treats it as a wildcard.
	recipeNodes, fpNodes := strconv.Itoa(c.Nodes), strconv.Itoa(f.NodeCount.Value)
	if c.Nodes == 0 {
		recipeNodes = ""
	}
	diffs["nodes"] = matchDim(recipeNodes, fpNodes, f.NodeCount.Value != 0)

	matched := true
	for _, d := range diffs {
		if d.Match == DimensionMismatched {
			matched = false
			break
		}
	}

	return MatchResult{Matched: matched, PerDimension: diffs}
}

// matchDim is the shared three-way comparison. fingerprintCaptured is
// false when the fingerprint did not detect this dimension (either by
// design — intent and platform — or by signal absence).
func matchDim(recipeRequires, fingerprintProvides string, fingerprintCaptured bool) DimensionDiff {
	diff := DimensionDiff{
		RecipeRequires:      recipeRequires,
		FingerprintProvides: fingerprintProvides,
	}
	switch {
	case isAny(recipeRequires):
		diff.Match = DimensionMatched
	case !fingerprintCaptured:
		diff.Match = DimensionUnknown
	case recipeRequires == fingerprintProvides:
		diff.Match = DimensionMatched
	default:
		diff.Match = DimensionMismatched
	}
	return diff
}

func isAny(v string) bool {
	return v == "" || v == criteriaAnyValue
}
