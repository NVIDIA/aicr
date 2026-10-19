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
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// The gpu-operator values path and toolkit variable read by
// warnNRIManagementAllowlist.
const (
	gpuOperatorToolkitEnvValuePath = "toolkit.env"
	nriManagementNamespacesEnvName = "NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES"
)

// warnNRIManagementAllowlist warns when the GPU Operator toolkit's NRI
// management allowlist names a namespace that no component in the bundle
// deploys into. Any pod in an allowlisted namespace can request every GPU on
// the node through the management CDI device, outside device-plugin
// allocation, so an entry left behind by a dropped component (for example
// --set nv-sentinel:enabled=false on RKE2) grants that access to whoever later
// creates the namespace. A subset bundle stays silent because the component
// may be deployed outside it.
func (b *DefaultBundler) warnNRIManagementAllowlist(recipeResult *recipe.RecipeResult, componentValues map[string]map[string]any) {
	if b.Config == nil || recipeResult == nil || len(b.Config.Bundlers()) > 0 {
		return
	}
	gpuValues, ok := componentValues[gpuOperatorComponentName]
	if !ok {
		return
	}
	allowlisted := nriManagementNamespaces(gpuValues)
	if len(allowlisted) == 0 {
		return
	}

	deployed := make(map[string]bool, len(recipeResult.ComponentRefs))
	for _, ref := range recipeResult.ComponentRefs {
		if _, bundled := componentValues[ref.Name]; bundled && ref.Namespace != "" {
			deployed[ref.Namespace] = true
		}
	}
	var orphaned []string
	for _, ns := range allowlisted {
		if !deployed[ns] {
			orphaned = append(orphaned, ns)
		}
	}
	if len(orphaned) == 0 {
		return
	}

	warning := fmt.Sprintf(
		"%[1]s %[2]s sets %[3]s to namespace(s) %[4]s, but no component in this bundle deploys there. "+
			"Any pod in an allowlisted namespace can request every GPU on the node through the "+
			"management CDI device, outside device-plugin allocation. If the component that needed "+
			"it was dropped (for example --set nv-sentinel:enabled=false), remove the entry from %[2]s; "+
			"on the stock RKE2 recipes it is the only entry, so --set-json 'gpuoperator:%[2]s=[]' does it.",
		gpuOperatorComponentName, gpuOperatorToolkitEnvValuePath, nriManagementNamespacesEnvName,
		strings.Join(orphaned, ", "))
	slog.Warn(warning,
		"component", gpuOperatorComponentName,
		"namespaces", orphaned)
	b.appendWarning(warning)
}

// nriManagementNamespaces returns the sorted, de-duplicated namespaces listed
// in the toolkit's NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES, a comma-separated
// value.
func nriManagementNamespaces(gpuValues map[string]any) []string {
	raw, _ := component.GetValueByPath(gpuValues, gpuOperatorToolkitEnvValuePath)
	env, _ := raw.([]any)
	var namespaces []string
	for _, entry := range env {
		m, ok := entry.(map[string]any)
		if !ok || m["name"] != nriManagementNamespacesEnvName {
			continue
		}
		value, _ := m["value"].(string)
		for _, ns := range strings.Split(value, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
}
