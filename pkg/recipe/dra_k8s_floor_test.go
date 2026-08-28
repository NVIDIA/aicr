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
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// auditedDRAChartFloors records, per DRA component, the chart version whose
// kubeVersion was read and the Kubernetes minor it declares.
//
// Both catalog DRA components are enrolled. OCP disables the generic
// nvidia-dra-driver-gpu (recipes/overlays/ocp.yaml sets enabled: false) and
// substitutes nvidia-dra-driver-gpu-ocp, so covering only the generic one would
// leave the OCP chain unguarded.
//
// Re-audit procedure when a pin moves: read `kubeVersion` from the chart at the
// new version and update the entry. TestDRAChartFloorAuditIsCurrent fails until
// you do, which is what couples this guard to the registry rather than letting
// it sit green at a stale floor.
var auditedDRAChartFloors = map[string]struct {
	version string
	minor   int
}{
	// oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu
	//   kubeVersion: ">=1.32.0-0"
	"nvidia-dra-driver-gpu":     {version: "0.4.1", minor: 32},
	"nvidia-dra-driver-gpu-ocp": {version: "0.4.1", minor: 32},
}

// draChartKubeVersionMinor is the highest audited floor across the enrolled DRA
// components — the value every recipe must clear, since each recipe carries one
// of them.
func draChartKubeVersionMinorFn() int {
	highest := 0
	for _, a := range auditedDRAChartFloors {
		if a.minor > highest {
			highest = a.minor
		}
	}
	return highest
}

// TestDRAChartFloorAuditIsCurrent fails when a DRA chart pin in registry.yaml
// moves away from the version whose kubeVersion was audited, so a chart bump
// cannot silently leave the floor guard asserting a stale minor.
func TestDRAChartFloorAuditIsCurrent(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	for name, audited := range auditedDRAChartFloors {
		cfg := registry.Get(name)
		if cfg == nil {
			t.Errorf("audited DRA component %q is not in the registry; remove it from "+
				"auditedDRAChartFloors or restore the component", name)
			continue
		}
		if cfg.Helm.DefaultVersion != audited.version {
			t.Errorf("%s is pinned at %q but its kubeVersion was audited at %q.\n"+
				"  Read `kubeVersion` from the chart at %s, update auditedDRAChartFloors,\n"+
				"  and raise the affected overlay floors if it moved. See #2402.",
				name, cfg.Helm.DefaultVersion, audited.version, cfg.Helm.DefaultVersion)
		}
	}
}

// k8sConstraintRE captures EVERY K8s.server.version declaration in a file and
// its raw value. FindAllStringSubmatch, not FindStringSubmatch: a file may carry
// more than one declaration, and checking only the first would let a later,
// lower one through.
// The value is matched irrespective of quoting: YAML accepts double-quoted,
// single-quoted and plain scalars, and a guard that only sees one style would
// not merely misparse the others — it would not see the declaration at all.
var k8sConstraintRE = regexp.MustCompile(
	`- name: K8s\.server\.version\s*\n\s*value:[ \t]*("[^"]*"|'[^']*'|[^\n#]*)`)

// geFloorRE matches the `>= 1.<minor>` form this guard can reason about.
var geFloorRE = regexp.MustCompile(`^\s*>=\s*1\.(\d+)`)

// TestOverlayK8sFloorsClearDRAChartFloor asserts no overlay or mixin declares a
// Kubernetes floor below the DRA chart's own kubeVersion.
//
// Every recipe carries a DRA driver: base.yaml declares nvidia-dra-driver-gpu,
// and OCP disables that one and substitutes nvidia-dra-driver-gpu-ocp. Both
// resolve to the same upstream chart and the same kubeVersion, so the floor
// applies catalog-wide either way.
//
// Why every declaration and not just base.yaml: constraints merge by name with
// the LATER overlay winning and no max comparison (see mergeValidation in
// validation.go). A leaf declaring ">= 1.30" silently overwrites a higher floor
// inherited from base, so raising base alone would not hold. This is the same
// last-wins hazard documented for driver floors in #2438.
//
// recipes/overlays/ocp.yaml already carried >= 1.32 for exactly this reason
// before the rest of the catalog was reconciled; its comment records the
// diagnosis.
func TestOverlayK8sFloorsClearDRAChartFloor(t *testing.T) {
	t.Parallel()

	efs := GetEmbeddedFS()

	var checked int
	err := fs.WalkDir(efs, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		if !strings.Contains(path, "overlays/") && !strings.Contains(path, "mixins/") {
			return nil
		}
		raw, readErr := efs.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range k8sConstraintRE.FindAllStringSubmatch(string(raw), -1) {
			checked++
			value := strings.Trim(strings.TrimSpace(m[1]), `"'`)

			g := geFloorRE.FindStringSubmatch(value)
			if g == nil {
				// Fail closed on any form this guard cannot interpret — an exact
				// pin (== 1.30), a range, or a bare version would each be just as
				// capable of admitting a sub-floor cluster, and silently skipping
				// them would make the guard weakest exactly where a future author
				// deviates from the established shape.
				t.Errorf("%s declares K8s.server.version %q, a form this guard cannot verify.\n"+
					"  It can only reason about \">= 1.<minor>\". Any other form may admit clusters\n"+
					"  below the pinned nvidia-dra-driver-gpu chart's kubeVersion \">=1.%d.0-0\",\n"+
					"  where Helm refuses the install. Either express it as a >= floor, or extend\n"+
					"  this guard to understand the new form. See #2402.",
					path, value, draChartKubeVersionMinorFn())
				continue
			}

			minor, convErr := strconv.Atoi(g[1])
			if convErr != nil {
				t.Errorf("%s: could not parse K8s.server.version minor from %q: %v", path, value, convErr)
				continue
			}
			if minor < draChartKubeVersionMinorFn() {
				t.Errorf("%s declares K8s.server.version %q, below the pinned "+
					"nvidia-dra-driver-gpu chart's kubeVersion \">=1.%d.0-0\".\n"+
					"  Every recipe inherits the DRA driver from base.yaml, and Helm refuses the\n"+
					"  install below the chart floor — so this recipe validates clean and then\n"+
					"  fails at `helm install`. Raise it to \">= 1.%d\".\n"+
					"  Raising base.yaml alone does NOT fix a leaf: constraints merge last-wins\n"+
					"  with no max comparison, so a lower leaf value overwrites a higher\n"+
					"  inherited one. See #2402.",
					path, value, draChartKubeVersionMinorFn(), draChartKubeVersionMinorFn())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded recipes: %v", err)
	}

	// Fail closed on a vacuous pass: if nothing matched, the guard is inert.
	if checked == 0 {
		t.Fatal("no K8s.server.version floors found in overlays or mixins — this guard " +
			"is vacuous. Either the constraint name changed or the embed pattern no " +
			"longer covers the recipe tree.")
	}
	t.Logf("verified %d K8s.server.version floor(s) clear the DRA chart floor", checked)
}
