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

package v1

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// GPU allocation policy values (#1327). The policy names the mechanism whole
// GPUs are requested through — configuration selects it, cluster inspection
// verifies it, and a mismatch fails closed. Backend and request syntax are
// deliberately distinct in the names: after KEP-5004 an extended-resource
// request can be served by either backend, so "extended-resource" alone would
// be ambiguous.
const (
	// GPUAllocationPolicyUnspecified means the ValidationInput carries no
	// resolved policy (standalone Go validator inputs without recipe
	// context, or inputs produced by pre-policy AICR versions): validators
	// keep their capability-driven automatic selection and verify nothing.
	// The --cncf-submission shell collector bypasses Go validator dispatch
	// entirely and is separate work (#1629) — it is NOT covered by this
	// constant's semantics.
	GPUAllocationPolicyUnspecified = "unspecified"

	// GPUAllocationPolicyDevicePluginExtendedResource is the production
	// default: whole GPUs via the device plugin, requested as the
	// nvidia.com/gpu extended resource.
	GPUAllocationPolicyDevicePluginExtendedResource = "device-plugin-extended-resource"

	// GPUAllocationPolicyDRAResourceClaim is the explicit recipe-level
	// opt-in: whole GPUs via DRA, requested through gpu.nvidia.com
	// ResourceClaims.
	GPUAllocationPolicyDRAResourceClaim = "dra-resource-claim"

	// GPUAllocationPolicyDRAExtendedResource is RESERVED for KEP-5004
	// (DRAExtendedResource): a nvidia.com/gpu extended-resource request
	// served by a DRA backend. Declared so the enum is complete, but never
	// produced by resolution today — AICR does not yet validate that path.
	GPUAllocationPolicyDRAExtendedResource = "dra-extended-resource"
)

// Component names and hydrated-value paths the policy resolution reads.
const (
	// draDriverGPUComponentName is the registry name of the NVIDIA DRA
	// driver component whose resources.gpus.enabled value is THE whole-GPU
	// allocation switch.
	draDriverGPUComponentName = "nvidia-dra-driver-gpu"
	// gpuOperatorComponentName is the registry name of the GPU operator
	// component whose devicePlugin.enabled value pins the device-plugin
	// advertiser.
	gpuOperatorComponentName = "gpu-operator"
	// gpuOperatorOCPComponentName is the OpenShift GPU operator component:
	// OCP recipes disable gpu-operator and carry this instead; its values
	// pin devicePlugin.enabled the same way.
	gpuOperatorOCPComponentName = "gpu-operator-ocp"

	// valuePathGPUsEnabled is the DRA driver's full-GPU allocation switch.
	valuePathGPUsEnabled = "resources.gpus.enabled"
	// valuePathGPUsEnabledOverride is the DRA driver chart's install-guard
	// waiver. It is NOT a mode input — only a validity gate: the upstream
	// chart refuses gpus.enabled=true without it.
	valuePathGPUsEnabledOverride = "gpuResourcesEnabledOverride"
	// valuePathDevicePluginEnabled is the GPU operator's device-plugin
	// toggle (the whole-GPU extended-resource advertiser).
	valuePathDevicePluginEnabled = "devicePlugin.enabled"
)

