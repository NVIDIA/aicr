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

import "strings"

// gpuSKURegistry maps an upper-cased substring of nvidia-smi's
// ProductName to the recipe.CriteriaAccelerator enum value. First
// match wins, so longer / more specific patterns must come first
// (e.g., "GB200" before "B200" so a Grace-Blackwell node is not
// labeled as a B200).
var gpuSKURegistry = []struct {
	pattern string
	sku     string
}{
	{"GB200", "gb200"},
	{"B200", "b200"},
	{"H100", "h100"},
	{"A100", "a100"},
	{"RTX PRO 6000", "rtx-pro-6000"},
	{"L40", "l40"},
}

// ParseGPUSKU normalizes a raw nvidia-smi ProductName string (e.g.
// "NVIDIA H100 80GB HBM3") to the matching recipe.CriteriaAccelerator
// enum value (e.g. "h100"). Returns "" when the model does not match
// any known SKU; callers treat the empty string as "fingerprint did
// not detect this dimension."
func ParseGPUSKU(model string) string {
	upper := strings.ToUpper(strings.TrimSpace(model))
	if upper == "" {
		return ""
	}
	for _, entry := range gpuSKURegistry {
		if strings.Contains(upper, entry.pattern) {
			return entry.sku
		}
	}
	return ""
}
