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

// Package allocpolicy is the canonical, dependency-neutral owner of the
// #1327 GPU allocation-policy descriptor and the shared advertiser
// vocabulary and tuple-coherence rules (ADR-015, "GKE amendment to the
// #1327 allocation-policy model").
//
// Before this package, the policy path vocabulary existed as private
// copies in pkg/bundler, pkg/validator/v1, and pkg/recipe. A copied map or
// a second evaluator would let a future policy key or advertiser value
// silently reopen the bypass the ADR-015 amendment closes, so every
// consumer — profile finalization, the hydrating artifact gate, bundler
// enforcement, and validation-time policy resolution — reads this
// descriptor and no other.
//
// The descriptor is APPEND-ONLY while any supported artifact may reference
// an entry: the profile lock closure is recomputed from it at every
// artifact boundary rather than persisted, so removing or renaming a
// selector path would silently unlock it on older authentic recipes the
// moment a newer binary recomputes the closure. Removing an entry requires
// a deprecated tombstone retained for the support window, or ending
// support for the affected artifacts — an apiVersion bump alone does not
// permit removal (ADR-011 transition windows keep the prior version
// accepted).
package allocpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// AdvertiserExternal is the only non-empty advertiser value. It declares
// that a platform-managed component outside the recipe advertises
// nvidia.com/gpu (e.g. GKE's managed device plugin). It is never inferred
// from effective values — only an explicit profile declaration produces it
// — preserving #1327's fail-closed posture for undeclared recipes.
const AdvertiserExternal = "external"

// Canonical component and selector-path vocabulary. These are the
// registry component names and hydrated-value paths whose values select
// the whole-GPU allocation policy.
const (
	// ComponentDRADriver is the NVIDIA DRA driver component whose
	// resources.gpus.enabled value is THE whole-GPU allocation switch.
	ComponentDRADriver = "nvidia-dra-driver-gpu"
	// ComponentGPUOperator is the GPU operator component whose
	// devicePlugin.enabled value pins the device-plugin advertiser.
	ComponentGPUOperator = "gpu-operator"
	// ComponentGPUOperatorOCP is the OpenShift GPU operator: OCP recipes
	// disable gpu-operator and carry this instead; its values pin
	// devicePlugin.enabled the same way.
	ComponentGPUOperatorOCP = "gpu-operator-ocp"
	// ComponentDRADriverOCP is the OpenShift DRA driver: OCP recipes
	// disable nvidia-dra-driver-gpu and carry this instead; its values
	// pin resources.gpus.enabled the same way (same chart, reused).
	ComponentDRADriverOCP = "nvidia-dra-driver-gpu-ocp"

	// PathDRAGPUsEnabled is the DRA driver's full-GPU allocation switch.
	PathDRAGPUsEnabled = "resources.gpus.enabled"
	// PathDRAGPUsEnabledOverride is the DRA driver chart's install-guard
	// waiver — a validity gate, not a mode input.
	PathDRAGPUsEnabledOverride = "gpuResourcesEnabledOverride"
	// PathDevicePluginEnabled is the GPU operator's device-plugin toggle
	// (the whole-GPU extended-resource advertiser).
	PathDevicePluginEnabled = "devicePlugin.enabled"
)

// Entry is one advertiser component's canonical policy-selector paths. The
// synthetic per-component presence path ("enabled") is deliberately NOT a
// selector path: referencing an advertiser component is not policy
// ownership, and the closure trigger must be able to distinguish the two
// (ADR-015: the synthetic presence path never triggers the closure).
type Entry struct {
	// Component is the registry component name.
	Component string
	// SelectorPaths are the non-synthetic hydrated-value paths whose
	// values select the allocation policy.
	SelectorPaths []string
}

// Descriptor returns the canonical descriptor in deterministic
// (lexicographic by component) order. Callers receive a fresh copy and may
// not mutate shared state.
func Descriptor() []Entry {
	return []Entry{
		{Component: ComponentGPUOperator, SelectorPaths: []string{PathDevicePluginEnabled}},
		{Component: ComponentGPUOperatorOCP, SelectorPaths: []string{PathDevicePluginEnabled}},
		{Component: ComponentDRADriver, SelectorPaths: []string{
			PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled,
		}},
		{Component: ComponentDRADriverOCP, SelectorPaths: []string{
			PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled,
		}},
	}
}

// SelectorPaths returns the descriptor's selector paths for one component
// (nil when the component has no descriptor entry).
func SelectorPaths(component string) []string {
	for _, entry := range Descriptor() {
		if entry.Component == component {
			return entry.SelectorPaths
		}
	}
	return nil
}

