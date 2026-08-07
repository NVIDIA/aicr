// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr

import (
	"context"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// gpuOperatorComponentName is the ComponentRef.Name used by the recipe
// registry for the NVIDIA GPU Operator. Kept in sync with
// recipes/registry.yaml — the auto-detect override only lands on the
// component that owns the driver.enabled Helm value.
const gpuOperatorComponentName = "gpu-operator"

// gpuOperatorManagedOverrideSet is the documented bundle-time override
// tuple that flips a preinstalled-driver recipe to GPU-Operator-managed
// mode: the operator installs the driver and toolkit, the runtimeClass
// follows the toolkit's handler, and the DRA kubelet plugin reads the
// operator install dir. Kept as one string so the resolution-time
// warning names the identical flag set the bundle-time
// CheckDriverOwnershipCoherence validation accepts.
//
// A private sibling of this constant (and of driverAbsentRemedy below)
// lives in pkg/bundler/validations: that package cannot import
// pkg/client/v1 (dependency cycle through pkg/bundler), and a shared
// package for two small helpers was rejected — the duplication is the
// agreed disposition. Keep both copies in sync.
const gpuOperatorManagedOverrideSet = "--set gpuoperator:driver.enabled=true " +
	"--set gpuoperator:toolkit.enabled=true " +
	"--set gpuoperator:operator.runtimeClass=nvidia " +
	"--set dradriver:nvidiaDriverRoot=/run/nvidia/driver"

// gkeGPUOperatorManagedOverrideSet extends the override tuple for GKE
// remedies. GKE preinstalled-driver profiles (Google driver installer,
// documented for both COS and Ubuntu node images) pin
// hostPaths.driverInstallDir to /home/kubernetes/bin/nvidia, so a flip
// to operator-managed mode must move BOTH driver roots to the operator
// container root — otherwise the DRA lockstep rule blocks the exact
// bundle this remedy recommends.
const gkeGPUOperatorManagedOverrideSet = gpuOperatorManagedOverrideSet +
	" --set gpuoperator:hostPaths.driverInstallDir=/run/nvidia/driver"

// driverAbsentRemedy returns the provider-appropriate remedy wording for
// the "preinstalled-driver recipe on a driverless cluster" mismatch,
// derived from the resolved criteria service and OS. AKS pools created
// with `--gpu-driver none` are fixed by recreating them without the flag
// (or the override set); GKE+COS gets the COS-only wording (the GPU
// Operator cannot install the driver on COS), GKE+Ubuntu gets the
// operator-managed remedy (the pinned operator supports GKE driver
// management only on Ubuntu node images), and any other GKE OS —
// unknown, any, or one GKE does not offer — keeps the combined wording;
// anything else gets the generic reprovision wording plus the override
// set.
func driverAbsentRemedy(service recipe.CriteriaServiceType, os recipe.CriteriaOSType, profiled bool) string {
	switch service { //nolint:exhaustive // only AKS and GKE have provider-specific wording; every other service takes the generic default
	case recipe.CriteriaServiceAKS:
		if !profiled {
			// Legacy pre-profile artifact: the ownership lock does not
			// apply (no metadata.selectedProfile), so the four-flag
			// bundle-time tuple remains this artifact's supported flip.
			return "Either recreate the GPU node pools without --gpu-driver " +
				"none (AKS installs the NVIDIA driver by default), or bundle " +
				"in GPU-Operator-managed mode: " + gpuOperatorManagedOverrideSet + "."
		}
		return "Either repair the AKS-managed driver install (recreate the " +
			"GPU node pools without --gpu-driver none; AKS installs the " +
			"NVIDIA driver by default) and recapture the snapshot, or switch " +
			"to operator-managed mode end to end: recreate the pools WITH " +
			"--gpu-driver none, recapture the snapshot, and regenerate with " +
			"--profile gpuStack=operator-managed. The operator-managed value's " +
			"constraint " +
			"requires the pools to read gpu-driver=None, so regenerating " +
			"against the current snapshot alone fails closed; and the " +
			"gpuStack profile owns the driver-ownership paths, so flipping " +
			"them via per-path --set overrides is rejected."
	case recipe.CriteriaServiceGKE:
		switch os { //nolint:exhaustive // COS and Ubuntu are the only GKE node images with specific wording; everything else (unknown, any, or an OS GKE does not offer) gets both supported GKE paths
		case recipe.CriteriaOSCOS:
			return "On GKE COS node images the GPU Operator cannot install the " +
				"driver. With the default gke-default gpuStack profile " +
				"(opt-out label absent) provision the GPU node pools with " +
				"the GKE-managed driver install (node pool " +
				"gpu-driver-version=default). With --profile " +
				"gpuStack=driver-installer (pools labeled " +
				"gke-no-default-nvidia-gpu-device-plugin=true) the label " +
				"forfeits the managed install — deploy Google's standalone " +
				"nvidia-driver-installer DaemonSet and create the pools " +
				"with gpu-driver-version=disabled instead; see " +
				"docs/integrator/gke-gpu-setup.md."
		case recipe.CriteriaOSUbuntu:
			// The pinned GPU Operator (v26.3.3) supports driver management
			// on GKE only on Ubuntu node images with containerd.
			return "On GKE Ubuntu node images the GPU Operator can manage " +
				"the driver: bundle in GPU-Operator-managed mode: " +
				gkeGPUOperatorManagedOverrideSet + "."
		default:
			// Unknown/any OS, or an OS GKE does not offer as a node image —
			// present both supported GKE paths without asserting the
			// recipe's OS supports either.
			return "On GKE COS node images the GPU Operator cannot install the " +
				"driver: provision the GPU node pools with the GKE-managed " +
				"driver install (node pool gpu-driver-version) instead. On " +
				"GKE Ubuntu node images the GPU Operator can manage the " +
				"driver, so those may bundle in GPU-Operator-managed mode: " +
				gkeGPUOperatorManagedOverrideSet + "."
		}
	default:
		return "Either reprovision the GPU nodes with a platform-installed " +
			"NVIDIA driver, or bundle in GPU-Operator-managed mode: " +
			gpuOperatorManagedOverrideSet + "."
	}
}

// gpuHardwareSubtypeName is the subtype name emitted by
// pkg/collector/gpu when writing the driver-loaded reading (see the
// subtypeHardware constant there). Re-declared locally so this file
// does not pull the collector package.
const gpuHardwareSubtypeName = "hardware"

// k8sPolicySubtypeName is the subtype under the K8s measurement that
// pkg/collector/k8s writes ClusterPolicy custom resource spec data
// into. Non-empty data indicates that at least one ClusterPolicy CRD
// is installed — in practice that means the GPU Operator (or a
// compatible policy CRD) is already running on the cluster.
const k8sPolicySubtypeName = "policy"

// gpuDriverState reports what the snapshot tells us about the NVIDIA
// kernel driver on the sampled GPU node.
//
// Cardinality note: the GPU collector runs on a single node the
// snapshotter Job schedules onto (via nvidia.com/gpu.present=true), so
// the state reflects that one sample. On homogeneous clusters — the
// common case — it is representative of every GPU pool. Mixed-pool
// clusters are out of scope for now and tracked separately (see #464).
// applyGPUDriverAutoOverride surfaces a slog.Warn when the topology
// signal indicates a non-uniform GPU pool so the fail-direction (some
// pools may come up driverless) is at least observable.
type gpuDriverState int

const (
	// gpuDriverUnknown means we lack a signal — no snapshot, no GPU
	// measurement in the snapshot, or the driver-loaded reading is
	// absent. The injection path treats this as "don't touch anything"
	// so callers who never provide a snapshot see no behavior change.
	gpuDriverUnknown gpuDriverState = iota

	// gpuDriverNotObserved means the GPU measurement is present but the
	// hardware subtype reports no GPU on the sampled node
	// (gpu-present=false or gpu-count=0). No override — the recipe's
	// static provider defaults stand.
	gpuDriverNotObserved

	// gpuDriverPreinstalled means the sampled GPU node has the nvidia
	// kernel module loaded. Auto-inject driver.enabled=false to prevent
	// the GPU Operator from installing a second driver on top of the
	// one the platform (AKS, GKE-COS, OKE bare-metal) has already
	// provisioned — but only when the resolved overlay already carries
	// the full coordinated preinstalled-driver profile (see
	// hasPreinstalledDriverProfile). Bare EKS gets a warning instead
	// of a half-configured Operator.
	gpuDriverPreinstalled

	// gpuDriverAbsent means the sampled GPU node does not have the
	// nvidia kernel module loaded. Never an override — and when the
	// resolved overlay declares the preinstalled-driver profile (AKS,
	// GKE-COS, OKE carry driver.enabled=false statically) the recipe
	// assumes a platform-provided driver the cluster does not have, so
	// the deployed bundle would leave GPU nodes driverless. Resolution
	// warns and records the state in Metadata.GPUDriverState; the
	// bundle-time CheckDriverOwnershipCoherence validation fails the
	// bundle. Legacy pre-profile artifacts unblock with the
	// GPU-Operator-managed --set overrides; ADR-015-profiled recipes
	// (AKS gpuStack) cannot — ownership paths are locked — and remedy
	// out-of-band (fix/recreate pools, recapture, regenerate; see
	// driverAbsentRemedy). Overlays whose operator installs the driver
	// (base, inherited by EKS) proceed unchanged.
	gpuDriverAbsent
)

// String returns a stable, log-friendly name for the state — used by
// the slog.Debug/Warn output on the no-op and gated paths so operators
// debugging "why didn't the override land" can trace the resolved
// classification without decoding an int.
func (s gpuDriverState) String() string {
	switch s {
	case gpuDriverUnknown:
		return "unknown"
	case gpuDriverNotObserved:
		return "not-observed"
	case gpuDriverPreinstalled:
		return "preinstalled"
	case gpuDriverAbsent:
		return "absent"
	}
	return "invalid"
}

// computeGPUDriverState reduces the snapshot to the single per-cluster
// signal used by the auto-detect override: is the NVIDIA driver already
// loaded on the sampled GPU node? The reducer is deliberately strict —
// a missing driver-loaded key returns Unknown, not Absent, so a stale
// snapshot produced by an older CLI cannot flip a hardened overlay.
func computeGPUDriverState(snap *snapshotter.Snapshot) gpuDriverState {
	if snap == nil || len(snap.Measurements) == 0 {
		return gpuDriverUnknown
	}

	var gpu *measurement.Measurement
	for _, m := range snap.Measurements {
		if m != nil && m.Type == measurement.TypeGPU {
			gpu = m
			break
		}
	}
	if gpu == nil {
		return gpuDriverUnknown
	}

	hw := gpu.GetSubtype(gpuHardwareSubtypeName)
	if hw == nil {
		return gpuDriverUnknown
	}

	// A hardware subtype that explicitly reports no GPU on the node
	// is a distinct signal from "we couldn't tell" — the sampled node
	// simply is not a GPU node. Bucket separately so it cannot be
	// confused with a driver-loaded=false GPU node.
	if present := hw.Get(measurement.KeyGPUPresent); present != nil {
		if b, ok := present.Any().(bool); ok && !b {
			return gpuDriverNotObserved
		}
	}
	if count := hw.Get(measurement.KeyGPUCount); count != nil {
		if isZeroCount(count.Any()) {
			return gpuDriverNotObserved
		}
	}

	loaded := hw.Get(measurement.KeyGPUDriverLoaded)
	if loaded == nil {
		return gpuDriverUnknown
	}
	b, ok := loaded.Any().(bool)
	if !ok {
		// A non-bool driver-loaded reading is a corrupt or older-schema
		// signal — fail closed to Unknown so the injection never lands
		// on ambiguous input (see CLAUDE.md's "fail-closed" guidance).
		return gpuDriverUnknown
	}
	if b {
		return gpuDriverPreinstalled
	}
	return gpuDriverAbsent
}

// isZeroCount treats int, int64, and JSON-decoded float64 uniformly.
// A JSON or sigs.k8s.io/yaml round-trip delivers integer readings as
// float64; the yaml.v3 path used by the local snapshot loader delivers
// them as int64. Both must produce the same NotObserved classification
// so a snapshot posted to /v1/recipe cannot slip past the zero-count
// gate. A non-integral float64 is rejected as an unknown format
// (returns false → non-zero), matching the fail-closed pattern
// documented in CLAUDE.md's anti-pattern list.
func isZeroCount(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 0
	case int64:
		return n == 0
	case float64:
		if float64(int64(n)) != n {
			return false
		}
		return int64(n) == 0
	}
	return false
}

