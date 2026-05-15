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

package helmfile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// testGenerateTimeout bounds the context for in-test Generate calls.
// Generation is local file I/O over a small fixture, so 30s is well
// over the realistic ceiling; the timeout is here to enforce the
// "I/O methods always carry a deadline" project rule, not to gate on
// performance. See pkg/bundler/deployer/helmfile docstring.
const testGenerateTimeout = 30 * time.Second

// TestSplitFoldersByCRD pins the partition logic for issue #914.
// Auxiliary -pre / -post folders inherit their parent's classification
// so the three travel together.
func TestSplitFoldersByCRD(t *testing.T) {
	folders := []localformat.Folder{
		{Dir: "001-cert-manager-pre", Parent: "cert-manager"},
		{Dir: "002-cert-manager", Parent: "cert-manager"},
		{Dir: "003-cert-manager-post", Parent: "cert-manager"},
		{Dir: "004-gpu-operator", Parent: "gpu-operator"},
		{Dir: "005-gpu-operator-post", Parent: "gpu-operator"},
		{Dir: "006-nodewright-operator", Parent: "nodewright-operator"},
		{Dir: "007-nodewright-customizations", Parent: "nodewright-customizations"},
	}
	crdSet := map[string]bool{
		"cert-manager":        true,
		"nodewright-operator": true,
	}

	crd, main := splitFoldersByCRD(folders, crdSet)

	wantCRDDirs := []string{
		"001-cert-manager-pre",
		"002-cert-manager",
		"003-cert-manager-post",
		"006-nodewright-operator",
	}
	wantMainDirs := []string{
		"004-gpu-operator",
		"005-gpu-operator-post",
		"007-nodewright-customizations",
	}
	if got := dirsOf(crd); !equalStringSlices(got, wantCRDDirs) {
		t.Errorf("crd dirs = %v, want %v", got, wantCRDDirs)
	}
	if got := dirsOf(main); !equalStringSlices(got, wantMainDirs) {
		t.Errorf("main dirs = %v, want %v", got, wantMainDirs)
	}
}

// TestSplitFoldersByCRD_NilCRDSet covers the "no CRD owners in this
// recipe" path: every folder lands in main, crd is empty. The
// downstream Generate switch then takes the single-file layout branch.
func TestSplitFoldersByCRD_NilCRDSet(t *testing.T) {
	folders := []localformat.Folder{
		{Dir: "001-foo", Parent: "foo"},
		{Dir: "002-bar", Parent: "bar"},
	}
	crd, main := splitFoldersByCRD(folders, nil)
	if len(crd) != 0 {
		t.Errorf("crd = %v, want empty", crd)
	}
	if len(main) != 2 {
		t.Errorf("main len = %d, want 2", len(main))
	}
}

// TestGenerate_SplitLayout drives Generator.Generate end-to-end with a
// cert-manager + gpu-operator recipe (the canonical mixed case) and
// asserts the three-file layout lands on disk with the expected
// pointers. Complements TestGenerate_Scenarios/upstream_helm_only,
// which pins the golden-file content; this test pins the runtime
// structure independently so a future refactor that swaps the golden
// shape can't silently break the helmfiles: list.
func TestGenerate_SplitLayout(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
			ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
		),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		Version: testBundlerVersion,
	}
	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), testGenerateTimeout)
	defer cancel()
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Top-level helmfile.yaml must be the multi-helmfiles document.
	topData, err := os.ReadFile(filepath.Join(outputDir, fileHelmfile))
	if err != nil {
		t.Fatalf("read top helmfile.yaml: %v", err)
	}
	var top TopHelmfile
	if err := yaml.Unmarshal(topData, &top); err != nil {
		t.Fatalf("parse top helmfile.yaml: %v", err)
	}
	if len(top.Helmfiles) != 2 {
		t.Fatalf("top helmfiles len = %d, want 2; doc:\n%s", len(top.Helmfiles), topData)
	}
	if top.Helmfiles[0].Path != fileCRDsHelmfile {
		t.Errorf("helmfiles[0] = %q, want %q (CRD layer must come first)",
			top.Helmfiles[0].Path, fileCRDsHelmfile)
	}
	if top.Helmfiles[1].Path != fileMainHelmfile {
		t.Errorf("helmfiles[1] = %q, want %q", top.Helmfiles[1].Path, fileMainHelmfile)
	}

	// Sub-helmfiles must each parse as a leaf Helmfile with the right releases.
	tests := []struct {
		file        string
		wantRelease string
	}{
		{fileCRDsHelmfile, "cert-manager"},
		{fileMainHelmfile, "gpu-operator"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(outputDir, tt.file))
			if err != nil {
				t.Fatalf("read %s: %v", tt.file, err)
			}
			var sub Helmfile
			if err := yaml.Unmarshal(data, &sub); err != nil {
				t.Fatalf("parse %s: %v", tt.file, err)
			}
			if len(sub.Releases) != 1 {
				t.Fatalf("%s releases len = %d, want 1", tt.file, len(sub.Releases))
			}
			if sub.Releases[0].Name != tt.wantRelease {
				t.Errorf("%s release name = %q, want %q",
					tt.file, sub.Releases[0].Name, tt.wantRelease)
			}
			if len(sub.Releases[0].Needs) != 0 {
				t.Errorf("%s release %q has unexpected needs %v (the cross-sub-helmfile edge must dissolve)",
					tt.file, tt.wantRelease, sub.Releases[0].Needs)
			}
		})
	}
}

