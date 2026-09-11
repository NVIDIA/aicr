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
	"io/fs"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// newNvsentinelAllowlistStore builds a MetadataStore backed by an
// in-memory provider whose registry.yaml declares nvsentinel with the
// given mixinSafeOverridePaths allowlist, plus any extraFiles (e.g. a
// values file a test's ComponentRef points at via ValuesFile).
func newNvsentinelAllowlistStore(tag string, allowlist []string, extraFiles map[string][]byte) *MetadataStore {
	var registry strings.Builder
	registry.WriteString("apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents:\n  - name: nvsentinel\n    displayName: NVSentinel\n    mixinSafeOverridePaths:\n")
	for _, p := range allowlist {
		registry.WriteString("      - " + p + "\n")
	}
	files := map[string][]byte{"registry.yaml": []byte(registry.String())}
	for k, v := range extraFiles {
		files[k] = v
	}
	return &MetadataStore{
		provider: newInMemoryProvider(tag, files),
		Mixins:   map[string]*RecipeMixin{},
	}
}

// addTestMixin registers a RecipeMixin named name with the given
// componentRefs into store.Mixins.
func addTestMixin(store *MetadataStore, name string, refs []ComponentRef) {
	m := &RecipeMixin{}
	m.Metadata.Name = name
	m.Spec.ComponentRefs = refs
	store.Mixins[name] = m
}

