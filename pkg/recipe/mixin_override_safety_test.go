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
	"strings"
	"testing"
)

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
			err := mixinOverridesSafeForMerge(provider, "test-mixin", tt.componentName, tt.mixinOverrides, tt.existingOverrides)
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

	err = mixinOverridesSafeForMerge(store.provider, "nvsentinel-observability", "nvsentinel",
		map[string]any{"global": map[string]any{"tracing": map[string]any{"enabled": true}}},
		map[string]any{"enabled": false})
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
	provider := newInMemoryProvider("values-file-collision", map[string][]byte{
		"registry.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: nvsentinel
    displayName: NVSentinel
    mixinSafeOverridePaths:
      - global.auditLogging.enabled
`),
		"values/nvsentinel-existing.yaml": []byte(`global:
  auditLogging:
    enabled: false
`),
	})

	store := &MetadataStore{
		provider: provider,
		Mixins:   map[string]*RecipeMixin{},
	}
	mixin := &RecipeMixin{}
	mixin.Metadata.Name = "test-mixin"
	mixin.Spec.ComponentRefs = []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	}
	store.Mixins["test-mixin"] = mixin

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
	provider := newInMemoryProvider("base-values-file-collision", map[string][]byte{
		"registry.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: nvsentinel
    displayName: NVSentinel
    mixinSafeOverridePaths:
      - global.auditLogging.enabled
`),
		"components/nvsentinel/values.yaml": []byte(`global:
  auditLogging:
    enabled: false
`),
		"values/nvsentinel-overlay.yaml": []byte(`global:
  tracing:
    insecure: false
`),
	})

	store := &MetadataStore{
		provider: provider,
		Mixins:   map[string]*RecipeMixin{},
	}
	mixin := &RecipeMixin{}
	mixin.Metadata.Name = "test-mixin"
	mixin.Spec.ComponentRefs = []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{"global": map[string]any{"auditLogging": map[string]any{"enabled": true}}}},
	}
	store.Mixins["test-mixin"] = mixin

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
