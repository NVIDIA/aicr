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

package localformat_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// ownsCRDsComponent is the shared input for the apply-crds.sh cases. Only
// Component.OwnsCRDs differs between the emitting and non-emitting variants,
// so a golden diff attributes any change to that one field.
func ownsCRDsComponent(ownsCRDs bool) localformat.Component {
	return localformat.Component{
		Name:       "k8s-aibom",
		Namespace:  "k8s-aibom-system",
		Repository: "oci://ghcr.io/googlecloudplatform/charts",
		ChartName:  "k8s-aibom",
		Version:    "1.3.0",
		IsOCI:      true,
		Values:     map[string]any{"replicaCount": 1},
		OwnsCRDs:   ownsCRDs,
	}
}

// TestWrite_ApplyCRDsUpstream covers the non-vendored shape: the script
// sources upstream.env and asks the remote chart for its CRDs.
func TestWrite_ApplyCRDsUpstream(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(true)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(res.Folders))
	}
	f := res.Folders[0]

	if !f.AppliesCRDs {
		t.Error("Folder.AppliesCRDs = false; deployers that bypass install.sh key their pre-apply step off it")
	}
	rel := filepath.Join(f.Dir, "apply-crds.sh")
	if !slices.Contains(f.Files, rel) {
		t.Errorf("Folder.Files missing %q; checksums and the BOM enumerate this list\ngot: %v", rel, f.Files)
	}

	assertGolden(t, outDir, "testdata/apply_crds_upstream", filepath.Join(f.Dir, "apply-crds.sh"))
	// install.sh carries the call; the golden pins that it runs before the
	// upgrade and is skipped under --dry-run.
	assertGolden(t, outDir, "testdata/apply_crds_upstream", filepath.Join(f.Dir, "install.sh"))

	assertExecutable(t, filepath.Join(outDir, rel))
}

// TestWrite_ApplyCRDsVendored covers the vendored shape: the wrapper chart
// resolves the upstream chart from charts/<chart>-<version>.tgz, so the script
// reads "./" and needs no network at deploy time.
func TestWrite_ApplyCRDsVendored(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:    outDir,
		Components:   []localformat.Component{ownsCRDsComponent(true)},
		VendorCharts: true,
		Puller:       &fakePuller{},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(res.Folders))
	}
	f := res.Folders[0]

	if got, want := f.Kind, localformat.KindLocalHelm; got != want {
		t.Fatalf("Folder.Kind = %v, want %v (vendored components wrap the chart)", got, want)
	}
	if !f.AppliesCRDs {
		t.Error("Folder.AppliesCRDs = false for a vendored ownsCRDs component")
	}

	assertGolden(t, outDir, "testdata/apply_crds_vendored", filepath.Join(f.Dir, "apply-crds.sh"))
	assertGolden(t, outDir, "testdata/apply_crds_vendored", filepath.Join(f.Dir, "install.sh"))
}

// TestWrite_NoApplyCRDsWithoutFlag is the negative half of the acceptance
// criteria: a component without the flag must be untouched. Asserting the
// file's absence rather than a no-op script keeps that visible on disk.
//
// The install.sh golden is shared with the upstream-emitting case's sibling
// directory on purpose: comparing the two golden files shows the whole
// difference the flag makes.
func TestWrite_NoApplyCRDsWithoutFlag(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(false)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	f := res.Folders[0]

	if f.AppliesCRDs {
		t.Error("Folder.AppliesCRDs = true without the registry flag")
	}
	path := filepath.Join(outDir, f.Dir, "apply-crds.sh")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("apply-crds.sh exists for a component that does not own its CRDs (stat err: %v)", statErr)
	}
	if rel := filepath.Join(f.Dir, "apply-crds.sh"); slices.Contains(f.Files, rel) {
		t.Errorf("Folder.Files lists %q for a non-owning component", rel)
	}

	assertGolden(t, outDir, "testdata/apply_crds_absent", filepath.Join(f.Dir, "install.sh"))
}

// TestWrite_NoApplyCRDsOnInjectedWrappers pins that the flag stays on the
// primary folder. Injected -pre / -post wrappers are AICR-rendered charts
// carrying raw manifests, not an upstream chart with a crds/ directory, so a
// script asking them for CRDs would apply nothing and confuse the bundle.
func TestWrite_NoApplyCRDsOnInjectedWrappers(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(true)},
		ComponentPostManifests: map[string]map[string][]byte{
			"k8s-aibom": {"cm.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")},
		},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 2 {
		t.Fatalf("want primary + injected -post folder, got %d", len(res.Folders))
	}

	for _, f := range res.Folders {
		wantApplies := f.Name == f.Parent
		if f.AppliesCRDs != wantApplies {
			t.Errorf("folder %s: AppliesCRDs = %v, want %v", f.Dir, f.AppliesCRDs, wantApplies)
		}
		_, statErr := os.Stat(filepath.Join(outDir, f.Dir, "apply-crds.sh"))
		if wantApplies && statErr != nil {
			t.Errorf("folder %s: missing apply-crds.sh: %v", f.Dir, statErr)
		}
		if !wantApplies && !os.IsNotExist(statErr) {
			t.Errorf("folder %s: apply-crds.sh present on an injected wrapper (stat err: %v)", f.Dir, statErr)
		}
	}
}

// TestApplyCRDsScript_GatesAndBounds pins two properties a golden diff alone
// would not defend, because regenerating goldens with -update would silently
// bless their removal.
//
// The release gate is the load-bearing one. Helm installs a chart's crds/
// directory itself on first install, so this script is only needed on upgrade.
// Without the gate every fresh install pays a registry round-trip to apply CRDs
// helm is about to create anyway, and any registry trouble becomes an install
// failure. That is not hypothetical: it hung the KWOK helm lanes, which deploy
// to a fresh cluster, until the gate was added.
//
// The bound matters because this runs inside deploy.sh's retry loop, which
// retries a component that exits non-zero but cannot interrupt one that never
// returns. An unbounded registry read therefore hangs the whole rollout rather
// than failing one component.
func TestApplyCRDsScript_GatesAndBounds(t *testing.T) {
	outDir := t.TempDir()

	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{ownsCRDsComponent(true)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	script, err := os.ReadFile(filepath.Join(outDir, res.Folders[0].Dir, "apply-crds.sh"))
	if err != nil {
		t.Fatalf("read apply-crds.sh: %v", err)
	}
	got := string(script)

	for _, want := range []string{
		// Skip unless the release already exists.
		"helm status k8s-aibom --namespace k8s-aibom-system",
		// Exit 0 on that path: a fresh install is not an error.
		"exit 0",
		// Bound the registry read.
		"timeout 90",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("apply-crds.sh missing %q\n%s", want, got)
		}
	}

	// The registry read must go through the bounded wrapper, not directly.
	for _, banned := range []string{
		"$(helm show crds",
		"$(helm show crds ./",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("apply-crds.sh calls %q outside run_bounded; an unbounded "+
				"registry read hangs the rollout instead of failing it\n%s", banned, got)
		}
	}
}

// assertExecutable fails when path is not executable. deploy.sh and the
// helmfile presync hook both invoke the script through `bash`, but an
// operator running it directly is the documented fallback.
func assertExecutable(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s mode = %v, want executable", path, info.Mode().Perm())
	}
}
