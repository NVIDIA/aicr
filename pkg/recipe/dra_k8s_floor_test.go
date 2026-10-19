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
	"github.com/NVIDIA/aicr/pkg/version"
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
	//   kubeVersion: ">=1.32.0-0" (unchanged from 0.4.1 to 0.5.0)
	"nvidia-dra-driver-gpu":     {version: "0.5.0", minor: 32},
	"nvidia-dra-driver-gpu-ocp": {version: "0.5.0", minor: 32},
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

// floorMajor is the Kubernetes major version the DRA chart floors sit on. The
// audited kubeVersion is ">=1.32.0-0", so every bound is compared as
// (major, minor) against (floorMajor, floorMinor).
const floorMajor = 1

// termClearsFloor reports whether a single parsed term, on its own, confines
// every version that satisfies it to at or above Kubernetes
// floorMajor.floorMinor.0.
//
// Only the lower-bounding operators can do that:
//
//   - ">= v" admits exactly [v, inf)
//   - "> v"  admits (v, inf); requiring v itself to clear the floor is one
//     unit conservative at v's own precision (one whole minor for a
//     minor-precision value) and never fails open, since the patch component
//     is unbounded (there is no "last" 1.31.x to fall back on)
//   - "== v" and a bare exact match admit only v
//
// "<", "<=", and "!=" place no lower bound at all, so they return false: they
// can narrow an alternative but can never be what lifts it above the floor.
//
// A value the shipping version parser cannot read, or one written with less
// than major.minor precision (">= 1"), returns an error. Comparing "1" against
// "1.32.0" at min-precision would report equal and wave a floorless expression
// through, so the guard refuses to reason about it instead.
func termClearsFloor(pc constraints.ParsedConstraint, floorMinor int) (bool, error) {
	switch pc.Operator {
	case constraints.OperatorLT, constraints.OperatorLTE, constraints.OperatorNE:
		return false, nil
	case constraints.OperatorGTE, constraints.OperatorGT, constraints.OperatorEQ, constraints.OperatorExact:
	default:
		return false, fmt.Errorf("unknown operator %q; the guard cannot prove a lower bound for it", pc.Operator)
	}

	v, err := version.ParseVersion(pc.Value)
	if err != nil {
		return false, fmt.Errorf("value %q is not a version the shipping parser can read: %w", pc.Value, err)
	}
	if v.Precision < 2 {
		return false, fmt.Errorf("value %q has only major precision; a floor must name at least major.minor", pc.Value)
	}
	if v.Major > floorMajor {
		return true, nil
	}
	return v.Major == floorMajor && v.Minor >= floorMinor, nil
}

// alternativeString renders one AND group for error messages.
func alternativeString(group []constraints.ParsedConstraint) string {
	terms := make([]string, 0, len(group))
	for i := range group {
		terms = append(terms, group[i].String())
	}
	return strings.Join(terms, " ")
}

// proveExpressionClearsFloor proves, symbolically, that no Kubernetes cluster
// below floorMajor.floorMinor.0 can satisfy expr. It returns nil only when the
// proof succeeds, and a describing error otherwise — including for any
// expression it cannot reason about, so the guard fails closed.
//
// Why symbolic and not by probing readings: the production grammar admits
// arbitrary OR-of-AND range expressions, so no finite list of probe versions
// covers it. ">= 1.32 || > 1.31.0 < 1.31.2" is a supported shape that a probe
// sweep over 1.N / 1.N.0 / 1.N.99 misses entirely while the production
// evaluator happily accepts a 1.31.1 cluster that Helm's ">=1.32.0-0" rejects.
//
// The proof: an AND group's satisfying set is the intersection of its terms, so
// the group clears the floor as soon as ANY ONE of its terms does — a single
// ">= 1.32" makes the whole group safe no matter what the others say. A
// compound expression's satisfying set is the union of its groups, so EVERY
// group must clear the floor; one loose alternative admits a sub-floor cluster
// regardless of how strict its siblings are.
func proveExpressionClearsFloor(expr string, floorMinor int) error {
	parsed, err := constraints.ParseCompoundConstraint(expr)
	if err != nil {
		return fmt.Errorf("the shipping constraint parser rejects it: %w", err)
	}
	if len(parsed.Alternatives) == 0 {
		return fmt.Errorf("it parsed to zero OR alternatives, so nothing bounds it")
	}

	for i, group := range parsed.Alternatives {
		if len(group) == 0 {
			return fmt.Errorf("OR alternative %d has no terms, so nothing bounds it", i+1)
		}
		cleared := false
		for j := range group {
			ok, termErr := termClearsFloor(group[j], floorMinor)
			if termErr != nil {
				return fmt.Errorf("OR alternative %d (%q): %w", i+1, alternativeString(group), termErr)
			}
			if ok {
				cleared = true
			}
		}
		if !cleared {
			return fmt.Errorf("OR alternative %d (%q) carries no lower bound at or above %d.%d.0, "+
				"so at least one cluster below the chart floor satisfies it",
				i+1, alternativeString(group), floorMajor, floorMinor)
		}
	}
	return nil
}

