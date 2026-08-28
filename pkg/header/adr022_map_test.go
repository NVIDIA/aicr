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

package header_test

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// adr022Row is one row of the ADR-022 §2 per-kind maturity map, bound to the
// constants and read gate the tree actually uses rather than to string
// literals. A row that disagrees with the map is a contract bug.
//
// The emitter constants live in five packages. Nothing previously tied them
// to the gate that must accept them or to the target they are headed for, so
// "did we get every emitter?" was answerable only by grep. That is the
// question the ADR-022 §3 emitter switch turns on, and this table answers it.
type adr022Row struct {
	// kind names the row as it appears in the ADR-022 §2 map.
	kind string

	// emitted is the value this release stamps on the artifact.
	emitted string

	// target is the §2 target the emitted value becomes at the emitter switch.
	target string

	// accepts is the kind/schema-scoped read gate guarding this row.
	accepts func(string) bool
}

// adr022Map mirrors the ADR-022 §2 per-kind maturity map. Every AICR wire kind
// with an emitter has a row; §7 requires a new kind to add one in the same
// change that introduces it. A kind emitted through more than one constant
// gets a row per constant, or an edit to the unbound one passes unnoticed.
//
// Scope: this file pins the *constant* contract — that each track constant
// routes to the right gate and target. It does not verify that an individual
// emit site selected the right constant. A catalog emitter that referenced
// RecipeResultAPIVersion instead of RecipeMetadataAPIVersion is invisible here,
// because the stable and authoring constants carry the same string until the
// emitter switch. That half lives in adr022_emit_test.go, which asserts the
// observed apiVersion on a real artifact against its track's constant.
//
// ComponentUpgrades (ADR-021) has a §2 row but no emitter yet, so it has no
// row here. It starts at header.GroupVersionV1Beta1, which
// IsSupportedAuthoringAPIVersion already accepts; add a row with its constant
// when its loader lands.
func adr022Map() []adr022Row {
	return []adr022Row{
		{
			kind:    "Snapshot",
			emitted: snapshotter.FullAPIVersion,
			target:  header.GroupVersionV1,
			accepts: header.IsSupportedAPIVersion,
		},
		{
			kind:    "RecipeResult (default resolved recipe)",
			emitted: recipe.RecipeResultAPIVersion,
			target:  header.GroupVersionV1,
			accepts: header.IsSupportedAPIVersion,
		},
		{
			kind:    "RecipeCriteria",
			emitted: recipe.RecipeCriteriaAPIVersion,
			target:  header.GroupVersionV1,
			accepts: header.IsSupportedAPIVersion,
		},
		{
			kind:    "BundleProvenance",
			emitted: localformat.ProvenanceAPIVersion,
			target:  header.GroupVersionV1,
			accepts: header.IsSupportedAPIVersion,
		},
		{
			kind:    "AICRConfig",
			emitted: config.APIVersion,
			target:  header.GroupVersionV1Beta1,
			accepts: header.IsSupportedAuthoringAPIVersion,
		},
		{
			kind:    "RecipeMetadata, RecipeMixin (catalog)",
			emitted: recipe.RecipeMetadataAPIVersion,
			target:  header.GroupVersionV1Beta1,
			accepts: header.IsSupportedAuthoringAPIVersion,
		},
		{
			kind:    "ComponentRegistry",
			emitted: recipe.ComponentRegistryAPIVersion,
			target:  header.GroupVersionV1Beta1,
			accepts: header.IsSupportedAuthoringAPIVersion,
		},
		{
			kind:    "RecipeMetadata, RecipeResult (profile-bearing)",
			emitted: recipe.RecipeProfileAPIVersion,
			target:  header.GroupVersionV1Beta2,
			accepts: header.IsSupportedProfileAPIVersion,
		},
		{
			// Second constant on the same track. A configuration-bearing
			// RecipeResult is stamped through ConfiguredRecipeResultAPIVersion
			// (accounting.go, runtimeinventory.go) rather than
			// RecipeProfileAPIVersion (metadata_store.go), so binding only the
			// latter would let a future edit repoint this one alone and still
			// pass every assertion below.
			kind:    "RecipeResult (configuration-bearing)",
			emitted: recipe.ConfiguredRecipeResultAPIVersion,
			target:  header.GroupVersionV1Beta2,
			accepts: header.IsSupportedProfileAPIVersion,
		},
	}
}

// TestADR022EmittedValueIsReadable asserts a binary reads what it writes. An
// emitter pointed at a value its own gate rejects would produce artifacts no
// AICR release can load, including the one that wrote them.
func TestADR022EmittedValueIsReadable(t *testing.T) {
	t.Parallel()

	for _, row := range adr022Map() {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()
			if !row.accepts(row.emitted) {
				t.Errorf("emitted apiVersion %q is rejected by this kind's read gate",
					row.emitted)
			}
		})
	}
}