// ResolveGPUAllocationPolicy resolves the whole-GPU allocation policy from
// the recipe's fully HYDRATED component values (base values + platform values
// files + overlays + mixins, via GetValuesForComponentWithContext — inline
// ComponentRef.Overrides alone would miss values-file content). The contract
// (issue #1327):
//
//	nvidia-dra-driver-gpu resources.gpus.enabled is THE switch:
//	  true  → dra-resource-claim
//	  false → device-plugin-extended-resource
//	Component absent or disabled → device-plugin-extended-resource
//	(no full-GPU DRA is possible without the driver).
//
// The device-plugin advertiser is read from an enabled gpu-operator OR
// gpu-operator-ocp componentRef (OCP recipes disable the former and carry the
// latter); if both are enabled, gpu-operator wins with a warning. When
// NEITHER is present and enabled, there is no device-plugin advertiser at all
// — externally managed advertisers are an explicit #1327 non-goal.
//
// Validity gates (ErrCodeInvalidRequest, fail closed at resolution time):
//   - an ENABLED nvidia-dra-driver-gpu component with resources.gpus.enabled
//     ABSENT: the chart's declared default (true) would diverge from any
//     silent resolution — the switch must be explicitly true or false.
//   - gpus.enabled=true with gpuResourcesEnabledOverride=false: the upstream
//     chart install guard rejects this combination.
//   - no whole-GPU advertiser: gpus.enabled explicitly false (or the DRA
//     component absent/disabled) AND no usable device-plugin advertiser
//     (devicePlugin.enabled=false, or no enabled GPU operator component).
//
// Transitional warnings (slog.Warn, never an error — today's stock recipes
// are dual-advertised; a later PR promotes these to errors):
//   - gpus.enabled=true with devicePlugin.enabled=true: dual advertisement;
//     still resolves dra-resource-claim (that is what schedules under kai).
//   - gpus.enabled=false with gpuResourcesEnabledOverride=true: the inert
//     waiver disarms the chart-guard tripwire.
//
// A nil RecipeResult (no recipe context) resolves to
// GPUAllocationPolicyUnspecified. When the DRA component is ENABLED,
// resources.gpus.enabled must be explicitly present and boolean — the
// upstream chart's declared default is true, so silently resolving an absent
// switch would diverge from what Helm deploys; absence is
// ErrCodeInvalidRequest (stock recipes always pin it via the component
// values). An absent gpuResourcesEnabledOverride is false (matches the chart
// default) and, on an enabled operator component, an absent
// devicePlugin.enabled is true (the upstream chart default, pinned
// explicitly in AICR's component values). A value present at one of the
// three paths with a non-boolean type is a configuration error.
func ResolveGPUAllocationPolicy(parent context.Context, r *recipe.RecipeResult) (string, error) {
	if r == nil {
		return GPUAllocationPolicyUnspecified, nil
	}

	// Hydration reads through the recipe's DataProvider (embedded data or a
	// --data directory); bound it so a hung backing store cannot stall the
	// caller indefinitely.
	ctx, cancel := context.WithTimeout(parent, defaults.FileReadTimeout)
	defer cancel()

	gpusEnabled := false
	overrideWaiver := false
	draRef := enabledComponentRef(r, draDriverGPUComponentName)
	if draRef != nil {
		values, err := r.GetValuesForComponentWithContext(ctx, draRef.Name)
		if err != nil {
			return "", err
		}
		var gpusEnabledSet bool
		gpusEnabled, gpusEnabledSet, err = lookupBoolValueFound(values, draRef.Name, valuePathGPUsEnabled)
		if err != nil {
			return "", err
		}
		if !gpusEnabledSet {
			// The upstream chart's DECLARED default is gpus.enabled=true, so
			// silently resolving an absent switch to device-plugin would
			// diverge from what Helm deploys (chart-guard failure without
			// the waiver, or dual advertisement with it). Configuration
			// selection must match rendered deployment intent — require the
			// explicit pin. Stock recipes always carry it via the component
			// values; only custom/SDK values files can omit it.
			return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"invalid GPU allocation configuration: component %q is enabled but %q is not set — the upstream chart's declared default (true) would diverge from the resolved allocation policy at deploy time; pin the value explicitly in the recipe (issue #1327)",
				draRef.Name, valuePathGPUsEnabled))
		}
		overrideWaiver, err = lookupBoolValue(values, draRef.Name, valuePathGPUsEnabledOverride, false)
		if err != nil {
			return "", err
		}
	}

	// The device-plugin advertiser comes from an ENABLED GPU operator
	// component — gpu-operator, or gpu-operator-ocp on OpenShift recipes
	// (which disable gpu-operator). No enabled operator component means no
	// device-plugin advertiser: externally managed advertisers are an
	// explicit #1327 non-goal, so devicePluginEnabled stays false and the
	// row-6 gate below fails closed when DRA is not the configured
	// mechanism either.
	devicePluginEnabled := false
	opRef := enabledComponentRef(r, gpuOperatorComponentName)
	if ocpRef := enabledComponentRef(r, gpuOperatorOCPComponentName); ocpRef != nil {
		if opRef != nil {
			slog.Warn("both gpu-operator and gpu-operator-ocp are enabled in the recipe; resolving devicePlugin.enabled from gpu-operator",
				"preferred", gpuOperatorComponentName, "ignored", gpuOperatorOCPComponentName)
		} else {
			opRef = ocpRef
		}
	}
	if opRef != nil {
		values, err := r.GetValuesForComponentWithContext(ctx, opRef.Name)
		if err != nil {
			return "", err
		}
		// Key absent → true: the upstream chart default (pinned explicitly
		// in AICR's component values).
		devicePluginEnabled, err = lookupBoolValue(values, opRef.Name, valuePathDevicePluginEnabled, true)
		if err != nil {
			return "", err
		}
	}

	if gpusEnabled {
		if !overrideWaiver {
			return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"invalid GPU allocation configuration: %s %s=true requires %s=true — the upstream chart install guard rejects this combination; set both in the recipe overlay (issue #1327)",
				draDriverGPUComponentName, valuePathGPUsEnabled, valuePathGPUsEnabledOverride))
		}
		if devicePluginEnabled {
			// TODO(#1327 follow-up): promote to an error after the
			// production-default flip lands.
			slog.Warn("transitional GPU allocation configuration: both full-GPU DRA and the device plugin are enabled — dual advertisement risks GPU over-admission; resolving policy to dra-resource-claim (that is what schedules under kai-scheduler)",
				"policy", GPUAllocationPolicyDRAResourceClaim,
				draDriverGPUComponentName, valuePathGPUsEnabled+"=true",
				gpuOperatorComponentName, valuePathDevicePluginEnabled+"=true")
		}
		return GPUAllocationPolicyDRAResourceClaim, nil
	}

	if !devicePluginEnabled {
		return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"invalid GPU allocation configuration: no whole-GPU advertiser — %s %s is false/absent, and the device plugin is unavailable (%s/%s absent or disabled in the recipe, or %s=false); enable exactly one mechanism in the recipe overlay (issue #1327)",
			draDriverGPUComponentName, valuePathGPUsEnabled,
			gpuOperatorComponentName, gpuOperatorOCPComponentName,
			valuePathDevicePluginEnabled))
	}

	if draRef != nil && overrideWaiver {
		// TODO(#1327 follow-up): promote to an error after the
		// production-default flip lands.
		slog.Warn("transitional GPU allocation configuration: gpuResourcesEnabledOverride=true with resources.gpus.enabled=false is an inert waiver that disarms the upstream chart's install-guard tripwire; resolving policy to device-plugin-extended-resource",
			"policy", GPUAllocationPolicyDevicePluginExtendedResource,
			"component", draDriverGPUComponentName)
	}
	return GPUAllocationPolicyDevicePluginExtendedResource, nil
}