// TestMixinOverridesSafeForMerge covers mixinOverridesSafeForMerge's
// allowlist and collision rules directly, independent of any real mixin
// file: only registry-allowlisted paths compose, and only when they don't
// collide with a path already set elsewhere.
func TestMixinOverridesSafeForMerge(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	provider := store.provider

	tests := []struct {
		name              string
		componentName     string
		mixinOverrides    map[string]any
		existingOverrides map[string]any
		wantErr           bool
		wantErrContains   string
	}{
		{
			name:          "nvsentinel allowlisted path, no existing overrides -> safe",
			componentName: "nvsentinel",
			mixinOverrides: map[string]any{
				"global": map[string]any{"auditLogging": map[string]any{"enabled": true}},
			},
		},
		{
			name:          "nvsentinel allowlisted path, non-colliding existing override -> safe",
			componentName: "nvsentinel",
			mixinOverrides: map[string]any{
				"global": map[string]any{"tracing": map[string]any{"enabled": true}},
			},
			existingOverrides: map[string]any{
				"global": map[string]any{"auditLogging": map[string]any{"enabled": true}},
			},
		},
		{
			// This is the exact shape of round 1/2's original bug: a mixin
			// reaching into an unrelated, already-chained component and
			// disabling its driver. gpu-operator declares no
			// mixinSafeOverridePaths, so every path on it is rejected.
			name:          "arbitrary override on an unrelated component (gpu-operator.driver.enabled) -> rejected",
			componentName: "gpu-operator",
			mixinOverrides: map[string]any{
				"driver": map[string]any{"enabled": false},
			},
			wantErr:         true,
			wantErrContains: "not in the component's registry-declared mixinSafeOverridePaths allowlist",
		},
		{
			// global.tracing.endpoint is deliberately excluded from
			// nvsentinel's allowlist (recipes/registry.yaml) so a mixin can
			// never supply it -- only an operator --set can.
			name:          "nvsentinel path outside the allowlist (tracing.endpoint) -> rejected",
			componentName: "nvsentinel",
			mixinOverrides: map[string]any{
				"global": map[string]any{"tracing": map[string]any{"endpoint": "sneaky.example:4317"}},
			},
			wantErr:         true,
			wantErrContains: "global.tracing.endpoint",
		},
		{
			name:          "exact collision with an existing (leaf or earlier-mixin) path -> rejected",
			componentName: "nvsentinel",
			mixinOverrides: map[string]any{
				"global": map[string]any{"tracing": map[string]any{"enabled": true}},
			},
			existingOverrides: map[string]any{
				"global": map[string]any{"tracing": map[string]any{"enabled": false}},
			},
			wantErr:         true,
			wantErrContains: "collides with path",
		},
		{
			// Ancestor collision: the leaf already set the whole
			// global.tracing subtree; a mixin setting a path underneath it
			// (global.tracing.enabled) collides even though the exact
			// strings differ.
			name:          "existing sets ancestor path, mixin sets descendant -> rejected",
			componentName: "nvsentinel",
			mixinOverrides: map[string]any{
				"global": map[string]any{"tracing": map[string]any{"enabled": true}},
			},
			existingOverrides: map[string]any{
				"global": map[string]any{"tracing": "not-actually-a-map-but-still-a-set-path"},
			},
			wantErr:         true,
			wantErrContains: "collides with path",
		},
		{
			name:          "component with no registry entry at all -> every path rejected",
			componentName: "not-a-real-component",
			mixinOverrides: map[string]any{
				"anything": true,
			},
			wantErr:         true,
			wantErrContains: "not in the component's registry-declared mixinSafeOverridePaths allowlist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var existingLayers []map[string]any
			if tt.existingOverrides != nil {
				existingLayers = []map[string]any{tt.existingOverrides}
			}
			err := mixinOverridesSafeForMerge(provider, "test-mixin", tt.componentName, tt.mixinOverrides, existingLayers)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Errorf("error = %v, want containing %q", err, tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

// TestMixinOverridesSafeForMerge_DisabledTargetDoesNotBlock covers the
// OCP-style shape (recipes/overlays/ocp.yaml sets nvsentinel's overrides to
// {enabled: false}): composing a mixin onto an already-disabled component
// is a no-op the recipe author should be warned about, not a hard error --
// disabling the target is a legitimate, deliberate chain decision this
// validation doesn't own.
func TestMixinOverridesSafeForMerge_DisabledTargetDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	disabledOverrides := map[string]any{"enabled": false}
	err = mixinOverridesSafeForMerge(store.provider, "nvsentinel-observability", "nvsentinel",
		map[string]any{"global": map[string]any{"tracing": map[string]any{"enabled": true}}},
		[]map[string]any{disabledOverrides})
	if err != nil {
		t.Fatalf("expected composing onto a disabled component to warn, not error, got: %v", err)
	}
}

// TestMixinNVSentinelObservability_ComposesCleanly proves the actual
// shipped mixin (recipes/mixins/nvsentinel-observability.yaml) composes
// onto an already-nvsentinel-chained leaf and produces exactly the
// documented audit-logging/tracing values, with global.tracing.endpoint
// left completely absent -- never a blank sentinel value, which would
// silently clobber a leaf's real endpoint on merge.
func TestMixinNVSentinelObservability_ComposesCleanly(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins["nvsentinel-observability"]; !ok {
		t.Fatalf("nvsentinel-observability mixin not present in metadata store; check recipes/mixins/nvsentinel-observability.yaml")
	}

	spec := RecipeMetadataSpec{
		Mixins: []string{"nvsentinel-observability"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.20.0", Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nvsentinel"},
		},
	}

	if _, err := store.mergeMixins(t.Context(), &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	var nvsentinel *ComponentRef
	for i := range spec.ComponentRefs {
		if spec.ComponentRefs[i].Name == "nvsentinel" {
			nvsentinel = &spec.ComponentRefs[i]
		}
	}
	if nvsentinel == nil {
		t.Fatal("nvsentinel component missing from merged spec")
	}

	global, _ := nvsentinel.Overrides["global"].(map[string]any)
	if global == nil {
		t.Fatal("overrides.global missing after mixin merge")
	}

	audit, _ := global["auditLogging"].(map[string]any)
	if audit == nil {
		t.Fatal("overrides.global.auditLogging missing")
	}
	wantAudit := map[string]any{
		"enabled":        true,
		"logRequestBody": false,
		"maxSizeMB":      100,
		"maxBackups":     7,
		"maxAgeDays":     30,
		"compress":       true,
	}
	for k, want := range wantAudit {
		if got := audit[k]; got != want {
			t.Errorf("auditLogging.%s = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}

	tracing, _ := global["tracing"].(map[string]any)
	if tracing == nil {
		t.Fatal("overrides.global.tracing missing")
	}
	if got := tracing["enabled"]; got != true {
		t.Errorf("tracing.enabled = %v, want true", got)
	}
	if got := tracing["insecure"]; got != false {
		t.Errorf("tracing.insecure = %v, want false", got)
	}
	if _, present := tracing["endpoint"]; present {
		t.Errorf("tracing.endpoint = %v, want absent -- endpoint must come only from an operator --set", tracing["endpoint"])
	}
}

// TestMixinNVSentinelObservability_RejectsLeafCollision proves a leaf that
// already configured tracing itself (e.g. because it composed the mixin's
// predecessor pattern, or set it directly) gets a hard compose-time error
// rather than having the mixin's value silently win via deepMergeMap's
// last-writer-wins semantics.
func TestMixinNVSentinelObservability_RejectsLeafCollision(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	spec := RecipeMetadataSpec{
		Mixins: []string{"nvsentinel-observability"},
		ComponentRefs: []ComponentRef{
			{
				Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.20.0", Source: "oci://ghcr.io/nvidia",
				Type: ComponentTypeHelm, Namespace: "nvsentinel",
				Overrides: map[string]any{
					"global": map[string]any{
						"tracing": map[string]any{
							"enabled":  true,
							"endpoint": "leaf-already-configured.example:4317",
						},
					},
				},
			},
		},
	}

	_, err = store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the collision on global.tracing.enabled, got nil")
	}
	if !strings.Contains(err.Error(), "collides with path") {
		t.Errorf("error = %v, want a collision message", err)
	}

	// The leaf's own value must be untouched -- confirms this fails BEFORE
	// any merge happens, not a partial merge that leaves a mix of old and
	// new state.
	nvsentinel := spec.ComponentRefs[0]
	global := nvsentinel.Overrides["global"].(map[string]any)
	tracing := global["tracing"].(map[string]any)
	if tracing["endpoint"] != "leaf-already-configured.example:4317" {
		t.Errorf("leaf's endpoint was mutated despite the rejected merge: %v", tracing["endpoint"])
	}
}

// TestMixinNVSentinelObservability_RejectsMultiMixinCollision proves two
// mixins touching the same path fail closed rather than the
// later-in-spec.mixins one silently winning. Modeled by listing the same
// mixin twice: mergeMixins folds each mixin's contribution into
// mergedSpec.ComponentRefs before the next runs (see mergeMixins), so a
// second application of any mixin touching the same paths exercises
// exactly the same "does this collide with what's already there" code path
// a second, genuinely different mixin would.
func TestMixinNVSentinelObservability_RejectsMultiMixinCollision(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	spec := RecipeMetadataSpec{
		Mixins: []string{"nvsentinel-observability", "nvsentinel-observability"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.20.0", Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nvsentinel"},
		},
	}

	_, err = store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the second mixin application as a collision, got nil")
	}
	if !strings.Contains(err.Error(), "collides with path") {
		t.Errorf("error = %v, want a collision message", err)
	}
}

// TestMixinOverridesSafeForMerge_RejectsEmptyMapAnywhere covers an empty
// map at any depth in a mixin's overrides: at an ancestor of an allowlisted
// path (global.auditLogging: {}, which fails the allowlist check since
// only its children are listed) and AT an allowlisted leaf path itself
// (global.auditLogging.enabled: {}, which matches the allowlist verbatim
// and would otherwise slip through, since deepMergeMap would then write a
// map where the chart expects a scalar). Both checked with no existing
// override at the target path, so there is nothing for the collision check
// to catch by coincidence -- the empty-map rejection has to do the work.
func TestMixinOverridesSafeForMerge_RejectsEmptyMapAnywhere(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	tests := []struct {
		name           string
		mixinOverrides map[string]any
	}{
		{
			name:           "empty map at an ancestor of an allowlisted path",
			mixinOverrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{}}},
		},
		{
			name:           "empty map at an allowlisted leaf path itself",
			mixinOverrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": map[string]any{}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mixinOverridesSafeForMerge(store.provider, "test-mixin", "nvsentinel", tt.mixinOverrides, nil)
			if err == nil {
				t.Fatal("expected the empty-map override to be rejected, got nil")
			}
			if !strings.Contains(err.Error(), "is an empty map") {
				t.Errorf("error = %v, want an empty-map rejection", err)
			}
		})
	}
}

