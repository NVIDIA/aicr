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
	"slices"
	"strings"
	"testing"
)

// dynamoSnapshotMixin is the opt-in mixin that links Snapshot to Dynamo the
// way Dynamo's own chart does: Snapshot installs as its own component, ahead of
// dynamo-platform, and the Dynamo operator's checkpoint feature is switched on.
const dynamoSnapshotMixin = "dynamo-snapshot"

// dynamoSnapshotStore loads the real embedded catalog, so these tests exercise
// the shipped mixin rather than a fixture. The store is cached and shared:
// tests that write into store.Overlays must use dynamoSnapshotUncachedStore.
func dynamoSnapshotStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[dynamoSnapshotMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", dynamoSnapshotMixin, dynamoSnapshotMixin)
	}
	return ctx, store
}

// dynamoSnapshotUncachedStore builds a store that bypasses the process-wide
// cache, so a test can compose the mixin onto overlays without leaving it
// composed for every test that runs afterwards.
func dynamoSnapshotUncachedStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("buildMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[dynamoSnapshotMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", dynamoSnapshotMixin, dynamoSnapshotMixin)
	}
	return ctx, store
}

// checkpointEnabled reports the dynamo-operator.checkpoint.enabled override on
// a dynamo-platform ref, and whether the override is present at all.
func checkpointEnabled(ref ComponentRef) (enabled, present bool) {
	operator, _ := ref.Overrides["dynamo-operator"].(map[string]any)
	checkpoint, _ := operator["checkpoint"].(map[string]any)
	value, present := checkpoint["enabled"]
	enabled, _ = value.(bool)
	return enabled, present
}

// TestDynamoSnapshotMixin_ComposesCleanly proves the shipped mixin merges onto
// a leaf that already carries dynamo-platform with its own dependency edges,
// which is what makes the mixin's dependencyRefs an additive merge.
func TestDynamoSnapshotMixin_ComposesCleanly(t *testing.T) {
	ctx, store := dynamoSnapshotStore(t)

	spec := RecipeMetadataSpec{
		Mixins: []string{dynamoSnapshotMixin},
		ComponentRefs: []ComponentRef{
			{Name: "gpu-operator", Type: ComponentTypeHelm},
			{
				Name: "dynamo-platform", Type: ComponentTypeHelm,
				DependencyRefs: []string{"grove", "cert-manager"},
			},
		},
	}
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	snapshot, ok := findComponentRefByName(spec.ComponentRefs, snapshotComponent)
	if !ok {
		t.Fatal("snapshot missing from merged spec")
	}
	if snapshot.ValuesFile != snapshotValuesFile {
		t.Errorf("snapshot valuesFile = %q, want %q", snapshot.ValuesFile, snapshotValuesFile)
	}
	// The registry default is the only source of the pin, so the dynamo-platform
	// pin and the snapshot pin cannot drift apart inside a recipe.
	if snapshot.Version != "" || snapshot.Tag != "" {
		t.Errorf("snapshot pins version %q / tag %q in the mixin; the registry default must be the only pin",
			snapshot.Version, snapshot.Tag)
	}
	if !slices.Contains(snapshot.DependencyRefs, "gpu-operator") {
		t.Errorf("snapshot dependencyRefs = %v, want gpu-operator: the agent needs its nvidia RuntimeClass and driver",
			snapshot.DependencyRefs)
	}

	dynamo, ok := findComponentRefByName(spec.ComponentRefs, "dynamo-platform")
	if !ok {
		t.Fatal("dynamo-platform missing from merged spec")
	}
	for _, dep := range []string{"grove", "cert-manager", snapshotComponent} {
		if !slices.Contains(dynamo.DependencyRefs, dep) {
			t.Errorf("dynamo-platform dependencyRefs = %v, want %s (the merge is additive)", dynamo.DependencyRefs, dep)
		}
	}
	if dynamo.Version != "" || dynamo.Tag != "" {
		t.Errorf("dynamo-platform pins version %q / tag %q in the mixin; the registry default must be the only pin",
			dynamo.Version, dynamo.Tag)
	}
	if enabled, present := checkpointEnabled(dynamo); !present || !enabled {
		t.Errorf("dynamo-operator.checkpoint.enabled = %v (present %v), want true", enabled, present)
	}
}

