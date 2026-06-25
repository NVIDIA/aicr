// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

// Package pciids resolves NVIDIA PCI device IDs (vendor 10de) to canonical
// GPUType strings, using a trimmed pci.ids snapshot embedded at build time.
package pciids

import (
	_ "embed"
	"strings"
)

//go:embed nvidia.ids
var nvidiaIDs string

var deviceNames map[string]string

func init() {
	deviceNames = parseVendorBlock(nvidiaIDs)
}

// parseVendorBlock reads a vendor block in pci.ids format and returns a map
// of lowercase hex device ID to the raw device name (e.g. "GH100 [H200 SXM 141GB]").
// The input is expected to start at the vendor line ("10de  NVIDIA Corporation")
// and contain tab-indented device lines and optional two-tab subsystem lines
// (subsystems are ignored).
func parseVendorBlock(input string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(input, "\n") {
		// Device lines are prefixed with exactly one tab; subsystem lines with two.
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		trimmed := strings.TrimPrefix(line, "\t")
		// Format: "<4-hex-id>  <name>"
		if len(trimmed) < 6 || trimmed[4] != ' ' {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(trimmed[:4]))
		name := strings.TrimSpace(trimmed[4:])
		if id == "" || name == "" {
			continue
		}
		out[id] = name
	}
	return out
}

// LookupNVIDIA returns a canonical GPUType for a given NVIDIA PCI device ID,
// in the same shape as parseGPUProductFromNvidiaSmi output (e.g. "NVIDIA-H200-SXM-141GB").
// Returns "" for unknown IDs. The input is a hex string, optionally prefixed with
// "0x", case-insensitive (e.g. "2335", "0x2335", "0X2335").
func LookupNVIDIA(deviceID string) string {
	id := strings.ToLower(strings.TrimSpace(deviceID))
	id = strings.TrimPrefix(id, "0x")
	name, ok := deviceNames[id]
	if !ok {
		return ""
	}
	return canonicalize(name)
}

// canonicalize converts a pci.ids device name into the GPUType shape used
// elsewhere in k8s-launch-kit (matches parseGPUProductFromNvidiaSmi).
// When a bracketed marketing name is present (e.g. "GH100 [H200 SXM 141GB]"),
// the bracket contents are preferred over the chip codename. The final string
// is prefixed with "NVIDIA-" if not already present, and spaces are replaced
// with dashes.
func canonicalize(raw string) string {
	name := strings.TrimSpace(raw)
	if open := strings.Index(name, "["); open >= 0 {
		if close := strings.Index(name[open:], "]"); close > 0 {
			if inner := strings.TrimSpace(name[open+1 : open+close]); inner != "" {
				name = inner
			}
		}
	}
	if !strings.HasPrefix(strings.ToUpper(name), "NVIDIA") {
		name = "NVIDIA " + name
	}
	return strings.ReplaceAll(name, " ", "-")
}
