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

package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// SubtypeAKSGPUPools is the K8s measurement subtype carrying the projection
// of AKS GPU agent-pool driver ownership (ADR-015 DD3). Profile constraints
// reference it as K8s.aks-gpu-pools.gpu-driver.
const SubtypeAKSGPUPools = "aks-gpu-pools"

// AKS gpuProfile.driver values, as published by the AgentPool API.
const (
	aksGPUDriverInstall = "Install"
	aksGPUDriverNone    = "None"

	// aksGPUDriverManaged is the projection's marker for a fully
	// AKS-managed GPU pool (gpuProfile.nvidia present). Managed pools are
	// out of AICR's scope, so the marker deliberately matches no declared
	// profile constraint and resolution fails closed with the observed
	// state in the error.
	aksGPUDriverManaged = "Managed"

	// aksGPUDriverMixed is the projection's marker for GPU pools that do
	// not agree on one driver mode. Like Managed, it matches no
	// constraint, failing closed with a diagnosable actual value.
	aksGPUDriverMixed = "Mixed"
)

// aksGPUVMSizePrefixes identifies GPU agent pools by VM size family. Azure's
// GPU-accelerated families are NC, ND, and NV (NG carries AMD GPUs and is
// intentionally excluded — AICR manages the NVIDIA stack).
var aksGPUVMSizePrefixes = []string{"standard_nc", "standard_nd", "standard_nv"}

// aksAMDGPUSizeMarkers excludes AMD accelerators that live INSIDE the
// NVIDIA-dominated families above: MI300X/MI325X are ND sizes
// (Standard_ND96isr_MI300X_v5), and Radeon Pro V620/V710 are NV sizes.
// Without this, an AMD pool (which AKS requires creating with
// --gpu-driver none) beside an NVIDIA Install pool would falsely
// aggregate to Mixed and reject a valid azure-managed cluster.
//
// Known limitation (deliberate): detection is prefix + marker based, so a
// future NVIDIA family outside NC/ND/NV would be silently skipped
// (fail-open for that pool) and a future AMD size without a marker here
// would be counted (fail-closed via Mixed). Manufacturer metadata is not
// present in the AgentPool object, so an exact vendor split is not
// available from this input; revisit if Azure adds one.
var aksAMDGPUSizeMarkers = []string{"_mi300x", "_mi325x", "_v620", "_v710"}

// aksAgentPool is the narrow slice of the `az aks nodepool list` JSON this
// projection reads. Unknown fields are ignored by design: the file is
// operator-supplied provider output, not an AICR contract.
type aksAgentPool struct {
	Name       string         `json:"name"`
	VMSize     string         `json:"vmSize"`
	GPUProfile *aksGPUProfile `json:"gpuProfile"`
}

type aksGPUProfile struct {
	Driver string            `json:"driver"`
	NVIDIA *aksNVIDIAProfile `json:"nvidia"`
}

// aksNVIDIAProfile is the AKS-managed GPU stack marker. AKS supports
// managementMode Managed (AKS installs device plugin/DCGM/health tooling —
// out of AICR scope) and Unmanaged (the user manages the Kubernetes GPU
// stack; the driver still follows gpuProfile.driver — a supported AICR
// configuration).
type aksNVIDIAProfile struct {
	ManagementMode string `json:"managementMode"`
}

// ProjectAKSGPUPools reads an `az aks nodepool list -o json` dump and
// projects the GPU pools' gpuProfile.driver values into the aks-gpu-pools
// subtype (ADR-015 DD3):
//
//   - Every GPU pool in Install mode (or with gpuProfile absent, the
//     provider's documented default) → gpu-driver: Install.
//   - Every GPU pool in None mode → gpu-driver: None.
//   - Disagreeing pools → gpu-driver: Mixed; a fully AKS-managed pool
//     (gpuProfile.nvidia with managementMode Managed — or an unknown or
//     empty mode) projects as Managed, while managementMode Unmanaged
//     follows the driver field (the user manages only the Kubernetes GPU
//     stack — a supported configuration). Neither Managed nor Mixed
//     matches a declared constraint, so resolution fails closed with the
//     observed value as the actual.
//   - An unknown driver string is preserved verbatim for the same reason.
//   - No GPU pools → the gpu-driver key is omitted entirely; constraint
//     evaluation reports the reading unavailable and fails closed.
//
// The file is operator-supplied via an explicit flag, so every failure here
// is an error, never a degraded-but-successful measurement: a typoed path or
// truncated dump must not masquerade as "reading unavailable" and steer a
// profile decision.
func ProjectAKSGPUPools(ctx context.Context, path string) (measurement.Subtype, error) {
	pools, err := readAKSAgentPools(ctx, path)
	if err != nil {
		return measurement.Subtype{}, err
	}

	data := make(map[string]measurement.Reading)
	modes := make(map[string]struct{})
	var descriptions []string
	gpuPools := 0

	for _, pool := range pools {
		if !isAKSGPUVMSize(pool.VMSize) {
			continue
		}
		gpuPools++
		mode := aksGPUDriverInstall // documented provider default when absent
		switch {
		case pool.GPUProfile == nil:
		case pool.GPUProfile.NVIDIA != nil &&
			!strings.EqualFold(pool.GPUProfile.NVIDIA.ManagementMode, "Unmanaged"):
			// Managed (or an unknown/empty managementMode with the nvidia
			// block present) is out of AICR scope — fail closed via the
			// Managed marker. Unmanaged delegates the Kubernetes GPU
			// stack to the user while the driver still follows
			// gpuProfile.driver, so it falls through to the driver field.
			mode = aksGPUDriverManaged
		case pool.GPUProfile.Driver != "":
			mode = pool.GPUProfile.Driver
		}
		modes[mode] = struct{}{}
		descriptions = append(descriptions, pool.Name+"="+mode)
	}

	sort.Strings(descriptions)
	data["gpu-pool-count"] = measurement.Int(gpuPools)
	if gpuPools > 0 {
		data["gpu-pools"] = measurement.Str(strings.Join(descriptions, ","))
		data["gpu-driver"] = measurement.Str(aggregateAKSGPUDriver(modes))
	}
	return measurement.Subtype{Name: SubtypeAKSGPUPools, Data: data}, nil
}

func aggregateAKSGPUDriver(modes map[string]struct{}) string {
	if len(modes) == 1 {
		for mode := range modes {
			return mode
		}
	}
	return aksGPUDriverMixed
}

func isAKSGPUVMSize(vmSize string) bool {
	lower := strings.ToLower(vmSize)
	for _, marker := range aksAMDGPUSizeMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, prefix := range aksGPUVMSizePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// readAKSAgentPools loads and decodes the pool dump through the shared
// provider-pools reader (see providerpools.go for the bounded-read rules and
// the pattern a new provider's projection follows).
func readAKSAgentPools(ctx context.Context, path string) ([]aksAgentPool, error) {
	raw, err := readBoundedPoolsFile(ctx, path, "AKS GPU pools", defaults.MaxAKSGPUPoolsBytes)
	if err != nil {
		return nil, err
	}

	var pools []aksAgentPool
	if err := json.Unmarshal(raw, &pools); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to decode AKS GPU pools file %q: expected the JSON array "+
				"emitted by `az aks nodepool list -o json`", path), err)
	}
	// json.Unmarshal accepts a top-level `null` into a slice without error
	// (leaving it nil) — that is not the documented az output and must not
	// masquerade as a successful zero-pool projection.
	if pools == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to decode AKS GPU pools file %q: got JSON null, expected the "+
				"JSON array emitted by `az aks nodepool list -o json`", path))
	}
	return pools, nil
}
