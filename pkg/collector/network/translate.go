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

package network

import (
	"fmt"
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	l8kconfig "github.com/nvidia/k8s-launch-kit/pkg/config"
)

// Subtype names emitted by toMeasurement. Mirror the schema contract in
// docs/integrator/measurement-api.md.
const (
	subtypeIdentity      = "identity"
	subtypeCapabilities  = "capabilities"
	subtypePFs           = "pfs"
	subtypeKernelModules = "kernel-modules"
)

// Context keys on the identity subtype.
const (
	identityCtxIdentifier   = "identifier"
	identityCtxMachineType  = "machineType"
	identityCtxGPUType      = "gpuType"
	identityCtxLinkType     = "linkType"
	identityCtxNodeSelector = "nodeSelector"
)

// Data keys on the identity subtype.
const (
	identityDataPFCount   = "pf-count"
	identityDataRailCount = "rail-count"
)

// Context keys on each pfs item.
const (
	pfCtxPCIAddress       = "pciAddress"
	pfCtxDeviceID         = "deviceID"
	pfCtxPSID             = "psid"
	pfCtxPartNumber       = "partNumber"
	pfCtxRDMADevice       = "rdmaDevice"
	pfCtxNetworkInterface = "networkInterface"
	// pfCtxModel is the human-readable NIC model string l8k discovery
	// reads from the device's VPD (e.g. "Nvidia ConnectX-7 NDR200/HDR
	// QSFP112 2-port PCIe Gen5 x16 InfiniBand Adapter"). Carries the
	// information operators look at when reasoning about which physical
	// adapter family is in a slot — distinct from deviceID (PCI ID) and
	// partNumber (NVIDIA SKU).
	pfCtxModel = "model"
	// pfCtxConnectedGPU is the GPU identifier (e.g. "GPU0") this PF is
	// affined to per preset topology, when known. Empty for PFs without
	// a preset overlay (no GPU affinity inferred from live discovery).
	pfCtxConnectedGPU = "connectedGPU"
	// pfCtxGPUProximity is the PCIe-topology proximity class between
	// this PF and its connectedGPU, sourced from preset topology
	// (e.g. "PIX", "PXB", "NODE"). Same nvidia-smi --topo classification
	// scheme aicr users see elsewhere.
	pfCtxGPUProximity = "gpuProximity"
)

// Data keys on each pfs item.
const (
	pfDataRail     = "rail"
	pfDataNumaNode = "numaNode"
	pfDataTraffic  = "traffic"
)

// toMeasurement converts a single l8k LaunchKitConfig group into the
// canonical NetworkTopology Measurement.
//
// Single-group enforcement: when cfg.ClusterConfig carries more than one
// entry, toMeasurement returns ErrCodeInvalidRequest with groupCount in
// the error context. Multi-group support is a planned future iteration.
//
// Empty inputs (cfg == nil, or cfg.ClusterConfig empty) return (nil, nil)
// — the caller treats absence as "no network topology to emit," matching
// how today's other collectors handle missing data.
func toMeasurement(cfg *l8kconfig.LaunchKitConfig) (*measurement.Measurement, error) {
	// nil cfg or empty ClusterConfig means the source carried no group
	// data — the caller (collector) translates that to "no NetworkTopology
	// Measurement to emit", which the snapshotter handles as a no-op via
	// its nil-Measurement guard. Matches the inactive-collector contract.
	//nolint:nilnil // intentional: nil/nil is the no-data contract
	if cfg == nil || len(cfg.ClusterConfig) == 0 {
		return nil, nil
	}
	if len(cfg.ClusterConfig) > 1 {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"multiple cluster-config groups are not yet supported; this iteration of NetworkTopology supports one group per snapshot",
			map[string]any{"groupCount": len(cfg.ClusterConfig)})
	}

	group := cfg.ClusterConfig[0]

	identity := buildIdentitySubtype(group)
	capabilities := buildCapabilitiesSubtype(group)
	pfs := buildPFsSubtype(group)
	kernelModules := buildKernelModulesSubtype(group)

	mb := measurement.NewMeasurement(measurement.TypeNetworkTopology).
		WithSubtype(identity).
		WithSubtype(capabilities)

	// The pfs subtype is only emitted when the group actually has PFs —
	// Subtype.Validate requires at least one of data/items, so an empty
	// pfs subtype would be rejected.
	if len(pfs.Items) > 0 {
		mb = mb.WithSubtype(pfs)
	}
	if len(kernelModules.Data) > 0 {
		mb = mb.WithSubtype(kernelModules)
	}
	return mb.Build(), nil
}

