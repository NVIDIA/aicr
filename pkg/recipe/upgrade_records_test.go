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
	"strings"
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
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	provider := defaultEmbeddedProvider

	var comps []upgrade.Component
	for i := range registry.Components {
		c := &registry.Components[i]
		if c.Upgrades.File == "" {
			continue
		}
		comps = append(comps, upgrade.Component{
			Name:          c.Name,
			File:          c.Upgrades.File,
			PinnedVersion: pinnedVersionFor(c),
		})
	}
	t.Logf("checking %d component(s) with upgrade records", len(comps))

	set, err := upgrade.Load(context.Background(), provider, comps)
	if err != nil {
		t.Fatalf("loading upgrade records: %v", err)
	}
	if err := set.Validate(comps); err != nil {
		t.Fatalf("upgrade records are not well-formed: %v", err)
	}

	orphans, walkErr := orphanUpgradeRecords(t.Context(), provider, comps)
	if walkErr != nil {
		t.Fatalf("enumerating %s: %v", upgradesDir, walkErr)
	}
	if len(orphans) > 0 {
		t.Errorf("upgrade record(s) %v are not referenced by any registry entry's upgrades.file, so nothing reads or validates them",
			orphans)
	}
}

// orphanUpgradeRecords returns the record files under upgradesDir that no
// component references.
//
// Validating what the registry points at is only as wide as the registry: a
// record whose upgrades.file path, //go:embed pattern, or filename is wrong is
// never read, and every rule in pkg/upgrade then passes vacuously over the
// empty component list that leaves. That is the cheap half of the embed
// interlock the first-record PR has to satisfy.
func orphanUpgradeRecords(ctx context.Context, provider DataProvider, comps []upgrade.Component) ([]string, error) {
	referenced := make(map[string]bool, len(comps))
	for _, c := range comps {
		referenced[path.Clean(c.File)] = true
	}

	var orphans []string
	err := provider.WalkDir(ctx, upgradesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// The directory is absent until the first record lands, and an
			// absent directory is not an orphan.
			if path.Clean(p) == upgradesDir {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".yaml") {
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
// in-memory tree. The embedded provider cannot stand in: //go:embed rejects a
// pattern matching nothing, so recipes/ carries no upgrades/*.yaml yet and the
// real tree cannot produce an orphan to detect.
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
			name:  "every record referenced",
			files: map[string]*fstest.MapFile{"upgrades/a.yaml": record, "upgrades/b.yaml": record},
			comps: []upgrade.Component{
				{Name: "a", File: "upgrades/a.yaml"},
				{Name: "b", File: "upgrades/b.yaml"},
			},
		},
		{
			name:  "a record no registry entry points at",
			files: map[string]*fstest.MapFile{"upgrades/a.yaml": record, "upgrades/stale.yaml": record},
			comps: []upgrade.Component{{Name: "a", File: "upgrades/a.yaml"}},
			want:  []string{"upgrades/stale.yaml"},
		},
		{
			name:  "every record orphaned, which is what a wrong embed pattern looks like",
			files: map[string]*fstest.MapFile{"upgrades/a.yaml": record, "upgrades/b.yaml": record},
			want:  []string{"upgrades/a.yaml", "upgrades/b.yaml"},
		},
		{
			name:  "non-yaml files are not records",
			files: map[string]*fstest.MapFile{"upgrades/README.md": record},
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

// upgradesDir is where ADR-021 puts records, relative to the recipes data root.
const upgradesDir = "upgrades"

func pinnedVersionFor(c *ComponentConfig) string {
	if c.Helm.DefaultVersion != "" {
		return c.Helm.DefaultVersion
	}
	return c.Kustomize.DefaultTag
}