// hasGPUOperatorClusterPolicy reports whether the snapshot's K8s
// measurement recorded any ClusterPolicy resources. A non-empty policy
// subtype indicates at least one ClusterPolicy CRD is installed —
// which in practice means the GPU Operator (or a compatible policy
// CRD) is already running on the cluster.
//
// Used to warn on the self-referential-signal case: driver-loaded=true
// on a snapshot taken AFTER an AICR deploy could be reporting the
// operator-managed driver, and injecting enabled=false + redeploying
// would tear that driver DaemonSet down. Operators are told to
// snapshot BEFORE deploying; the warning is the observability that
// helps them recognize the mistake.
func hasGPUOperatorClusterPolicy(snap *snapshotter.Snapshot) bool {
	if snap == nil {
		return false
	}
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeK8s {
			continue
		}
		sub := m.GetSubtype(k8sPolicySubtypeName)
		if sub == nil {
			continue
		}
		if len(sub.Data) > 0 {
			return true
		}
	}
	return false
}

// hasHeterogeneousGPUPool reports whether the snapshot's topology
// measurement records multiple distinct values for any GPU-scoped node
// label (nvidia.com/gpu.*) or the standard instance-type label. The
// topology encoder disambiguates such keys by appending ".<value>", so
// any key of that shape is our proxy for "the sampled GPU node is not
// representative of every GPU pool" and warrants a warning.
//
// This is a hint, not a gate: mixed-pool support requires per-node
// collector fan-out (#464), and until that lands a single-node sample
// remains the ground truth. Callers are told which direction they can
// fail toward — some non-preinstalled pools may come up driverless
// after the injection.
func hasHeterogeneousGPUPool(snap *snapshotter.Snapshot) bool {
	if snap == nil {
		return false
	}
	for _, m := range snap.Measurements {
		if m == nil || m.Type != measurement.TypeNodeTopology {
			continue
		}
		labels := m.GetSubtype("label")
		if labels == nil {
			continue
		}
		for k := range labels.Data {
			if !isDisambiguatedLabelKey(k) {
				continue
			}
			switch {
			case strings.HasPrefix(k, "nvidia.com/gpu."):
				return true
			case strings.HasPrefix(k, "node.kubernetes.io/instance-type."):
				return true
			}
		}
	}
	return false
}

