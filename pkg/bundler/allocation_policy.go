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
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// gpuAllocationPolicyPaths maps component name → the value paths that select
// the whole-GPU allocation policy (issue #1327). The policy is resolved and
// VERIFIED from the recipe at validation time, so bundle-time changes to
// these keys are being phased out in favor of recipe overlays. The
// component-level "enabled" toggle counts too: disabling an advertiser
// component at bundle time (--set <component>:enabled=false, honored by
// filterEnabledComponents) changes the allocation policy exactly like the
// nested keys. gpu-operator-ocp is the OpenShift advertiser (policy
// resolution reads its devicePlugin.enabled the same way).
var gpuAllocationPolicyPaths = map[string][]string{
	"nvidia-dra-driver-gpu": {"resources.gpus.enabled", "gpuResourcesEnabledOverride", config.ComponentEnabledKey},
	"gpu-operator":          {"devicePlugin.enabled", config.ComponentEnabledKey},
	"gpu-operator-ocp":      {"devicePlugin.enabled", config.ComponentEnabledKey},
}

// enforceAllocationPolicyOverrides applies the #1327 bundle-time override
// policy AFTER alias resolution through the recipe-bound registry (so
// --set dradriver:... and --set nvidia-dra-driver-gpu:... are treated
// identically, and REST/SDK callers that populate the config directly are
// covered too):
//
//   - --dynamic declarations targeting an allocation-policy key are REJECTED
//     (ErrCodeInvalidRequest): a value deferred to install time is unknowable
//     when the policy is resolved and verified at validation time.
//   - Static --set / --set-json / --set-file / config-file overrides on these
//     keys WARN (deprecation, slog.Warn + deployment note) — the documented
//     AKS workflow still uses them, so they are not rejected yet; allocation
//     mode should move to a recipe overlay.
//
// A path matches a key when it equals the key, is a parent of it (an object
// override that can contain the key), or is a child of it (an override that
// would reshape the key's boolean value).
func (b *DefaultBundler) enforceAllocationPolicyOverrides(provider recipe.DataProvider) error {
	if b.Config == nil {
		return nil
	}

	dynamicValues, err := b.buildDynamicValuesMap(provider)
	if err != nil {
		return err
	}

	// Sort component names for deterministic error/warning ordering.
	components := make([]string, 0, len(gpuAllocationPolicyPaths))
	for name := range gpuAllocationPolicyPaths {
		components = append(components, name)
	}
	sort.Strings(components)

	for _, component := range components {
		keys := gpuAllocationPolicyPaths[component]

		for _, path := range dynamicValues[component] {
			if key := matchingPolicyKey(path, keys); key != "" {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"--dynamic declaration %s:%s targets the GPU allocation-policy key %q: the allocation policy is resolved and verified from the recipe at validation time, so this value cannot be deferred to install time — set it in a recipe overlay instead (issue #1327)",
					component, path, key))
			}
		}

		var staticPaths []string
		for path := range b.getValueOverridesForComponent(component, provider) {
			if matchingPolicyKey(path, keys) != "" {
				staticPaths = append(staticPaths, path)
			}
		}
		for path := range b.getTypedValueOverridesForComponent(component, provider) {
			if matchingPolicyKey(path, keys) != "" {
				staticPaths = append(staticPaths, path)
			}
		}
		if len(staticPaths) > 0 {
			sort.Strings(staticPaths)
			warning := fmt.Sprintf(
				"deprecated: bundle-time override of GPU allocation-policy key(s) %s on component %q — the allocation policy validators verify is resolved from the recipe, so a bundle-time change will be reported as recipe/cluster drift; move the allocation mode to a recipe overlay (issue #1327)",
				strings.Join(staticPaths, ", "), component)
			slog.Warn(warning)
			b.warnings = append(b.warnings, warning)
		}
	}
	return nil
}

// matchingPolicyKey returns the first allocation-policy key the override
// path touches ("" when none): exact match, a parent object override that
// contains the key, or a sub-path override beneath the key.
func matchingPolicyKey(path string, keys []string) string {
	for _, key := range keys {
		if path == key || strings.HasPrefix(key, path+".") || strings.HasPrefix(path, key+".") {
			return key
		}
	}
	return ""
}
