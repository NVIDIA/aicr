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

package recipe

import (
	"context"
	stderrors "errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func gkeCriteria() *Criteria {
	return &Criteria{
		Service:     CriteriaServiceGKE,
		Accelerator: CriteriaAcceleratorH100,
		OS:          CriteriaOSCOS,
		Intent:      CriteriaIntentTraining,
	}
}

// TestGKEGpuStackProfileResolution pins the GKE family conversion (issue
// #1761 rollout PR 3): the gke-cos overlay declares gpuStack with default
// gke-default (the only value a default-provisioned GKE cluster satisfies
// — the opt-out label forfeits GKE's managed driver install, so
// bundle-installer carries the bundle's own gcp-driver-installer) and
// alternative bundle-installer,
// leaves inherit it, the selection is recorded with the advertiser, and
// the #1755 node-set constraint is carried per value with the correct
// predicate direction.
func TestGKEGpuStackProfileResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		selection      string
		wantValue      string
		wantAdvertiser string
		wantConstraint string
	}{
		{
			name:           "default selection is gke-default with the external advertiser and the negated predicate",
			selection:      "",
			wantValue:      "gke-default",
			wantAdvertiser: allocpolicy.AdvertiserExternal,
			wantConstraint: "!gke-no-default-nvidia-gpu-device-plugin",
		},
		{
			name:           "explicit bundle-installer records no advertiser and the positive label predicate",
			selection:      "gpuStack=bundle-installer",
			wantValue:      "bundle-installer",
			wantAdvertiser: "",
			wantConstraint: "gke-no-default-nvidia-gpu-device-plugin=true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteriaWithProfile(
				t.Context(), gkeCriteria(), tt.selection)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
			}
			selected := result.Metadata.SelectedProfile
			if selected == nil {
				t.Fatal("metadata.selectedProfile is nil")
			}
			if selected.Name != "gpuStack" || selected.Value != tt.wantValue {
				t.Errorf("selectedProfile = %s=%s, want gpuStack=%s", selected.Name, selected.Value, tt.wantValue)
			}
			if selected.Advertiser != tt.wantAdvertiser {
				t.Errorf("advertiser = %q, want %q", selected.Advertiser, tt.wantAdvertiser)
			}
			if result.APIVersion != RecipeProfileAPIVersion {
				t.Errorf("apiVersion = %q, want %q", result.APIVersion, RecipeProfileAPIVersion)
			}
			owned := selected.OwnedPaths["gpu-operator"]
			if len(owned) != 2 || owned[0] != "devicePlugin.enabled" || owned[1] != "enabled" {
				t.Errorf("ownedPaths[gpu-operator] = %v, want [devicePlugin.enabled enabled]", owned)
			}
			// The installer's gate and presence are declaration-wide owned
			// paths (issue #1716): locked for EVERY selection, including the
			// two values where the component renders nothing.
			installerOwned := selected.OwnedPaths["gcp-driver-installer"]
			if len(installerOwned) != 2 || installerOwned[0] != "enabled" || installerOwned[1] != "installer.enabled" {
				t.Errorf("ownedPaths[gcp-driver-installer] = %v, want [enabled installer.enabled]", installerOwned)
			}
			var found bool
			for _, c := range result.Constraints {
				if c.Name == "NodeTopology.gpu-nodes.label" {
					found = true
					if c.Value != tt.wantConstraint {
						t.Errorf("label constraint value = %q, want %q", c.Value, tt.wantConstraint)
					}
					if c.Remediation == "" {
						t.Error("label constraint carries no remediation")
					}
				}
			}
			if !found {
				t.Error("NodeTopology.gpu-nodes.label constraint not carried in the artifact")
			}
		})
	}
}