// isDisambiguatedLabelKey reports whether k is the disambiguated form
// the topology encoder produces (Key + "." + Value). The single-value
// form of an nvidia.com/gpu.<name> label already carries dots inside
// the fixed prefix (nvidia.com and gpu.<name>), so the check strips
// the base label prefix first and asks whether the *tail* — the value
// the encoder appended — is non-empty. For instance-type, whose
// single-value form ends at the "instance-type" segment, presence of
// any non-empty tail after the trailing dot is enough.
func isDisambiguatedLabelKey(k string) bool {
	if suffix, ok := strings.CutPrefix(k, "nvidia.com/gpu."); ok {
		// The single-value label key ends here (e.g. "product",
		// "count", "family"). The encoder appends "." + <value> only
		// on divergence, so any dot in the tail is our disambiguation
		// signal. A trailing dot with no value is not real divergence.
		dot := strings.IndexByte(suffix, '.')
		return dot >= 0 && dot < len(suffix)-1
	}
	if suffix, ok := strings.CutPrefix(k, "node.kubernetes.io/instance-type."); ok {
		return len(suffix) > 0
	}
	return false
}

// hasPreinstalledDriverProfile reports whether the resolved recipe's
// gpu-operator component values (base + valuesFile + recorded
// Overrides, via GetValuesForComponentWithContext) already declare
// driver.enabled=false. That is the marker for a preinstalled-driver
// overlay — one that also carries the coordinated toolkit / gdrcopy /
// hostPaths.driverInstallDir settings the AKS azure-managed
// profile value documents as required together.
//
// Bare EKS overlays lack this marker; auto-detect skips them so
// callers get a warning instead of a half-configured Operator (driver
// off, toolkit + gdrcopy still on with no operator-managed driver
// root). Fixing that case requires a full preinstalled-profile
// overlay (tracked separately) — this gate keeps the current PR from
// regressing it into a strictly worse state.
func hasPreinstalledDriverProfile(ctx context.Context, r *recipe.RecipeResult) bool {
	if r == nil {
		return false
	}
	values, err := r.GetValuesForComponentWithContext(ctx, gpuOperatorComponentName)
	if err != nil {
		// A read error (e.g. context deadline) is NOT "no preinstalled
		// profile" — returning false silently here would suppress the
		// absent-driver mismatch warning in the caller's gpuDriverAbsent
		// branch. It never produces a wrong artifact (the only-false policy
		// makes the absent case a no-op), but surface the error so the lost
		// observability is visible rather than silent.
		slog.Warn("gpu-operator preinstalled-driver profile check: failed to read "+
			"component values; treating as no preinstalled-driver profile "+
			"(an absent-driver mismatch warning may be suppressed for this resolve)",
			"component", gpuOperatorComponentName, "error", err)
		return false
	}
	if len(values) == 0 {
		return false
	}
	driver, ok := values["driver"].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := driver["enabled"].(bool)
	if !ok {
		return false
	}
	return !enabled
}

