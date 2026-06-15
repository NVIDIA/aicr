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

import "strings"

// Normalized accelerator SKU tokens. This is a *descriptive* discovery
// vocabulary, intentionally broader than pkg/recipe's supported-accelerator
// enum: it names the actual datacenter GPU so the snapshot is accurate, while
// recipe matching still enforces support separately (an unsupported SKU is
// parsed down to "any" by pkg/fingerprint.ToCriteria). pkg/fingerprint's name
// normalizer (ParseGPUSKU) emits the same vocabulary from product-name strings
// so the PCI and GFD-label discovery paths agree.
const (
	skuA10        = "a10"
	skuA16        = "a16"
	skuA30        = "a30"
	skuA40        = "a40"
	skuA100       = "a100"
	skuA800       = "a800"
	skuB100       = "b100"
	skuB200       = "b200"
	skuB300       = "b300"
	skuGB200      = "gb200"
	skuGB300      = "gb300"
	skuGH200      = "gh200"
	skuH20        = "h20"
	skuH100       = "h100"
	skuH200       = "h200"
	skuH800       = "h800"
	skuL2         = "l2"
	skuL4         = "l4"
	skuL20        = "l20"
	skuL40        = "l40"
	skuL40G       = "l40g"
	skuL40S       = "l40s"
	skuP4         = "p4"
	skuP6         = "p6"
	skuP10        = "p10"
	skuP40        = "p40"
	skuP100       = "p100"
	skuT4         = "t4"
	skuT10        = "t10"
	skuT40        = "t40"
	skuV100       = "v100"
	skuV100S      = "v100s"
	skuRTXPro6000 = "rtx-pro-6000"
)

// gpuDeviceIDToSKU maps an NVIDIA PCI device ID (lower-case 4-hex, vendor
// 0x10de) to a normalized accelerator SKU. It enables driver-free SKU
// identification from PCI enumeration alone — no nvidia-smi, no GFD label —
// so the fingerprint can name the accelerator on day-0 nodes before the GPU
// Operator has labeled them.
//
// Scope is modern *datacenter* GPUs (Pascal→Blackwell) plus the workstation
// RTX PRO 6000 (a recipe-supported accelerator). Consumer (GeForce), legacy
// Quadro/Tesla, Tegra, vGPU/GRID, automotive (DRIVE), and converged variants
// (A100X, H100 CNX, L40 CNX) are intentionally excluded and fall through to
// "unknown-sku".
//
// Entries are sourced from the canonical pci.ids database (the data lspci
// uses). Maintenance: add an entry only when NVIDIA ships a new datacenter
// SKU or board/device ID. Unknown device IDs are safe — they map to "" and
// the fingerprint records unknown-sku rather than guessing.
var gpuDeviceIDToSKU = map[string]string{
	// Pascal (datacenter).
	"15f7": skuP100, // Tesla P100 PCIe 12GB
	"15f8": skuP100, // Tesla P100 PCIe 16GB
	"15f9": skuP100, // Tesla P100 SXM2 16GB
	"1b38": skuP40,  // Tesla P40
	"1b39": skuP10,  // Tesla P10
	"1bb3": skuP4,   // Tesla P4
	"1bb4": skuP6,   // Tesla P6

	// Volta.
	"1db1": skuV100,  // Tesla V100 SXM2 16GB
	"1db4": skuV100,  // Tesla V100 PCIe 16GB
	"1db5": skuV100,  // Tesla V100 SXM2 32GB
	"1db6": skuV100,  // Tesla V100 PCIe 32GB
	"1db8": skuV100,  // Tesla V100 SXM3 32GB
	"1df5": skuV100,  // Tesla V100 SXM2 16GB
	"1df6": skuV100S, // Tesla V100S PCIe 32GB

	// Turing (datacenter).
	"1e35": skuT10, // Tesla T10
	"1e38": skuT40, // Tesla T40 24GB
	"1eb8": skuT4,  // Tesla T4

	// Ampere (datacenter).
	"20b0": skuA100, // A100 SXM4 40GB
	"20b1": skuA100, // A100 PCIe 40GB
	"20b2": skuA100, // A100 SXM4 80GB
	"20b3": skuA100, // A100-SXM-64GB
	"20b5": skuA100, // A100 PCIe 80GB
	"20f1": skuA100, // A100 PCIe 40GB
	"20b7": skuA30,  // A30 PCIe
	"20bd": skuA800, // A800 SXM4 40GB
	"20f3": skuA800, // A800-SXM4-80GB
	"20f5": skuA800, // A800 80GB PCIe
	"20f6": skuA800, // A800 40GB PCIe
	"2235": skuA40,  // A40
	"2236": skuA10,  // A10
	"25b6": skuA16,  // A2 / A16

	// Hopper.
	"2330": skuH100,  // H100 SXM5 80GB
	"2331": skuH100,  // H100 PCIe
	"2336": skuH100,  // H100
	"2337": skuH100,  // H100 SXM5 64GB
	"2338": skuH100,  // H100 SXM5 96GB
	"2339": skuH100,  // H100 SXM5 94GB
	"233d": skuH100,  // H100 96GB
	"2335": skuH200,  // H200 SXM 141GB
	"233b": skuH200,  // H200 NVL
	"2322": skuH800,  // H800 PCIe
	"2324": skuH800,  // H800
	"230c": skuH20,   // H20 NVL16
	"230e": skuH20,   // H20 NVL16
	"2329": skuH20,   // H20
	"232c": skuH20,   // H20 HBM3e
	"2342": skuGH200, // GH200 120GB / 480GB
	"2348": skuGH200, // GH200 144G HBM3e

	// Ada (datacenter).
	"26b5": skuL40,  // L40
	"26b8": skuL40G, // L40G (China L40 variant)
	"26b9": skuL40S, // L40S
	"26b7": skuL20,  // L20
	"26ba": skuL20,  // L20
	"27b6": skuL2,   // L2
	"27b8": skuL4,   // L4

	// Blackwell.
	"2901": skuB200,  // B200
	"2909": skuB200,  // HGX B200 168GB
	"2920": skuB100,  // TS4 / B100
	"29bc": skuB100,  // B100
	"2941": skuGB200, // HGX GB200
	"3182": skuB300,  // B300 SXM6 AC
	"31a1": skuGB300, // GB300 MaxQ
	"31c2": skuGB300, // GB300
	"31c3": skuGB300, // GB300

	// RTX PRO 6000 Blackwell (workstation; recipe-supported). Non-"D" editions.
	"2bb1": skuRTXPro6000, // RTX PRO 6000 Blackwell Workstation Edition
	"2bb4": skuRTXPro6000, // RTX PRO 6000 Blackwell Max-Q Workstation Edition
	"2bb5": skuRTXPro6000, // RTX PRO 6000 Blackwell Server Edition
}

// skuForDeviceID returns the normalized accelerator SKU for an NVIDIA PCI
// device ID, or "" when the ID is not a recognized datacenter GPU. The lookup
// is case-insensitive and tolerates an optional "0x" prefix.
func skuForDeviceID(deviceID string) string {
	id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(deviceID)), "0x")
	return gpuDeviceIDToSKU[id]
}