// TestADR022TargetIsReadableBeforeTheEmitterSwitch asserts the ADR-022 §3
// reader-first invariant: every §2 target parses now, a release before any
// emitter writes it. Without this a rollback to the current release cannot
// read artifacts the next release produced.
func TestADR022TargetIsReadableBeforeTheEmitterSwitch(t *testing.T) {
	t.Parallel()

	for _, row := range adr022Map() {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()
			if !row.accepts(row.target) {
				t.Errorf("ADR-022 target apiVersion %q is rejected by this kind's read gate; "+
					"§3 requires readers to accept the target before emitters write it",
					row.target)
			}
		})
	}
}

// TestADR022EmittersAreStillOnAlpha pins the migration stage. AICR is in
// ADR-022 §3 Release N: readers accept both tracks, emitters still write the
// alpha values.
//
// The emitter switch (v0.22, issue #2416) makes this test fail, which is the
// point — it forces that release to update this table rather than flipping
// constants and discovering the blast radius in review. When it does, invert
// this to assert emitted == target and delete the alpha branch.
func TestADR022EmittersAreStillOnAlpha(t *testing.T) {
	t.Parallel()

	alpha := map[string]bool{
		header.GroupVersion:             true,
		header.RecipeResultGroupVersion: true,
	}

	for _, row := range adr022Map() {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()
			if !alpha[row.emitted] {
				t.Errorf("emitted apiVersion %q is not an alpha value; if this is the "+
					"ADR-022 emitter switch, update this test and the migration table "+
					"in RELEASING.md together", row.emitted)
			}
			if row.emitted == row.target {
				t.Errorf("emitted apiVersion equals the target %q; emitters do not "+
					"switch until Release N+1", row.target)
			}
		})
	}
}

// TestADR022TracksShareAlphaButNotTargets is why the stable and authoring
// emitter constants are separate despite carrying the same string today.
//
// header.StableGroupVersion == header.AuthoringGroupVersion during the
// reader-first release, so a package that aliases either one, or aliases
// header.GroupVersion directly, looks correct now and silently emits the wrong
// value at the switch. Snapshot goes to aicr.run/v1 while AICRConfig goes to
// aicr.run/v1beta1; one shared constant cannot serve both.
func TestADR022TracksShareAlphaButNotTargets(t *testing.T) {
	t.Parallel()

	if header.StableGroupVersion != header.AuthoringGroupVersion {
		t.Fatalf("the tracks have already diverged (stable %q, authoring %q); "+
			"this test documents the reader-first release and needs updating",
			header.StableGroupVersion, header.AuthoringGroupVersion)
	}

	if header.GroupVersionV1 == header.GroupVersionV1Beta1 {
		t.Errorf("stable and authoring targets are both %q; ADR-022 §2 sends them "+
			"to different maturities", header.GroupVersionV1)
	}
}

// TestADR022RowUsesItsTracksGate asserts each kind is guarded by the gate for
// its own track, not merely by some gate that happens to accept the alpha
// value all three share today.
//
// Every gate accepts header.GroupVersion during the reader-first release, so a
// kind wired to the wrong one still reads its own artifacts and looks correct.
// The targets are what distinguish the tracks, so that is what this checks: a
// gate must accept its row's target and reject the other two.
func TestADR022RowUsesItsTracksGate(t *testing.T) {
	t.Parallel()

	allTargets := []string{
		header.GroupVersionV1,
		header.GroupVersionV1Beta1,
		header.GroupVersionV1Beta2,
	}

	for _, row := range adr022Map() {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()
			for _, target := range allTargets {
				accepted := row.accepts(target)
				if target == row.target && !accepted {
					t.Errorf("gate rejects this row's own target %q", target)
				}
				if target != row.target && accepted {
					t.Errorf("gate accepts %q, which belongs to another track; "+
						"this row's target is %q, so it is wired to the wrong gate",
						target, row.target)
				}
			}
		})
	}
}

// TestADR022GatesRejectEmptyAndUnknown asserts every gate fails closed. The
// empty string is rejected here by design: loaders that still tolerate a
// missing apiVersion special-case it before calling the gate, and ADR-022 §3
// retires that tolerance at Release N+2 (issue #2417).
func TestADR022GatesRejectEmptyAndUnknown(t *testing.T) {
	t.Parallel()

	// aicr.run/v1alpha1 in particular has never been valid on this domain:
	// ADR-013 moved the version to v1alpha2 at the domain rename, so the
	// legacy pairing was aicr.nvidia.com/v1alpha1.
	rejected := []string{
		"",
		"aicr.run/v1alpha1",
		"aicr.run/v1alpha9",
		"aicr.nvidia.com/v1alpha1",
		"v1",
	}

	for _, row := range adr022Map() {
		t.Run(row.kind, func(t *testing.T) {
			t.Parallel()
			for _, version := range rejected {
				if row.accepts(version) {
					t.Errorf("read gate accepted %q, want rejected", version)
				}
			}
		})
	}
}
