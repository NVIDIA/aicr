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
	"io/fs"
	"path"
	"reflect"
	"slices"
	stdsort "sort"
	"strings"
	"testing"
	"testing/fstest"

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
// bundle. The AICR Operator performs that step and holds the steps after it
// for acknowledgement.
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
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	stdsort.Strings(names)

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
				t.Errorf("%s %s group %d (deployers %v) has %d %q steps, want exactly 1; "+
					"see docs/contributor/upgrade-records.md", component, where, gi, g.Deployers, n, applyStepID)
			}
		}
	}
	for _, name := range names {
		rec := set[name]
		for i := range rec.Transitions {
			tr := &rec.Transitions[i]
			if tr.Verdict == upgrade.VerdictManual {
				check(name, "transition "+tr.From+" -> "+tr.To, tr.StepsByDeployer)
			}
		}
		if r := rec.Replaces; r != nil && r.Verdict == upgrade.VerdictManual {
			check(name, "replaces "+r.Component, r.StepsByDeployer)
		}
	}
	t.Logf("checked %d manual step group(s)", checked)
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
