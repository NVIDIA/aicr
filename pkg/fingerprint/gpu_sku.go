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

import (
	"strings"
	"unicode"
)

// gpuSKURegistry maps a sequence of upper-cased product-name tokens to
// the recipe.CriteriaAccelerator enum value. Matching is performed on
// whole-token boundaries (see ParseGPUSKU), not raw substrings, so a
// SKU is never collapsed onto a shorter one whose name it merely
// contains: "L40S" is not read as L40, "RTX A1000" is not read as
// A100, and "GB200"/"GH200" are not read as B200/H200. Because tokens
// match exactly, entry order is not significant for correctness, and
// no per-collision ordering or exclusion workarounds are needed.
//
// SKUs we do not model — e.g. GH200, A800, H800, L4, RTX A1000 — match
// no entry and resolve to "" (treated by callers as "SKU not
// recognized"), which keeps the underlying unknown-sku signal intact.
var gpuSKURegistry = []struct {
	tokens []string
	sku    string
}{
	{[]string{"GB300"}, "gb300"},
	{[]string{"GB200"}, "gb200"},
	{[]string{"B200"}, "b200"},
	{[]string{"H100"}, "h100"},
	{[]string{"H200"}, "h200"},
	{[]string{"A100"}, "a100"},
	{[]string{"RTX", "PRO", "6000"}, "rtx-pro-6000"},
	{[]string{"L40S"}, "l40s"},
	{[]string{"L40"}, "l40"},
}

// ParseGPUSKU normalizes a raw nvidia-smi ProductName (e.g. "NVIDIA
// H100 80GB HBM3") or NFD topology label (e.g. "NVIDIA-L40S") to the
// matching recipe.CriteriaAccelerator enum value (e.g. "h100"). The
// model is upper-cased and split into tokens on whitespace and hyphens,
// then matched against gpuSKURegistry on contiguous token sequences.
// Returns "" when the model matches no known SKU; callers treat the
// empty string as "fingerprint did not detect this dimension."
func ParseGPUSKU(model string) string {
	tokens := tokenizeProductName(model)
	if len(tokens) == 0 {
		return ""
	}
	for _, entry := range gpuSKURegistry {
		if containsTokenSequence(tokens, entry.tokens) {
			return entry.sku
		}
	}
	return ""
}

// tokenizeProductName upper-cases model and splits it into tokens on
// runs of whitespace and hyphens. Hyphens are delimiters so that both
// nvidia-smi's space-separated names ("NVIDIA A100 80GB") and NFD's
// hyphenated labels ("NVIDIA-A100-80GB") tokenize identically.
func tokenizeProductName(model string) []string {
	return strings.FieldsFunc(strings.ToUpper(model), func(r rune) bool {
		return unicode.IsSpace(r) || r == '-'
	})
}

// containsTokenSequence reports whether seq appears as a contiguous run
// of tokens within tokens.
func containsTokenSequence(tokens, seq []string) bool {
	if len(seq) == 0 || len(seq) > len(tokens) {
		return false
	}
	for i := 0; i+len(seq) <= len(tokens); i++ {
		match := true
		for j, want := range seq {
			if tokens[i+j] != want {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