// TestMergeMixins_RejectsDuplicateComponentRefNameWithinOneMixin covers a
// single mixin file declaring the same component name twice: each entry
// must not be validated only against the pre-mixin state (never against
// each other), since RecipeMetadataSpec.Merge would otherwise collapse
// them last-writer-wins with no further check.
func TestMergeMixins_RejectsDuplicateComponentRefNameWithinOneMixin(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	dup := &RecipeMixin{}
	dup.Metadata.Name = "test-duplicate-refs"
	dup.Spec.ComponentRefs = []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"tracing": map[string]any{"enabled": true}}}},
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"tracing": map[string]any{"enabled": false}}}},
	}
	// store is the process-wide sync.Once-cached singleton (loadMetadataStore) --
	// remove the synthetic entry after the test so it can't leak into other
	// tests or race with concurrent readers.
	store.Mixins["test-duplicate-refs"] = dup
	t.Cleanup(func() { delete(store.Mixins, "test-duplicate-refs") })

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-duplicate-refs"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.20.0", Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nvsentinel"},
		},
	}

	_, err = store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the duplicate componentRef name, got nil")
	}
	if !strings.Contains(err.Error(), "more than once in its own componentRefs list") {
		t.Errorf("error = %v, want a duplicate-name rejection", err)
	}
}

