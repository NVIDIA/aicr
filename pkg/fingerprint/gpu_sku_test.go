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
		// --- Supported SKUs: nvidia-smi (space-separated) product names ---
		// H100 variants.
		{"H100 80GB HBM3", "NVIDIA H100 80GB HBM3", "h100"},
		{"H100 PCIe", "NVIDIA H100 PCIe", "h100"},
		{"H100 NVL", "NVIDIA H100 NVL", "h100"},
		{"H100 bare", "NVIDIA H100", "h100"},
		// H200 variants.
		{"H200 bare", "NVIDIA H200", "h200"},
		{"H200 NVL", "NVIDIA H200 NVL", "h200"},
		{"H200 141GB HBM3e", "NVIDIA H200 141GB HBM3e", "h200"},
		// A100 variants (SXM, PCIe, hyphenated suffixes).
		{"A100 SXM4 40GB", "NVIDIA A100-SXM4-40GB", "a100"},
		{"A100 SXM4 80GB", "NVIDIA A100-SXM4-80GB", "a100"},
		{"A100 PCIE 40GB", "NVIDIA A100-PCIE-40GB", "a100"},
		{"A100 80GB PCIe", "NVIDIA A100 80GB PCIe", "a100"},
		// B200 variants.
		{"B200 bare", "NVIDIA B200", "b200"},
		{"HGX B200", "NVIDIA HGX B200", "b200"},
		// GB200 variants (distinct token; no ordering dependency — "GB200" ≠ "B200" under exact-token matching).
		{"GB200 bare", "NVIDIA GB200", "gb200"},
		{"GB200 NVL72", "NVIDIA GB200 NVL72", "gb200"},
		{"GB200 Grace Blackwell", "NVIDIA GB200 Grace Blackwell Superchip", "gb200"},
		// L40.
		{"L40 bare", "NVIDIA L40", "l40"},
		// RTX PRO 6000 (multi-token; editions append trailing words).
		{"RTX PRO 6000 Blackwell", "NVIDIA RTX PRO 6000 Blackwell", "rtx-pro-6000"},
		{"RTX PRO 6000 Server Edition", "NVIDIA RTX PRO 6000 Blackwell Server Edition", "rtx-pro-6000"},
		{"RTX PRO 6000 Workstation Edition", "NVIDIA RTX PRO 6000 Blackwell Workstation Edition", "rtx-pro-6000"},

		// --- Supported SKUs: NFD hyphenated-label forms (nvidia.com/gpu.product) ---
		// NFD replaces spaces with hyphens; token splitting must yield the
		// same result as the space-separated nvidia-smi form.
		{"label H100", "NVIDIA-H100-80GB-HBM3", "h100"},
		{"label H100 PCIe", "NVIDIA-H100-PCIe", "h100"},
		{"label H200", "NVIDIA-H200", "h200"},
		{"label A100 SXM4 80GB", "NVIDIA-A100-SXM4-80GB", "a100"},
		{"label B200", "NVIDIA-B200", "b200"},
		{"label GB200 NVL72", "NVIDIA-GB200-NVL72", "gb200"},
		{"label L40", "NVIDIA-L40", "l40"},
		{"label RTX PRO 6000", "NVIDIA-RTX-PRO-6000-Blackwell-Server-Edition", "rtx-pro-6000"},

		// --- Collision guards: distinct SKUs sharing a substring with a ---
		// --- supported one must NOT be misclassified (the bug this fixes) ---
		// GH200 (Grace Hopper) vs H200 / H100.
		{"GH200 480GB", "NVIDIA GH200 480GB", ""},
		{"GH200 bare", "NVIDIA GH200", ""},
		{"GH200 144GB HBM3e", "NVIDIA GH200 144GB HBM3e", ""},
		{"GH200 label", "NVIDIA-GH200-480GB", ""},
		// L40S / L4 / L20 / L2 vs L40.
		{"L40S", "NVIDIA L40S", ""},
		{"L40S label", "NVIDIA-L40S", ""},
		{"L4", "NVIDIA L4", ""},
		{"L20", "NVIDIA L20", ""},
		{"L2", "NVIDIA L2", ""},
		// A800 / A40 / A30 / A16 / A10 / A2 vs A100.
		{"A800 80GB PCIe", "NVIDIA A800 80GB PCIe", ""},
		{"A800 SXM4", "NVIDIA A800-SXM4-80GB", ""},
		{"A40", "NVIDIA A40", ""},
		{"A30", "NVIDIA A30", ""},
		{"A16", "NVIDIA A16", ""},
		{"A10", "NVIDIA A10", ""},
		{"A2", "NVIDIA A2", ""},
		// H800 / H20 (export SKUs) vs H100 / H200.
		{"H800", "NVIDIA H800", ""},
		{"H20", "NVIDIA H20", ""},
		// B100 vs B200.
		{"B100", "NVIDIA B100", ""},
		// RTX Ampere workstation cards vs A100 (RTX A1000 substring-matched A100).
		{"RTX A1000", "NVIDIA RTX A1000", ""},
		{"RTX A1000 Laptop", "NVIDIA RTX A1000 Laptop GPU", ""},
		{"RTX A2000", "NVIDIA RTX A2000", ""},
		{"RTX A4000", "NVIDIA RTX A4000", ""},
		{"RTX A5000", "NVIDIA RTX A5000", ""},
		{"RTX A6000", "NVIDIA RTX A6000", ""},
		// Other "RTX ... 6000" cards lacking the PRO token vs RTX PRO 6000.
		{"RTX 6000 Ada", "NVIDIA RTX 6000 Ada Generation", ""},
		{"Quadro RTX 6000", "Quadro RTX 6000", ""},
		// GeForce consumer cards.
		{"GeForce RTX 4090", "NVIDIA GeForce RTX 4090", ""},
		{"GeForce RTX 5090", "NVIDIA GeForce RTX 5090", ""},
		// Older datacenter SKUs (no enum coverage).
		{"V100 SXM2", "Tesla V100-SXM2-16GB", ""},
		{"V100 PCIE", "Tesla V100-PCIE-32GB", ""},
		{"T4", "Tesla T4", ""},
		{"P100", "Tesla P100-PCIE-16GB", ""},
		// Grace CPU (no GPU SKU).
		{"Grace CPU", "NVIDIA Grace", ""},

		// --- Normalization and edge cases ---
		{"lowercase product name", "nvidia h100 80gb hbm3", "h100"},
		{"leading/trailing whitespace", "  NVIDIA H100  ", "h100"},
		{"collapsed double spaces", "NVIDIA  H100", "h100"},
		{"empty string", "", ""},
		{"whitespace only", "   ", ""},
		{"non-NVIDIA AMD", "AMD MI300X", ""},
		{"non-NVIDIA Intel", "Intel Gaudi 2", ""},
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
