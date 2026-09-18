// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// untrackedComponents lists components with no upstream chart to track, each
// with the reason it cannot be covered. An entry that stops applying (the
// component gains a defaultRepository, or disappears) fails this test rather
// than silently rotting — the versionPinExemptions pattern in
// pkg/recipe/version_pin_guard_test.go.
var untrackedComponents = map[string]string{
	"gke-nccl-tcpxo":            "manifest-only: in-tree manifests, no upstream chart",
	"dranet":                    "manifest-only: in-tree manifests, no upstream chart",
	"dra-node-labeler":          "manifest-only: in-tree manifests, no upstream chart",
	"rdma-netns-exclusive":      "manifest-only: in-tree manifests, no upstream chart",
	"gcp-driver-installer":      "manifest-only: in-tree manifests, no upstream chart",
	"nodewright-customizations": "manifest-only: in-tree customization manifests",
	"gpu-operator-ocp-olm":      "OpenShift OLM variant: subscription manifests, no chart",
	"gpu-operator-ocp":          "OpenShift manifest variant, no chart",
	"network-operator-ocp-olm":  "OpenShift OLM variant: subscription manifests, no chart",
	"network-operator-ocp":      "OpenShift manifest variant, no chart",
	"nfd-ocp-olm":               "OpenShift OLM variant: subscription manifests, no chart",
	"nfd-ocp":                   "OpenShift manifest variant, no chart",
	"cert-manager-ocp-olm":      "OpenShift OLM variant: subscription manifests, no chart",
	"cert-manager-ocp":          "OpenShift manifest variant, no chart",
}

func testRepoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..")
}

// TestRegistryPinsAreRenovateTracked fails closed: every component with an
// upstream chart must carry a `# renovate:` annotation whose datasource,
// depName, and registryUrl agree with the neighboring defaultRepository and
// defaultChart, or be listed in untrackedComponents with a reason. A component
// added later cannot silently drop out of the weekly drift report.
func TestRegistryPinsAreRenovateTracked(t *testing.T) {
	pins, err := LoadPins(testRepoRoot(t))
	if err != nil {
		t.Fatalf("LoadPins: %v", err)
	}
	if len(pins) == 0 {
		t.Fatal("LoadPins returned no components; parser is broken")
	}

	used := make(map[string]bool, len(untrackedComponents))
	tracked := 0
	for _, p := range pins {
		if p.Kustomize {
			// Kustomize defaultTag coverage is deliberately unwritten (spec
			// decision 3). The first Kustomize component must come with it.
			t.Errorf("component %q uses kustomize: defaultTag drift coverage is not implemented; "+
				"extend tools/drift-report before adding a Kustomize component", p.Component)
			continue
		}
		if p.Repository == "" {
			reason, ok := untrackedComponents[p.Component]
			if !ok {
				t.Errorf("component %q has no defaultRepository and is not in untrackedComponents; "+
					"add it with a written reason", p.Component)
				continue
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("untrackedComponents[%q] has an empty reason", p.Component)
			}
			used[p.Component] = true
			continue
		}
		if _, listed := untrackedComponents[p.Component]; listed {
			t.Errorf("component %q is in untrackedComponents but has defaultRepository %q; remove the entry",
				p.Component, p.Repository)
		}
		tracked++
		if !p.Annotated {
			t.Errorf("component %q (line %d) has no `# renovate:` annotation above defaultVersion",
				p.Component, p.Line)
			continue
		}
		wantDS, wantDep, wantURL := expectedDep(p.Repository, p.Chart)
		if p.Datasource != wantDS {
			t.Errorf("component %q: annotation datasource=%q, want %q", p.Component, p.Datasource, wantDS)
		}
		if p.DepName != wantDep {
			t.Errorf("component %q: annotation depName=%q, want %q (from repository %q, chart %q)",
				p.Component, p.DepName, wantDep, p.Repository, p.Chart)
		}
		if p.RegistryURL != wantURL {
			t.Errorf("component %q: annotation registryUrl=%q, want %q", p.Component, p.RegistryURL, wantURL)
		}
		if p.Version == "" {
			t.Errorf("component %q: annotated but defaultVersion is empty", p.Component)
		}
	}

	if tracked != 35 {
		t.Errorf("tracked chart pins = %d, want 35; update this count and the spec deliberately", tracked)
	}

	// report.go joins Renovate's lookups by DepName alone (lookups[p.DepName]),
	// so two pins sharing one DepName share whichever Lookup entry
	// ParseRenovateReport's map kept last. That is safe only when the two
	// pins are genuine twins (e.g. the -ocp variants: same Repository, same
	// Chart, deliberately sharing one annotation) — an HTTP chart's DepName is
	// deliberately just the bare chart name (the repository travels
	// separately as registryUrl), so a *different* repository can collide on
	// the same bare name without either pin's YAML looking wrong on its own.
	// Assert every DepName collision is a real twin, so a future colliding
	// bare chart name fails CI here instead of silently sharing a lookup and
	// letting one pin's Latest apply to both.
	coords := make(map[string]Pin, len(pins))
	for _, p := range pins {
		if p.DepName == "" {
			continue
		}
		prev, seen := coords[p.DepName]
		if !seen {
			coords[p.DepName] = p
			continue
		}
		if prev.Repository != p.Repository || prev.Chart != p.Chart {
			t.Errorf("depName %q is shared by %q (repository %q, chart %q) and %q (repository %q, chart %q) "+
				"but they are not the same chart; tools/drift-report/report.go joins Renovate lookups by "+
				"depName alone, so these two pins would silently share one Lookup and one pin's Latest "+
				"would be applied to the other — give them distinct depName annotations",
				p.DepName, prev.Component, prev.Repository, prev.Chart, p.Component, p.Repository, p.Chart)
		}
	}

	var stale []string
	for name := range untrackedComponents {
		if !used[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale untrackedComponents entries (component gone or now tracked): %v", stale)
	}
}

func TestExpectedDep(t *testing.T) {
	tests := []struct {
		name                          string
		repo, chart                   string
		wantDS, wantDep, wantRegistry string
	}{
		{"http chart with repo prefix", "https://charts.jetstack.io", "jetstack/cert-manager",
			"helm", "cert-manager", "https://charts.jetstack.io"},
		{"http chart bare name", "https://kubernetes-sigs.github.io/node-feature-discovery/charts", "node-feature-discovery",
			"helm", "node-feature-discovery", "https://kubernetes-sigs.github.io/node-feature-discovery/charts"},
		{"oci chart at registry root", "oci://ghcr.io/nvidia", "nvsentinel",
			"docker", "ghcr.io/nvidia/nvsentinel", ""},
		{"oci chart in a charts path", "oci://ghcr.io/nvidia/nodewright/charts", "nodewright",
			"docker", "ghcr.io/nvidia/nodewright/charts/nodewright", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds, dep, reg := expectedDep(tt.repo, tt.chart)
			if ds != tt.wantDS || dep != tt.wantDep || reg != tt.wantRegistry {
				t.Errorf("expectedDep(%q,%q) = (%q,%q,%q), want (%q,%q,%q)",
					tt.repo, tt.chart, ds, dep, reg, tt.wantDS, tt.wantDep, tt.wantRegistry)
			}
		})
	}
}