// applyGPUDriverAutoOverride injects driver.enabled=false into the
// gpu-operator ComponentRef's Overrides map when the snapshot reports
// a pre-installed driver on the sampled GPU node AND the resolved
// overlay already declares the coordinated preinstalled-driver profile.
//
// When a selected configuration profile owns driver.enabled, the function
// never mutates: an agreeing profile value makes the injection redundant and
// a disagreeing one must win, so ownership short-circuits before any write.
//
// Policy is only-false: the function never forces driver.enabled=true.
// The injection is a no-op on rendered Helm output for overlays that
// already carry the profile (the resolved recipe records the override
// explicitly for auditability, so the emitted YAML is not byte-for-byte
// identical); bare EKS gets a slog.Warn instead so the operator
// knows to add a proper preinstalled overlay before the deploy will do
// the right thing.
//
// The inverse mismatch is enforced at bundle generation: the observed
// driver state is recorded in the result's Metadata.GPUDriverState, and
// when the sampled GPU node has NO driver loaded but the resolved
// overlay declares the preinstalled-driver profile, this function emits
// a slog.Warn here and the bundle-time CheckDriverOwnershipCoherence
// validation (severity error, recipes/registry.yaml) fails the bundle.
// On ADR-015-profiled recipes (the AKS gpuStack family) the reachable
// shape is Install-mode pools whose sampled node has no loaded driver
// (failed AKS install, mid-reimage) — a None-pool snapshot never gets
// here, failing the profile constraint at resolution — and the remedy
// is out-of-band (repair or recreate pools, recapture, regenerate; the
// ownership paths are profile-owned so per-path --set flips are
// rejected). On legacy pre-profile artifacts the bundle unblocks with
// the GPU-Operator-managed --set overrides. The check cannot hard-fail
// at resolution: `aicr recipe` has no --set, so erroring here would
// leave supported legacy GPU-Operator-managed clusters with no way to
// reach the bundle-time overrides intended for them.
//
// Merge precedence for the final Helm values is
// base values.yaml → ValuesFile → Overrides (see pkg/recipe/adapter.go).
// CLI --set flags still supersede everything.
func applyGPUDriverAutoOverride(ctx context.Context, r *recipe.RecipeResult, snap *snapshotter.Snapshot) {
	state := computeGPUDriverState(snap)
	if r == nil {
		slog.Debug("gpu-operator driver auto-detect: nil recipe result",
			"state", state.String())
		return
	}
	// Record the observation on the result (and thus in the emitted
	// recipe YAML) so bundle-time validations can enforce ownership
	// coherence once --set overrides are known. Reset first so the field
	// always reflects THIS snapshot: unknown/not-observed leave it empty
	// (an older-schema or GPU-less snapshot must not trip the bundle gate),
	// and re-resolving a result previously marked "absent" against a later
	// unusable snapshot must not retain the stale bundle-blocking state.
	r.Metadata.GPUDriverState = ""
	switch state {
	case gpuDriverPreinstalled:
		r.Metadata.GPUDriverState = recipe.GPUDriverStatePreinstalled
	case gpuDriverAbsent:
		r.Metadata.GPUDriverState = recipe.GPUDriverStateAbsent
	case gpuDriverUnknown, gpuDriverNotObserved:
		// No usable driver signal — leave the field empty so the
		// bundle-time gate stays disarmed.
	}
	if state == gpuDriverAbsent && hasPreinstalledDriverProfile(ctx, r) {
		var service recipe.CriteriaServiceType
		var osCriteria recipe.CriteriaOSType
		if r.Criteria != nil {
			service = r.Criteria.Service
			osCriteria = r.Criteria.OS
		}
		slog.Warn("gpu-operator driver mismatch: the resolved recipe assumes a "+
			"platform-preinstalled NVIDIA driver and container toolkit "+
			"(gpu-operator driver.enabled=false — e.g. the AKS azure-managed "+
			"default), but the sampled GPU node reports no NVIDIA kernel "+
			"driver loaded. Deploying this configuration would leave GPU "+
			"nodes driverless, so `aicr bundle` will fail for this recipe "+
			"(CheckDriverOwnershipCoherence). "+driverAbsentRemedy(service, osCriteria, r.Metadata.SelectedProfile != nil),
			"component", gpuOperatorComponentName,
			"state", state.String())
		return
	}
	if state != gpuDriverPreinstalled {
		slog.Debug("gpu-operator driver auto-detect: no-op",
			"state", state.String(),
			"component", gpuOperatorComponentName,
			"reason", "driver state is not preinstalled")
		return
	}
	if r.OwnsProfilePath(gpuOperatorComponentName, "driver.enabled") {
		driverEnabled, known := profileOwnedDriverEnabled(r)
		if known && !driverEnabled {
			slog.Debug("gpu-operator driver auto-detect: selected profile owns driver.enabled; skipping mutation",
				"state", state.String(),
				"component", gpuOperatorComponentName)
		} else {
			slog.Warn("gpu-operator driver auto-detect: selected profile owns driver.enabled; "+
				"skipping mutation even though a pre-installed driver was observed. "+
				"If the selected profile keeps driver.enabled=true, applying it may install "+
				"a second driver. Select a preinstalled-driver profile or regenerate the recipe.",
				"state", state.String(),
				"component", gpuOperatorComponentName,
				"profile", r.Metadata.SelectedProfile.Name+"="+r.Metadata.SelectedProfile.Value)
		}
		return
	}
	if !hasPreinstalledDriverProfile(ctx, r) {
		slog.Warn("gpu-operator driver auto-detect: pre-installed driver observed on sampled node, "+
			"but the resolved overlay is not a preinstalled-driver profile "+
			"(gpu-operator values do not declare driver.enabled=false). Skipping "+
			"injection to avoid a half-configured Operator (driver off, toolkit "+
			"and gdrcopy still enabled with no operator-managed driver root). "+
			"Use a preinstalled-profile overlay (AKS, GKE-COS, OKE) or an overlay "+
			"that declares the full coordinated profile.",
			"component", gpuOperatorComponentName,
			"state", state.String())
		return
	}
	if hasGPUOperatorClusterPolicy(snap) {
		slog.Warn("gpu-operator driver auto-detect: driver-loaded=true AND a ClusterPolicy is already "+
			"present in the snapshot. AICR may have installed this driver on a prior "+
			"deploy; re-resolving from a post-deploy snapshot and re-applying can tear "+
			"the operator-managed driver DaemonSet down and leave new GPU nodes "+
			"driverless. Recommendation: capture the snapshot BEFORE deploying the "+
			"GPU Operator.",
			"component", gpuOperatorComponentName)
	}
	if hasHeterogeneousGPUPool(snap) {
		slog.Warn("gpu-operator driver auto-detect: topology reports non-uniform GPU labels "+
			"across nodes. The GPU collector samples a single node, so the injected "+
			"driver.enabled=false will apply cluster-wide; non-preinstalled GPU pools "+
			"may come up driverless. Mixed-pool support is tracked in #464.",
			"component", gpuOperatorComponentName)
	}
	for i := range r.ComponentRefs {
		if r.ComponentRefs[i].Name != gpuOperatorComponentName {
			continue
		}
		ref := &r.ComponentRefs[i]
		// Deep-copy Overrides (and any driver submap) before mutating so
		// this write cannot alias into the sync.Once-cached MetadataStore
		// or leak driver.enabled=false into a shared registry entry —
		// see CLAUDE.md's "deep-copy helper that recurses into maps"
		// anti-pattern. DeepCopyAnyMap(nil) returns a fresh empty map.
		ref.Overrides = serializer.DeepCopyAnyMap(ref.Overrides)
		driverAny, _ := ref.Overrides["driver"].(map[string]any)
		if driverAny == nil {
			driverAny = map[string]any{}
			ref.Overrides["driver"] = driverAny
		}
		driverAny["enabled"] = false
		slog.Info("auto-disabled gpu-operator driver install: pre-installed driver detected in snapshot",
			"component", gpuOperatorComponentName,
			"reason", "driver-loaded=true")
		return
	}
	slog.Debug("gpu-operator driver auto-detect: no gpu-operator component ref in resolved recipe",
		"state", state.String())
}

func profileOwnedDriverEnabled(r *recipe.RecipeResult) (bool, bool) {
	ref := r.GetComponentRef(gpuOperatorComponentName)
	if ref == nil {
		return false, false
	}
	driver, ok := ref.Overrides["driver"].(map[string]any)
	if !ok {
		return false, false
	}
	enabled, ok := driver["enabled"].(bool)
	return enabled, ok
}
