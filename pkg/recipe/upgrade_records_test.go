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
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	stdsort "sort"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"aicr.run/pkg/recipe/providertest"
	"aicr.run/pkg/upgrade"
)

// TestRealUpgradeRecordsWellFormed runs the ADR-021 well-formedness rules over
// every upgrade record the registry references, and fails on any record file
// the registry does not reference.
//
// It is inert while no component sets upgrades.file, and becomes live the
// moment one does. It asserts no verdict — only that whatever was authored is
// well-formed — so "make the test green" cannot be satisfied by writing safe.
func TestRealUpgradeRecordsWellFormed(t *testing.T) {
	provider := defaultEmbeddedProvider

	set, comps, err := LoadUpgradeRecords(context.Background(), provider)
	if err != nil {
		t.Fatalf("loading upgrade records: %v", err)
	}
	t.Logf("checking %d component(s) with upgrade records", len(comps))

	if err := set.Validate(comps); err != nil {
		t.Fatalf("upgrade records are not well-formed: %v", err)
	}

	orphans, walkErr := orphanUpgradeRecords(t.Context(), provider, comps)
	if walkErr != nil {
		t.Fatalf("enumerating %s: %v", componentsDir, walkErr)
	}
	if len(orphans) > 0 {
		t.Errorf("upgrade record(s) %v are not referenced by any registry entry's upgrades.file, so nothing reads or validates them",
			orphans)
	}
}

// crdManualDeployers are the deployers whose bundles never apply a changed
// crds/ directory: helmfile emits no hook (ADR-021), and terraform upgrades
// through helm_release, which skips crds/ like Helm and has no CRD step.
var crdManualDeployers = []string{"helmfile", "terraform"}

// TestOwnsCRDsRecordsCarryCRDStepForManualDeployers checks the authoring
// obligation ADR-021 records: a manual or blocked transition of an ownsCRDs
// component must give helmfile and terraform a step that applies the CRDs,
// rather than letting them fall into a remainder group written for deployers
// that apply CRDs on their own. It asserts that a step exists, not a verdict.
func TestOwnsCRDsRecordsCarryCRDStepForManualDeployers(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	set, _, err := LoadUpgradeRecords(context.Background(), defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("loading upgrade records: %v", err)
	}

	checked := 0
	for _, name := range registry.Names() {
		cfg := registry.Get(name)
		if cfg == nil || !cfg.OwnsCRDs {
			continue
		}
		rec, ok := set[name]
		if !ok {
			continue
		}
		for i := range rec.Transitions {
			tr := &rec.Transitions[i]
			if tr.Verdict != upgrade.VerdictManual && tr.Verdict != upgrade.VerdictBlocked {
				continue
			}
			checked++
			for _, d := range crdManualDeployers {
				if !hasExplicitGroup(tr, d) {
					t.Errorf("%s transition %q -> %q is %s but leaves deployer %q to the remainder group, "+
						"whose steps describe another deployer's CRD path; add an explicit group for it (ADR-021)",
						name, tr.From, tr.To, tr.Verdict, d)
					continue
				}
				if !hasCRDStep(tr.StepsFor(d)) {
					t.Errorf("%s transition %q -> %q is %s but gives deployer %q no step that applies the CRDs; "+
						"add an explicit group for it (ADR-021)", name, tr.From, tr.To, tr.Verdict, d)
				}
			}
		}
	}
	t.Logf("checked %d manual/blocked transition(s) of ownsCRDs components", checked)
}

// hasExplicitGroup reports whether some group names d, so that the steps d
// receives were written for it rather than inherited from the remainder.
func hasExplicitGroup(tr *upgrade.Transition, d string) bool {
	for _, g := range tr.StepsByDeployer {
		if slices.Contains(g.Deployers, d) {
			return true
		}
	}
	return false
}

// hasCRDStep reports whether some step applies CRDs, by id: an "apply" step
// that names CRDs. A verify step that mentions CRDs does not count, since the
// remainder groups verify what the deployer applied and that is the group
// helmfile and terraform must not inherit.
func hasCRDStep(steps []upgrade.Step) bool {
	for _, s := range steps {
		id := strings.ToLower(s.ID)
		if strings.Contains(id, "apply") && strings.Contains(id, "crd") {
			return true
		}
	}
	return false
}

// applyStepID is the id of the step where a deployer applies the regenerated
// bundle.
const applyStepID = "apply"