// TestMergeMixins_DetectsValuesFileCollision covers an existing ComponentRef
// that sets an allowlisted path via ValuesFile instead of inline Overrides:
// mixinOverridesSafeForMerge must still treat it as already set.
func TestMergeMixins_DetectsValuesFileCollision(t *testing.T) {
	store := newNvsentinelAllowlistStore("values-file-collision", []string{"global.auditLogging.enabled"}, map[string][]byte{
		"values/nvsentinel-existing.yaml": []byte(`global:
  auditLogging:
    enabled: false
`),
	})
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", ValuesFile: "values/nvsentinel-existing.yaml"},
		},
	}

	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the mixin override colliding with a ValuesFile-set path, got nil")
	}
	if !strings.Contains(err.Error(), "collides with path") {
		t.Errorf("error = %v, want a collision rejection", err)
	}
}

// TestMergeMixins_DetectsBaseValuesFileCollision covers the same collision
// but set in the component's implicit base values.yaml
// (components/<name>/values.yaml), not the overlay ValuesFile itself:
// resolveComponentValues loads base then overlays ValuesFile on top, so the
// collision check must see the merged result, not just the overlay file.
func TestMergeMixins_DetectsBaseValuesFileCollision(t *testing.T) {
	store := newNvsentinelAllowlistStore("base-values-file-collision", []string{"global.auditLogging.enabled"}, map[string][]byte{
		"components/nvsentinel/values.yaml": []byte(`global:
  auditLogging:
    enabled: false
`),
		"values/nvsentinel-overlay.yaml": []byte(`global:
  tracing:
    insecure: false
`),
	})
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", ValuesFile: "values/nvsentinel-overlay.yaml"},
		},
	}

	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the mixin override colliding with a base values.yaml path, got nil")
	}
	if !strings.Contains(err.Error(), "collides with path") {
		t.Errorf("error = %v, want a collision rejection", err)
	}
}