// TestGenerate_AllCRD_CollapsesToSingleFile pins the edge case where
// every referenced component installs CRDs: the split would have no
// main layer to add value, so the generator collapses back to the
// legacy single-file layout instead of emitting an empty
// releases.yaml. Regression guard so this branch doesn't silently
// shift to the multi-file path.
func TestGenerate_AllCRD_CollapsesToSingleFile(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io"),
		),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
		},
		Version: testBundlerVersion,
	}
	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), testGenerateTimeout)
	defer cancel()
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// helmfile.yaml must be a leaf, not a TopHelmfile.
	data, err := os.ReadFile(filepath.Join(outputDir, fileHelmfile))
	if err != nil {
		t.Fatalf("read helmfile.yaml: %v", err)
	}
	var top TopHelmfile
	_ = yaml.Unmarshal(data, &top)
	if len(top.Helmfiles) > 0 {
		t.Errorf("expected leaf helmfile.yaml (no helmfiles: list), got top-level:\n%s", data)
	}
	var sub Helmfile
	if err := yaml.Unmarshal(data, &sub); err != nil {
		t.Fatalf("parse helmfile.yaml as leaf: %v", err)
	}
	if len(sub.Releases) != 1 || sub.Releases[0].Name != "cert-manager" {
		t.Errorf("releases = %+v, want exactly cert-manager", sub.Releases)
	}

	// crds.yaml / releases.yaml must NOT have been emitted.
	assertSubHelmfilesAbsent(t, outputDir, "single-file collapse path")
}

// TestGenerate_NoCRD_KeepsSingleFile is the inverse guard: a recipe
// with zero InstallsCRDs components stays on the legacy single-file
// layout. Catches a regression where every Generate call accidentally
// flipped to the split path.
func TestGenerate_NoCRD_KeepsSingleFile(t *testing.T) {
	g := &Generator{
		RecipeResult: recipeWith(
			ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia"),
		),
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		Version: testBundlerVersion,
	}
	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), testGenerateTimeout)
	defer cancel()
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	assertSubHelmfilesAbsent(t, outputDir, "no-CRD bundle")
}

// assertSubHelmfilesAbsent fails the test if the split-layout
// sub-helmfiles are present in outputDir. Distinguishes "file
// exists" (test failure) from any other os.Stat error (test fatal —
// hides real filesystem problems if treated as "absent").
func assertSubHelmfilesAbsent(t *testing.T, outputDir, context string) {
	t.Helper()
	for _, name := range []string{fileCRDsHelmfile, fileMainHelmfile} {
		path := filepath.Join(outputDir, name)
		_, err := os.Stat(path)
		switch {
		case err == nil:
			t.Errorf("unexpected %s present in %s", name, context)
		case os.IsNotExist(err):
			// Expected — the absence-of-sub-helmfile invariant.
		default:
			t.Fatalf("stat %s: %v", path, err)
		}
	}
}

// === helpers ===

func dirsOf(folders []localformat.Folder) []string {
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.Dir)
	}
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Ensure the recipe package's CRD-marked components stay in the
// registry. A renamed/removed component without a corresponding test
// update would silently disable the issue #914 fix for that name.
// Sentinel guard rather than a full registry audit.
func TestRegistry_CRDOwnersStillMarked(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	mustBeMarked := []string{
		"cert-manager",
		"nodewright-operator",
		"kube-prometheus-stack",
		"nvsentinel",
		"agentgateway-crds",
		"slinky-slurm-operator-crds",
	}
	for _, name := range mustBeMarked {
		cfg := registry.Get(name)
		if cfg == nil {
			t.Errorf("registry missing component %q (rename or deletion?)", name)
			continue
		}
		if !cfg.InstallsCRDs {
			t.Errorf("component %q must keep installsCRDs: true (issue #914)", name)
		}
	}
}