// TestGKEGpuStackHappyPathThroughHydrationGate is the ADR-required
// external-advertiser happy path: invalid-tuple tests alone would pass an
// implementation that rejects every advertiser "external"; this proves the
// gke-default value clears the hydrating coherence gate (devicePlugin off,
// DRA off) and the closure joins the effective lock set.
func TestGKEGpuStackHappyPathThroughHydrationGate(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(
		t.Context(), gkeCriteria(), "gpuStack=gke-default")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
	}
	if err := result.PrepareAndValidateWithContext(context.Background()); err != nil {
		t.Fatalf("PrepareAndValidateWithContext() rejected the gke-default happy path: %v", err)
	}

	lock := result.EffectiveLockSet()
	draPaths := lock[allocpolicy.ComponentDRADriver]
	if len(draPaths) == 0 {
		t.Fatal("effective lock set carries no nvidia-dra-driver-gpu entry: the closure did not join")
	}
	wantLocked := []string{"enabled", allocpolicy.PathDRAGPUsEnabledOverride, allocpolicy.PathDRAGPUsEnabled}
	for _, want := range wantLocked {
		found := slices.Contains(draPaths, want)
		if !found {
			t.Errorf("closure-locked paths %v missing %q", draPaths, want)
		}
	}
	// The closure is never persisted: the artifact's recorded ownership
	// stays the declaration-derived set.
	if len(result.Metadata.SelectedProfile.OwnedPaths[allocpolicy.ComponentDRADriver]) != 0 {
		t.Error("closure paths leaked into persisted ownedPaths")
	}

	// The recipe-scoped descriptor-identity input (ADR-015 currentness):
	// the closure-contributing entries are the ENABLED descriptor
	// components in descriptor order — gpu-operator and the DRA driver;
	// gpu-operator-ocp is absent from GKE recipes and must not contribute.
	entries := result.ClosureDescriptorEntries()
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Component)
	}
	want := []string{allocpolicy.ComponentGPUOperator, allocpolicy.ComponentDRADriver}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ClosureDescriptorEntries() components = %v, want %v", got, want)
	}
}

// TestGKEGpuStackClosureLockRejectsPolicyDivergence pins the closure's
// bite: a candidate diverging at a closure-locked path the declaration
// never named fails the pre-output invariant, and a subset omitting the
// closure-locked component fails because the invariant cannot evaluate
// locked paths absent from the output.
func TestGKEGpuStackClosureLockRejectsPolicyDivergence(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(
		t.Context(), gkeCriteria(), "gpuStack=gke-default")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
	}
	ctx := context.Background()

	// Hydrate every component the lock covers — the profile's owned set
	// plus its closure — rather than a hardcoded list, so adding a
	// componentRef to the declaration (nvsentinel, #2181) cannot silently
	// turn "clean candidate passes" into a missing-values failure.
	hydrate := func(t *testing.T) map[string]map[string]any {
		t.Helper()
		candidate := make(map[string]map[string]any)
		for component := range result.EffectiveLockSet() {
			values, hErr := result.GetValuesForComponentWithContext(ctx, component)
			if hErr != nil {
				t.Fatalf("hydrate %s: %v", component, hErr)
			}
			candidate[component] = values
		}
		if len(candidate) == 0 {
			t.Fatal("lock set is empty — every subtest below would be vacuous")
		}
		return candidate
	}

	t.Run("clean candidate passes", func(t *testing.T) {
		t.Parallel()
		if err := result.ValidateProfileLock(ctx, result.ComponentRefs, hydrate(t), nil); err != nil {
			t.Fatalf("ValidateProfileLock() rejected a recipe-identical candidate: %v", err)
		}
	})

	t.Run("divergence at a closure-locked path fails", func(t *testing.T) {
		t.Parallel()
		candidate := hydrate(t)
		resources, ok := candidate[allocpolicy.ComponentDRADriver]["resources"].(map[string]any)
		if !ok {
			t.Fatal("hydrated DRA values carry no resources map")
		}
		gpus, ok := resources["gpus"].(map[string]any)
		if !ok {
			t.Fatal("hydrated DRA values carry no resources.gpus map")
		}
		gpus["enabled"] = true
		err := result.ValidateProfileLock(ctx, result.ComponentRefs, candidate, nil)
		if err == nil || !strings.Contains(err.Error(), allocpolicy.PathDRAGPUsEnabled) {
			t.Fatalf("ValidateProfileLock() error = %v, want closure divergence at %s", err, allocpolicy.PathDRAGPUsEnabled)
		}
	})

	t.Run("dynamic export of a closure-locked path fails even with the value unchanged", func(t *testing.T) {
		t.Parallel()
		dynamic := map[string][]string{
			allocpolicy.ComponentDRADriver: {allocpolicy.PathDRAGPUsEnabled},
		}
		err := result.ValidateProfileLock(ctx, result.ComponentRefs, hydrate(t), dynamic)
		if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("ValidateProfileLock() error = %v, want install-time mutability rejection", err)
		}
	})

	t.Run("subset omitting the closure-locked component fails", func(t *testing.T) {
		t.Parallel()
		var subset []ComponentRef
		for _, ref := range result.ComponentRefs {
			if ref.Name == allocpolicy.ComponentDRADriver {
				continue
			}
			subset = append(subset, ref)
		}
		err := result.ValidateProfileLock(ctx, subset, hydrate(t), nil)
		if err == nil || !strings.Contains(err.Error(), allocpolicy.ComponentDRADriver) {
			t.Fatalf("ValidateProfileLock() error = %v, want closure-component omission rejection", err)
		}
	})
}