// TestMergeMixins_ValidatesOverridesOnNewlyIntroducedComponent covers a
// mixin that introduces a component NOT already in the recipe's chain:
// mergeMixins must still run mixinOverridesSafeForMerge on its Overrides.
// Without this, the existingComponents[c.Name] continue skips validation
// entirely, letting a mixin set a registered component's non-allowlisted
// path (e.g. nvsentinel's global.tracing.endpoint) just by being the first
// to introduce that component into a given chain.
func TestMergeMixins_ValidatesOverridesOnNewlyIntroducedComponent(t *testing.T) {
	store := newNvsentinelAllowlistStore("new-component-overrides", []string{"global.auditLogging.enabled"}, nil)
	addTestMixin(store, "test-mixin", []ComponentRef{
		{
			Name:   "nvsentinel",
			Chart:  "nvsentinel",
			Source: "oci://ghcr.io/nvidia",
			Type:   ComponentTypeHelm,
			Overrides: map[string]any{
				"global": map[string]any{"tracing": map[string]any{"endpoint": "sneaky.example:4317"}},
			},
		},
	})

	// nvsentinel is deliberately absent from ComponentRefs: the mixin is
	// the one introducing it.
	spec := RecipeMetadataSpec{
		Mixins:        []string{"test-mixin"},
		ComponentRefs: []ComponentRef{},
	}

	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject a non-allowlisted override on a newly-introduced component, got nil")
	}
	if !strings.Contains(err.Error(), "global.tracing.endpoint") {
		t.Errorf("error = %v, want rejection naming global.tracing.endpoint", err)
	}
}

// TestMergeMixins_DoesNotCorruptCachedMixinOnSecondMixin covers two mixins
// applied to the same recipe, where the first introduces a component fresh
// and the second sets an allowlisted override on that now-existing
// component. Merge's "new component from overlay" path stores the
// ComponentRef by value, aliasing its Overrides map with the cached mixin's
// own map (store.Mixins is process-wide cached, sync.Once). If the second
// mixin's merge writes into that aliased map, it permanently corrupts the
// first mixin's cached definition for every later build sharing this store.
func TestMergeMixins_DoesNotCorruptCachedMixinOnSecondMixin(t *testing.T) {
	store := newNvsentinelAllowlistStore("cache-corruption", []string{"global.auditLogging.enabled", "global.tracing.enabled"}, nil)
	addTestMixin(store, "introduces-nvsentinel", []ComponentRef{
		{
			Name:   "nvsentinel",
			Chart:  "nvsentinel",
			Source: "oci://ghcr.io/nvidia",
			Type:   ComponentTypeHelm,
			Overrides: map[string]any{
				"global": map[string]any{"auditLogging": map[string]any{"enabled": true}},
			},
		},
	})
	addTestMixin(store, "adds-tracing", []ComponentRef{
		{
			Name:      "nvsentinel",
			Overrides: map[string]any{"global": map[string]any{"tracing": map[string]any{"enabled": true}}},
		},
	})

	spec := RecipeMetadataSpec{
		Mixins:        []string{"introduces-nvsentinel", "adds-tracing"},
		ComponentRefs: []ComponentRef{},
	}
	if _, err := store.mergeMixins(t.Context(), &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	// The cached introducer mixin must be untouched by the second mixin's
	// merge: it should still declare only auditLogging.enabled.
	cachedOverrides := store.Mixins["introduces-nvsentinel"].Spec.ComponentRefs[0].Overrides
	global, _ := cachedOverrides["global"].(map[string]any)
	if _, hasTracing := global["tracing"]; hasTracing {
		t.Fatalf("cached mixin %q was mutated by a later mixin's merge: %+v", "introduces-nvsentinel", cachedOverrides)
	}
}

// TestMergeMixins_NullClearedPathStillCollides covers a leaf that
// explicitly clears an allowlisted path with YAML null, leaving a sibling
// key at the same parent untouched. resolveComponentValues' merged result
// can no longer see the cleared key (mergeValues deletes a nil-valued key
// outright), so collision detection must use the raw, unmerged layers
// instead -- otherwise a mixin can silently reinstate a value the leaf
// deliberately unset.
func TestMergeMixins_NullClearedPathStillCollides(t *testing.T) {
	store := newNvsentinelAllowlistStore("null-cleared-collision", []string{"global.auditLogging.enabled"}, map[string][]byte{
		"values/existing.yaml": []byte(`global:
  auditLogging:
    enabled: true
    maxSizeMB: 50
`),
	})
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{
				Name:       "nvsentinel",
				ValuesFile: "values/existing.yaml",
				Overrides:  map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": nil}}},
			},
		},
	}

	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the mixin override colliding with a leaf's explicit null, got nil")
	}
	if !strings.Contains(err.Error(), "collides with path") {
		t.Errorf("error = %v, want a collision rejection", err)
	}
}

