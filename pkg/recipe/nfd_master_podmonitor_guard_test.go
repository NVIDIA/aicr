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
	"sort"
	"testing"
)

// TestNFDMasterPodMonitorNeedsCRDEdge ensures every built-in recipe that
// ships the nfd-master PodMonitor manifest also installs
// prometheus-operator-crds and orders nfd after it. Without the direct edge
// the two components share a sync band and the PodMonitor apply races the
// CRD (issue #928). See #3265.
func TestNFDMasterPodMonitorNeedsCRDEdge(t *testing.T) {
	const (
		nfdComponent       = "nfd"
		crdsComponent      = "prometheus-operator-crds"
		podMonitorManifest = "components/nfd/manifests/master-podmonitor.yaml"
	)

	ctx := t.Context()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	overlayNames := make([]string, 0, len(store.Overlays))
	for overlayName, overlay := range store.Overlays {
		if overlay.Spec.Criteria != nil {
			overlayNames = append(overlayNames, overlayName)
		}
	}
	sort.Strings(overlayNames)

	shipped := 0
	for _, overlayName := range overlayNames {
		result, err := store.BuildRecipeResult(ctx, store.Overlays[overlayName].Spec.Criteria)
		if err != nil {
			t.Fatalf("BuildRecipeResult(%s): %v", overlayName, err)
		}
		nfdRef := result.GetComponentRef(nfdComponent)
		if nfdRef == nil || !nfdRef.IsEnabled() || !slices.Contains(nfdRef.ManifestFiles, podMonitorManifest) {
			continue
		}
		shipped++
		if !slices.Contains(nfdRef.DependencyRefs, crdsComponent) {
			t.Errorf("%s: nfd ships %s but does not list %s in dependencyRefs",
				overlayName, podMonitorManifest, crdsComponent)
		}
		crdsRef := result.GetComponentRef(crdsComponent)
		if crdsRef == nil || !crdsRef.IsEnabled() {
			t.Errorf("%s: nfd ships %s but %s is not installed; the PodMonitor apply would fail",
				overlayName, podMonitorManifest, crdsComponent)
		}
	}
	if shipped == 0 {
		t.Fatalf("no resolved recipe ships %s; this guard would be vacuous", podMonitorManifest)
	}
}