// TestAKSShapedProfileDoesNotTriggerClosure pins the negative direction of
// the closure trigger: locks follow ownership, so a profile owning only
// driver/toolkit/runtime paths (the AKS shape) leaves allocation-policy
// keys unlocked, and its effective lock set equals its declared ownedPaths.
func TestAKSShapedProfileDoesNotTriggerClosure(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(
		t.Context(), aksCriteria(), "")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
	}
	if result.profileClosureTriggered() {
		t.Fatal("AKS azure-managed profile triggered the closure: locks must follow ownership")
	}
	lock := result.EffectiveLockSet()
	if _, locked := lock[allocpolicy.ComponentDRADriver]; locked {
		// The AKS declaration owns nvidiaDriverRoot on the DRA component;
		// that entry is legitimate. What must NOT appear is a policy
		// selector path.
		for _, path := range lock[allocpolicy.ComponentDRADriver] {
			if path == allocpolicy.PathDRAGPUsEnabled || path == allocpolicy.PathDRAGPUsEnabledOverride {
				t.Errorf("AKS lock set gained closure path %q without advertisement ownership", path)
			}
		}
	}
	if _, locked := lock[allocpolicy.ComponentGPUOperatorOCP]; locked {
		t.Error("AKS lock set gained gpu-operator-ocp: closure must not fire")
	}
	if entries := result.ClosureDescriptorEntries(); len(entries) != 0 {
		t.Errorf("ClosureDescriptorEntries() = %v, want empty: the AKS shape does not trigger the closure", entries)
	}
}

// TestCoherenceGateRejectsExternalWithDevicePluginEnabled pins the
// hydration-boundary dual-advertisement rejection on a raw artifact: a
// forged gke-default artifact whose overrides re-enable the device plugin
// fails PrepareAndValidateWithContext — the invalid tuple never reaches an
// output writer.
func TestCoherenceGateRejectsExternalWithDevicePluginEnabled(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(
		t.Context(), gkeCriteria(), "gpuStack=gke-default")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
	}
	ref := result.GetComponentRef("gpu-operator")
	if ref == nil {
		t.Fatal("gpu-operator componentRef missing")
	}
	plugin, ok := ref.Overrides["devicePlugin"].(map[string]any)
	if !ok {
		t.Fatal("gpu-operator overrides carry no devicePlugin map")
	}
	plugin["enabled"] = true

	err = result.PrepareAndValidateWithContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "devicePlugin.enabled=true") {
		t.Fatalf("PrepareAndValidateWithContext() error = %v, want dual-advertisement rejection", err)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}

