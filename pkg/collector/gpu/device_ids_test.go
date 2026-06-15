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

package gpu

import "testing"

func TestSKUForDeviceID(t *testing.T) {
	tests := []struct {
		name     string
		deviceID string
		want     string
	}{
		// Datacenter SKUs — representative device IDs per family. The table is
		// a descriptive discovery vocabulary, so it names the real SKU whether
		// or not AICR has recipe coverage for it.
		{"P100", "15f8", "p100"},
		{"P40", "1b38", "p40"},
		{"P4", "1bb3", "p4"},
		{"V100 SXM2", "1db1", "v100"},
		{"V100S", "1df6", "v100s"},
		{"T4", "1eb8", "t4"},
		{"T40", "1e38", "t40"},
		{"A100 SXM4 40GB", "20b0", "a100"},
		{"A100 PCIe 80GB", "20b5", "a100"},
		{"A30", "20b7", "a30"},
		{"A800", "20bd", "a800"},
		{"A40", "2235", "a40"},
		{"A10", "2236", "a10"},
		{"A16", "25b6", "a16"},
		{"H100 SXM5 80GB", "2330", "h100"},
		{"H100 PCIe", "2331", "h100"},
		{"H200 SXM 141GB", "2335", "h200"},
		{"H200 NVL", "233b", "h200"},
		{"H800 PCIe", "2322", "h800"},
		{"H20", "2329", "h20"},
		{"H20 NVL16", "230c", "h20"},
		{"GH200", "2342", "gh200"},
		{"L40", "26b5", "l40"},
		{"L40S", "26b9", "l40s"},
		{"L40G", "26b8", "l40g"},
		{"L20", "26b7", "l20"},
		{"L2", "27b6", "l2"},
		{"L4", "27b8", "l4"},
		{"B200", "2901", "b200"},
		{"HGX B200", "2909", "b200"},
		{"B100", "2920", "b100"},
		{"GB200", "2941", "gb200"},
		{"B300", "3182", "b300"},
		{"GB300", "31c2", "gb300"},
		{"RTX PRO 6000 Workstation", "2bb1", "rtx-pro-6000"},
		{"RTX PRO 6000 Server", "2bb5", "rtx-pro-6000"},

		// Case-insensitive + 0x prefix + whitespace tolerance.
		{"uppercase", "26B9", "l40s"},
		{"0x prefix", "0x2330", "h100"},
		{"0X uppercase prefix", "0X2330", "h100"},
		{"whitespace", "  2330  ", "h100"},

		// Excluded by scope/curation — must resolve to "" (unknown):
		// converged accelerators (own product, distinct from base GPU),
		{"A100X converged", "20b8", ""},
		{"H100 CNX converged", "2313", ""},
		{"L40 CNX converged", "26f5", ""},
		// consumer GeForce / workstation RTX A-series / legacy Quadro,
		{"GeForce RTX 4090", "2684", ""},
		{"RTX A6000 workstation", "2230", ""},
		{"RTX 6000 Ada workstation", "26b1", ""},
		{"Quadro RTX 6000", "1e30", ""},
		// vGPU/automotive,
		{"GRID A100B", "20bf", ""},
		{"DRIVE A100", "20bb", ""},
		// NVSwitch (not a GPU class device),
		{"A100 NVSwitch", "1af1", ""},

		// Edge cases.
		{"empty", "", ""},
		{"unknown id", "ffff", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skuForDeviceID(tt.deviceID); got != tt.want {
				t.Errorf("skuForDeviceID(%q) = %q, want %q", tt.deviceID, got, tt.want)
			}
		})
	}
}
