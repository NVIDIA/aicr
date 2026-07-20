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
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests pin both sides of the version-precedence contract decided in
// issue #1616 (default-equal overlay pins removed; registry defaultVersion is
// the single fallback):
//
//   - (a) a componentRef with NO version pin resolves to the registry
//     default, so an external --data registry that overrides defaultVersion
//     takes effect for every overlay that does not intentionally diverge; and
//   - (b) an explicit componentRef pin (an exemption-backed intentional
//     divergence, see versionPinExemptions) still wins over an external
//     registry default.
//
// Before #1616, embedded overlays carried pins that exactly equaled the
// registry default; those pins accidentally shielded leaf resolution from
// external registry overrides because ApplyRegistryDefaults fills Version
// only when it is empty. The sweep made side (a) reachable; these tests keep
// both sides from regressing.

// writeExternalRegistryOverride builds an external-registry fixture directory
// whose registry.yaml overrides exactly the named embedded components'
// helm.defaultVersion. Each override entry is a full copy of the embedded
// ComponentConfig (mergeByName replaces entries wholesale, so a partial entry
// would drop the repository/chart coordinates and change more than the
// version).
func writeExternalRegistryOverride(t *testing.T, overrides map[string]string) string {
	t.Helper()

	embedded, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	ext := ComponentRegistry{
		APIVersion: embedded.APIVersion,
		Kind:       embedded.Kind,
	}
	for name, version := range overrides {
		cfg := embedded.Get(name)
		if cfg == nil {
			t.Fatalf("component %q not found in embedded registry", name)
		}
		override := *cfg
		override.Helm.DefaultVersion = version
		ext.Components = append(ext.Components, override)
	}

	data, err := yaml.Marshal(&ext)
	if err != nil {
		t.Fatalf("marshal external registry: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), data, 0o600); err != nil {
		t.Fatalf("write external registry.yaml: %v", err)
	}
	return dir
}

// resolveWithExternalRegistry resolves the named embedded overlay's criteria
// through a LayeredDataProvider whose external dir is externalDir, and
// returns the resolved ref for component.
func resolveWithExternalRegistry(t *testing.T, externalDir, overlayName, component string) *ComponentRef {
	t.Helper()

	layered, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), "."), LayeredProviderConfig{
		ExternalDir: externalDir,
	})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	t.Cleanup(func() {
		EvictCachedStore(layered)
		EvictCachedRegistry(layered)
		EvictCachedCriteriaRegistry(layered)
	})

	ctx := context.Background()
	store, err := LoadMetadataStoreFor(ctx, layered)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	overlay, ok := store.GetRecipeByName(overlayName)
	if !ok {
		t.Fatalf("overlay %q not found in store", overlayName)
	}
	result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
	if err != nil {
		t.Fatalf("BuildRecipeResult(%s): %v", overlayName, err)
	}
	ref := result.GetComponentRef(component)
	if ref == nil {
		t.Fatalf("component %q not present in resolved %s recipe", component, overlayName)
	}
	return ref
}

// TestExternalRegistryDefaultWinsWithoutPin pins side (a): cert-manager
// carries no version pin in any base/overlay/mixin (post-#1616), so an
// external registry override of its defaultVersion must reach the resolved
// leaf.
func TestExternalRegistryDefaultWinsWithoutPin(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	const externalVersion = "v99.9.9-external"
	dir := writeExternalRegistryOverride(t, map[string]string{
		"cert-manager": externalVersion,
	})

	ref := resolveWithExternalRegistry(t, dir, "a100-eks-ubuntu-training", "cert-manager")
	if ref.Version != externalVersion {
		t.Errorf("cert-manager resolved version = %q, want external registry default %q; "+
			"an unpinned componentRef must inherit the merged (external-over-embedded) "+
			"registry default", ref.Version, externalVersion)
	}
}

// TestExplicitPinWinsOverExternalDefault pins side (b): the aks overlay's
// kube-prometheus-stack pin is the one sanctioned divergence
// (versionPinExemptions, #700) — it must survive an external registry
// override of the component's defaultVersion.
func TestExplicitPinWinsOverExternalDefault(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	// The pin this test protects; keep in lockstep with the aks overlay and
	// its versionPinExemptions entry.
	const aksPin = "83.7.0"

	dir := writeExternalRegistryOverride(t, map[string]string{
		"kube-prometheus-stack": "99.0.0",
	})

	ref := resolveWithExternalRegistry(t, dir, "a100-aks-ubuntu-training", "kube-prometheus-stack")
	if ref.Version != aksPin {
		t.Errorf("kube-prometheus-stack resolved version = %q, want the aks overlay's "+
			"explicit pin %q; an intentional (exempted) pin must win over an external "+
			"registry defaultVersion", ref.Version, aksPin)
	}
}
