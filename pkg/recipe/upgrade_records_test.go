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
	stdsort "sort"
	"testing"
	"testing/fstest"

	"github.com/NVIDIA/aicr/pkg/upgrade"
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

// mapFSProvider is the DataProvider subset orphanUpgradeRecords uses, over an
// in-memory tree. The embedded provider cannot stand in: no component has
// authored a components/<name>/upgrades.yaml yet, so the real tree cannot
// produce an orphan to detect.
type mapFSProvider struct {
	DataProvider
	fsys fs.FS
}

func (p mapFSProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fs.WalkDir(p.fsys, root, fn)
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
			provider := mapFSProvider{fsys: fstest.MapFS(tt.files)}
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