// TestDynamoSnapshotMixin_ComposesOntoEveryDynamoLeaf resolves each shipped
// *-inference-dynamo leaf with the mixin composed and checks what an
// integrator gets: snapshot installed before dynamo-platform, after the GPU
// operator, with checkpointing on.
func TestDynamoSnapshotMixin_ComposesOntoEveryDynamoLeaf(t *testing.T) {
	ctx, store := dynamoSnapshotUncachedStore(t)

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil || !strings.HasSuffix(name, "-inference-dynamo") {
			continue
		}
		overlay.Spec.Mixins = append(overlay.Spec.Mixins, dynamoSnapshotMixin)
		resolved++
		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("resolving %s with the %s mixin composed: %v", name, dynamoSnapshotMixin, err)
			}
			// Without this the subtest would pass for a leaf whose criteria
			// resolved to some other overlay entirely.
			if !slices.Contains(result.Metadata.AppliedOverlays, name) {
				t.Fatalf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
			}

			if _, ok := findComponentRefByName(result.ComponentRefs, snapshotComponent); !ok {
				t.Fatalf("%s resolved without snapshot", name)
			}
			dynamo, ok := findComponentRefByName(result.ComponentRefs, "dynamo-platform")
			if !ok {
				t.Fatalf("%s resolved without dynamo-platform", name)
			}
			if !slices.Contains(dynamo.DependencyRefs, snapshotComponent) {
				t.Errorf("%s: snapshot edge missing from dynamo-platform (%v)", name, dynamo.DependencyRefs)
			}
			if enabled, present := checkpointEnabled(dynamo); !present || !enabled {
				t.Errorf("%s: dynamo-operator.checkpoint.enabled = %v (present %v), want true", name, enabled, present)
			}

			order := result.DeploymentOrder
			snapshotAt := slices.Index(order, snapshotComponent)
			dynamoAt := slices.Index(order, "dynamo-platform")
			if snapshotAt < 0 || dynamoAt < 0 {
				t.Fatalf("%s: deployment order %v lacks snapshot or dynamo-platform", name, order)
			}
			if snapshotAt > dynamoAt {
				t.Errorf("%s: snapshot is deployed after dynamo-platform (%v)", name, order)
			}
			if gpuAt := slices.Index(order, "gpu-operator"); gpuAt >= 0 && gpuAt > snapshotAt {
				t.Errorf("%s: gpu-operator is deployed after snapshot (%v)", name, order)
			}
		})
	}
	if resolved == 0 {
		t.Fatal("no *-inference-dynamo leaves were found -- the walker is broken")
	}
}

// TestDynamoSnapshotMixin_NotAdoptedByShippedRecipes pins the opt-in claim:
// resolving every shipped leaf as a user would yields no snapshot component and
// leaves the Dynamo checkpoint feature at the chart default (off).
func TestDynamoSnapshotMixin_NotAdoptedByShippedRecipes(t *testing.T) {
	ctx, store := dynamoSnapshotStore(t)

	for name, overlay := range store.Overlays {
		if slices.Contains(overlay.Spec.Mixins, dynamoSnapshotMixin) {
			t.Errorf("overlay %s declares the %s mixin; it must stay opt-in, adopted only by an integrator's own overlay",
				name, dynamoSnapshotMixin)
		}
	}

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Errorf("resolving %s: %v", name, err)
			continue
		}
		resolved++
		if _, ok := findComponentRefByName(result.ComponentRefs, snapshotComponent); ok {
			t.Errorf("%s resolves with snapshot; the component must not reach a shipped recipe", name)
		}
		if dynamo, ok := findComponentRefByName(result.ComponentRefs, "dynamo-platform"); ok {
			if _, present := checkpointEnabled(dynamo); present {
				t.Errorf("%s resolves with a dynamo-operator.checkpoint override; the mixin must not reach a shipped recipe", name)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}