// TestMergeMixins_UnrelatedDottedKeyDoesNotBreakComposition covers a
// component whose existing values contain an unrelated key with a literal
// dot (e.g. a podAnnotations entry, a common Helm/K8s pattern) elsewhere in
// the tree. Collision detection must never have to walk or validate that
// key: it only indexes the exact segments of the mixin's own allowlisted
// paths, so composition must succeed.
func TestMergeMixins_UnrelatedDottedKeyDoesNotBreakComposition(t *testing.T) {
	store := newNvsentinelAllowlistStore("dotted-key", []string{"global.auditLogging.enabled"}, map[string][]byte{
		"values/existing.yaml": []byte(`podAnnotations:
  example.com/key: value
`),
	})
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", ValuesFile: "values/existing.yaml"},
		},
	}

	if _, err := store.mergeMixins(t.Context(), &spec); err != nil {
		t.Fatalf("mergeMixins: %v, want success despite the unrelated dotted key", err)
	}
}

// TestMergeMixins_EmptyMapAncestorStillCollides covers a leaf that
// explicitly clears a whole subtree to {} (e.g. global.auditLogging: {}),
// then a mixin tries to populate a path underneath it
// (global.auditLogging.enabled). pathConfiguredInRaw must treat the empty
// map itself as a configured ancestor: deeper segments of the mixin's path
// cannot exist inside an empty map, so walking off the end of it must not
// be read as "not configured."
func TestMergeMixins_EmptyMapAncestorStillCollides(t *testing.T) {
	store := newNvsentinelAllowlistStore("empty-map-ancestor", []string{"global.auditLogging.enabled"}, nil)
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{
				Name:      "nvsentinel",
				Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{}}},
			},
		},
	}

	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to reject the mixin populating a path under a leaf's empty-map ancestor, got nil")
	}
	if !strings.Contains(err.Error(), "collides with path") {
		t.Errorf("error = %v, want a collision rejection", err)
	}
}

// TestMergeMixins_StructuralOnlyMixinSkipsValuesFileIO covers a mixin that
// only sets structural fields (Namespace, PreManifestFiles -- the os-talos
// shape) on an existing component with no Overrides at all. mergeMixins
// must not read that component's ValuesFile to build collision layers: an
// unrelated, unavailable/malformed values file must not be able to break
// composition for a mixin that never touches overrides.
func TestMergeMixins_StructuralOnlyMixinSkipsValuesFileIO(t *testing.T) {
	store := newNvsentinelAllowlistStore("structural-only", []string{"global.auditLogging.enabled"}, nil)
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Namespace: "privileged-nvsentinel"},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			// ValuesFile points at a path the provider doesn't have: if
			// mergeMixins tried to read it (it shouldn't, since the mixin
			// sets no Overrides), this would fail with a read error
			// instead of the success this test expects.
			{Name: "nvsentinel", ValuesFile: "values/does-not-exist.yaml"},
		},
	}

	if _, err := store.mergeMixins(t.Context(), &spec); err != nil {
		t.Fatalf("mergeMixins: %v, want success -- a structural-only mixin must not need to read the target's values", err)
	}
}

