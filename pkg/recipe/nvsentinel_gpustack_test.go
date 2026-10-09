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
	"slices"
	"strings"
	"testing"
)

const nvsentinelComponent = "nvsentinel"

// nvsentinelRuntimeClassPaths are the NVSentinel values that name the NVIDIA
// RuntimeClass, which must match gpu-operator's operator.runtimeClass.
var nvsentinelRuntimeClassPaths = []string{
	"metadata-collector.runtimeClassName",
	"janitor.config.controllers.gpuReset.resetJob.runtimeClassName",
}

// TestNVSentinelConfigurationMatrix pins the exact per-platform NVSentinel
// configuration issue #2181 specifies, resolved from the real embedded
// catalog.
//
// Two independent defects share this configuration boundary:
//
//   - #2175: the NVSentinel labeler infers driver presence from a driver pod.
//     Where no pod source installs the driver, the
//     nvsentinel.dgxc.nvidia.com/driver.installed label is never applied and
//     metadata-collector plus both syslog-health-monitor DaemonSets sit at
//     zero desired pods — silently, with no error and no event.
//   - #2176: metadata-collector requests the NVIDIA RuntimeClass by name. The
//     ClusterPolicy controller names that object after the GPU Operator's
//     operator.runtimeClass, so under AKS azure-managed the chart default
//     "nvidia" does not exist and admission rejects every pod.
//
// Explicit false/named values in the pod-managed variants are required, not
// incidental: they keep the paths profile-owned so they cannot be turned into
// an unsafe hybrid at bundle or install time.
func TestNVSentinelConfigurationMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		criteria *Criteria
		profile  string
		// wantAssume is the expected labeler.assumeDriverInstalled;
		// nil means the path must be left unset (chart default false).
		wantAssume *bool
		// wantRuntimeClass is the expected value of every
		// nvsentinelRuntimeClassPaths entry; "" means left unset
		// (chart default nvidia).
		wantRuntimeClass string
	}{
		{
			name:             "AKS azure-managed: node image installs the driver and names the runtime class",
			criteria:         aksCriteria(),
			profile:          "gpuStack=azure-managed",
			wantAssume:       ptr(true),
			wantRuntimeClass: "nvidia-container-runtime",
		},
		{
			name:             "AKS operator-managed: the operator's driver pod is the evidence",
			criteria:         aksCriteria(),
			profile:          "gpuStack=operator-managed",
			wantAssume:       ptr(false),
			wantRuntimeClass: "nvidia",
		},
		{
			name:       "GKE-COS gke-default: driver ships in the node image, no driver pod",
			criteria:   gkeCriteria(),
			profile:    "gpuStack=gke-default",
			wantAssume: ptr(true),
		},
		{
			name:       "GKE-COS bundle-installer: the bundle-carried installer supplies a driver pod",
			criteria:   gkeCriteria(),
			profile:    "gpuStack=bundle-installer",
			wantAssume: ptr(false),
		},
		{
			name: "OKE oci-managed: image driver + OKE's plugin, no driver pod",
			criteria: &Criteria{
				Service:     CriteriaServiceOKE,
				Accelerator: CriteriaAcceleratorA100,
				OS:          CriteriaOSOracleLinux,
				Intent:      CriteriaIntentTraining,
			},
			profile:    "gpuStack=oci-managed",
			wantAssume: ptr(true),
		},
		{
			name: "OKE operator-managed: the operator's driver pod is the evidence",
			criteria: &Criteria{
				Service:     CriteriaServiceOKE,
				Accelerator: CriteriaAcceleratorA100,
				OS:          CriteriaOSOracleLinux,
				Intent:      CriteriaIntentTraining,
			},
			profile:    "gpuStack=operator-managed",
			wantAssume: ptr(false),
		},
		{
			name: "Kind: nvkind host-installs the driver (overlay-level, no profile)",
			criteria: &Criteria{
				Service:     CriteriaServiceKind,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
			},
			wantAssume: ptr(true),
		},
		{
			name: "EKS: the operator installs the driver, both chart defaults already match",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				OS:          CriteriaOSUbuntu,
				Intent:      CriteriaIntentTraining,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, tt.profile)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile(%q) error = %v", tt.profile, err)
			}

			// Control: a row whose recipe silently lost its nvsentinel
			// component would satisfy every "left unset" expectation
			// vacuously.
			ref := result.GetComponentRef(nvsentinelComponent)
			if ref == nil {
				t.Fatal("nvsentinel componentRef missing — the assertions below would be vacuous")
			}
			if !ref.IsEnabled() {
				t.Fatal("nvsentinel is disabled — the assertions below would be vacuous")
			}

			values, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
			}

			assume, assumeSet := nestedBool(values, "labeler", "assumeDriverInstalled")
			switch {
			case tt.wantAssume == nil && assumeSet:
				t.Fatalf("labeler.assumeDriverInstalled = %v, want it left unset.\n"+
					"  The knob skips driver-pod detection and labels every GPU node\n"+
					"  unconditionally, so it belongs only where no pod source installs\n"+
					"  the driver. See #2175.", assume)
			case tt.wantAssume != nil && !assumeSet:
				t.Fatalf("labeler.assumeDriverInstalled is unset, want %v.\n"+
					"  Without it the driver.installed label is never applied and three\n"+
					"  DaemonSets silently sit at zero desired pods. See #2175.", *tt.wantAssume)
			case tt.wantAssume != nil && assume != *tt.wantAssume:
				t.Fatalf("labeler.assumeDriverInstalled = %v, want %v", assume, *tt.wantAssume)
			}

			for _, path := range nvsentinelRuntimeClassPaths {
				raw, state := profileValueAtPath(values, path)
				runtimeClass, isString := raw.(string)
				switch {
				case tt.wantRuntimeClass == "" && state != PathAbsent:
					t.Fatalf("%s = %#v, want it left unset "+
						"(the chart default already matches operator.runtimeClass)", path, raw)
				case tt.wantRuntimeClass != "" && !isString:
					t.Fatalf("%s = %#v, want %q.\n"+
						"  Left unset the API server rejects every pod that requests it with\n"+
						"  `RuntimeClass \"nvidia\" not found`.", path, raw, tt.wantRuntimeClass)
				case tt.wantRuntimeClass != "" && runtimeClass != tt.wantRuntimeClass:
					t.Fatalf("%s = %q, want %q", path, runtimeClass, tt.wantRuntimeClass)
				}
			}
		})
	}
}

