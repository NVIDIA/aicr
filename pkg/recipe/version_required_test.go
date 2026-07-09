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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// These tests pin the #1615 invariant: an ENABLED Helm componentRef that is
// not manifest-only must carry a chart version, enforced at ValidateCoherence
// — the shared boundary all three RecipeResult entry paths converge on:
//
//   - criteria resolution (finalizeRecipeResult, exercised via an external
//     registry whose Helm component omits defaultVersion; the embedded
//     registry is guarded separately by bom-pinning-check),
//   - file load (LoadFromFileWithProvider -> PrepareAndValidate), and
//   - adoption (client adoptRecipe -> PrepareAndValidate, exercised here at
//     the PrepareAndValidate boundary the client calls).
//
// Without the check, helmfile/flux/argocd emit the empty version verbatim and
// Helm resolves "latest" at install time — a silent stale-default failure.

// wantEmptyVersionError asserts the #1615 rejection shape: an
// ErrCodeInvalidRequest that names the component and the missing version.
func wantEmptyVersionError(t *testing.T, err error, component string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error for Helm component %q without a chart version, got nil", component)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %v", err, errors.ErrCodeInvalidRequest)
	}
	for _, want := range []string{component, "chart version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestResolveRejectsEmptyHelmVersion_ExternalRegistry drives the criteria-
// resolution path against an external-style registry whose Helm component
// declares a repository and chart but no defaultVersion, and whose
// componentRef pins nothing: the resolved ref would reach the deployers with
// Version == "", so resolution must fail closed.
func TestResolveRejectsEmptyHelmVersion_ExternalRegistry(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	provider := newInMemoryProvider("external-unpinned", map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/unpinned-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: unpinned-leaf
spec:
  criteria:
    service: eks
  componentRefs:
    - name: unpinned-helm
`),
		"registry.yaml": []byte(`apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components:
  - name: unpinned-helm
    displayName: Unpinned Helm Component
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/unpinned-helm
`),
	})

	ctx := context.Background()
	// Loading/building also populates the component- and criteria-registry
	// caches keyed by provider; evict all three so no package-global state
	// leaks past this test.
	t.Cleanup(func() {
		EvictCachedStore(provider)
		EvictCachedRegistry(provider)
		EvictCachedCriteriaRegistry(provider)
	})
	store, err := LoadMetadataStoreFor(ctx, provider)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	overlay, ok := store.GetRecipeByName("unpinned-leaf")
	if !ok {
		t.Fatal("unpinned-leaf overlay not found in store")
	}

	_, err = store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
	wantEmptyVersionError(t, err, "unpinned-helm")

	// The same registry with an overlay-level version pin resolves cleanly:
	// the invariant demands a version, not a registry default specifically.
	provider2 := newInMemoryProvider("external-pinned", map[string][]byte{
		"overlays/base.yaml": provider.files["overlays/base.yaml"],
		"registry.yaml":      provider.files["registry.yaml"],
		"overlays/pinned-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: pinned-leaf
spec:
  criteria:
    service: eks
  componentRefs:
    - name: unpinned-helm
      version: "1.2.3"
`),
	})
	t.Cleanup(func() {
		EvictCachedStore(provider2)
		EvictCachedRegistry(provider2)
		EvictCachedCriteriaRegistry(provider2)
	})
	store2, err := LoadMetadataStoreFor(ctx, provider2)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor(pinned): %v", err)
	}
	pinned, ok := store2.GetRecipeByName("pinned-leaf")
	if !ok {
		t.Fatal("pinned-leaf overlay not found in store")
	}
	result, err := store2.BuildRecipeResult(ctx, pinned.Spec.Criteria)
	if err != nil {
		t.Fatalf("BuildRecipeResult(pinned): %v", err)
	}
	if ref := result.GetComponentRef("unpinned-helm"); ref == nil || ref.Version != "1.2.3" {
		t.Errorf("pinned ref = %+v, want version 1.2.3", ref)
	}

	// A whitespace-only defaultVersion is treated as populated by
	// ApplyRegistryDefaults but trims to empty in Helm — it must be rejected
	// the same as an omitted one.
	provider3 := newInMemoryProvider("external-whitespace", map[string][]byte{
		"overlays/base.yaml":          provider.files["overlays/base.yaml"],
		"overlays/unpinned-leaf.yaml": provider.files["overlays/unpinned-leaf.yaml"],
		"registry.yaml": []byte(`apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components:
  - name: unpinned-helm
    displayName: Unpinned Helm Component
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/unpinned-helm
      defaultVersion: "   "
`),
	})
	t.Cleanup(func() {
		EvictCachedStore(provider3)
		EvictCachedRegistry(provider3)
		EvictCachedCriteriaRegistry(provider3)
	})
	store3, err := LoadMetadataStoreFor(ctx, provider3)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor(whitespace): %v", err)
	}
	wsLeaf, ok := store3.GetRecipeByName("unpinned-leaf")
	if !ok {
		t.Fatal("unpinned-leaf overlay not found in whitespace store")
	}
	_, err = store3.BuildRecipeResult(ctx, wsLeaf.Spec.Criteria)
	wantEmptyVersionError(t, err, "unpinned-helm")
}