// ValidateAdvertiser rejects any advertiser value outside the reserved
// vocabulary: empty (no declaration) and AdvertiserExternal.
func ValidateAdvertiser(advertiser string) error {
	switch advertiser {
	case "", AdvertiserExternal:
		return nil
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unknown advertiser %q; the only supported value is %q", advertiser, AdvertiserExternal))
	}
}

// Observation is the effective advertiser tuple read from a recipe's
// hydrated component values. Boolean fields are pointers so an
// absent/undeterminable reading is distinguishable from an explicit false;
// a component that is absent or disabled contributes a nil reading.
type Observation struct {
	// Advertiser is the artifact's declared advertiser ("" when none).
	Advertiser string
	// DevicePluginEnabled is the effective devicePlugin.enabled of the
	// enabled GPU operator component, when determinable.
	DevicePluginEnabled *bool
	// DRAGPUsEnabled is the effective resources.gpus.enabled of the
	// enabled DRA driver component, when determinable. nil means the DRA
	// component is absent or disabled — no whole-GPU DRA is possible then.
	DRAGPUsEnabled *bool
	// DRAGPUsEnabledOverride is the effective gpuResourcesEnabledOverride
	// (the upstream chart's install-guard waiver) of the enabled DRA
	// driver component, when determinable. An absent key on an enabled
	// component reads false (the chart default); nil means the DRA
	// component is absent or disabled.
	DRAGPUsEnabledOverride *bool
	// GPUOperatorComponent names the operator component whose values
	// produced DevicePluginEnabled. Diagnostics only — it feeds error
	// guidance (OCP recipes carry gpu-operator-ocp) and never a verdict.
	// Empty defaults to ComponentGPUOperator.
	GPUOperatorComponent string
}

// CheckCoherence is the single shared evaluator of the #1327 tuple-coherence
// rules for every advertiser shape, failing closed (ErrCodeInvalidRequest).
// The hydrating artifact gate (pkg/recipe) and the validation-time resolver
// (pkg/validator/v1 ResolveGPUAllocationPolicy) both delegate their tuple
// verdicts here, so over these #1327 tuple rows an artifact the gate emits is
// exactly an artifact validation accepts — gate/resolver symmetry (ADR-015).
// The #1685 dual-operator rejection is resolution-time-only and deliberately
// not part of this evaluator: it is a resolver-side check on the recipe's
// component set, not a tuple reading, and bundle-time rejection is a separate
// follow-up.
//
// Under a declared external advertiser (ADR-015 GKE amendment), the external
// plugin is THE advertiser in the exactly-one invariant:
//
//   - devicePlugin.enabled=true is dual advertisement (the external
//     advertiser and the operator's device plugin would both advertise
//     nvidia.com/gpu);
//   - DRA resources.gpus.enabled=true is dual advertisement through DRA
//     ResourceClaims;
//   - a remaining gpuResourcesEnabledOverride=true is an inert waiver that
//     disarms the upstream chart's install-guard tripwire.
//
// Under an empty advertiser the rules mirror the resolver's #1327 contract
// verbatim (issue #1327 rows):
//
//   - gpus.enabled=true without the override waiver: the upstream chart
//     install guard rejects the combination;
//   - gpus.enabled=true with devicePlugin.enabled=true: dual advertisement;
//   - neither DRA nor the device plugin advertising: no whole-GPU
//     advertiser at all;
//   - gpus.enabled=false with the waiver set: inert waiver.
//
// Advertiser SELECTION (which policy value a valid tuple resolves to) is not
// coherence and stays in the resolver.
func CheckCoherence(o Observation) error {
	if err := ValidateAdvertiser(o.Advertiser); err != nil {
		return err
	}
	devicePluginEnabled := o.DevicePluginEnabled != nil && *o.DevicePluginEnabled
	gpusEnabled := o.DRAGPUsEnabled != nil && *o.DRAGPUsEnabled
	overrideWaiver := o.DRAGPUsEnabledOverride != nil && *o.DRAGPUsEnabledOverride
	operatorName := o.GPUOperatorComponent
	if operatorName == "" {
		operatorName = ComponentGPUOperator
	}

	if o.Advertiser == AdvertiserExternal {
		if devicePluginEnabled {
			return errors.New(errors.ErrCodeInvalidRequest,
				"advertiser \"external\" conflicts with devicePlugin.enabled=true: the external advertiser and the GPU operator's device plugin would both advertise nvidia.com/gpu")
		}
		if gpusEnabled {
			return errors.New(errors.ErrCodeInvalidRequest,
				"advertiser \"external\" conflicts with DRA resources.gpus.enabled=true: the external advertiser and DRA ResourceClaims would both advertise whole GPUs")
		}
		if overrideWaiver {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"invalid GPU allocation configuration: %s %s=true with %s=false is an inert waiver that disarms the upstream chart's install-guard tripwire; remove it (issue #1327)",
				ComponentDRADriver, PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled))
		}
		return nil
	}

	// Empty advertiser: the resolver's non-external #1327 verdicts.
	if gpusEnabled {
		if !overrideWaiver {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"invalid GPU allocation configuration: %s %s=true requires %s=true — the upstream chart install guard rejects this combination; set both in the recipe overlay (issue #1327)",
				ComponentDRADriver, PathDRAGPUsEnabled, PathDRAGPUsEnabledOverride))
		}
		if devicePluginEnabled {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"invalid GPU allocation configuration: dual advertisement — %s %s=true and %s %s=true both advertise whole GPUs, risking GPU over-admission; exactly one advertiser is required. For the production default set %s=false (with %s=false); for the experimental DRA opt-in flip all three values together: %s=true, %s=true, and %s %s=false (issue #1327)",
				ComponentDRADriver, PathDRAGPUsEnabled,
				operatorName, PathDevicePluginEnabled,
				PathDRAGPUsEnabled, PathDRAGPUsEnabledOverride,
				PathDRAGPUsEnabled, PathDRAGPUsEnabledOverride,
				operatorName, PathDevicePluginEnabled))
		}
		return nil
	}
	if !devicePluginEnabled {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"invalid GPU allocation configuration: no whole-GPU advertiser — %s %s is false/absent, and the device plugin is unavailable (%s/%s absent or disabled in the recipe, or %s=false); enable exactly one mechanism in the recipe overlay (issue #1327)",
			ComponentDRADriver, PathDRAGPUsEnabled,
			ComponentGPUOperator, ComponentGPUOperatorOCP,
			PathDevicePluginEnabled))
	}
	if overrideWaiver {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"invalid GPU allocation configuration: %s %s=true with %s=false is an inert waiver that disarms the upstream chart's install-guard tripwire protecting the device-plugin default; set %s=false for the production default, or — for the experimental DRA opt-in — flip all three values together: %s=true, %s=true, and %s %s=false (issue #1327)",
			ComponentDRADriver, PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled,
			PathDRAGPUsEnabledOverride,
			PathDRAGPUsEnabled, PathDRAGPUsEnabledOverride,
			operatorName, PathDevicePluginEnabled))
	}
	return nil
}