// TestAKSRuntimeClassNamesAgreeUnderEveryProfileValue pins the #2176
// invariant directly: the RuntimeClass the GPU Operator creates and the ones
// metadata-collector and janitor's GPU reset Job request must be the same
// string under every gpuStack value.
//
// All three are owned by the same profile value, so they are consistent by
// construction rather than by convention; CheckNVSentinelRuntimeClassCoherence
// backs up only the metadata-collector name. This test asserts the property
// itself, so it still fails if a future edit moves any path out of the profile.
func TestAKSRuntimeClassNamesAgreeUnderEveryProfileValue(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"azure-managed", "operator-managed"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			result, err := NewBuilder().BuildFromCriteriaWithProfile(
				t.Context(), aksCriteria(), "gpuStack="+value)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}

			operatorValues, err := result.GetValuesForComponentWithContext(t.Context(), "gpu-operator")
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(gpu-operator): %v", err)
			}
			operatorClass, ok := nestedString(operatorValues, "operator", "runtimeClass")
			if !ok {
				t.Fatal("gpu-operator operator.runtimeClass is unset — the comparison would be vacuous")
			}

			sentinelValues, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
			}
			for _, path := range nvsentinelRuntimeClassPaths {
				raw, _ := profileValueAtPath(sentinelValues, path)
				sentinelClass, ok := raw.(string)
				if !ok {
					t.Fatalf("nvsentinel %s = %#v, want a string — the comparison would be vacuous", path, raw)
				}
				if operatorClass != sentinelClass {
					t.Fatalf("operator.runtimeClass = %q but nvsentinel %s = %q; "+
						"the API server rejects every pod that requests it when they differ",
						operatorClass, path, sentinelClass)
				}
			}
		})
	}
}

