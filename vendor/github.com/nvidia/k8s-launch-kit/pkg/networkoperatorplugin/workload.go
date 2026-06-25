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

package networkoperatorplugin

import (
	"fmt"
	"os"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	sigyaml "sigs.k8s.io/yaml"
)

// patchWorkloadManifest reads a user-provided workload manifest, patches it with
// network annotations, resources, namespace, and node affinity, then serializes
// it back to YAML.
func patchWorkloadManifest(manifestPath string, cfg *config.LaunchKitConfig, group *config.ClusterConfig) (string, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", fmt.Errorf("failed to read workload manifest %s: %w", manifestPath, err)
	}

	var obj map[string]interface{}
	if err := sigyaml.Unmarshal(data, &obj); err != nil {
		return "", fmt.Errorf("failed to parse workload manifest %s: %w", manifestPath, err)
	}

	kind, _ := obj["kind"].(string)

	// Locate the metadata and pod spec paths based on the kind
	var podMeta, podSpec map[string]interface{}
	switch kind {
	case "Pod":
		podMeta = ensureMap(obj, "metadata")
		podSpec = ensureMap(obj, "spec")
	case "Deployment", "DaemonSet", "StatefulSet", "Job", "ReplicaSet":
		spec := ensureMap(obj, "spec")
		template := ensureMap(spec, "template")
		podMeta = ensureMap(template, "metadata")
		podSpec = ensureMap(template, "spec")
	default:
		return "", fmt.Errorf("unsupported workload kind %q: expected Pod, Deployment, DaemonSet, StatefulSet, or Job", kind)
	}

	// Set namespace
	meta := ensureMap(obj, "metadata")
	if cfg.PodNamespace != "" {
		meta["namespace"] = cfg.PodNamespace
	}

	// Add group suffix to name if present
	if name, ok := meta["name"].(string); ok && group.Identifier != "" {
		meta["name"] = name + "-" + group.Identifier
	}

	// Inject network annotation
	annotation := buildNetworkAnnotation(cfg, group)
	if annotation != "" {
		annotations := ensureMap(podMeta, "annotations")
		annotations["k8s.v1.cni.cncf.io/networks"] = annotation
	}

	// Inject network resources into the first container
	resources := buildNetworkResources(cfg, group)
	if len(resources) > 0 {
		containers, ok := podSpec["containers"].([]interface{})
		if ok && len(containers) > 0 {
			container := containers[0].(map[string]interface{})
			res := ensureMap(container, "resources")
			requests := ensureMap(res, "requests")
			limits := ensureMap(res, "limits")
			for k, v := range resources {
				requests[k] = v
				limits[k] = v
			}
		}
	}

	// Inject node affinity if group has a NodeSelector
	if len(group.NodeSelector) > 0 {
		affinity := buildNodeAffinity(group.NodeSelector)
		podSpec["affinity"] = affinity
	}

	out, err := sigyaml.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("failed to serialize patched workload: %w", err)
	}
	return string(out), nil
}

// buildNetworkAnnotation builds the k8s.v1.cni.cncf.io/networks annotation
// value based on the profile type and cluster config group.
func buildNetworkAnnotation(cfg *config.LaunchKitConfig, group *config.ClusterConfig) string {
	if cfg.Profile == nil {
		return ""
	}

	ewPFs := filterEastWestPFs(group.PFs)
	isMultirail := cfg.Profile.Multirail

	// Spectrum-X profiles
	if isSpectrumX(cfg) {
		var parts []string
		if cfg.Profile.SpectrumX.MultiplaneMode == "swplb" {
			for i := range ewPFs {
				for p := 0; p < cfg.Profile.SpectrumX.NumberOfPlanes; p++ {
					parts = append(parts, fmt.Sprintf("rail-%d-plane-%d", i, p))
				}
			}
		} else {
			for i := range ewPFs {
				parts = append(parts, fmt.Sprintf("rail-%d", i))
			}
		}
		return strings.Join(parts, ",")
	}

	// Determine network name based on deployment type
	var networkName string
	switch cfg.Profile.Deployment {
	case "sriov":
		if cfg.Sriov != nil {
			networkName = cfg.Sriov.NetworkName
		}
	case "host_device":
		if cfg.Hostdev != nil {
			networkName = cfg.Hostdev.NetworkName
		}
	case "rdma_shared":
		if cfg.Profile.Fabric == "infiniband" && cfg.Ipoib != nil {
			networkName = cfg.Ipoib.NetworkName
		} else if cfg.Macvlan != nil {
			networkName = cfg.Macvlan.NetworkName
		}
	}

	if networkName == "" {
		return ""
	}

	suffix := ""
	if group.Identifier != "" {
		suffix = "-" + group.Identifier
	}

	if isMultirail {
		var parts []string
		for _, pf := range ewPFs {
			if pf.Rail != nil {
				parts = append(parts, fmt.Sprintf("%s-rail-%d%s", networkName, *pf.Rail, suffix))
			}
		}
		return strings.Join(parts, ",")
	}

	return networkName + suffix
}