// TestMergeMixins_UnregisteredNewComponentOverridesStayFree covers a mixin
// introducing a component with NO registry entry at all: unlike a
// registered component (e.g. nvsentinel), there's no owner-declared
// allowlist to bypass, so the mixin keeps the pre-existing freedom to set
// any overrides. Only registered components are validated on introduction
// -- scoping the check to "registered" rather than "already in this
// chain" is what lets an unrelated, private mixin-introduced component
// stay override-free without every component needing a registry entry.
func TestMergeMixins_UnregisteredNewComponentOverridesStayFree(t *testing.T) {
	provider := newInMemoryProvider("unregistered-new-component", map[string][]byte{
		"registry.yaml": []byte("apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents: []\n"),
	})
	store := &MetadataStore{
		provider: provider,
		Mixins:   map[string]*RecipeMixin{},
	}
	addTestMixin(store, "test-mixin", []ComponentRef{
		{
			Name:      "my-addon",
			Chart:     "my-addon",
			Source:    "oci://ghcr.io/example",
			Type:      ComponentTypeHelm,
			Overrides: map[string]any{"replicaCount": 3},
		},
	})

	spec := RecipeMetadataSpec{
		Mixins:        []string{"test-mixin"},
		ComponentRefs: []ComponentRef{},
	}
	if _, err := store.mergeMixins(t.Context(), &spec); err != nil {
		t.Fatalf("mergeMixins: %v, want success -- an unregistered component has no allowlist to enforce", err)
	}
}

// TestMergeMixins_RejectsValuesFileOnNewRegisteredComponent covers a mixin
// introducing a registered component fresh, supplying a non-allowlisted
// value (nvsentinel's excluded global.tracing.endpoint) through ValuesFile
// instead of inline Overrides -- a side channel mixinOverridesSafeForMerge
// never inspects. Two shapes: ValuesFile alongside an allowlisted inline
// Overrides path, and ValuesFile with no inline Overrides at all (which
// would otherwise skip validation entirely via the len(c.Overrides)==0
// early continue).
func TestMergeMixins_RejectsValuesFileOnNewRegisteredComponent(t *testing.T) {
	registryFiles := map[string][]byte{
		"values/sneaky.yaml": []byte(`global:
  tracing:
    enabled: true
    endpoint: sneaky.example:4317
`),
	}
	allowlist := []string{"global.tracing.enabled"}

	t.Run("alongside allowlisted inline overrides", func(t *testing.T) {
		store := newNvsentinelAllowlistStore("valuesfile-with-overrides", allowlist, registryFiles)
		addTestMixin(store, "test-mixin", []ComponentRef{
			{
				Name:       "nvsentinel",
				Chart:      "nvsentinel",
				Source:     "oci://ghcr.io/nvidia",
				Type:       ComponentTypeHelm,
				ValuesFile: "values/sneaky.yaml",
				Overrides:  map[string]any{"global": map[string]any{"tracing": map[string]any{"enabled": true}}},
			},
		})
		spec := RecipeMetadataSpec{Mixins: []string{"test-mixin"}, ComponentRefs: []ComponentRef{}}
		_, err := store.mergeMixins(t.Context(), &spec)
		if err == nil {
			t.Fatal("expected mergeMixins to reject valuesFile on a newly-introduced registered component, got nil")
		}
		if !strings.Contains(err.Error(), "valuesFile") {
			t.Errorf("error = %v, want a valuesFile rejection", err)
		}
	})

	t.Run("valuesFile only, no inline overrides", func(t *testing.T) {
		store := newNvsentinelAllowlistStore("valuesfile-only", allowlist, registryFiles)
		addTestMixin(store, "test-mixin", []ComponentRef{
			{
				Name:       "nvsentinel",
				Chart:      "nvsentinel",
				Source:     "oci://ghcr.io/nvidia",
				Type:       ComponentTypeHelm,
				ValuesFile: "values/sneaky.yaml",
			},
		})
		spec := RecipeMetadataSpec{Mixins: []string{"test-mixin"}, ComponentRefs: []ComponentRef{}}
		_, err := store.mergeMixins(t.Context(), &spec)
		if err == nil {
			t.Fatal("expected mergeMixins to reject a valuesFile-only override on a newly-introduced registered component, got nil")
		}
		if !strings.Contains(err.Error(), "valuesFile") {
			t.Errorf("error = %v, want a valuesFile rejection", err)
		}
	})
}