// probeReadings returns the Kubernetes readings used to show that a
// declaration admits at least one cluster. Minor-precision probes alone are
// not enough: a patch-precision range such as ">= 1.34.3 < 1.35.0" is
// satisfied by no "1.N.0" reading, so a correct floor would be reported as
// admitting nothing. The declared bounds are therefore probed as well.
//
// The result is a fresh slice. Appending to `supported` in place would write
// into a backing array shared by every declaration under test whenever it has
// spare capacity.
func probeReadings(supported []string, parsed *constraints.CompoundConstraint) []string {
	readings := make([]string, 0, len(supported))
	readings = append(readings, supported...)
	for _, group := range parsed.Alternatives {
		for i := range group {
			readings = append(readings, group[i].Value)
		}
	}
	return readings
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
// the LATER overlay winning and no max comparison (see RecipeMetadataSpec.Merge
// in metadata.go; validation-phase constraints merge the same way in
// mergeValidationPhase in validation.go). A leaf declaring ">= 1.30" silently
// overwrites a higher floor inherited from base, so raising base alone would
// not hold. This is the same
// last-wins hazard documented for driver floors in #2438.
//
// recipes/overlays/ocp.yaml already carried >= 1.32 for exactly this reason
// before the rest of the catalog was reconciled; its comment records the
// diagnosis.
//
// How it checks: each declaration is decoded typed, parsed with the shipping
// parser (constraints.ParseCompoundConstraint), and then PROVEN — symbolically,
// over the parsed OR-of-AND structure — to carry a lower bound at or above the
// chart floor on every alternative. See proveExpressionClearsFloor.
//
// Sampling the evaluator with a list of sub-floor readings was tried and is not
// sufficient: the grammar admits arbitrary ranges, and ">= 1.32 || > 1.31.0
// < 1.31.2" slips past any fixed probe list while admitting a 1.31.1 cluster.
// The guard fails closed on any expression the parser rejects, on any operator
// or value it cannot reason about, and on any expression no supported reading
// satisfies.
func TestOverlayK8sFloorsClearDRAChartFloor(t *testing.T) {
	t.Parallel()

	floorMinor := draChartKubeVersionMinor()
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
			verifyK8sFloorDeclaration(t, decl, floorMinor, supported)
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
func verifyK8sFloorDeclaration(t *testing.T, decl k8sFloorDeclaration, floorMinor int, supported []string) {
	t.Helper()

	if err := proveExpressionClearsFloor(decl.value, floorMinor); err != nil {
		t.Errorf("%s (%s) declares K8s.server.version %q, which this guard cannot prove\n"+
			"  clears the pinned nvidia-dra-driver-gpu chart's kubeVersion \">=1.%d.0-0\":\n"+
			"  %v\n"+
			"  Every recipe inherits the DRA driver from base.yaml, and Helm refuses the\n"+
			"  install below the chart floor — so a recipe that admits a lower cluster\n"+
			"  validates clean and then fails at `helm install`. Raise it to \">= 1.%d\",\n"+
			"  or, if the expression is genuinely safe in a form the guard cannot yet\n"+
			"  prove, extend proveExpressionClearsFloor rather than loosening it.\n"+
			"  Raising base.yaml alone does NOT fix a leaf: constraints merge last-wins\n"+
			"  with no max comparison, so a lower leaf value overwrites a higher\n"+
			"  inherited one. See #2402.",
			decl.file, decl.location, decl.value, floorMinor, err, floorMinor)
		return
	}

	parsed, err := constraints.ParseCompoundConstraint(decl.value)
	if err != nil {
		t.Errorf("%s (%s): %v", decl.file, decl.location, err)
		return
	}

	for _, reading := range probeReadings(supported, parsed) {
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

	t.Errorf("%s (%s) declares K8s.server.version %q, which none of the probed Kubernetes\n"+
		"  readings (1.%d through 1.60 plus the expression's declared bounds) satisfies.\n"+
		"  It admits no cluster the catalog supports, so this guard cannot show it is a\n"+
		"  floor rather than a typo, and fails closed. See #2402.",
		decl.file, decl.location, decl.value, floorMinor)
}

// TestProveExpressionClearsFloor pins the prover's behavior on the shapes the
// catalog guard has to withstand. Every "must fail" row is a permanent
// regression control: each one is an expression a real author could write that
// the production evaluator accepts for a sub-floor cluster.
//
// The last row is the shape that defeated the previous probe-sampling guard: a
// safe first alternative followed by a narrow sub-floor range. It is kept here
// permanently so no future rewrite can reintroduce sampling and stay green.
func TestProveExpressionClearsFloor(t *testing.T) {
	t.Parallel()

	const floorMinor = 32

	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"simple floor at the chart minor", ">= 1.32", false},
		{"simple floor with patch", ">= 1.32.4", false},
		{"floor above the chart minor", ">= 1.33.0", false},
		{"major above the floor", ">= 2.0", false},
		{"range whose lower bound clears the floor", ">= 1.32.4 < 1.35.0", false},
		{"every alternative clears the floor", ">= 1.34.3-gke.1318000 < 1.35.0 || >= 1.35.0-gke.2745000", false},
		{"upper-bounded term does not lift a cleared group", ">= 1.32 < 1.33", false},

		{"floor below the chart minor", ">= 1.30", true},
		{"simple compound with a low alternative", ">= 1.32 || >= 1.29", true},
		{"exact pin below the chart minor", "== 1.30", true},
		{"bare exact pin below the chart minor", "1.30", true},
		{"greater-than below the chart minor", "> 1.31", true},
		{"only an upper bound", "< 1.40", true},
		{"only a not-equal", "!= 1.30", true},
		{"major-only precision", ">= 1", true},
		{"non-version value", ">= stable", true},
		{"empty expression", "", true},
		{"empty OR clause", ">= 1.32 ||", true},
		// The control: accepted by the production evaluator for a 1.31.1
		// cluster, which Helm's ">=1.32.0-0" rejects. A probe sweep over
		// 1.N / 1.N.0 / 1.N.99 / v1.N.0 misses it entirely.
		{"narrow sub-floor range hidden behind a safe alternative", ">= 1.32 || > 1.31.0 < 1.31.2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := proveExpressionClearsFloor(tt.expr, floorMinor)
			if (err != nil) != tt.wantErr {
				t.Fatalf("proveExpressionClearsFloor(%q, %d) error = %v, wantErr %v",
					tt.expr, floorMinor, err, tt.wantErr)
			}
		})
	}
}

