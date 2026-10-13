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
	"log/slog"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"

	"k8s.io/apimachinery/pkg/util/validation"
)

// nodewrightAutoTaintAnnotationPrefix is the node annotation nodewright-operator
// writes for autoTaintNewNodes, suffixed with the runtime-required taint key
// (autoTaintAnnotationKey, NVIDIA/nodewright#703).
const nodewrightAutoTaintAnnotationPrefix = "nodewright.nvidia.com/autoTaint_"

// CheckNodewrightRuntimeRequiredTaintKey reports a runtime-required taint whose key
// cannot name the annotation nodewright-operator derives from it, which rules
// out a DNS prefix and any key over 53 characters. Before v0.20.0 the operator
// accepted such a key and stopped reconciling every NodeWright once it
// auto-tainted a node; from v0.20.0 it refuses to start. The registry wires it
// at severity: error so the bundle fails instead of the deploy.
//
// Every source that can land on the component's accelerated taintStrPaths is
// checked: the workload-gate taint, and the effective value after --set,
// --set-json and --set-file. Rejecting a bad key from any of them avoids
// re-deriving which one the bundler lets win. A nil bundlerConfig (the
// values-only SDK path) still checks the recipe's own values. A --dynamic
// declaration on a taint path is rejected, because its install-time value in
// cluster-values.yaml is never checked.
func CheckNodewrightRuntimeRequiredTaintKey(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if recipeResult == nil || !checkConditions(recipeResult, conditions) {
		return nil, nil
	}
	ref := recipeResult.GetComponentRef(componentName)
	if ref == nil {
		return nil, nil
	}
	provider := recipeResult.DataProvider()
	keys := componentOverrideKeys(componentName, provider)
	if componentDisabled(ref, bundlerConfig, keys) {
		return nil, nil
	}

	var taintKeys []string
	if bundlerConfig != nil {
		if gate := bundlerConfig.WorkloadGateTaint(); gate != nil {
			taintKeys = append(taintKeys, gate.Key)
		}
	}
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return nil, []error{aicrerrors.WrapWithContext(aicrerrors.ErrCodeInternal,
			"cannot verify the runtime-required taint key: failed to load the component registry",
			err, map[string]any{logKeyComponent: componentName})}
	}
	var warnings []string
	paths := registry.Get(componentName).GetAcceleratedTaintStrPaths()
	if len(paths) > 0 {
		values, valuesErr := effectiveComponentValues(ctx, recipeResult, bundlerConfig, componentName, keys, "the runtime-required taint key")
		if valuesErr != nil {
			return nil, []error{valuesErr}
		}
		for _, p := range paths {
			if raw, ok := component.GetValueByPath(values, p); ok {
				if s, isString := raw.(string); isString && s != "" {
					taintKeys = append(taintKeys, taintStringKey(s))
				}
			}
		}
		if bundlerConfig != nil {
			for _, hit := range dynamicPathIntersections(bundlerConfig, keys, paths) {
				msg := fmt.Sprintf("%s: --dynamic %s:%s targets the runtime-required taint path %s, whose "+
					"install-time value in cluster-values.yaml is never checked; set the taint statically "+
					"(--workload-gate or --set) instead",
					componentName, hit.key, hit.path, hit.guard)
				slog.Warn(msg, logKeyComponent, componentName)
				warnings = append(warnings, msg)
			}
		}
	}

	for _, key := range slices.Compact(slices.Sorted(slices.Values(taintKeys))) {
		if len(validation.IsQualifiedName(nodewrightAutoTaintAnnotationPrefix+key)) > 0 {
			msg := fmt.Sprintf("%s: runtime-required taint key %q cannot be used: the operator writes it into "+
				"the node annotation %s<key>, so it must have no DNS prefix and be at most 53 characters; "+
				"use an unprefixed key such as the default nodewright.nvidia.com",
				componentName, key, nodewrightAutoTaintAnnotationPrefix)
			slog.Warn(msg, logKeyComponent, componentName)
			warnings = append(warnings, msg)
		}
	}
	return warnings, nil
}

// taintStringKey returns the key of a taint in key=value:effect or key:effect
// form. Neither the value nor the effect can contain '=' or ':'.
func taintStringKey(s string) string {
	if i := strings.IndexAny(s, "=:"); i >= 0 {
		return s[:i]
	}
	return s
}
