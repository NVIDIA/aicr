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

// This guard lives in the external recipe_test package so it can import
// pkg/constraints — the production constraint parser and evaluator — which
// itself imports pkg/recipe. An in-package test could not, and would be forced
// to reimplement comparison logic that the shipping evaluator already owns.
package recipe_test

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// k8sServerVersionConstraint is the measurement path whose floor this guard
// audits.
const k8sServerVersionConstraint = "K8s.server.version"

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
func draChartKubeVersionMinor() int {
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

	registry, err := recipe.GetComponentRegistry()
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

// subFloorProbeVersions returns the Kubernetes version strings that MUST NOT
// satisfy any catalog floor: every minor below the DRA chart's kubeVersion,
// rendered in each shape a real cluster reading or a recipe author's exact pin
// can take.
//
// Probing the production evaluator with concrete readings — rather than
// pattern-matching the expression text — is what makes this guard independent
// of the expression's *form*. A prefix match on ">= 1.<minor>" is defeated by a
// compound expression (">= 1.32 || >= 1.29" begins with a safe floor and is
// still satisfied by 1.29.7, because the shipping parser treats "||" as OR),
// and a bare-string exact pin ("1.30") carries no operator to match at all.
// Both are caught here because both admit a sub-floor reading.
func subFloorProbeVersions(floorMinor int) []string {
	probes := make([]string, 0, 2+4*floorMinor)
	probes = append(probes, "0.99", "0.99.99")
	for minor := range floorMinor {
		probes = append(probes,
			fmt.Sprintf("1.%d", minor),
			fmt.Sprintf("1.%d.0", minor),
			fmt.Sprintf("1.%d.99", minor),
			fmt.Sprintf("v1.%d.0", minor),
		)
	}
	return probes
}

// supportedProbeVersions returns readings at or above the floor, used only to
// prove a constraint is not vacuous. An expression satisfied by nothing admits
// no sub-floor cluster and would pass the sub-floor sweep trivially, but it
// also rejects every cluster the catalog claims to support — a typo, not a
// floor. Failing on it keeps the guard closed against expressions it cannot
// show are meaningful.
func supportedProbeVersions(floorMinor int) []string {
	var probes []string
	for minor := floorMinor; minor <= 60; minor++ {
		probes = append(probes, fmt.Sprintf("1.%d.0", minor))
	}
	return probes
}

// k8sFloorDeclaration is one typed K8s.server.version constraint located in the
// catalog, with the structural path it was found at for error reporting.
type k8sFloorDeclaration struct {
	file     string
	location string
	value    string
}

// collectK8sFloorDeclarations decodes one recipe metadata document and returns
// every K8s.server.version constraint it declares, from every field that can
// carry one: spec.constraints, each validation phase, and each profile value's
// constraints and readinessConstraints.
//
// Decoding to the typed form is what closes the YAML-layout hole: a mapping
// written "value:" before "name:" is the same Constraint after unmarshalling,
// so key order, quoting style, comments, and indentation are all invisible to
// this guard by construction rather than by widening a regex.
func collectK8sFloorDeclarations(file string, raw []byte) ([]k8sFloorDeclaration, error) {
	var metadata recipe.RecipeMetadata
	if err := yaml.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}

	var found []k8sFloorDeclaration
	collect := func(location string, cs []recipe.Constraint) {
		for _, c := range cs {
			if c.Name != k8sServerVersionConstraint {
				continue
			}
			found = append(found, k8sFloorDeclaration{file: file, location: location, value: c.Value})
		}
	}

	spec := metadata.Spec
	collect("spec.constraints", spec.Constraints)

	if v := spec.Validation; v != nil {
		for _, phase := range []struct {
			name string
			p    *recipe.ValidationPhase
		}{
			{"readiness", v.Readiness},
			{"deployment", v.Deployment},
			{"performance", v.Performance},
			{"conformance", v.Conformance},
		} {
			if phase.p != nil {
				collect("spec.validation."+phase.name+".constraints", phase.p.Constraints)
			}
		}
	}

	if p := spec.Profile; p != nil {
		for valueName, pv := range p.Values {
			collect(fmt.Sprintf("spec.profile.values[%s].constraints", valueName), pv.Constraints)
			collect(fmt.Sprintf("spec.profile.values[%s].readinessConstraints", valueName), pv.ReadinessConstraints)
		}
	}

	return found, nil
}