// TestProveExpressionRejectsWhatTheEvaluatorAdmits is the adversarial control
// for the row above: it proves independently that the production evaluator
// really does accept a sub-floor cluster for that expression, so the "must
// fail" verdict is grounded in behavior rather than in the prover's own
// opinion. Without this, a prover bug that rejected everything would still
// make the table green.
func TestProveExpressionRejectsWhatTheEvaluatorAdmits(t *testing.T) {
	t.Parallel()

	const expr = ">= 1.32 || > 1.31.0 < 1.31.2"

	parsed, err := constraints.ParseCompoundConstraint(expr)
	if err != nil {
		t.Fatalf("ParseCompoundConstraint(%q): %v", expr, err)
	}
	satisfied, err := parsed.Evaluate("1.31.1")
	if err != nil {
		t.Fatalf("Evaluate(1.31.1): %v", err)
	}
	if !satisfied {
		t.Fatalf("expected the production evaluator to accept 1.31.1 for %q; if this "+
			"changed, the control in TestProveExpressionClearsFloor needs rebasing", expr)
	}
	if err := proveExpressionClearsFloor(expr, 32); err == nil {
		t.Fatalf("prover accepted %q even though the evaluator admits a 1.31.1 cluster", expr)
	}
}

// TestProbeSetAdmitsPatchPrecisionRange is the regression control for the
// probe sweep used by verifyK8sFloorDeclaration. The sweep used to try only
// "1.N.0" readings, which no patch-precision range can satisfy: ">= 1.34.3
// < 1.35.0" clears proveExpressionClearsFloor, yet 1.34.0 is below its lower
// bound and 1.35.0 is excluded by its upper one. A correct floor was therefore
// reported as admitting no supported release.
//
// Reverting probeReadings to return `supported` unchanged makes this test
// fail, which is the point: the catalog currently declares no patch-precision
// range, so the defect is latent and nothing else would catch a regression.
func TestProbeSetAdmitsPatchPrecisionRange(t *testing.T) {
	const expr = ">= 1.34.3 < 1.35.0"

	parsed, err := constraints.ParseCompoundConstraint(expr)
	if err != nil {
		t.Fatalf("parsing %q: %v", expr, err)
	}

	for _, reading := range supportedProbeVersions(32) {
		satisfied, evalErr := parsed.Evaluate(reading)
		if evalErr != nil {
			t.Fatalf("evaluating %q against %q: %v", expr, reading, evalErr)
		}
		if satisfied {
			t.Fatalf("expected no minor-precision reading to satisfy %q, but %q did;\n"+
				"  this test no longer proves the bound-probing loop is required", expr, reading)
		}
	}

	var admitted bool
	for _, reading := range probeReadings(supportedProbeVersions(32), parsed) {
		satisfied, evalErr := parsed.Evaluate(reading)
		if evalErr != nil {
			t.Fatalf("evaluating %q against probe %q: %v", expr, reading, evalErr)
		}
		admitted = admitted || satisfied
	}
	if !admitted {
		t.Errorf("no declared bound of %q satisfies it, so the probe sweep would still\n"+
			"  fail a correct floor. See #2402.", expr)
	}
}
