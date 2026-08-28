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

// draChartKubeVersionMinor is the minor version floor the pinned
// nvidia-dra-driver-gpu chart declares:
//
//	kubeVersion: ">=1.32.0-0"
//
// Helm REJECTS the install outright when the cluster is below it, so a recipe
// declaring a lower K8s.server.version admits clusters that pass every
// recipe-time check and then fail at `helm install`.
//
// Sourced from the chart, not from a running cluster. If the DRA chart pin in
// recipes/registry.yaml moves to a version with a different kubeVersion, this
// constant and the affected overlays must move together — that coupling is the
// point of the guard.
const draChartKubeVersionMinor = 32

// k8sFloorRE captures the minor version from a K8s.server.version constraint
// expressed as a floor. Only `>=` forms are checked: an exact pin or a range is
// a deliberate statement that should be reviewed on its own terms.
var k8sFloorRE = regexp.MustCompile(`(?s)- name: K8s\.server\.version\s*\n\s*value: "\s*>=\s*1\.(\d+)`)

// TestOverlayK8sFloorsClearDRAChartFloor asserts no overlay or mixin declares a
// Kubernetes floor below the DRA chart's own kubeVersion.
//
// Every recipe inherits nvidia-dra-driver-gpu from base.yaml — no overlay
// removes or disables it — so the chart's floor applies catalog-wide.
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
		m := k8sFloorRE.FindStringSubmatch(string(raw))
		if m == nil {
			return nil
		}
		checked++
		minor, convErr := strconv.Atoi(m[1])
		if convErr != nil {
			t.Errorf("%s: could not parse K8s.server.version minor %q: %v", path, m[1], convErr)
			return nil
		}
		if minor < draChartKubeVersionMinor {
			t.Errorf("%s declares K8s.server.version \">= 1.%d\", below the pinned "+
				"nvidia-dra-driver-gpu chart's kubeVersion \">=1.%d.0-0\".\n"+
				"  Every recipe inherits the DRA driver from base.yaml, and Helm refuses the\n"+
				"  install below the chart floor — so this recipe validates clean and then\n"+
				"  fails at `helm install`. Raise it to \">= 1.%d\".\n"+
				"  Raising base.yaml alone does NOT fix a leaf: constraints merge last-wins\n"+
				"  with no max comparison, so a lower leaf value overwrites a higher\n"+
				"  inherited one. See #2402.",
				path, minor, draChartKubeVersionMinor, draChartKubeVersionMinor)
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
