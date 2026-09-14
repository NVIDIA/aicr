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
	stderrors "errors"
	"reflect"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// The embedded catalog declares no profile, so the unit tests around
// ValidateProfileDeclaration and applyEffectiveProfile exercise the pieces in
// isolation and nothing drives the whole path. These tests layer a profiled
// overlay over the embedded data — the supported out-of-tree adoption route —
// and resolve through the ordinary Builder so criteria matching, declaration
// discovery, value selection, and ownership locking run together.
const profileOverlayDir = "testdata/profile-overlay"

func profiledBuilder(t *testing.T) *Builder {
	t.Helper()
	provider, err := NewLayeredDataProvider(
		NewEmbeddedDataProvider(GetEmbeddedFS(), "."),
		LayeredProviderConfig{ExternalDir: profileOverlayDir},
	)
	if err != nil {
		t.Fatalf("NewLayeredDataProvider() error = %v", err)
	}
	return NewBuilder(WithDataProvider(provider))
}

func profiledCriteria() *Criteria {
	return &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		OS:          CriteriaOSUbuntu,
		Intent:      CriteriaIntentTraining,
		Platform:    CriteriaPlatformKubeflow,
	}
}

func TestProfileResolutionEndToEnd(t *testing.T) {
	wantOwned := map[string][]string{
		"gpu-operator":          {"driver.enabled", "enabled"},
		"nvidia-dra-driver-gpu": {"enabled", "nvidiaDriverRoot"},
	}

	tests := []struct {
		name           string
		selection      string
		wantValue      string
		wantDriver     bool
		wantDriverRoot string
	}{
		{
			// No selection: the declaration's default applies.
			name:           "default value",
			selection:      "",
			wantValue:      "preinstalled",
			wantDriver:     false,
			wantDriverRoot: "/",
		},
		{
			name:           "explicit default",
			selection:      "gpuStack=preinstalled",
			wantValue:      "preinstalled",
			wantDriver:     false,
			wantDriverRoot: "/",
		},
		{
			name:           "explicit non-default",
			selection:      "gpuStack=operator-managed",
			wantValue:      "operator-managed",
			wantDriver:     true,
			wantDriverRoot: "/run/nvidia/driver",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := profiledBuilder(t).BuildFromCriteriaWithProfile(
				t.Context(), profiledCriteria(), tt.selection)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}

			// A profiled composition must stamp the gated apiVersion; the
			// biconditional is enforced in both directions.
			if result.APIVersion != RecipeProfileAPIVersion {
				t.Fatalf("apiVersion = %q, want %q", result.APIVersion, RecipeProfileAPIVersion)
			}
			selected := result.Metadata.SelectedProfile
			if selected == nil {
				t.Fatal("metadata.selectedProfile is nil for a profiled composition")
			}
			if selected.Name != "gpuStack" || selected.Value != tt.wantValue {
				t.Fatalf("selectedProfile = %s=%s, want gpuStack=%s",
					selected.Name, selected.Value, tt.wantValue)
			}
			if selected.Advertiser != "" {
				t.Fatalf("advertiser = %q, want empty (this fixture declares none)", selected.Advertiser)
			}

			// ownedPaths is the union across every value of the declaration,
			// not just the selected one, and carries the synthetic presence
			// path for each referenced component.
			if !reflect.DeepEqual(selected.OwnedPaths, wantOwned) {
				t.Fatalf("ownedPaths = %#v, want %#v", selected.OwnedPaths, wantOwned)
			}

			// The selected fragment's overrides must actually reach the
			// componentRefs, not merely be recorded in metadata.
			operator := result.GetComponentRef("gpu-operator")
			if operator == nil {
				t.Fatal("gpu-operator componentRef missing")
			}
			driver, ok := operator.Overrides["driver"].(map[string]any)
			if !ok {
				t.Fatalf("gpu-operator overrides.driver = %#v, want map", operator.Overrides["driver"])
			}
			if enabled, _ := driver["enabled"].(bool); enabled != tt.wantDriver {
				t.Fatalf("driver.enabled = %v, want %v", driver["enabled"], tt.wantDriver)
			}

			dra := result.GetComponentRef("nvidia-dra-driver-gpu")
			if dra == nil {
				t.Fatal("nvidia-dra-driver-gpu componentRef missing")
			}
			if root, _ := dra.Overrides["nvidiaDriverRoot"].(string); root != tt.wantDriverRoot {
				t.Fatalf("nvidiaDriverRoot = %q, want %q", dra.Overrides["nvidiaDriverRoot"], tt.wantDriverRoot)
			}

			// The artifact must survive the shared raw-artifact gate it will
			// meet again on the bundle path.
			if err := result.PrepareAndValidateWithContext(t.Context()); err != nil {
				t.Fatalf("PrepareAndValidateWithContext() error = %v", err)
			}
		})
	}
}