// applyStepExempt maps a component to the only deployer that can carry it.
// Its other groups redirect to that deployer and apply nothing: runai-cluster's
// bootstrap token is an install-time secret only helm may declare (ADR-029).
var applyStepExempt = map[string]string{"runai-cluster": "helm"}

// TestManualRecordsNameTheirApplyStep checks that every group of a manual
// transition or replacement names its deployer's apply step "apply", exactly
// once, so the AICR Operator can tell the steps before the apply from those
// after it. It asserts the step's id, not a verdict.
func TestManualRecordsNameTheirApplyStep(t *testing.T) {
	set, _, err := LoadUpgradeRecords(context.Background(), defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("loading upgrade records: %v", err)
	}
	violations, checked := applyStepViolations(set)
	for _, v := range violations {
		t.Error(v)
	}
	t.Logf("checked %d manual step group(s)", checked)
}

// applyStepViolations returns, in component order, a message for each group
// of a manual transition or replacement in set that does not hold exactly one
// apply step, and the number of groups it checked.
func applyStepViolations(set upgrade.Set) ([]string, int) {
	var violations []string
	checked := 0
	check := func(component, where string, groups []upgrade.StepGroup) {
		for gi, g := range groups {
			if carrier, ok := applyStepExempt[component]; ok && !groupCovers(groups, gi, carrier) {
				continue
			}
			checked++
			n := 0
			for _, s := range g.Steps {
				if s.ID == applyStepID {
					n++
				}
			}
			if n != 1 {
				covers := "remainder"
				if g.Deployers != nil {
					covers = fmt.Sprintf("deployers %v", g.Deployers)
				}
				violations = append(violations, fmt.Sprintf("%s %s group %d (%s) has %d %q steps, want exactly 1; "+
					"see docs/contributor/upgrade-records.md", component, where, gi, covers, n, applyStepID))
			}
		}
	}
	eachManualRecord(set, check)
	return violations, checked
}

// eachManualRecord calls fn, in component order, with the step groups of
// each manual transition and replacement in set.
func eachManualRecord(set upgrade.Set, fn func(component, where string, groups []upgrade.StepGroup)) {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	stdsort.Strings(names)
	for _, name := range names {
		rec := set[name]
		for i := range rec.Transitions {
			tr := &rec.Transitions[i]
			if tr.Verdict == upgrade.VerdictManual {
				fn(name, "transition "+tr.From+" -> "+tr.To, tr.StepsByDeployer)
			}
		}
		if r := rec.Replaces; r != nil && r.Verdict == upgrade.VerdictManual {
			fn(name, "replaces "+r.Component, r.StepsByDeployer)
		}
	}
}