// Identity returns the deterministic identity of the FULL canonical
// descriptor: IdentityFor(Descriptor()). Kept for callers that need the
// global descriptor identity (e.g. diagnostics); evidence production and
// verification use the recipe-scoped IdentityFor instead (ADR-015
// descriptor-currentness).
func Identity() string {
	return IdentityFor(Descriptor())
}

// IdentityFor returns the deterministic identity of the supplied descriptor
// entries: a sha256 over the canonical sorted "component:path" enumeration.
// Evidence predicates record the identity of the entries contributing to
// the attested recipe's effective closure — the closure is recomputed
// rather than persisted, so a descriptor expansion changes no recipe
// digest, and only this identity distinguishes evidence produced before an
// expansion from evidence produced after it. Scoping the identity to the
// contributing entries keeps an expansion that does not touch a recipe's
// closure (e.g. a new GKE-only entry) from spuriously invalidating that
// recipe's evidence (ADR-015 descriptor-currentness). The empty set — the
// closure did not trigger, or no descriptor component is enabled — hashes
// the empty canonical string and is stable.
func IdentityFor(entries []Entry) string {
	sum := sha256.Sum256([]byte(canonicalStringFor(entries)))
	return hex.EncodeToString(sum[:])
}

// canonicalStringFor renders entries as the sorted "component:path\n"
// enumeration. Paths are first merged per component, then components and
// each component's merged path set are sorted and deduplicated, so the
// identity depends only on the SET of component:path pairs — independent of
// input order and of multiplicity, even when a caller passes the same
// component (or the same path) across multiple entries (sort.Slice alone is
// unstable and keyed on Component only, which would make
// duplicate-component identities order-dependent). In-repo callers pass
// unique components and paths, so for Descriptor() (already in canonical
// order) this reproduces the historical canonical string byte-for-byte,
// keeping the pinned full-descriptor identity stable.
func canonicalStringFor(entries []Entry) string {
	merged := make(map[string][]string, len(entries))
	components := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, seen := merged[entry.Component]; !seen {
			components = append(components, entry.Component)
		}
		merged[entry.Component] = append(merged[entry.Component], entry.SelectorPaths...)
	}
	sort.Strings(components)
	var b strings.Builder
	for _, component := range components {
		paths := append([]string(nil), merged[component]...)
		sort.Strings(paths)
		paths = slices.Compact(paths)
		for _, path := range paths {
			b.WriteString(component)
			b.WriteString(":")
			b.WriteString(path)
			b.WriteString("\n")
		}
	}
	return b.String()
}
