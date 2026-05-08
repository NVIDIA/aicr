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
	diffs["service"] = matchString(c.Service, f.Service.Value)
	diffs["accelerator"] = matchString(c.Accelerator, f.Accelerator.Value)
	diffs["os"] = matchString(c.OS, f.OS.Value)
	diffs["intent"] = matchUnknownDim(c.Intent)
	diffs["platform"] = matchUnknownDim(c.Platform)
	diffs["nodes"] = matchNodes(c.Nodes, f.NodeCount.Value)

	matched := true
	for _, d := range diffs {
		if d.Match == DimensionMismatched {
			matched = false
			break
		}
	}

	return MatchResult{Matched: matched, PerDimension: diffs}
}

// matchString implements the per-dimension three-way comparison for
// string-valued dimensions.
func matchString(recipeRequires, fingerprintProvides string) DimensionDiff {
	diff := DimensionDiff{
		RecipeRequires:      recipeRequires,
		FingerprintProvides: fingerprintProvides,
	}
	if isAny(recipeRequires) {
		diff.Match = DimensionMatched
		return diff
	}
	if isAny(fingerprintProvides) {
		diff.Match = DimensionUnknown
		return diff
	}
	if recipeRequires == fingerprintProvides {
		diff.Match = DimensionMatched
		return diff
	}
	diff.Match = DimensionMismatched
	return diff
}

// matchUnknownDim handles criteria fields the fingerprint deliberately
// does not capture (intent, platform). The recipe is matched when
// generic; a specific value is unknown — the maintainer reviews it
// without the fingerprint contradicting the bundle.
func matchUnknownDim(recipeRequires string) DimensionDiff {
	diff := DimensionDiff{RecipeRequires: recipeRequires}
	if isAny(recipeRequires) {
		diff.Match = DimensionMatched
		return diff
	}
	diff.Match = DimensionUnknown
	return diff
}

// matchNodes implements the per-dimension comparison for node count.
// Zero on either side is treated as "any."
func matchNodes(recipeRequires, fingerprintProvides int) DimensionDiff {
	diff := DimensionDiff{
		RecipeRequires:      strconv.Itoa(recipeRequires),
		FingerprintProvides: strconv.Itoa(fingerprintProvides),
	}
	if recipeRequires == 0 {
		diff.Match = DimensionMatched
		return diff
	}
	if fingerprintProvides == 0 {
		diff.Match = DimensionUnknown
		return diff
	}
	if recipeRequires == fingerprintProvides {
		diff.Match = DimensionMatched
		return diff
	}
	diff.Match = DimensionMismatched
	return diff
}

// isAny reports whether a string criteria field is unset or wildcarded.
func isAny(v string) bool {
	return v == "" || v == criteriaAnyValue
}