// TestProfileSelectionIsolatesOwnedPaths pins the blast radius of a selection:
// switching values may move the owned paths and the recorded selection, and
// nothing else.
func TestProfileSelectionIsolatesOwnedPaths(t *testing.T) {
	build := func(selection string) *RecipeResult {
		t.Helper()
		result, err := profiledBuilder(t).BuildFromCriteriaWithProfile(
			t.Context(), profiledCriteria(), selection)
		if err != nil {
			t.Fatalf("BuildFromCriteriaWithProfile(%q) error = %v", selection, err)
		}
		return result
	}

	first := build("gpuStack=preinstalled")
	second := build("gpuStack=operator-managed")

	if len(first.ComponentRefs) != len(second.ComponentRefs) {
		t.Fatalf("componentRef count changed: %d vs %d",
			len(first.ComponentRefs), len(second.ComponentRefs))
	}
	for i := range first.ComponentRefs {
		a, b := first.ComponentRefs[i], second.ComponentRefs[i]
		if a.Name != b.Name {
			t.Fatalf("componentRef order changed at %d: %q vs %q", i, a.Name, b.Name)
		}
		if a.Name == "gpu-operator" || a.Name == "nvidia-dra-driver-gpu" {
			continue // the two components the profile owns
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("unowned component %q differs between profile values", a.Name)
		}
	}
	if first.Metadata.SelectedProfile == nil || second.Metadata.SelectedProfile == nil {
		t.Fatal("selectedProfile is nil for an explicitly selected profile")
	}
	firstOwned := first.Metadata.SelectedProfile.OwnedPaths
	secondOwned := second.Metadata.SelectedProfile.OwnedPaths
	if !reflect.DeepEqual(firstOwned, secondOwned) {
		t.Fatal("ownedPaths must be declaration-wide and identical across values")
	}
}

func TestProfileResolutionFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		selection string
		wantErr   string
	}{
		{
			name:      "unknown value",
			selection: "gpuStack=does-not-exist",
			wantErr:   `has no value "does-not-exist"`,
		},
		{
			name:      "wrong declaration name",
			selection: "notAProfile=preinstalled",
			wantErr:   "declares profile",
		},
		{
			name:      "malformed selection",
			selection: "gpuStack",
			wantErr:   "name=value",
		},
		{
			// "gpuStack=" splits cleanly on its one "=", so it passes the
			// shape check and fails on the empty value's identifier pattern —
			// a different rejection than the malformed case above.
			name:      "empty value",
			selection: "gpuStack=",
			wantErr:   "must match [A-Za-z0-9._-]+",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := profiledBuilder(t).BuildFromCriteriaWithProfile(
				t.Context(), profiledCriteria(), tt.selection)
			if err == nil {
				t.Fatal("BuildFromCriteriaWithProfile() error = nil, want rejection")
			}
			// Match the message, not only the code: every rejection on this
			// path is ErrCodeInvalidRequest, so the code alone cannot tell the
			// intended failure from an unrelated validation error.
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestUnprofiledCriteriaStayLegacy is the other half of the biconditional: a
// composition that reaches no profile declaration keeps the legacy apiVersion
// and records no selection, even with the profiled overlay present.
func TestUnprofiledCriteriaStayLegacy(t *testing.T) {
	criteria := profiledCriteria()
	criteria.Platform = "" // no longer matches the profiled leaf

	result, err := profiledBuilder(t).BuildFromCriteria(t.Context(), criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	if result.APIVersion != RecipeResultAPIVersion {
		t.Fatalf("apiVersion = %q, want %q", result.APIVersion, RecipeResultAPIVersion)
	}
	if result.Metadata.SelectedProfile != nil {
		t.Fatalf("selectedProfile = %#v, want nil for an unprofiled composition",
			result.Metadata.SelectedProfile)
	}
}