// pathErrorProvider wraps a DataProvider and returns a fixed error for one
// specific path, delegating everything else -- used to test that
// existingRawOverrideLayers propagates a non-not-found read error instead
// of silently treating it as "no base layer."
type pathErrorProvider struct {
	delegate DataProvider
	failPath string
	failErr  error
}

func (p *pathErrorProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if path == p.failPath {
		return nil, p.failErr
	}
	return p.delegate.ReadFile(ctx, path)
}

func (p *pathErrorProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	return p.delegate.WalkDir(ctx, root, fn)
}

func (p *pathErrorProvider) Source(path string) string { return p.delegate.Source(path) }

// TestMergeMixins_PropagatesTransientBaseValuesReadError covers a
// non-not-found failure reading an existing component's implicit base
// values.yaml (e.g. a transient provider/NFS/permission error).
// existingRawOverrideLayers must propagate it, not silently treat it as
// "no base layer" -- otherwise a mixin could overwrite an allowlisted path
// the (unreadable) base file actually sets, since that layer would simply
// be missing from the collision check.
func TestMergeMixins_PropagatesTransientBaseValuesReadError(t *testing.T) {
	base := newNvsentinelAllowlistStore("transient-base-read-error", []string{"global.auditLogging.enabled"}, map[string][]byte{
		"values/overlay.yaml": []byte("global:\n  tracing:\n    insecure: false\n"),
	})
	store := &MetadataStore{
		provider: &pathErrorProvider{
			delegate: base.provider,
			failPath: "components/nvsentinel/values.yaml",
			failErr:  stderrors.New("connection reset by peer"),
		},
		Mixins: map[string]*RecipeMixin{},
	}
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", ValuesFile: "values/overlay.yaml"},
		},
	}
	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to propagate the transient base-values read error, got nil")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("error = %v, want it to wrap the underlying transient error", err)
	}
}

// TestMergeMixins_PreservesStructuredErrorCodeFromValuesRead covers a
// structured error (e.g. ErrCodeTimeout, as EmbeddedDataProvider.ReadFile
// returns on a canceled context) surfacing from a values-file read during
// collision-layer construction. It must reach the caller with its original
// code intact, not be flattened to ErrCodeInternal, so SDK/HTTP callers can
// distinguish a retryable timeout from a genuine internal failure.
func TestMergeMixins_PreservesStructuredErrorCodeFromValuesRead(t *testing.T) {
	base := newNvsentinelAllowlistStore("structured-error-propagation", []string{"global.auditLogging.enabled"}, nil)
	store := &MetadataStore{
		provider: &pathErrorProvider{
			delegate: base.provider,
			failPath: "values/overlay.yaml",
			failErr:  aicrerrors.New(aicrerrors.ErrCodeTimeout, "context canceled before reading"),
		},
		Mixins: map[string]*RecipeMixin{},
	}
	addTestMixin(store, "test-mixin", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-mixin"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", ValuesFile: "values/overlay.yaml"},
		},
	}
	_, err := store.mergeMixins(t.Context(), &spec)
	if err == nil {
		t.Fatal("expected mergeMixins to propagate the values-file read error, got nil")
	}
	// Checked on the OUTERMOST error (err itself, not something further
	// down its Unwrap chain), not via errors.Is: errors.Is would still find
	// ErrCodeTimeout by walking the chain even if Wrap had demoted it to
	// Cause under a new ErrCodeInternal wrapper -- callers that read the
	// top-level code directly (e.g. an HTTP status mapper) only see this
	// level.
	se, ok := stderrors.AsType[*aicrerrors.StructuredError](err)
	if !ok {
		t.Fatalf("error = %#v, want a top-level *StructuredError", err)
	}
	if se.Code != aicrerrors.ErrCodeTimeout {
		t.Errorf("top-level code = %v, want ErrCodeTimeout preserved, not flattened to ErrCodeInternal", se.Code)
	}
}