// buildNetworkResources builds the resource requests/limits map based on the
// profile type and cluster config group.
func buildNetworkResources(cfg *config.LaunchKitConfig, group *config.ClusterConfig) map[string]string {
	if cfg.Profile == nil {
		return nil
	}

	ewPFs := filterEastWestPFs(group.PFs)
	isMultirail := cfg.Profile.Multirail

	resources := map[string]string{}

	// Spectrum-X profiles
	if isSpectrumX(cfg) {
		if cfg.Profile.SpectrumX.MultiplaneMode == "swplb" {
			for i := range ewPFs {
				for p := 0; p < cfg.Profile.SpectrumX.NumberOfPlanes; p++ {
					resources[fmt.Sprintf("nvidia.com/rail_%d_plane_%d", i, p)] = "1"
				}
			}
		} else {
			for i := range ewPFs {
				resources[fmt.Sprintf("nvidia.com/rail_%d", i)] = "1"
			}
		}
		return resources
	}

	// Non-Spectrum-X profiles
	var resourcePrefix, resourceName string
	switch cfg.Profile.Deployment {
	case "sriov":
		resourcePrefix = "nvidia.com"
		if cfg.Sriov != nil {
			resourceName = cfg.Sriov.ResourceName
		}
	case "host_device":
		resourcePrefix = "nvidia.com"
		if cfg.Hostdev != nil {
			resourceName = cfg.Hostdev.ResourceName
		}
	case "rdma_shared":
		resourcePrefix = "rdma"
		if cfg.RdmaShared != nil {
			resourceName = cfg.RdmaShared.ResourceName
		}
	}

	if resourceName == "" {
		return nil
	}

	if isMultirail {
		for _, pf := range ewPFs {
			if pf.Rail != nil {
				key := fmt.Sprintf("%s/%s_rail_%d", resourcePrefix, resourceName, *pf.Rail)
				resources[key] = "1"
			}
		}
	} else {
		key := fmt.Sprintf("%s/%s", resourcePrefix, resourceName)
		resources[key] = "1"
	}

	return resources
}

// buildNodeAffinity builds a Kubernetes node affinity structure from a label selector map.
func buildNodeAffinity(nodeSelector map[string]string) map[string]interface{} {
	var expressions []interface{}
	for key, value := range nodeSelector {
		expr := map[string]interface{}{
			"key": key,
		}
		if value != "" {
			expr["operator"] = "In"
			expr["values"] = []interface{}{value}
		} else {
			expr["operator"] = "Exists"
		}
		expressions = append(expressions, expr)
	}

	return map[string]interface{}{
		"nodeAffinity": map[string]interface{}{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]interface{}{
				"nodeSelectorTerms": []interface{}{
					map[string]interface{}{
						"matchExpressions": expressions,
					},
				},
			},
		},
	}
}

// ensureMap ensures that the given key in the parent map exists and is a map.
// If it doesn't exist, an empty map is created and assigned.
func ensureMap(parent map[string]interface{}, key string) map[string]interface{} {
	v, ok := parent[key]
	if !ok || v == nil {
		m := map[string]interface{}{}
		parent[key] = m
		return m
	}
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	m := map[string]interface{}{}
	parent[key] = m
	return m
}

// isWorkloadTemplate returns true if the filename matches the default example
// workload template naming convention.
func isWorkloadTemplate(filename string) bool {
	return strings.Contains(filename, "example-daemonset")
}
