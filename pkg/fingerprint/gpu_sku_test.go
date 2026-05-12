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

import "testing"

func TestParseGPUSKU(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{"H100 80GB HBM3", "NVIDIA H100 80GB HBM3", "h100"},
		{"H100 PCIe", "NVIDIA H100 PCIe", "h100"},
		{"GB200 NVL72", "NVIDIA GB200", "gb200"},
		{"GB200 wins over B200 substring", "NVIDIA GB200 NVL72", "gb200"},
		{"B200", "NVIDIA B200", "b200"},
		{"A100 40GB", "NVIDIA A100-SXM4-40GB", "a100"},
		{"A100 80GB", "NVIDIA A100 80GB PCIe", "a100"},
		{"L40", "NVIDIA L40", "l40"},
		{"L40S", "NVIDIA L40S", "l40"},
		{"RTX PRO 6000", "NVIDIA RTX PRO 6000 Blackwell", "rtx-pro-6000"},
		{"lowercase product name", "nvidia h100 80gb hbm3", "h100"},
		{"trims whitespace", "  NVIDIA H100  ", "h100"},
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"unknown SKU", "NVIDIA T4", ""},
		{"non-NVIDIA", "AMD MI300X", ""},
		{"random garbage", "not-a-gpu", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseGPUSKU(tt.model); got != tt.want {
				t.Errorf("ParseGPUSKU(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}
