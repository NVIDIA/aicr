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

// SubtypeGKEGPUPools is the K8s measurement subtype carrying GKE GPU
// node-pool driver-installation ownership, produced by ProjectGKEGPUPools
// and addressed as K8s.gke-gpu-pools.gpu-driver-installation.
const SubtypeGKEGPUPools = "gke-gpu-pools"

const (
	// gkeDriverInstallDisabled means every GPU pool's accelerators carry
	// gpuDriverInstallationConfig.gpuDriverVersion=INSTALLATION_DISABLED.
	gkeDriverInstallDisabled = "Disabled"

	// gkeDriverInstalled means GKE installs the driver, matching an
	// accelerator's gpuDriverVersion of DEFAULT, LATEST, or an absent
	// field (the provider's documented default when unspecified).
	gkeDriverInstalled = "Installed"

	// gkeDriverMixed marks GPU pools, or accelerators within one pool,
	// that disagree. Disagreement matches no declared constraint, so
	// resolution fails closed with the observed states as the actual.
	gkeDriverMixed = "Mixed"
)

// gkeGPUDriverVersionInstalled is the set of
// gpuDriverInstallationConfig.gpuDriverVersion values meaning GKE installs
// the driver, comprising the API's documented default (unspecified/empty)
// and its two explicit installed modes.
var gkeGPUDriverVersionInstalled = map[string]struct{}{
	"":                               {},
	"GPU_DRIVER_VERSION_UNSPECIFIED": {},
	"DEFAULT":                        {},
	"LATEST":                         {},
}

const gkeGPUDriverVersionDisabled = "INSTALLATION_DISABLED"

// gkeAccelerator is the narrow shape read from each
// config.accelerators[] entry. Unknown fields are ignored by design,
// since the file is operator-supplied provider output, not an AICR
// contract.
type gkeAccelerator struct {
	AcceleratorType             string                          `json:"acceleratorType"`
	GPUDriverInstallationConfig *gkeGPUDriverInstallationConfig `json:"gpuDriverInstallationConfig"`
}

type gkeGPUDriverInstallationConfig struct {
	GPUDriverVersion string `json:"gpuDriverVersion"`
}

// gkeNodePool is the narrow slice of the
// `gcloud container node-pools list -o json` JSON this projection reads.
type gkeNodePool struct {
	Name   string         `json:"name"`
	Config *gkeNodeConfig `json:"config"`
}

type gkeNodeConfig struct {
	Accelerators []gkeAccelerator `json:"accelerators"`
}

// ProjectGKEGPUPools reads a `gcloud container node-pools list --cluster
// <cluster> --format=json` dump and projects every GPU pool's
// gpuDriverInstallationConfig.gpuDriverVersion into the gke-gpu-pools
// subtype. For each pool with a non-empty config.accelerators list:
//
//   - INSTALLATION_DISABLED on every accelerator sets gpu-driver-
//     installation to Disabled.
//   - DEFAULT, LATEST, or an absent/empty gpuDriverVersion on every
//     accelerator (the provider's documented default) sets it to
//     Installed.
//   - Disagreement, within one pool or across pools, sets it to Mixed.
//   - An unrecognized gpuDriverVersion string is preserved verbatim.
//
// The key is omitted when there are no GPU pools. Every read or decode
// failure returns an error rather than a degraded reading, so a typoed
// path or truncated dump can't resolve as available-but-empty.
func ProjectGKEGPUPools(ctx context.Context, path string) (measurement.Subtype, error) {
	pools, err := readGKENodePools(ctx, path)
	if err != nil {
		return measurement.Subtype{}, err
	}

	data := make(map[string]measurement.Reading)
	modes := make(map[string]struct{})
	var descriptions []string
	gpuPools := 0

	for _, pool := range pools {
		// GKE always populates Accelerators for accelerator-attached
		// pools, including A2/A3/G2 families that carry no separate
		// --accelerator flag.
		if pool.Config == nil || len(pool.Config.Accelerators) == 0 {
			continue
		}
		gpuPools++
		poolModes := make(map[string]struct{})
		for _, acc := range pool.Config.Accelerators {
			poolModes[gkeAcceleratorInstallMode(acc)] = struct{}{}
		}
		mode := aggregateGKEGPUDriver(poolModes)
		modes[mode] = struct{}{}
		descriptions = append(descriptions, pool.Name+"="+mode)
	}

	sort.Strings(descriptions)
	data["gpu-pool-count"] = measurement.Int(gpuPools)
	if gpuPools > 0 {
		data["gpu-pools"] = measurement.Str(strings.Join(descriptions, ","))
		data["gpu-driver-installation"] = measurement.Str(aggregateGKEGPUDriver(modes))
	}
	return measurement.Subtype{Name: SubtypeGKEGPUPools, Data: data}, nil
}

// gkeAcceleratorInstallMode normalizes one accelerator entry's
// gpuDriverInstallationConfig.gpuDriverVersion into a projection state.
func gkeAcceleratorInstallMode(acc gkeAccelerator) string {
	version := ""
	if acc.GPUDriverInstallationConfig != nil {
		version = acc.GPUDriverInstallationConfig.GPUDriverVersion
	}
	if version == gkeGPUDriverVersionDisabled {
		return gkeDriverInstallDisabled
	}
	if _, ok := gkeGPUDriverVersionInstalled[version]; ok {
		return gkeDriverInstalled
	}
	// An unknown provider value is preserved verbatim, so the fail-closed
	// error names what was actually observed.
	return version
}

// aggregateGKEGPUDriver returns the single mode shared by modes, or
// gkeDriverMixed when they disagree.
func aggregateGKEGPUDriver(modes map[string]struct{}) string {
	if len(modes) == 1 {
		for mode := range modes {
			return mode
		}
	}
	return gkeDriverMixed
}

// readGKENodePools loads and decodes the pool dump via the shared
// provider-pools reader (see providerpools.go).
func readGKENodePools(ctx context.Context, path string) ([]gkeNodePool, error) {
	raw, err := readBoundedPoolsFile(ctx, path, "GKE GPU pools", defaults.MaxGKEGPUPoolsBytes)
	if err != nil {
		return nil, err
	}

	var pools []gkeNodePool
	if err := json.Unmarshal(raw, &pools); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to decode GKE GPU pools file %q: expected the JSON array "+
				"emitted by `gcloud container node-pools list --cluster <cluster> --format=json`", path), err)
	}
	// json.Unmarshal accepts a top-level `null` into a slice without error,
	// leaving it nil. That is not the documented gcloud output, and it must
	// not masquerade as a successful zero-pool projection.
	if pools == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to decode GKE GPU pools file %q: got JSON null, expected the "+
				"JSON array emitted by `gcloud container node-pools list --cluster <cluster> --format=json`", path))
	}
	return pools, nil
}