// TestLoadRejectsEmptyHelmVersion drives the file-load path: a hand-authored
// hydrated RecipeResult with an enabled chart-referencing Helm ref and no
// version bypasses ApplyRegistryDefaults entirely, so PrepareAndValidate (via
// LoadFromFileWithProvider) must reject it.
func TestLoadRejectsEmptyHelmVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "recipe.yaml")
	content := `kind: RecipeResult
apiVersion: aicr.run/v1alpha2
criteria:
  service: eks
componentRefs:
  - name: hand-authored-helm
    type: Helm
    source: https://charts.example.com
    chart: hand-authored-helm
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadFromFileWithProvider(t.Context(), path, "", "test", nil)
	wantEmptyVersionError(t, err, "hand-authored-helm")

	// Whitespace-only is empty: Helm trims the argument and installs latest.
	wsPath := filepath.Join(dir, "recipe-ws.yaml")
	wsContent := content + "    version: \"   \"\n"
	if writeErr := os.WriteFile(wsPath, []byte(wsContent), 0o600); writeErr != nil {
		t.Fatalf("WriteFile: %v", writeErr)
	}
	_, err = LoadFromFileWithProvider(t.Context(), wsPath, "", "test", nil)
	wantEmptyVersionError(t, err, "hand-authored-helm")
}

// TestPrepareAndValidateRejectsEmptyHelmVersion pins the adopt boundary:
// client adoptRecipe funnels externally-supplied RecipeResults through
// PrepareAndValidate, which must reject an enabled chart-referencing Helm ref
// without a version — and must keep accepting the shapes that legitimately
// carry none.
func TestPrepareAndValidateRejectsEmptyHelmVersion(t *testing.T) {
	chartRef := func(version string) ComponentRef {
		return ComponentRef{
			Name:    "adopted-helm",
			Type:    ComponentTypeHelm,
			Source:  "https://charts.example.com",
			Chart:   "adopted-helm",
			Version: version,
		}
	}

	tests := []struct {
		name    string
		refs    []ComponentRef
		wantErr bool
		wantMsg string // substring the error must contain ("" = the version message)
	}{
		{name: "chart ref without version rejected", refs: []ComponentRef{chartRef("")}, wantErr: true},
		{
			// Whitespace-only is empty: Helm trims the argument and installs
			// latest, the exact silent failure the invariant targets.
			name: "chart ref with whitespace-only version rejected", refs: []ComponentRef{chartRef("   ")},
			wantErr: true,
		},
		{
			// A PADDED version diverges between the trimmed validator view
			// and the deployers' raw view: NormalizeVersion only strips a
			// "v" prefix, so " 1.0.0" lands verbatim in a Flux HelmRelease
			// (a broken semver range) and in helm --version arguments.
			name: "chart ref with padded version rejected", refs: []ComponentRef{chartRef(" 1.0.0")},
			wantErr: true, wantMsg: "surrounding whitespace",
		},
		{
			name: "chart ref with trailing-space version rejected", refs: []ComponentRef{chartRef("1.0.0 ")},
			wantErr: true, wantMsg: "surrounding whitespace",
		},
		{name: "chart ref with version accepted", refs: []ComponentRef{chartRef("1.0.0")}, wantErr: false},
		{name: "chart ref with v-prefixed version accepted", refs: []ComponentRef{chartRef("v1.0.0")}, wantErr: false},
		{
			// The Flux generator strips a leading "v" and omits an empty
			// remainder — a literal "v" is effectively no version at all.
			name: "chart ref with literal v version rejected", refs: []ComponentRef{chartRef("v")},
			wantErr: true, wantMsg: "normalize",
		},
		{
			// A chart name with no source is undeployable: Flux skips
			// HelmRepository creation for an empty source, and localformat's
			// chart pull rejects a missing repository.
			name: "chart without source rejected",
			refs: []ComponentRef{{
				Name: "chart-only", Type: ComponentTypeHelm, Chart: "chart-only", Version: "1.0.0",
			}},
			wantErr: true, wantMsg: "no source repository",
		},
		{
			// A source with no chart name is a long-standing deployable
			// shape: the deployers fall back to the component name for the
			// chart (localformat renderInputFor; the flux generator gained
			// the same fallback).
			name: "source without chart accepted",
			refs: []ComponentRef{{
				Name: "source-only", Type: ComponentTypeHelm,
				Source: "https://charts.example.com", Version: "1.0.0",
			}},
			wantErr: false,
		},
		{
			// A bare "v" is rejected for OCI sources too: the tag pulls
			// literally, but vendored wrappers normalize it to a fabricated
			// "0.1.0" — one recipe, two conflicting chart identities.
			name: "oci source with bare v tag rejected",
			refs: []ComponentRef{{
				Name: "oci-comp", Type: ComponentTypeHelm, Chart: "oci-comp",
				Source: "oci://registry.example.com/charts", Version: "v",
			}},
			wantErr: true, wantMsg: "normalize",
		},
		{
			// v-prefixed full versions stay valid on OCI sources.
			name: "oci source with v-prefixed version accepted",
			refs: []ComponentRef{{
				Name: "oci-comp", Type: ComponentTypeHelm, Chart: "oci-comp",
				Source: "oci://registry.example.com/charts", Version: "v1.2.3",
			}},
			wantErr: false,
		},
		{
			// Whitespace-only chart/source are rejected outright: the
			// deployers compare these fields raw, so a whitespace value would
			// validate as manifest-only here but deploy as a broken external
			// chart there.
			name: "whitespace-only chart rejected",
			refs: []ComponentRef{{
				Name: "ws-chart", Type: ComponentTypeHelm, Chart: "   ",
				ManifestFiles: []string{"components/ws-chart/manifests/a.yaml"},
			}},
			wantErr: true, wantMsg: "surrounding whitespace",
		},
		{
			name: "whitespace-only source rejected",
			refs: []ComponentRef{{
				Name: "ws-source", Type: ComponentTypeHelm, Source: "  ", Version: "1.0.0",
			}},
			wantErr: true, wantMsg: "surrounding whitespace",
		},
		{
			// A PADDED (not merely whitespace-only) source diverges between
			// the trimmed validator view and the deployers' raw view — e.g.
			// " oci://…" would be graded OCI here but non-OCI by flux.
			name: "padded oci source rejected",
			refs: []ComponentRef{{
				Name: "padded-oci", Type: ComponentTypeHelm,
				Source: " oci://registry.example.com/charts", Version: "v",
			}},
			wantErr: true, wantMsg: "surrounding whitespace",
		},
		{
			// A husk (no chart, no source, no manifests) references nothing
			// deployable, version or not; fail closed.
			name: "empty husk rejected", refs: []ComponentRef{{Name: "husk", Type: ComponentTypeHelm}},
			wantErr: true, wantMsg: "no deployable primary",
		},
		{
			name:    "versioned husk rejected",
			refs:    []ComponentRef{{Name: "husk", Type: ComponentTypeHelm, Version: "1.0.0"}},
			wantErr: true, wantMsg: "no deployable primary",
		},
		{
			// Manifest-only Helm refs (e.g. nodewright-customizations) have no
			// chart version to require.
			name: "manifest-only helm accepted",
			refs: []ComponentRef{{
				Name: "nodewright-customizations", Type: ComponentTypeHelm,
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning.yaml"},
			}},
			wantErr: false,
		},
		{
			// Manifest-only refs may set a version (it lands in the rendered
			// chart's .Chart.Version / helm.sh/chart label).
			name: "manifest-only helm with version accepted",
			refs: []ComponentRef{{
				Name: "nodewright-customizations", Type: ComponentTypeHelm, Version: "1.0.0",
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning.yaml"},
			}},
			wantErr: false,
		},
		{
			// A PADDED version is rejected on manifest-only refs too:
			// localformat's renderInputFor propagates it raw into
			// .Chart.Version, and the manifests bake it into the
			// helm.sh/chart label — an invalid label value.
			name: "manifest-only helm with padded version rejected",
			refs: []ComponentRef{{
				Name: "nodewright-customizations", Type: ComponentTypeHelm, Version: " 1.0.0 ",
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning.yaml"},
			}},
			wantErr: true, wantMsg: "surrounding whitespace",
		},
		{
			// A bare "v" on a manifest-only ref is fabricated into "0.1.0"
			// by NormalizeVersionWithDefault — reject the degenerate value
			// rather than ship a version nobody set.
			name: "manifest-only helm with bare v version rejected",
			refs: []ComponentRef{{
				Name: "nodewright-customizations", Type: ComponentTypeHelm, Version: "v",
				ManifestFiles: []string{"components/nodewright-customizations/manifests/tuning.yaml"},
			}},
			wantErr: true, wantMsg: "normalize",
		},
		{
			// Pre-manifests are auxiliary to a primary release; a ref whose
			// only content is pre-manifests has no deployable primary and
			// must NOT ride the manifest-only exemption.
			name: "pre-manifest-only helm without version rejected",
			refs: []ComponentRef{{
				Name: "quota-only", Type: ComponentTypeHelm,
				PreManifestFiles: []string{"components/quota-only/manifests/quota.yaml"},
			}},
			wantErr: true, wantMsg: "no deployable primary",
		},
		{
			// Supplying a version does not make a pre-only ref deployable:
			// localformat would classify it as an upstream release with no
			// chart to pull.
			name: "pre-manifest-only helm with version rejected",
			refs: []ComponentRef{{
				Name: "quota-only", Type: ComponentTypeHelm, Version: "1.0.0",
				PreManifestFiles: []string{"components/quota-only/manifests/quota.yaml"},
			}},
			wantErr: true, wantMsg: "no deployable primary",
		},
		{
			// A primary-manifest ref that also carries pre-manifests keeps
			// the exemption — the primary content is what qualifies it.
			name: "manifest-only with pre-manifests accepted",
			refs: []ComponentRef{{
				Name: "nodewright-customizations", Type: ComponentTypeHelm,
				ManifestFiles:    []string{"components/nodewright-customizations/manifests/tuning.yaml"},
				PreManifestFiles: []string{"components/nodewright-customizations/manifests/ns.yaml"},
			}},
			wantErr: false,
		},
		{
			// Disabled refs are excluded from the bundle; their shape never
			// reaches a deployer.
			name: "disabled chart ref without version accepted",
			refs: []ComponentRef{{
				Name: "off", Type: ComponentTypeHelm, Chart: "off",
				Overrides: map[string]any{"enabled": false},
			}},
			wantErr: false,
		},
		{
			// Lowercase type is backward-compatible input and must not dodge
			// the version requirement.
			name: "lowercase helm chart ref without version rejected",
			refs: []ComponentRef{{
				Name: "adopted-helm", Type: ComponentType("helm"),
				Source: "https://charts.example.com", Chart: "adopted-helm",
			}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RecipeResult{ComponentRefs: tt.refs}
			err := r.PrepareAndValidate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("PrepareAndValidate() error = %v, want nil", err)
				}
				return
			}
			if tt.wantMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantMsg)
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want %v", err, errors.ErrCodeInvalidRequest)
				}
				return
			}
			wantEmptyVersionError(t, err, tt.refs[0].Name)
		})
	}
}

// TestIsManifestOnlyHelm pins the predicate directly — in particular the
// Helm-type guard, whose only production caller (pkg/health) pre-filters by
// type, so this is the coverage that keeps the exported name honest.
func TestIsManifestOnlyHelm(t *testing.T) {
	manifests := []string{"components/x/manifests/a.yaml"}
	tests := []struct {
		name string
		ref  ComponentRef
		want bool
	}{
		{"canonical helm manifest-only", ComponentRef{Type: ComponentTypeHelm, ManifestFiles: manifests}, true},
		{"lowercase helm manifest-only", ComponentRef{Type: ComponentType("helm"), ManifestFiles: manifests}, true},
		{"kustomize with manifests is not manifest-only helm", ComponentRef{Type: ComponentTypeKustomize, ManifestFiles: manifests}, false},
		{"unknown type with manifests is not manifest-only helm", ComponentRef{Type: ComponentType("flux"), ManifestFiles: manifests}, false},
		{"empty type with manifests is not manifest-only helm", ComponentRef{ManifestFiles: manifests}, false},
		{"helm with chart is not manifest-only", ComponentRef{Type: ComponentTypeHelm, Chart: "c", ManifestFiles: manifests}, false},
		{"helm with source is not manifest-only", ComponentRef{Type: ComponentTypeHelm, Source: "https://r", ManifestFiles: manifests}, false},
		{"helm without manifests is not manifest-only", ComponentRef{Type: ComponentTypeHelm}, false},
		{"helm with only pre-manifests is not manifest-only", ComponentRef{Type: ComponentTypeHelm, PreManifestFiles: manifests}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.IsManifestOnlyHelm(); got != tt.want {
				t.Errorf("IsManifestOnlyHelm() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestComponentRefChartHelpers pins the exported chart-shape helpers directly:
// HasExternalChart requires a Helm type AND a source (a chart name alone
// references nothing pullable), and EffectiveChart falls back to the
// component name exactly when Chart is unset.
func TestComponentRefChartHelpers(t *testing.T) {
	tests := []struct {
		name          string
		ref           ComponentRef
		wantExternal  bool
		wantEffective string
	}{
		{"source and chart", ComponentRef{Name: "n", Type: ComponentTypeHelm, Source: "https://r", Chart: "c"}, true, "c"},
		{"source only falls back", ComponentRef{Name: "n", Type: ComponentTypeHelm, Source: "https://r"}, true, "n"},
		{"lowercase helm source only", ComponentRef{Name: "n", Type: ComponentType("helm"), Source: "https://r"}, true, "n"},
		{"chart only does not qualify", ComponentRef{Name: "n", Type: ComponentTypeHelm, Chart: "c"}, false, "c"},
		{"neither does not qualify", ComponentRef{Name: "n", Type: ComponentTypeHelm}, false, "n"},
		{"kustomize with source does not qualify", ComponentRef{Name: "n", Type: ComponentTypeKustomize, Source: "git://x"}, false, "n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.HasExternalChart(); got != tt.wantExternal {
				t.Errorf("HasExternalChart() = %v, want %v", got, tt.wantExternal)
			}
			if got := tt.ref.EffectiveChart(); got != tt.wantEffective {
				t.Errorf("EffectiveChart() = %q, want %q", got, tt.wantEffective)
			}
		})
	}
}
