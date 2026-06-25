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

// Package releases is the embedded catalog of supported Network Operator
// release lines. It is consumed by:
//
//   - pkg/networkoperatorplugin (ApplyNetworkOperatorRelease, validate.go)
//     to populate per-line versions / repositories / helm-repo URLs on a
//     LaunchKitConfig.
//   - pkg/networkoperatorplugin/discovery (WithRelease option) to let
//     library callers pick a release without manually constructing the
//     config fields.
//   - pkg/cmd (flag validation and help text).
//
// The catalog is the single source of truth for what "26.4" / "26.1" /
// "25.10" actually resolve to. Bump entries here when a patch release
// ships; add a new key for a new minor.
package releases

import (
	_ "embed"
	"fmt"
	"sort"
	"sync"

	"gopkg.in/yaml.v2"
)

//go:embed releases.yaml
var releasesYAML []byte

// Release describes a single Network Operator minor release: image tags for
// the operator and DOCA driver. Keyed by MAJOR.MINOR (e.g. "26.4") in the
// embedded catalog — patch bumps update the values in place.
type Release struct {
	NetworkOperator ReleaseNetworkOperator `yaml:"networkOperator"`
	DOCADriver      ReleaseDOCADriver      `yaml:"docaDriver"`
}

type ReleaseNetworkOperator struct {
	Version          string `yaml:"version"`
	ComponentVersion string `yaml:"componentVersion"`
	// Repository is the registry path for COMPONENT images managed by
	// the operator (ofedDriver, nvIpam, multus, device plugins, …).
	// Rendered into NicClusterPolicy / NicNodePolicy templates.
	Repository string `yaml:"repository"`
	// OperatorRepository is the registry path for the network-operator
	// BINARY image itself. Distinct from Repository because stable
	// releases publish the operator under `nvcr.io/nvidia/cloud-native`
	// while components live under `nvcr.io/nvidia/mellanox`. Rendered
	// into the helm chart's `operator.repository` value.
	OperatorRepository string `yaml:"operatorRepository"`
	// HelmRepoURL is the Helm chart repository URL for the network-operator
	// chart. Per-release because stable releases publish to .../nvidia while
	// beta/staging releases publish to .../nvstaging/mellanox.
	HelmRepoURL string `yaml:"helmRepoURL"`
}

type ReleaseDOCADriver struct {
	Version string `yaml:"version"`
}

type releasesFile struct {
	Releases map[string]Release `yaml:"releases"`
}

var (
	releasesOnce sync.Once
	releasesMap  map[string]Release
	releasesErr  error
)

// loadReleases parses the embedded catalog once. Subsequent calls return the
// memoized result.
func loadReleases() (map[string]Release, error) {
	releasesOnce.Do(func() {
		var f releasesFile
		if err := yaml.Unmarshal(releasesYAML, &f); err != nil {
			releasesErr = fmt.Errorf("failed to parse embedded releases.yaml: %w", err)
			return
		}
		if len(f.Releases) == 0 {
			releasesErr = fmt.Errorf("embedded releases.yaml has no entries")
			return
		}
		releasesMap = f.Releases
	})
	return releasesMap, releasesErr
}

// LookupRelease returns the catalog entry for the given MAJOR.MINOR key.
// The bool is false when the key is not present.
func LookupRelease(release string) (Release, bool) {
	m, err := loadReleases()
	if err != nil {
		return Release{}, false
	}
	r, ok := m[release]
	return r, ok
}

// SupportedReleases returns the catalog keys sorted ascending. Used in flag
// help text and validation error messages.
func SupportedReleases() []string {
	m, err := loadReleases()
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