// The apply-step rule flags a manual group with no apply step or more than
// one, names a remainder group as such, checks a manual replacement, and
// leaves safe and blocked records and an exempt component's redirect alone.
func TestApplyStepViolations(t *testing.T) {
	steps := func(ids ...string) []upgrade.Step {
		out := make([]upgrade.Step, 0, len(ids))
		for _, id := range ids {
			out = append(out, upgrade.Step{ID: id, Description: id})
		}
		return out
	}
	record := func(component string, verdict upgrade.Verdict, groups ...upgrade.StepGroup) upgrade.Set {
		return upgrade.Set{component: {Component: component, Transitions: []upgrade.Transition{
			{From: "<1.0.0", To: "1.0.0", Verdict: verdict, StepsByDeployer: groups}}}}
	}
	tests := []struct {
		name string
		set  upgrade.Set
		want []string
	}{
		{"one apply per group", record("nfd", upgrade.VerdictManual,
			upgrade.StepGroup{Deployers: []string{"helm"}, Steps: steps("widen", "apply", "narrow")},
			upgrade.StepGroup{Steps: steps("widen", "apply", "narrow")}), nil},
		{"no apply step", record("nfd", upgrade.VerdictManual,
			upgrade.StepGroup{Deployers: []string{"helm"}, Steps: steps("widen", "upgrade-nfd")}),
			[]string{`nfd transition <1.0.0 -> 1.0.0 group 0 (deployers [helm]) has 0 "apply" steps`}},
		{"two apply steps", record("nfd", upgrade.VerdictManual,
			upgrade.StepGroup{Deployers: []string{"helm"}, Steps: steps("apply", "apply")}),
			[]string{`group 0 (deployers [helm]) has 2 "apply" steps`}},
		{"remainder named", record("kueue", upgrade.VerdictManual,
			upgrade.StepGroup{Steps: steps("sync-kueue")}),
			[]string{`group 0 (remainder) has 0 "apply" steps`}},
		{"blocked and safe not checked", upgrade.Set{
			"grove": {Component: "grove", Transitions: []upgrade.Transition{
				{From: "<1.0.0", To: "1.0.0", Verdict: upgrade.VerdictBlocked, StepsByDeployer: []upgrade.StepGroup{{Steps: steps("land")}}},
				{From: ">=1.0.0 <2.0.0", To: "2.0.0", Verdict: upgrade.VerdictSafe}}}}, nil},
		{"manual replacement checked", upgrade.Set{"kai-scheduler": {Component: "kai-scheduler", Replaces: &upgrade.Replaces{
			Component: "old-scheduler", Verdict: upgrade.VerdictManual, StepsByDeployer: []upgrade.StepGroup{{Steps: steps("migrate")}}}}},
			[]string{`kai-scheduler replaces old-scheduler group 0 (remainder) has 0 "apply" steps`}},
		{"exempt component's carrier group checked", record("runai-cluster", upgrade.VerdictManual,
			upgrade.StepGroup{Deployers: []string{"helm"}, Steps: steps("rerun-install")},
			upgrade.StepGroup{Steps: steps("use-helm-deployer")}),
			[]string{`runai-cluster transition <1.0.0 -> 1.0.0 group 0 (deployers [helm]) has 0 "apply" steps`}},
		{"exempt component's redirect skipped", record("runai-cluster", upgrade.VerdictManual,
			upgrade.StepGroup{Deployers: []string{"helm"}, Steps: steps("apply")},
			upgrade.StepGroup{Steps: steps("use-helm-deployer")}), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := applyStepViolations(tt.set)
			if len(got) != len(tt.want) {
				t.Fatalf("applyStepViolations() = %q, want %d violation(s) containing %q", got, len(tt.want), tt.want)
			}
			for i := range got {
				if !strings.Contains(got[i], tt.want[i]) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// The RuntimeRollout CRD's bounds on the steps the AICR Operator holds for
// acknowledgement (RolloutStep.PostApplySteps in api/operator/v1alpha1),
// which the operator checks an assessed record against before it creates a
// rollout.
const (
	operatorHeldSteps        = 16
	operatorHeldStepIDLength = 63
)

// operatorHeldSteps and operatorHeldStepIDLength are the bounds the chart's
// RuntimeRollout CRD puts on a step's postApplySteps, in every version.
func TestOperatorHoldBoundsMatchTheCRD(t *testing.T) {
	crdPath := filepath.Join("..", "..", "charts", "aicr-operator", "files", "crds", "operator.aicr.run_runtimerollouts.yaml")
	data, err := os.ReadFile(crdPath) //nolint:gosec // fixed in-repo CRD path
	if err != nil {
		t.Fatalf("read %s: %v", crdPath, err)
	}
	var crd map[string]any
	if err = yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("parse %s: %v", crdPath, err)
	}
	dig := func(node any, keys ...string) map[string]any {
		t.Helper()
		m, ok := node.(map[string]any)
		for _, k := range keys {
			if !ok {
				break
			}
			m, ok = m[k].(map[string]any)
		}
		if !ok {
			t.Fatalf("%s has no %v", crdPath, keys)
		}
		return m
	}
	versions, _ := dig(crd, "spec")["versions"].([]any)
	if len(versions) == 0 {
		t.Fatalf("%s has no versions", crdPath)
	}
	for _, v := range versions {
		held := dig(v, "schema", "openAPIV3Schema", "properties", "spec", "properties", "steps", "items", "properties", "postApplySteps")
		if held["maxItems"] != operatorHeldSteps || dig(held, "items")["maxLength"] != operatorHeldStepIDLength {
			t.Errorf("postApplySteps maxItems %v, items.maxLength %v; the record gate checks %d and %d",
				held["maxItems"], dig(held, "items")["maxLength"], operatorHeldSteps, operatorHeldStepIDLength)
		}
	}
}

// TestManualRecordsFitTheOperatorsHold checks that no group of a manual
// transition or replacement holds more steps after its apply step (or steps
// in all, when it names none) than the AICR Operator records for
// acknowledgement, nor a longer held step id. Past either, an operator
// upgrade stalls with TransitionBlocked.
func TestManualRecordsFitTheOperatorsHold(t *testing.T) {
	set, _, err := LoadUpgradeRecords(context.Background(), defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("loading upgrade records: %v", err)
	}
	for _, v := range holdViolations(set) {
		t.Error(v)
	}
}

// holdViolations returns, in component order, a message for each group of a
// manual transition or replacement in set whose held steps exceed what the
// AICR Operator records.
func holdViolations(set upgrade.Set) []string {
	var violations []string
	eachManualRecord(set, func(component, where string, groups []upgrade.StepGroup) {
		for gi, g := range groups {
			held := g.Steps
			if i := slices.IndexFunc(g.Steps, func(s upgrade.Step) bool { return s.ID == applyStepID }); i >= 0 {
				held = g.Steps[i+1:]
			}
			if len(held) > operatorHeldSteps {
				violations = append(violations, fmt.Sprintf("%s %s group %d holds %d steps for the AICR Operator to acknowledge, more than %d; "+
					"see docs/contributor/upgrade-records.md", component, where, gi, len(held), operatorHeldSteps))
			}
			for _, step := range held {
				if n := utf8.RuneCountInString(step.ID); n > operatorHeldStepIDLength {
					violations = append(violations, fmt.Sprintf("%s %s group %d holds step id %q of %d characters, more than %d; "+
						"see docs/contributor/upgrade-records.md", component, where, gi, step.ID, n, operatorHeldStepIDLength))
				}
			}
		}
	})
	return violations
}

// The hold rule counts the steps after the apply step, or every step when
// there is none, checks only held step ids, and leaves safe records alone.
func TestHoldViolations(t *testing.T) {
	steps := func(ids ...string) []upgrade.Step {
		out := make([]upgrade.Step, 0, len(ids))
		for _, id := range ids {
			out = append(out, upgrade.Step{ID: id, Description: id})
		}
		return out
	}
	ids := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, fmt.Sprintf("step-%d", i))
		}
		return out
	}
	long := strings.Repeat("x", operatorHeldStepIDLength+1)
	record := func(verdict upgrade.Verdict, s []upgrade.Step) upgrade.Set {
		return upgrade.Set{"nfd": {Component: "nfd", Transitions: []upgrade.Transition{
			{From: "<1.0.0", To: "1.0.0", Verdict: verdict, StepsByDeployer: []upgrade.StepGroup{{Steps: s}}}}}}
	}
	tests := []struct {
		name string
		set  upgrade.Set
		want []string
	}{
		{"at the limit after the apply", record(upgrade.VerdictManual, steps(append([]string{"before", "apply"}, ids(operatorHeldSteps)...)...)), nil},
		{"one too many after the apply", record(upgrade.VerdictManual, steps(append([]string{"apply"}, ids(operatorHeldSteps+1)...)...)),
			[]string{fmt.Sprintf("holds %d steps", operatorHeldSteps+1)}},
		{"no apply step holds every step", record(upgrade.VerdictManual, steps(ids(operatorHeldSteps+1)...)),
			[]string{fmt.Sprintf("holds %d steps", operatorHeldSteps+1)}},
		{"a held id at the limit", record(upgrade.VerdictManual, steps("apply", strings.Repeat("x", operatorHeldStepIDLength))), nil},
		{"a long held id", record(upgrade.VerdictManual, steps("apply", long)), []string{fmt.Sprintf("of %d characters", len(long))}},
		// Characters, not bytes, as the API server counts them.
		{"a multibyte held id at the limit", record(upgrade.VerdictManual, steps("apply", strings.Repeat("é", operatorHeldStepIDLength))), nil},
		{"a multibyte held id too long", record(upgrade.VerdictManual, steps("apply", strings.Repeat("é", operatorHeldStepIDLength+1))),
			[]string{fmt.Sprintf("of %d characters", operatorHeldStepIDLength+1)}},
		{"a long id before the apply", record(upgrade.VerdictManual, steps(long, "apply")), nil},
		{"a safe record", record(upgrade.VerdictSafe, steps(ids(operatorHeldSteps+1)...)), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := holdViolations(tt.set)
			if len(got) != len(tt.want) {
				t.Fatalf("holdViolations() = %q, want %d violation(s) containing %q", got, len(tt.want), tt.want)
			}
			for i := range got {
				if !strings.Contains(got[i], tt.want[i]) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// groupCovers reports whether groups[gi] holds the steps for deployer: it
// names the deployer, or it is the remainder and no other group names it.
func groupCovers(groups []upgrade.StepGroup, gi int, deployer string) bool {
	if len(groups[gi].Deployers) > 0 {
		return slices.Contains(groups[gi].Deployers, deployer)
	}
	for _, g := range groups {
		if slices.Contains(g.Deployers, deployer) {
			return false
		}
	}
	return true
}

// orphanUpgradeRecords returns the components/<name>/upgrades.yaml files that
// no component's upgrades.file references.
//
// Validating what the registry points at is only as wide as the registry: a
// record whose upgrades.file path or filename is wrong is never read, and
// every rule in pkg/upgrade then passes vacuously over the empty component
// list that leaves. This walk is what catches that: a
// components/<name>/upgrades.yaml with a typo'd or missing registry
// reference.
func orphanUpgradeRecords(ctx context.Context, provider DataProvider, comps []upgrade.Component) ([]string, error) {
	referenced := make(map[string]bool, len(comps))
	for _, c := range comps {
		referenced[path.Clean(c.File)] = true
	}

	var orphans []string
	err := provider.WalkDir(ctx, componentsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// componentsDir is absent only in a synthetic test tree; the
			// real data root always has one, and an absent directory is
			// not an orphan.
			if path.Clean(p) == componentsDir {
				return fs.SkipAll
			}
			return err
		}
		// A record is exactly components/<name>/upgrades.yaml — one
		// path segment below componentsDir — so a same-named file
		// nested deeper (e.g. under a component's manifests/) is not
		// mistaken for one.
		if d.IsDir() || d.Name() != upgradesFileName || path.Dir(path.Dir(p)) != componentsDir {
			return nil
		}
		if !referenced[path.Clean(p)] {
			orphans = append(orphans, path.Clean(p))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	stdsort.Strings(orphans)
	return orphans, nil
}

func TestOrphanUpgradeRecords(t *testing.T) {
	record := &fstest.MapFile{Data: []byte("kind: ComponentUpgrades\n")}
	tests := []struct {
		name  string
		files map[string]*fstest.MapFile
		comps []upgrade.Component
		want  []string
	}{
		{
			name:  "absent directory is not an orphan",
			files: map[string]*fstest.MapFile{"registry.yaml": record},
		},
		{
			name: "every record referenced",
			files: map[string]*fstest.MapFile{
				"components/a/upgrades.yaml": record,
				"components/b/upgrades.yaml": record,
			},
			comps: []upgrade.Component{
				{Name: "a", File: "components/a/upgrades.yaml"},
				{Name: "b", File: "components/b/upgrades.yaml"},
			},
		},
		{
			name: "a record no registry entry points at",
			files: map[string]*fstest.MapFile{
				"components/a/upgrades.yaml":     record,
				"components/stale/upgrades.yaml": record,
			},
			comps: []upgrade.Component{{Name: "a", File: "components/a/upgrades.yaml"}},
			want:  []string{"components/stale/upgrades.yaml"},
		},
		{
			name: "every record orphaned, which is what an unwired upgrades.file looks like",
			files: map[string]*fstest.MapFile{
				"components/a/upgrades.yaml": record,
				"components/b/upgrades.yaml": record,
			},
			want: []string{"components/a/upgrades.yaml", "components/b/upgrades.yaml"},
		},
		{
			name:  "non-upgrades files are not records",
			files: map[string]*fstest.MapFile{"components/a/values.yaml": record, "components/a/README.md": record},
		},
		{
			name:  "a same-named file below the component directory is not a record",
			files: map[string]*fstest.MapFile{"components/a/manifests/migrations/upgrades.yaml": record},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No component has authored a components/<name>/upgrades.yaml
			// yet, so the embedded tree has no orphan to detect.
			provider := providertest.New(fstest.MapFS(tt.files))
			got, err := orphanUpgradeRecords(t.Context(), provider, tt.comps)
			if err != nil {
				t.Fatalf("orphanUpgradeRecords: %v", err)
			}
			// Both sides are nil when empty: orphanUpgradeRecords never
			// preallocates, and an omitted want field is nil.
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("orphans = %v, want %v", got, tt.want)
			}
		})
	}
}

// componentsDir holds every component's data, relative to the recipes data
// root. ADR-021 upgrade records live one level below it, at
// componentsDir/<name>/upgradesFileName.
const componentsDir = "components"

// upgradesFileName is the ADR-021 record's filename within a component's
// directory.
const upgradesFileName = "upgrades.yaml"
