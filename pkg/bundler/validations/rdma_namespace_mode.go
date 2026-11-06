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

package validations

import (
	"context"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// rdmaCNIValueKey is the network-operator value that chains the rdma CNI into
// a recipe's RDMA attachment. It is not a chart key; only recipes whose
// manifests read it set it.
const rdmaCNIValueKey = "rdmaCni"

// CheckRDMANamespaceModeCoherent warns when the recipe's snapshot evidence of
// the RDMA subsystem's network namespace mode (metadata.rdmaNamespaceMode)
// disagrees with how the bundle attaches RDMA devices: host-device alone
// (rdmaCni.enabled false or unset) works only in shared mode, and the rdma
// CNI (rdmaCni.enabled=true) only in exclusive mode. Either
// mismatch leaves NCCL pods without RDMA, which surfaces only when a workload
// starts. A recipe without the evidence gets a note instead. Applies only
// when network-operator's effective values carry rdmaCni; the registry also
// scopes it to the recipes whose manifests read the value.
func CheckRDMANamespaceModeCoherent(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	ref := recipeResult.GetComponentRef(componentName)
	if ref == nil {
		return nil, nil
	}
	keys := componentOverrideKeys(componentName, recipeResult.DataProvider())
	if componentDisabled(ref, bundlerConfig, keys) {
		return nil, nil
	}
	values, err := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, keys, "RDMA namespace mode coherence")
	if err != nil {
		return nil, []error{err}
	}
	rdmaCNI, present := values[rdmaCNIValueKey].(map[string]any)
	if !present {
		return nil, nil
	}
	enabled := fmt.Sprint(rdmaCNI["enabled"]) == overrideValueTrue

	const fixExclusive = "set exclusive mode on every RDMA node " +
		"(`options ib_core netns_mode=0` in /etc/modprobe.d/ib_core.conf, then reboot)"
	switch mode := recipeResult.Metadata.RDMANamespaceMode; {
	case mode == recipe.RDMANamespaceModeShared && enabled:
		return []string{fmt.Sprintf(
			"%s: the snapshot's nodes run the RDMA subsystem in shared network namespace mode, "+
				"but the bundle chains the rdma CNI, which refuses shared mode, so pods attaching "+
				"the RDMA network will not start. Bundle with --set %s:%s.enabled=false to attach "+
				"with host-device alone, or %s and recapture the snapshot",
			componentName, componentName, rdmaCNIValueKey, fixExclusive)}, nil
	case mode == recipe.RDMANamespaceModeExclusive && !enabled:
		return []string{fmt.Sprintf(
			"%s: the snapshot's nodes run the RDMA subsystem in exclusive network namespace mode, "+
				"but the bundle attaches with host-device alone, which leaves each pod's RDMA "+
				"device in the host namespace. Bundle with --set %s:%s.enabled=true to chain the rdma CNI",
			componentName, componentName, rdmaCNIValueKey)}, nil
	case mode == recipe.RDMANamespaceModeExclusive, mode == recipe.RDMANamespaceModeShared:
		return nil, nil
	case mode == "":
		want := "exclusive"
		if !enabled {
			want = "shared"
		}
		return []string{fmt.Sprintf(
			"%s: no metadata.rdmaNamespaceMode snapshot evidence was recorded, so the RDMA "+
				"attachment (%s.enabled=%t) was not checked against the nodes. It needs %s RDMA "+
				"network namespace mode; regenerate the recipe from a current snapshot to verify",
			componentName, rdmaCNIValueKey, enabled, want)}, nil
	default:
		return []string{fmt.Sprintf(
			"%s: metadata.rdmaNamespaceMode=%q is not exclusive or shared, so the RDMA attachment "+
				"(%s.enabled=%t) was not checked against the nodes",
			componentName, mode, rdmaCNIValueKey, enabled)}, nil
	}
}