// ToValidationInputWithContext converts a RecipeResult to a ValidationInput
// like ToValidationInput, and additionally resolves GPUAllocationPolicy from
// the recipe's hydrated component values (see ResolveGPUAllocationPolicy).
// Returns (nil, nil) for a nil RecipeResult; returns ErrCodeInvalidRequest
// when the recipe's allocation configuration is invalid (fail closed at
// conversion time, before any validator Job is deployed).
func ToValidationInputWithContext(ctx context.Context, r *recipe.RecipeResult) (*ValidationInput, error) {
	validation := ToValidationInput(r)
	if validation == nil {
		return nil, nil //nolint:nilnil // mirrors ToValidationInput's nil-in/nil-out contract
	}
	policy, err := ResolveGPUAllocationPolicy(ctx, r)
	if err != nil {
		return nil, err
	}
	validation.GPUAllocationPolicy = policy
	return validation, nil
}

// enabledComponentRef returns the recipe's componentRef for name when it is
// present AND enabled (Overrides.enabled != false), nil otherwise. A disabled
// component contributes nothing to policy resolution — it will not be
// deployed, so its values cannot select an allocation mechanism.
func enabledComponentRef(r *recipe.RecipeResult, name string) *recipe.ComponentRef {
	ref := r.GetComponentRef(name)
	if ref == nil || !ref.IsEnabled() {
		return nil
	}
	return ref
}

// lookupBoolValue walks a dot-separated path through hydrated component
// values and returns the boolean at the leaf, or fallback when any path
// segment is absent. A segment that is present but not descendable, or a leaf
// that is present but not a boolean, is a configuration error
// (ErrCodeInvalidRequest, fail closed): a malformed allocation-policy value
// must not silently resolve to a default.
func lookupBoolValue(values map[string]any, component, path string, fallback bool) (bool, error) {
	v, found, err := lookupBoolValueFound(values, component, path)
	if err != nil {
		return false, err
	}
	if !found {
		return fallback, nil
	}
	return v, nil
}

// lookupBoolValueFound is lookupBoolValue's presence-aware core: found
// reports whether the leaf exists, so callers can distinguish an explicit
// value from chart-default inheritance — the DRA switch REQUIRES an explicit
// value because the chart's declared default (gpus.enabled=true) diverges
// from any silent resolution default (see ResolveGPUAllocationPolicy).
func lookupBoolValueFound(values map[string]any, component, path string) (val, found bool, err error) {
	parts := strings.Split(path, ".")
	var current any = values
	for i, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return false, false, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q value %q: segment %q is not a map — cannot resolve the GPU allocation policy from a malformed value",
				component, path, strings.Join(parts[:i], ".")))
		}
		v, exists := m[part]
		if !exists || v == nil {
			// Explicit YAML null (a common "empty this map" idiom) is
			// treated exactly like an absent key — `devicePlugin: null`
			// and `devicePlugin: {}` must not diverge (the latter falls
			// through to the absent-leaf path). Malformed non-null,
			// non-map, non-bool values still fail closed below.
			return false, false, nil
		}
		current = v
	}
	b, ok := current.(bool)
	if !ok {
		return false, false, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"component %q value %q must be a boolean, got %T — cannot resolve the GPU allocation policy from a malformed value",
			component, path, current))
	}
	return b, true, nil
}
