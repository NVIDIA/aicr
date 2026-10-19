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
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestPrometheusOperatorAppVersionLockstep ensures built-in recipes pair the
// stack and CRDs charts from the same Prometheus Operator release. Their chart
// versions are independent, so compatibility is matched by appVersion. See #2792.
func TestPrometheusOperatorAppVersionLockstep(t *testing.T) {
	const (
		stackComponent = "kube-prometheus-stack"
		crdsComponent  = "prometheus-operator-crds"
	)

	// Chart.yaml appVersion for every chart pin used by a built-in recipe.
	auditedAppVersions := map[string]map[string]string{
		stackComponent: {
			"83.7.0": "v0.90.1",
			"84.4.0": "v0.90.1",
		},
		crdsComponent: {
			"28.0.1": "v0.90.1",
		},
	}

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

	type chartVersionPair struct {
		stackVersion string
		crdsVersion  string
	}
	overlaysByVersionPair := make(map[chartVersionPair][]string)
	for _, overlayName := range overlayNames {
		result, err := store.BuildRecipeResult(ctx, store.Overlays[overlayName].Spec.Criteria)
		if err != nil {
			t.Fatalf("BuildRecipeResult(%s): %v", overlayName, err)
		}
		stackRef := result.GetComponentRef(stackComponent)
		crdsRef := result.GetComponentRef(crdsComponent)
		if stackRef == nil || crdsRef == nil || !stackRef.IsEnabled() || !crdsRef.IsEnabled() {
			continue
		}
		versions := chartVersionPair{stackVersion: stackRef.Version, crdsVersion: crdsRef.Version}
		overlaysByVersionPair[versions] = append(overlaysByVersionPair[versions], overlayName)
	}
	if len(overlaysByVersionPair) == 0 {
		t.Fatalf("no resolved recipe installs both %s and %s; this guard would be vacuous",
			stackComponent, crdsComponent)
	}

	usedChartVersions := map[string]map[string]bool{stackComponent: {}, crdsComponent: {}}
	lookupAppVersion := func(componentName, chartVersion string, overlayNames []string) (string, bool) {
		usedChartVersions[componentName][chartVersion] = true
		appVersion, ok := auditedAppVersions[componentName][chartVersion]
		if !ok {
			t.Errorf("%s %s (installed by %s) has no audited appVersion; add it to auditedAppVersions",
				componentName, chartVersion, summarizeOverlayNames(overlayNames))
		}
		return appVersion, ok
	}

	for versions, overlayNames := range overlaysByVersionPair {
		stackAppVersion, stackRecorded := lookupAppVersion(
			stackComponent, versions.stackVersion, overlayNames)
		crdsAppVersion, crdsRecorded := lookupAppVersion(
			crdsComponent, versions.crdsVersion, overlayNames)
		if !stackRecorded || !crdsRecorded {
			continue
		}
		if strings.TrimPrefix(stackAppVersion, "v") != strings.TrimPrefix(crdsAppVersion, "v") {
			t.Errorf("%s install %s %s (appVersion %s) with %s %s (appVersion %s); "+
				"bump both charts to the same Prometheus Operator release",
				summarizeOverlayNames(overlayNames),
				stackComponent, versions.stackVersion, stackAppVersion,
				crdsComponent, versions.crdsVersion, crdsAppVersion)
		}
	}

	for componentName, appVersions := range auditedAppVersions {
		for chartVersion := range appVersions {
			if !usedChartVersions[componentName][chartVersion] {
				t.Errorf("auditedAppVersions entry %s %s is installed by no recipe; delete it",
					componentName, chartVersion)
			}
		}
	}
}

func summarizeOverlayNames(overlayNames []string) string {
	const maxListedOverlays = 3
	if len(overlayNames) <= maxListedOverlays {
		return strings.Join(overlayNames, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(overlayNames[:maxListedOverlays], ", "), len(overlayNames)-maxListedOverlays)
}