// TestNVSentinelReferencedInEveryProfileValue pins the catalog choice that
// makes NVSentinel mandatory on the profiled families.
//
// Today union totality happens to backstop this: the references assign leaf
// paths, so dropping one from a single value produces a path-set mismatch
// that ValidateProfileDeclaration rejects. That backstop is incidental, not
// structural. Totality is evaluated over leaf-flattened override paths BEFORE
// synthetic presence is added, and the engine deliberately exempts
// presence-only componentRefs from it (see
// TestValidateProfileDeclaration/"presence-only componentRef is exempt from
// union totality"). So the moment a future edit makes these references
// presence-only — or moves the leaf values elsewhere — totality stops
// covering them and NVSentinel's presence lock silently becomes dependent on
// which value the user selected.
//
// Pinning the reference directly means this test keeps holding under that
// edit, when the incidental backstop would not.
func TestNVSentinelReferencedInEveryProfileValue(t *testing.T) {
	t.Parallel()

	store, err := buildMetadataStore(t.Context(), defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("build metadata store: %v", err)
	}

	for _, overlayName := range []string{"aks", "gke-cos"} {
		t.Run(overlayName, func(t *testing.T) {
			t.Parallel()

			overlay, ok := store.Overlays[overlayName]
			if !ok {
				t.Fatalf("embedded catalog is missing the %s overlay", overlayName)
			}
			decl := overlay.Spec.Profile
			if decl == nil {
				t.Fatalf("%s overlay declares no profile — this test would be vacuous", overlayName)
			}
			if len(decl.Values) < 2 {
				t.Fatalf("%s profile has %d value(s); a single-value profile makes this vacuous",
					overlayName, len(decl.Values))
			}

			for valueName, value := range decl.Values {
				referenced := slices.ContainsFunc(value.ComponentRefs, func(ref ProfileComponentRef) bool {
					return ref.Name == nvsentinelComponent
				})
				if !referenced {
					t.Errorf("%s profile value %q does not reference %s.\n"+
						"  NVSentinel is a required component for this family, and presence is\n"+
						"  locked per referenced component. Union totality does NOT catch this:\n"+
						"  presence-only references are exempt from it by design. Reference it\n"+
						"  in every value. See #2181.", overlayName, valueName, nvsentinelComponent)
				}
			}
		})
	}
}

// TestNVSentinelPresenceLockedOnProfiledFamilies pins the presence half of
// the ownership contract.
//
// Naming a component in a profile declaration locks more than the leaf paths
// it assigns: the declaration-wide synthetic "enabled" marker is added for
// every referenced component, and ValidateProfileLock then rejects any output
// where that component is absent or disabled. Configuring NVSentinel from the
// profile therefore also makes it mandatory on AKS and GKE-COS —
// `--set nv-sentinel:enabled=false` and an API `bundlers=` list omitting it
// both fail closed.
//
// That is the intended result, not a side effect: NVSentinel is required for
// these deployments (#2181). The test exists so the contract is visible and
// any future relaxation is a deliberate edit.
func TestNVSentinelPresenceLockedOnProfiledFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		criteria *Criteria
	}{
		{name: "aks default", criteria: aksCriteria()},
		{name: "gke-cos default", criteria: gkeCriteria()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			result, err := NewBuilder().BuildFromCriteriaWithProfile(ctx, tt.criteria, "")
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}

			owned, ok := result.Metadata.SelectedProfile.OwnedPaths[nvsentinelComponent]
			if !ok {
				t.Fatal("gpuStack does not own nvsentinel")
			}
			if !slices.Contains(owned, "enabled") {
				t.Fatalf("nvsentinel ownedPaths = %v, want the synthetic presence marker %q",
					owned, "enabled")
			}

			hydrate := func() map[string]map[string]any {
				candidate := make(map[string]map[string]any)
				for component := range result.EffectiveLockSet() {
					values, hErr := result.GetValuesForComponentWithContext(ctx, component)
					if hErr != nil {
						t.Fatalf("hydrate %s: %v", component, hErr)
					}
					candidate[component] = values
				}
				return candidate
			}

			// Control: the recipe's own output must pass, otherwise the
			// rejection below proves nothing.
			if lockErr := result.ValidateProfileLock(ctx, result.ComponentRefs, hydrate(), nil); lockErr != nil {
				t.Fatalf("ValidateProfileLock() rejected a recipe-identical candidate: %v", lockErr)
			}

			// Mirrors what `--set nv-sentinel:enabled=false` and an API
			// bundlers= list omitting nvsentinel produce: the component is
			// filtered out of the candidate refs before the lock runs.
			kept := make([]ComponentRef, 0, len(result.ComponentRefs))
			for _, ref := range result.ComponentRefs {
				if ref.Name == nvsentinelComponent {
					continue
				}
				kept = append(kept, ref)
			}
			if len(kept) == len(result.ComponentRefs) {
				t.Fatal("nvsentinel is not present in the resolved componentRefs")
			}
			lockErr := result.ValidateProfileLock(ctx, kept, hydrate(), nil)
			if lockErr == nil || !strings.Contains(lockErr.Error(), "absent or disabled") {
				t.Fatalf("ValidateProfileLock() error = %v, want nvsentinel absent-or-disabled rejection", lockErr)
			}
		})
	}
}