func buildIdentitySubtype(g l8kconfig.ClusterConfig) measurement.Subtype {
	b := measurement.NewSubtypeBuilder(subtypeIdentity)
	if g.Identifier != "" {
		b = b.WithContext(identityCtxIdentifier, g.Identifier)
	}
	if g.MachineType != "" {
		b = b.WithContext(identityCtxMachineType, g.MachineType)
	}
	if g.GPUType != "" {
		b = b.WithContext(identityCtxGPUType, g.GPUType)
	}
	// linkType is always present per the NetworkTopology shape contract
	// in docs/integrator/measurement-api.md — empty string is the
	// documented "unknown fabric" sentinel, distinct from a missing key.
	// Consumers that decide on InfiniBand vs Ethernet based on this
	// field need a stable key to read, even when discovery couldn't
	// prove the fabric (no east-west port produced a confirmed and
	// unanimous verdict).
	b = b.WithContext(identityCtxLinkType, g.LinkType)
	if sel := flattenNodeSelector(g.NodeSelector); sel != "" {
		b = b.WithContext(identityCtxNodeSelector, sel)
	}
	b = b.SetInt(identityDataPFCount, len(g.PFs))
	b = b.SetInt(identityDataRailCount, distinctRailCount(g.PFs))
	return b.Build()
}

func buildCapabilitiesSubtype(g l8kconfig.ClusterConfig) measurement.Subtype {
	var sriov, rdma, ib bool
	if g.Capabilities != nil && g.Capabilities.Nodes != nil {
		sriov = g.Capabilities.Nodes.Sriov
		rdma = g.Capabilities.Nodes.Rdma
		ib = g.Capabilities.Nodes.Ib
	}
	return measurement.NewSubtypeBuilder(subtypeCapabilities).
		SetBool("sriov", sriov).
		SetBool("rdma", rdma).
		SetBool("ib", ib).
		Build()
}

func buildPFsSubtype(g l8kconfig.ClusterConfig) measurement.Subtype {
	b := measurement.NewSubtypeBuilder(subtypePFs)
	for i := range g.PFs {
		pf := g.PFs[i]
		ctx := map[string]string{}
		if pf.PciAddress != "" {
			ctx[pfCtxPCIAddress] = pf.PciAddress
		}
		if pf.DeviceID != "" {
			ctx[pfCtxDeviceID] = pf.DeviceID
		}
		if pf.PSID != "" {
			ctx[pfCtxPSID] = pf.PSID
		}
		if pf.PartNumber != "" {
			ctx[pfCtxPartNumber] = pf.PartNumber
		}
		if pf.RdmaDevice != "" {
			ctx[pfCtxRDMADevice] = pf.RdmaDevice
		}
		if pf.NetworkInterface != "" {
			ctx[pfCtxNetworkInterface] = pf.NetworkInterface
		}
		if pf.Model != "" {
			ctx[pfCtxModel] = pf.Model
		}
		if pf.ConnectedGPU != "" {
			ctx[pfCtxConnectedGPU] = pf.ConnectedGPU
		}
		if pf.GPUProximity != "" {
			ctx[pfCtxGPUProximity] = pf.GPUProximity
		}
		data := map[string]measurement.Reading{}
		if pf.Rail != nil {
			data[pfDataRail] = measurement.Int(*pf.Rail)
		}
		if pf.NumaNode != nil {
			data[pfDataNumaNode] = measurement.Int(*pf.NumaNode)
		}
		if pf.Traffic != "" {
			data[pfDataTraffic] = measurement.Str(pf.Traffic)
		}
		b = b.WithItem(measurement.ItemEntry{Context: ctx, Data: data})
	}
	return b.Build()
}

func buildKernelModulesSubtype(g l8kconfig.ClusterConfig) measurement.Subtype {
	b := measurement.NewSubtypeBuilder(subtypeKernelModules)
	for i, m := range g.StorageModules {
		b = b.SetString(fmt.Sprintf("storage.%d", i), m)
	}
	for i, m := range g.ThirdPartyRDMAModules {
		b = b.SetString(fmt.Sprintf("thirdParty.%d", i), m)
	}
	return b.Build()
}

// distinctRailCount counts the number of distinct rail indices across
// the group's PFs. PFs with no rail (e.g. north-south DPUs) are ignored.
func distinctRailCount(pfs []l8kconfig.PFConfig) int {
	seen := map[int]struct{}{}
	for _, pf := range pfs {
		if pf.Rail == nil {
			continue
		}
		seen[*pf.Rail] = struct{}{}
	}
	return len(seen)
}

// flattenNodeSelector reduces an l8k NodeSelector to a single label=value
// selector string suitable for the identity subtype's Context. l8k
// convention is one key per group, so picking any entry is safe; result
// is sorted for determinism when the map has multiple entries.
func flattenNodeSelector(sel map[string]string) string {
	if len(sel) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		out += k + "=" + sel[k]
	}
	return out
}
