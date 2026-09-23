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

	"github.com/Masterminds/semver/v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// SubtypeGKEGPUPools is the K8s measurement subtype carrying GKE GPU
// node-pool driver-installation ownership, produced by ProjectGKEGPUPools
// and addressed as K8s.gke-gpu-pools.gpu-driver-installation.
const SubtypeGKEGPUPools = "gke-gpu-pools"

const (
	// gkeDriverInstallDisabled means GKE never installs a driver, whether
	// from an explicit INSTALLATION_DISABLED or an omitted, empty, or
	// unspecified value that gkeOmittedDriverMode resolves to no-install.
	gkeDriverInstallDisabled = "Disabled"

	// gkeDriverInstalled means GKE installs the driver, whether from an
	// explicit DEFAULT or LATEST or an omitted, empty, or unspecified
	// value that gkeOmittedDriverMode resolves to install-by-default.
	gkeDriverInstalled = "Installed"

	// gkeDriverNotConfigured is gkeOmittedDriverMode's fallback for a
	// missing gpuDriverInstallationConfig when the pool's version can't
	// be resolved to Installed or Disabled.
	gkeDriverNotConfigured = "NotConfigured"

	// gkeDriverNotInstalled is gkeOmittedDriverMode's fallback for an
	// empty or unspecified gpuDriverVersion when the pool's version can't
	// be resolved to Installed or Disabled.
	gkeDriverNotInstalled = "NotInstalled"

	// gkeDriverMixed marks GPU pools, or accelerators within one pool,
	// that disagree. Disagreement matches no declared constraint, so
	// resolution fails closed with the observed states as the actual.
	gkeDriverMixed = "Mixed"
)

// gkeGPUDriverVersionInstalled is the set of
// gpuDriverInstallationConfig.gpuDriverVersion values GKE documents as
// explicit installed modes.
var gkeGPUDriverVersionInstalled = map[string]struct{}{
	"DEFAULT": {},
	"LATEST":  {},
}

const (
	gkeGPUDriverVersionDisabled    = "INSTALLATION_DISABLED"
	gkeGPUDriverVersionUnspecified = "GPU_DRIVER_VERSION_UNSPECIFIED"
)

var (
	// gkeOmittedDriverDefaultThreshold is the GKE version at and after
	// which an omitted, empty, or unspecified gpuDriverVersion defaults
	// to installing the driver. Below it, GKE installs no driver
	// regardless of auto-provisioning.
	gkeOmittedDriverDefaultThreshold = semver.MustParse("1.30.1-gke.1156000")

	// gkeOmittedDriverDefaultThresholdNAP is the later version at and
	// after which that default extends to node-auto-provisioned pools.
	// Between the two thresholds, an auto-provisioned pool still gets no
	// driver even at a version where a regular pool would.
	gkeOmittedDriverDefaultThresholdNAP = semver.MustParse("1.32.2-gke.1297000")
)

// gkeOmittedDriverMode resolves GKE's documented default for an omitted,
// empty, or unspecified gpuDriverVersion, using poolVersion and
// autoprovisioned. It returns fallback if poolVersion can't be parsed, so
// the result is never a guess.
func gkeOmittedDriverMode(poolVersion string, autoprovisioned bool, fallback string) string {
	v, err := semver.StrictNewVersion(poolVersion)
	if err != nil || !strings.HasPrefix(v.Prerelease(), "gke.") {
		// A real GKE node-pool version always carries a "gke.<build>"
		// prerelease. Anything else can't be trusted against thresholds
		// expressed in that same form.
		return fallback
	}
	if v.LessThan(gkeOmittedDriverDefaultThreshold) {
		return gkeDriverInstallDisabled
	}
	if autoprovisioned && v.LessThan(gkeOmittedDriverDefaultThresholdNAP) {
		return gkeDriverInstallDisabled
	}
	return gkeDriverInstalled
}

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
	Name        string                  `json:"name"`
	Version     string                  `json:"version"`
	Config      *gkeNodeConfig          `json:"config"`
	Autoscaling *gkeNodePoolAutoscaling `json:"autoscaling"`
}

type gkeNodeConfig struct {
	Accelerators []gkeAccelerator `json:"accelerators"`
}

type gkeNodePoolAutoscaling struct {
	Autoprovisioned bool `json:"autoprovisioned"`
}

// ProjectGKEGPUPools projects each GPU pool's driver-installation mode
// from the `gcloud container node-pools list --cluster <cluster>
// --format=json` dump at path into the gke-gpu-pools subtype. For each
// pool with a non-empty config.accelerators list, gpuDriverVersion maps
// to gpu-driver-installation as:
//
//   - INSTALLATION_DISABLED on every accelerator sets it to Disabled.
//   - DEFAULT or LATEST on every accelerator sets it to Installed.
//   - Absent, empty, or GPU_DRIVER_VERSION_UNSPECIFIED resolves against
//     the pool's version and auto-provisioning flag (see
//     gkeOmittedDriverMode), falling back to NotConfigured or
//     NotInstalled when the version can't be resolved.
//   - Disagreement, within a pool or across pools, sets it to Mixed.
//   - Anything else is preserved verbatim.
//
// The key is omitted when there are no GPU pools. A read or decode
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
		autoprovisioned := pool.Autoscaling != nil && pool.Autoscaling.Autoprovisioned
		poolModes := make(map[string]struct{})
		for _, acc := range pool.Config.Accelerators {
			poolModes[gkeAcceleratorInstallMode(acc, pool.Version, autoprovisioned)] = struct{}{}
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

// gkeAcceleratorInstallMode normalizes acc's gpuDriverVersion into a
// projection state, resolving an omitted value against poolVersion and
// autoprovisioned.
func gkeAcceleratorInstallMode(acc gkeAccelerator, poolVersion string, autoprovisioned bool) string {
	if acc.GPUDriverInstallationConfig == nil {
		return gkeOmittedDriverMode(poolVersion, autoprovisioned, gkeDriverNotConfigured)
	}
	version := acc.GPUDriverInstallationConfig.GPUDriverVersion
	if version == gkeGPUDriverVersionDisabled {
		return gkeDriverInstallDisabled
	}
	if version == "" || version == gkeGPUDriverVersionUnspecified {
		return gkeOmittedDriverMode(poolVersion, autoprovisioned, gkeDriverNotInstalled)
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
