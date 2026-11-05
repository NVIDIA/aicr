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
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// overridePrefixFor returns the component key an operator writes in front of
// a --set or --dynamic path for comp: its first override alias when it has
// one, else its name.
func overridePrefixFor(comp *recipe.ComponentConfig) string {
	if len(comp.ValueOverrideKeys) > 0 {
		return comp.ValueOverrideKeys[0]
	}
	return comp.Name
}

// isEmptyValue reports whether v counts as unset for requiredValues: nil, an
// empty string, or an empty map or slice. Numbers and booleans are never
// empty, since false and 0 are deliberate settings.
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	default:
		return false
	}
}

// declaredDynamic reports whether path, or an ancestor or descendant of it,
// is in dynPaths. A dynamic declaration on an ancestor moves the whole
// subtree to install time, and one on a descendant leaves the parent map
// unshippable either way.
func declaredDynamic(path string, dynPaths map[string]struct{}) bool {
	for dyn := range dynPaths {
		if overridePathsIntersect(dyn, path) {
			return true
		}
	}
	return false
}

// validateRequiredAndSecretValues enforces the registry's requiredValues and
// installTimeSecretPaths for componentName. values must already carry every
// override, including --set-json/--set-file and scheduling injection, since
// a required path is judged on what the bundle ships and a secret path on
// anything an override could have written into it (ADR-029).
func (b *DefaultBundler) validateRequiredAndSecretValues(componentName string, values map[string]any, provider recipe.DataProvider) error {
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return errors.WrapWithContext(errors.ErrCodeInternal,
			"failed to load component registry for required value validation",
			err,
			map[string]any{errCtxKeyComponent: componentName})
	}
	comp := registry.Get(componentName)
	if comp == nil || (len(comp.RequiredValues) == 0 && len(comp.InstallTimeSecretPaths) == 0) {
		return nil
	}
	dynPaths := b.dynamicPathSetFor(componentName, provider)
	prefix := overridePrefixFor(comp)

	for _, path := range comp.InstallTimeSecretPaths {
		if v, ok := component.GetValueByPath(values, path); ok && !isEmptyValue(v) {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q: %q is an install-time secret and must not be set in a values file, an overlay or with --set; "+
					"declare it with --dynamic %s:%s and write the value into the bundle's cluster-values.yaml right before install",
				componentName, path, prefix, path))
		}
	}

	var missingSet, missingDynamic []string
	for _, path := range comp.RequiredValues {
		if declaredDynamic(path, dynPaths) {
			continue
		}
		if v, ok := component.GetValueByPath(values, path); ok && !isEmptyValue(v) {
			continue
		}
		if slices.Contains(comp.InstallTimeSecretPaths, path) {
			missingDynamic = append(missingDynamic, path)
		} else {
			missingSet = append(missingSet, path)
		}
	}
	if len(missingSet) == 0 && len(missingDynamic) == 0 {
		return nil
	}
	var hints []string
	if len(missingSet) > 0 {
		hints = append(hints, fmt.Sprintf("%s (pass --set %s:<path>=<value>, or declare --dynamic %s:<path> to supply it at install time)",
			strings.Join(missingSet, ", "), prefix, prefix))
	}
	if len(missingDynamic) > 0 {
		hints = append(hints, fmt.Sprintf("%s (an install-time secret: declare --dynamic %s:<path> and fill it in cluster-values.yaml before install)",
			strings.Join(missingDynamic, ", "), prefix))
	}
	return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
		"component %q: required values not set: %s", componentName, strings.Join(hints, "; ")))
}

// rejectInstallTimeSecretsOutsideHelm refuses every deployer but helm for a
// component with an install-time secret. helm's install.sh reads the filled
// value from cluster-values.yaml on the install machine and refuses to run
// while the stub is empty; flux, fleet, terraform and argocd-helm write the
// filled value into the artifacts they generate or the state they keep, and
// helmfile reads the file at apply time like helm but carries no stub check,
// so an empty token reaches the chart. A component is held to
// this once one of its secret paths is required or declared --dynamic; a
// secret path left empty and optional ships as an empty stub, which no
// deployer persists anything for. refs is the filtered component list, so a
// disabled or filtered-out component is not judged.
func (b *DefaultBundler) rejectInstallTimeSecretsOutsideHelm(refs []recipe.ComponentRef, provider recipe.DataProvider) error {
	if b.Config == nil || b.Config.Deployer() == config.DeployerHelm {
		return nil
	}
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for install-time secret validation")
	}
	for _, ref := range refs {
		comp := registry.Get(ref.Name)
		if comp == nil || len(comp.InstallTimeSecretPaths) == 0 {
			continue
		}
		dynPaths := b.dynamicPathSetFor(ref.Name, provider)
		for _, secret := range comp.InstallTimeSecretPaths {
			if !slices.Contains(comp.RequiredValues, secret) && !declaredDynamic(secret, dynPaths) {
				continue
			}
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q: %q is an install-time secret, which only the helm deployer supports: its install.sh reads the value "+
					"from cluster-values.yaml and refuses to run while it is empty; the %q deployer either stores the filled value in the "+
					"artifacts it generates or installs without that check. Bundle with --deployer helm",
				ref.Name, secret, b.Config.Deployer()))
		}
	}
	return nil
}