// TestOverlayK8sFloorsClearDRAChartFloor asserts no overlay or mixin declares a
// Kubernetes floor that admits a cluster below the DRA chart's own kubeVersion.
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
//
// How it checks, and why not by reading the expression: each declaration is
// decoded typed, parsed with the shipping parser
// (constraints.ParseCompoundConstraint), and then EVALUATED against concrete
// sub-floor readings with the shipping evaluator. The guard therefore asserts
// the property that actually matters — "no supported-but-too-old cluster
// satisfies this" — instead of asserting the expression is spelled a
// particular way. It fails closed on any expression the parser rejects and on
// any expression no supported reading satisfies.
func TestOverlayK8sFloorsClearDRAChartFloor(t *testing.T) {
	t.Parallel()

	floorMinor := draChartKubeVersionMinor()
	subFloor := subFloorProbeVersions(floorMinor)
	supported := supportedProbeVersions(floorMinor)

	efs := recipe.GetEmbeddedFS()

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
		decls, decodeErr := collectK8sFloorDeclarations(path, raw)
		if decodeErr != nil {
			t.Errorf("%s: could not decode recipe metadata: %v", path, decodeErr)
			return nil
		}

		for _, decl := range decls {
			checked++
			verifyK8sFloorDeclaration(t, decl, floorMinor, subFloor, supported)
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

// verifyK8sFloorDeclaration checks one declaration against the DRA chart floor
// using the production parser and evaluator.
func verifyK8sFloorDeclaration(t *testing.T, decl k8sFloorDeclaration, floorMinor int, subFloor, supported []string) {
	t.Helper()

	parsed, err := constraints.ParseCompoundConstraint(decl.value)
	if err != nil {
		t.Errorf("%s (%s) declares K8s.server.version %q, which the shipping constraint\n"+
			"  parser rejects: %v\n"+
			"  An expression aicr cannot parse cannot be shown to clear the pinned\n"+
			"  nvidia-dra-driver-gpu chart's kubeVersion \">=1.%d.0-0\". See #2402.",
			decl.file, decl.location, decl.value, err, floorMinor)
		return
	}

	for _, reading := range subFloor {
		satisfied, evalErr := parsed.Evaluate(reading)
		if evalErr != nil {
			t.Errorf("%s (%s) declares K8s.server.version %q, which the shipping evaluator\n"+
				"  could not evaluate against the Kubernetes reading %q: %v\n"+
				"  The guard fails closed: an expression whose result is unknown may admit a\n"+
				"  cluster below the chart floor \">=1.%d.0-0\". See #2402.",
				decl.file, decl.location, decl.value, reading, evalErr, floorMinor)
			return
		}
		if satisfied {
			t.Errorf("%s (%s) declares K8s.server.version %q, which is SATISFIED by a\n"+
				"  Kubernetes %s cluster — below the pinned nvidia-dra-driver-gpu chart's\n"+
				"  kubeVersion \">=1.%d.0-0\".\n"+
				"  Every recipe inherits the DRA driver from base.yaml, and Helm refuses the\n"+
				"  install below the chart floor — so this recipe validates clean and then\n"+
				"  fails at `helm install`. Raise it to \">= 1.%d\".\n"+
				"  Raising base.yaml alone does NOT fix a leaf: constraints merge last-wins\n"+
				"  with no max comparison, so a lower leaf value overwrites a higher\n"+
				"  inherited one. See #2402.",
				decl.file, decl.location, decl.value, reading, floorMinor, floorMinor)
			return
		}
	}

	for _, reading := range supported {
		satisfied, evalErr := parsed.Evaluate(reading)
		if evalErr != nil {
			t.Errorf("%s (%s) declares K8s.server.version %q, which the shipping evaluator\n"+
				"  could not evaluate against the Kubernetes reading %q: %v. See #2402.",
				decl.file, decl.location, decl.value, reading, evalErr)
			return
		}
		if satisfied {
			return
		}
	}

	t.Errorf("%s (%s) declares K8s.server.version %q, which no Kubernetes release from\n"+
		"  1.%d through 1.60 satisfies. It admits no cluster the catalog supports, so this\n"+
		"  guard cannot show it is a floor rather than a typo, and fails closed. See #2402.",
		decl.file, decl.location, decl.value, floorMinor)
}