// TestCoherenceGateRejectsBundleInstallerIncoherentTuples pins the
// empty-advertiser (bundle-installer) side of the gate/resolver symmetry
// over the shared #1327 tuple rows: the artifact gate applies the full
// tuple verdicts for EVERY closure-triggering profile, not only a declared
// external advertiser. A forged bundle-installer artifact whose overrides
// enable DRA whole-GPU advertisement next to the operator's device plugin
// (dual advertisement), or leave an inert chart-guard waiver, must fail
// PrepareAndValidateWithContext — the same tuple rows
// ResolveGPUAllocationPolicy rejects at validation time must never reach an
// output writer, because validation is not guaranteed to run before deploy.
// (The #1685 dual-operator rejection is resolver-side only and not mirrored
// here; it is outside the tuple rows this test covers.)
func TestCoherenceGateRejectsBundleInstallerIncoherentTuples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		overrides map[string]any
		wantMsg   string
	}{
		{
			// Mark Chmarny's #2044 repro: operator plugin (enabled by the
			// bundle-installer fragment) plus DRA ResourceClaims would both
			// advertise whole GPUs.
			name: "dual advertisement: DRA gpus enabled with waiver next to the operator plugin",
			overrides: map[string]any{
				"resources":                            map[string]any{"gpus": map[string]any{"enabled": true}},
				allocpolicy.PathDRAGPUsEnabledOverride: true,
			},
			wantMsg: "dual advertisement",
		},
		{
			name: "chart guard: DRA gpus enabled without the override waiver",
			overrides: map[string]any{
				"resources": map[string]any{"gpus": map[string]any{"enabled": true}},
			},
			wantMsg: "chart install guard",
		},
		{
			name: "inert waiver: override set while gpus stays disabled",
			overrides: map[string]any{
				allocpolicy.PathDRAGPUsEnabledOverride: true,
			},
			wantMsg: "inert waiver",
		},
	}
	t.Run("stock bundle-installer tuple passes the gate", func(t *testing.T) {
		t.Parallel()
		result, err := NewBuilder().BuildFromCriteriaWithProfile(
			t.Context(), gkeCriteria(), "gpuStack=bundle-installer")
		if err != nil {
			t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
		}
		if err := result.PrepareAndValidateWithContext(context.Background()); err != nil {
			t.Fatalf("PrepareAndValidateWithContext() rejected the bundle-installer happy path: %v", err)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteriaWithProfile(
				t.Context(), gkeCriteria(), "gpuStack=bundle-installer")
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
			}
			ref := result.GetComponentRef(allocpolicy.ComponentDRADriver)
			if ref == nil {
				t.Fatal("nvidia-dra-driver-gpu componentRef missing")
			}
			if ref.Overrides == nil {
				ref.Overrides = map[string]any{}
			}
			maps.Copy(ref.Overrides, tt.overrides)

			err = result.PrepareAndValidateWithContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("PrepareAndValidateWithContext() error = %v, want %q rejection", err, tt.wantMsg)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

// TestCoherenceGateRejectsAbsentDRAGPUsEnabled pins the fail-closed reading
// of an ABSENT resources.gpus.enabled on an enabled DRA component: the
// pinned chart's declared default is true, so treating absence as unknown
// would let a gke-default (external-advertiser) artifact whose custom
// values omit the stock pin pass generation and bundling while deploying a
// second whole-GPU advertiser (#1327 dual advertisement). The gate must
// reject the ambiguous state, mirroring the validation-time resolver. The
// absence is forged via an explicit null leaf: mergeValues deletes a key
// whose source value is nil, so the hydrated observation is PathAbsent —
// the same reading a custom --data values file that omits the pin
// produces.
func TestCoherenceGateRejectsAbsentDRAGPUsEnabled(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(
		t.Context(), gkeCriteria(), "gpuStack=gke-default")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
	}
	ref := result.GetComponentRef(allocpolicy.ComponentDRADriver)
	if ref == nil {
		t.Fatal("nvidia-dra-driver-gpu componentRef missing")
	}
	if ref.Overrides == nil {
		ref.Overrides = map[string]any{}
	}
	ref.Overrides["resources"] = map[string]any{"gpus": map[string]any{"enabled": nil}}
	// Waive the chart-guard tripwire: the upstream chart would then
	// install with its declared default (true) — live dual advertisement,
	// not an install failure.
	ref.Overrides[allocpolicy.PathDRAGPUsEnabledOverride] = true

	err = result.PrepareAndValidateWithContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), allocpolicy.PathDRAGPUsEnabled) ||
		!strings.Contains(err.Error(), "not set") {

		t.Fatalf("PrepareAndValidateWithContext() error = %v, want absent-switch rejection at %s",
			err, allocpolicy.PathDRAGPUsEnabled)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
	}
}
