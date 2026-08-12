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

package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v2"
)

const (
	SpectrumXProfileLabel            = "network.nvidia.com/operator.nic-configuration.spectrum-x-profile"
	SpectrumXProfileConfigMapDataKey = "profile"
)

// SpectrumXProfileConfigRequired reports whether the RA version must be
// supplied to NIC Configuration Operator via a profile ConfigMap.
func SpectrumXProfileConfigRequired(ra string) bool {
	switch ra {
	case "", "RA2.1", "RA2.2":
		return false
	default:
		return true
	}
}

// NormalizeSpectrumXProfileConfig accepts either a full Spectrum-X profile
// ConfigMap YAML or the raw data.profile body. Full ConfigMaps contribute their
// metadata.name; rendered manifests always supply the canonical namespace and
// label themselves.
func NormalizeSpectrumXProfileConfig(spcx *ProfileSpectrumX) error {
	if spcx == nil {
		return nil
	}

	if topologyType := strings.TrimSpace(spcx.TopologyType); topologyType != "" {
		normalized, err := NormalizeSpectrumXTopologyType(topologyType)
		if err != nil {
			return err
		}
		spcx.TopologyType = normalized
	}
	if ipVersion := strings.TrimSpace(spcx.IPVersion); ipVersion != "" {
		normalized, err := NormalizeSpectrumXIPVersion(ipVersion)
		if err != nil {
			return err
		}
		spcx.IPVersion = normalized
	}

	if strings.TrimSpace(spcx.Profile) == "" {
		return nil
	}

	raw := spcx.Profile
	cm, ok, err := parseSpectrumXProfileConfigMap(raw)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	if strings.TrimSpace(cm.Metadata.Name) == "" {
		return fmt.Errorf("spectrum-x profile ConfigMap input is missing metadata.name")
	}
	if strings.TrimSpace(cm.Data[SpectrumXProfileConfigMapDataKey]) == "" {
		return fmt.Errorf("spectrum-x profile ConfigMap input is missing non-empty data.%s", SpectrumXProfileConfigMapDataKey)
	}
	if spcx.ConfigMapName != "" && spcx.ConfigMapName != cm.Metadata.Name {
		return fmt.Errorf("profile.spectrumX.configMapName %q does not match input ConfigMap metadata.name %q",
			spcx.ConfigMapName, cm.Metadata.Name)
	}

	spcx.ConfigMapName = cm.Metadata.Name
	spcx.Profile = cm.Data[SpectrumXProfileConfigMapDataKey]
	return nil
}

func NormalizeSpectrumXTopologyType(topologyType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(topologyType)) {
	case SpectrumXTopology2Tier, "2-tier-poc":
		return SpectrumXTopology2Tier, nil
	case SpectrumXTopology3Tier:
		return SpectrumXTopology3Tier, nil
	default:
		return "", fmt.Errorf("topologyType must be one of: %s, %s",
			SpectrumXTopology2Tier, SpectrumXTopology3Tier)
	}
}

func NormalizeSpectrumXIPVersion(ipVersion string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(ipVersion)) {
	case SpectrumXIPVersionIPv4:
		return SpectrumXIPVersionIPv4, nil
	case SpectrumXIPVersionIPv6:
		return SpectrumXIPVersionIPv6, nil
	default:
		return "", fmt.Errorf("ipVersion must be one of: %s, %s",
			SpectrumXIPVersionIPv4, SpectrumXIPVersionIPv6)
	}
}

func SpectrumXDefaultHostFirstOctet(topologyType string) int {
	if topologyType == SpectrumXTopology3Tier {
		return 10
	}
	return 172
}

type spectrumXProfileConfigMap struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

func parseSpectrumXProfileConfigMap(raw string) (*spectrumXProfileConfigMap, bool, error) {
	var cm spectrumXProfileConfigMap
	if err := yaml.Unmarshal([]byte(raw), &cm); err != nil {
		return nil, false, fmt.Errorf("failed to parse spectrum-x profile input: %w", err)
	}

	if cm.Kind == "" {
		return nil, false, nil
	}
	if !strings.EqualFold(cm.Kind, "ConfigMap") {
		return nil, false, fmt.Errorf("spectrum-x profile input kind must be ConfigMap, got %q", cm.Kind)
	}
	return &cm, true, nil
}
