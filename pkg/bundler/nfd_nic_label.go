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

package bundler

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/component"
)

// The nfd component name, values paths, stripped feature key, and label
// spellings read by warnNFDNICLabelMismatch. The Mellanox NIC label counts with
// or without its prefix, because nfd-master adds the prefix to an
// un-namespaced label.
const (
	nfdComponentName              = "nfd"
	nfdNoPublishFeaturesValuePath = "worker.config.core.noPublishFeatures"
	nfdCustomSourcesValuePath     = "worker.config.sources.custom"
	nfdPCIDeviceFeature           = "pci.device"
	nfdMellanoxNICLabel           = "feature.node.kubernetes.io/pci-15b3.present"
	nfdMellanoxNICLabelUnprefixed = "pci-15b3.present"
)

// warnNFDNICLabelMismatch warns when network-operator and the nfd-worker rule
// that labels Mellanox NIC nodes disagree in the final bundle. With
// network-operator present and pci.device stripped from the published
// features, only an nfd-worker sources.custom rule can produce the label its
// NicClusterPolicy selects on. Without network-operator, that rule makes a
// GPU Operator that manages the driver with GPUDirect RDMA and no host MOFED
// wait for a MOFED driver nothing installs; a subset bundle stays silent
// because network-operator may be deployed outside it.
//
// The check reads only worker.config.core.noPublishFeatures and the static
// sources.custom labels; worker.extraArgs overrides and labelsTemplate rules
// are not replayed.
func (b *DefaultBundler) warnNFDNICLabelMismatch(componentValues map[string]map[string]any) {
	if b.Config == nil {
		return
	}
	nfdValues, ok := componentValues[nfdComponentName]
	if !ok {
		return
	}
	_, networkOperator := componentValues[networkOperatorComponentName]
	hasRule := nfdLabelsMellanoxNIC(nfdValues)

	var warning string
	switch {
	case networkOperator && !hasRule && nfdStripsFeature(nfdValues, nfdPCIDeviceFeature):
		warning = fmt.Sprintf(
			"%[1]s is in this bundle, but %[2]s strips %[3]s (%[4]s) and has no nfd-worker rule "+
				"labelling %[5]s (%[6]s). %[1]s's NodeFeatureRule reads %[3]s, so after nfd-worker "+
				"restarts nothing labels the NVIDIA NIC nodes, and the pods that select on that label "+
				"(the NicClusterPolicy operands) leave them. Select components/nfd/values-nvidia-nics.yaml "+
				"(values-nvidia-nics-aks.yaml on AKS) as the %[2]s valuesFile in the overlay that adds %[1]s, "+
				"or include its rule in a %[6]s override.",
			networkOperatorComponentName, nfdComponentName, nfdPCIDeviceFeature,
			nfdNoPublishFeaturesValuePath, nfdMellanoxNICLabel, nfdCustomSourcesValuePath)
	case !networkOperator && hasRule && len(b.Config.Bundlers()) == 0:
		warning = fmt.Sprintf(
			"%[1]s labels NVIDIA NIC nodes %[2]s (%[3]s), but %[4]s is not in this bundle. The GPU "+
				"Operator validator waits on those nodes for the %[4]s MOFED driver when the GPU Operator "+
				"manages the driver: %[5]s=true, %[6]s=true and driver.rdma.useHostMofed=false. Unless %[4]s "+
				"is installed outside this bundle, drop the rule with --set-json '%[1]s:%[3]s=[]', which "+
				"removes every nfd-worker custom rule.",
			nfdComponentName, nfdMellanoxNICLabel, nfdCustomSourcesValuePath,
			networkOperatorComponentName, gpuDriverEnabledValuePath, gpuDriverRDMAEnabledValuePath)
	default:
		return
	}

	slog.Warn(warning,
		"network_operator_in_bundle", networkOperator,
		"nic_label_rule", hasRule)
	b.appendWarning(warning)
}

// nfdStripsFeature mirrors nfd-worker v0.19.0's noPublishFeatures match: a
// pattern ending in "*" matches by prefix, any other pattern exactly.
func nfdStripsFeature(nfdValues map[string]any, feature string) bool {
	patterns, _ := component.GetValueByPath(nfdValues, nfdNoPublishFeaturesValuePath)
	list, _ := patterns.([]any)
	for _, p := range list {
		pattern, ok := p.(string)
		if !ok {
			continue
		}
		if prefix, wildcard := strings.CutSuffix(pattern, "*"); wildcard {
			if strings.HasPrefix(feature, prefix) {
				return true
			}
		} else if feature == pattern {
			return true
		}
	}
	return false
}

func nfdLabelsMellanoxNIC(nfdValues map[string]any) bool {
	rules, _ := component.GetValueByPath(nfdValues, nfdCustomSourcesValuePath)
	list, _ := rules.([]any)
	for _, r := range list {
		rule, ok := r.(map[string]any)
		if !ok {
			continue
		}
		labels, _ := rule["labels"].(map[string]any)
		_, prefixed := labels[nfdMellanoxNICLabel]
		_, unprefixed := labels[nfdMellanoxNICLabelUnprefixed]
		if prefixed || unprefixed {
			return true
		}
	}
	return false
}
