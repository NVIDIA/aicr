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

	"github.com/nvidia/k8s-launch-kit/pkg/config"
)

// SynthesizeClusterConfig builds a config.ClusterConfig from a topology preset.
// This is the entry point used by `l8k generate --for <name>`: instead of
// discovering hardware against a live cluster, we construct a clusterConfig
// group entirely from the preset's static description plus a user-provided
// node selector.
//
// presetName is the directory name passed via --for (e.g.
// "PowerEdge-XE9680-H200"). It is used as the synthesized group's Identifier
// so that variants of the same machine type produce distinct NicNodePolicy
// names — `nnpName()` lowercases and truncates to 30 chars, so composite
// directory names render safely.
//
// Returns an error if the preset is missing the capabilities.nodes block —
// without it, FindApplicableProfile cannot match a profile.
func SynthesizeClusterConfig(
	presetName string,
	preset *Topology,
	nodeSelector map[string]string,
) (config.ClusterConfig, error) {
	if preset == nil {
		return config.ClusterConfig{}, fmt.Errorf("preset is nil")
	}
	if preset.Capabilities == nil || preset.Capabilities.Nodes == nil {
		return config.ClusterConfig{}, fmt.Errorf(
			"preset has no capabilities block; add 'capabilities.nodes.{sriov,rdma,ib}' to its topology.yaml to use it with --for")
	}

	pfs := make([]config.PFConfig, 0, len(preset.PFs))
	for _, pp := range preset.PFs {
		pfs = append(pfs, config.PFConfig{
			DeviceID:         pp.DeviceID,
			RdmaDevice:       pp.RdmaDevice,
			PciAddress:       pp.PciAddress,
			NetworkInterface: pp.NetworkInterface,
			Traffic:          pp.Traffic,
			Rail:             pp.Rail,
			PSID:             pp.PSID,
			PartNumber:       pp.PartNumber,
			NumaNode:         pp.NumaNode,
			ConnectedGPU:     pp.ConnectedGPU,
			GPUProximity:     pp.GPUProximity,
		})
	}

	return config.ClusterConfig{
		Identifier:    presetName,
		MachineType:   preset.MachineType,
		GPUType:       preset.GPUType,
		PresetApplied: true,
		Capabilities:  preset.Capabilities,
		PFs:           pfs,
		WorkerNodes:   nil,
		NodeSelector:  nodeSelector,
	}, nil
}
