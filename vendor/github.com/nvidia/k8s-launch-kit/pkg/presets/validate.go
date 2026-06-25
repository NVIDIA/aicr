// Copyright 2025 NVIDIA CORPORATION & AFFILIATES
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

package presets

import (
	"fmt"
	"sort"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// Deviation field names, shared between ValidatePreset (which produces them)
// and HasTopologyDeviation (which consumes them) so the two can't drift.
const (
	deviationFieldPFCount    = "pfCount"
	deviationFieldPCIAddress = "pciAddress"
	deviationFieldDeviceID   = "deviceID"
)

// HasTopologyDeviation reports whether any deviation reflects a hardware-shape
// mismatch — PF count, PCI address, or device ID (NIC model). These are the
// fields that make a preset unsafe to apply: overlaying a preset's
// authoritative topology (traffic / rail / NUMA / GPU affinity) onto hardware
// whose PCI layout differs corrupts the live-discovered classification, because
// ApplyPreset matches PFs by PCI address — a single coincidentally-overlapping
// address inherits the preset's unrelated fields. The discovery path uses this
// to skip enrichment entirely when the hardware drifts from the preset.
//
// Part numbers and PSIDs are intentionally not deviation fields (firmware/SKU
// variants are expected), so they never block application.
func HasTopologyDeviation(deviations []config.PresetDeviationEntry) bool {
	for _, d := range deviations {
		switch d.Field {
		case deviationFieldPFCount, deviationFieldPCIAddress, deviationFieldDeviceID:
			return true
		}
	}
	return false
}

// ValidatePreset compares a topology preset against the discovered hardware
// for a single group and returns the field-level deviations (empty when
// the preset matches exactly).
//
// Every discrepancy — PF count mismatch, PCI address drift, device-ID
// drift — is recorded as a deviation. The discovery pipeline treats any of
// these as a hard block on enrichment: when HasTopologyDeviation reports a
// drift it does NOT call ApplyPreset (overlaying a preset onto hardware with
// a different PCI layout corrupts the live-discovered traffic/rail
// classification), records the deviations on ClusterConfig.PresetDeviation,
// and warns on every subsequent config load. The operator keeps the live
// discovery results plus a loud reminder that the cluster diverges from the
// matched preset.
//
// Part numbers and PSIDs are not strict criteria (firmware variants are
// expected) so they're not checked here.
func ValidatePreset(preset *Topology, discoveredPFs []config.PFConfig) []config.PresetDeviationEntry {
	var deviations []config.PresetDeviationEntry

	if len(preset.PFs) != len(discoveredPFs) {
		deviations = append(deviations, config.PresetDeviationEntry{
			Field:    deviationFieldPFCount,
			Expected: fmt.Sprintf("%d", len(preset.PFs)),
			Got:      fmt.Sprintf("%d", len(discoveredPFs)),
			Detail:   "PF count differs from preset",
		})
	}

	presetAddrs := make(map[string]string, len(preset.PFs)) // pciAddr -> deviceID
	for _, pf := range preset.PFs {
		presetAddrs[pf.PciAddress] = pf.DeviceID
	}
	discoveredAddrs := make(map[string]string, len(discoveredPFs)) // pciAddr -> deviceID
	for _, pf := range discoveredPFs {
		discoveredAddrs[pf.PciAddress] = pf.DeviceID
	}

	// PCI addresses present in the preset but missing from discovery.
	for addr := range presetAddrs {
		if _, ok := discoveredAddrs[addr]; !ok {
			deviations = append(deviations, config.PresetDeviationEntry{
				Field:    deviationFieldPCIAddress,
				Expected: addr,
				Detail:   "preset PCI address not present on discovered hardware",
			})
		}
	}
	// PCI addresses present on discovery but absent from the preset.
	for addr := range discoveredAddrs {
		if _, ok := presetAddrs[addr]; !ok {
			deviations = append(deviations, config.PresetDeviationEntry{
				Field:  deviationFieldPCIAddress,
				Got:    addr,
				Detail: "discovered PCI address not present in preset",
			})
		}
	}

	// Device-ID drift on PCI addresses present in both.
	for addr, presetDevID := range presetAddrs {
		discoveredDevID, ok := discoveredAddrs[addr]
		if !ok {
			continue
		}
		if presetDevID != discoveredDevID {
			deviations = append(deviations, config.PresetDeviationEntry{
				Field:    deviationFieldDeviceID,
				Expected: fmt.Sprintf("%s@%s", presetDevID, addr),
				Got:      fmt.Sprintf("%s@%s", discoveredDevID, addr),
				Detail:   "device ID at PCI address differs from preset",
			})
		}
	}

	// Stable ordering so the YAML diff is deterministic across runs.
	sort.Slice(deviations, func(i, j int) bool {
		if deviations[i].Field != deviations[j].Field {
			return deviations[i].Field < deviations[j].Field
		}
		if deviations[i].Expected != deviations[j].Expected {
			return deviations[i].Expected < deviations[j].Expected
		}
		return deviations[i].Got < deviations[j].Got
	})
	return deviations
}