// TestOKENVSentinelValueIsProfileOwned pins the OKE family's conversion to
// the gpuStack profile: assumeDriverInstalled moved from the overlay's
// componentRefs into the profile values, so OKE now receives the same
// generated install-time profile lock as AKS and GKE-COS — the asymmetry the
// previous version of this test documented is gone.
func TestOKENVSentinelValueIsProfileOwned(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), &Criteria{
		Service:     CriteriaServiceOKE,
		Accelerator: CriteriaAcceleratorA100,
		OS:          CriteriaOSOracleLinux,
		Intent:      CriteriaIntentTraining,
	}, "")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
	}

	// The value is present under the declared default ...
	values, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
	}
	if assume, ok := nestedBool(values, "labeler", "assumeDriverInstalled"); !ok || !assume {
		t.Fatalf("OKE labeler.assumeDriverInstalled = %v (set: %v), want true under oci-managed", assume, ok)
	}

	// ... and profile-owned.
	selected := result.Metadata.SelectedProfile
	if selected == nil || selected.Name != "gpuStack" || selected.Value != "oci-managed" {
		t.Fatalf("selectedProfile = %#v, want gpuStack=oci-managed", selected)
	}
	owned := selected.OwnedPaths[nvsentinelComponent]
	wantOwned := []string{
		"enabled", "janitor.config.controllers.gpuReset.resetJob.hostDriverRootPath",
		"janitor.config.controllers.gpuReset.serviceManager.name",
		"janitor.config.controllers.gpuReset.serviceManager.spec.apps",
		"janitor.config.controllers.gpuReset.serviceManager.spec.namespace",
		"janitor.config.controllers.gpuReset.serviceManager.spec.restoreTimeout",
		"janitor.config.controllers.gpuReset.serviceManager.spec.teardownTimeout",
		"labeler.assumeDriverInstalled",
	}
	if !slices.Equal(owned, wantOwned) {
		t.Fatalf("ownedPaths[nvsentinel] = %v, want %v", owned, wantOwned)
	}
	if len(result.EffectiveLockSet()) == 0 {
		t.Fatal("OKE profile lock set is empty, want the gpuStack-owned paths")
	}
}

func ptr[T any](v T) *T { return &v }

func nestedBool(values map[string]any, parent, leaf string) (bool, bool) {
	block, ok := values[parent].(map[string]any)
	if !ok {
		return false, false
	}
	value, ok := block[leaf].(bool)

	return value, ok
}

func nestedString(values map[string]any, parent, leaf string) (string, bool) {
	block, ok := values[parent].(map[string]any)
	if !ok {
		return "", false
	}
	value, ok := block[leaf].(string)

	return value, ok
}
