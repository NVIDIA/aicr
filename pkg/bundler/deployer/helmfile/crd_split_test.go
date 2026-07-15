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

// Tests for the helmfile bundler's dependency-level partition. The
// stratified layout (issue #914) emits one sub-helmfile per DAG level
// computed from recipe ComponentRef.DependencyRefs. Sub-helmfiles are
// processed sequentially by `helmfile`, so by the time level K diffs,
// every release in levels 0..K-1 has fully applied (CRDs registered,
// REST mapper warm).

package helmfile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// testGenerateTimeout bounds the context for in-test Generate calls.
// Generation is local file I/O over a small fixture, so 30s is well
// over the realistic ceiling; the timeout is here to enforce the
// "I/O methods always carry a deadline" project rule, not to gate on
// performance. See pkg/bundler/deployer/helmfile docstring.
const testGenerateTimeout = 30 * time.Second

// TestSplitFoldersByLevel pins the partition logic for the stratified
// layout (issue #914). Folders inherit their parent's DAG level so
// auxiliary -pre / -post folders travel with the primary, and all
// folders at level i land in out[i].
func TestSplitFoldersByLevel(t *testing.T) {
	folders := []localformat.Folder{
		{Dir: "001-cert-manager-pre", Parent: "cert-manager"},
		{Dir: "002-cert-manager", Parent: "cert-manager"},
		{Dir: "003-cert-manager-post", Parent: "cert-manager"},
		{Dir: "004-gpu-operator", Parent: "gpu-operator"},
		{Dir: "005-gpu-operator-post", Parent: "gpu-operator"},
		{Dir: "006-nodewright-operator", Parent: "nodewright-operator"},
		{Dir: "007-nodewright-customizations", Parent: "nodewright-customizations"},
	}
	levels := [][]string{
		{"cert-manager", "nodewright-operator"},
		{"gpu-operator", "nodewright-customizations"},
	}

	got := splitFoldersByLevel(folders, levels)

	if len(got) != 2 {
		t.Fatalf("level count = %d, want 2", len(got))
	}
	wantLevel0 := []string{
		"001-cert-manager-pre",
		"002-cert-manager",
		"003-cert-manager-post",
		"006-nodewright-operator",
	}
	wantLevel1 := []string{
		"004-gpu-operator",
		"005-gpu-operator-post",
		"007-nodewright-customizations",
	}
	if dirs := dirsOf(got[0]); !equalStringSlices(dirs, wantLevel0) {
		t.Errorf("level[0] dirs = %v, want %v", dirs, wantLevel0)
	}
	if dirs := dirsOf(got[1]); !equalStringSlices(dirs, wantLevel1) {
		t.Errorf("level[1] dirs = %v, want %v", dirs, wantLevel1)
	}
}

// TestSplitFoldersByLevel_EmptyLevels covers the defensive fallback:
// when levels is empty (recipe with no components, or topological-sort
// failure) the partition returns a single bucket containing all
// folders so callers see a non-empty partition rather than silently
// dropping work.
func TestSplitFoldersByLevel_EmptyLevels(t *testing.T) {
	folders := []localformat.Folder{
		{Dir: "001-foo", Parent: "foo"},
		{Dir: "002-bar", Parent: "bar"},
	}
	got := splitFoldersByLevel(folders, nil)
	if len(got) != 1 {
		t.Fatalf("fallback partition count = %d, want 1", len(got))
	}
	if len(got[0]) != len(folders) {
		t.Errorf("fallback level[0] has %d folders, want %d", len(got[0]), len(folders))
	}
}

// TestSplitFoldersByLevel_UnknownParentLandsInLevel0 documents the
// defensive behavior: a folder whose parent isn't represented in any
// level (e.g., a wrapper for a name the DAG doesn't know) is placed
// at level 0 instead of being silently dropped.
func TestSplitFoldersByLevel_UnknownParentLandsInLevel0(t *testing.T) {
	folders := []localformat.Folder{
		{Dir: "001-known", Parent: "known"},
		{Dir: "002-orphan", Parent: "phantom"},
	}
	levels := [][]string{{"known"}}
	got := splitFoldersByLevel(folders, levels)
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("expected both folders in level[0], got %v", got)
	}
}

// TestGenerate_MultiLevelLayout drives Generator.Generate end-to-end
// with a two-component recipe where gpu-operator depends on cert-manager.
// Asserts the stratified layout: top-level helmfile.yaml carries a
// helmfiles: list referencing level-0.yaml + level-1.yaml in order,
// each sub-helmfile is a valid leaf, and the cross-level needs: edge
// dissolves (sub-helmfile sequencing handles cross-level ordering).
func TestGenerate_MultiLevelLayout(t *testing.T) {
	cm := ref("cert-manager", "cert-manager", "cert-manager", "v1.17.2", "https://charts.jetstack.io")
	gpu := ref("gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3", "https://helm.ngc.nvidia.com/nvidia")
	gpu.DependencyRefs = []string{"cert-manager"}
	g := &Generator{
		RecipeResult: recipeWith(cm, gpu),
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

	// Top-level helmfile.yaml carries the helmfiles: list.
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
	wantPaths := []string{"level-0.yaml", "level-1.yaml"}
	for i, want := range wantPaths {
		if top.Helmfiles[i].Path != want {
			t.Errorf("helmfiles[%d] = %q, want %q (dependency-order sequence is load-bearing)",
				i, top.Helmfiles[i].Path, want)
		}
	}

	// Each level sub-helmfile holds the expected release.
	tests := []struct {
		file        string
		wantRelease string
	}{
		{"level-0.yaml", "cert-manager"},
		{"level-1.yaml", "gpu-operator"},
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
				t.Errorf("%s release %q has unexpected needs %v (cross-level edges must dissolve)",
					tt.file, tt.wantRelease, sub.Releases[0].Needs)
			}
		})
	}
}

// TestGenerate_SingleLevelCollapse pins the optimization where a bundle
// whose DAG produces only one non-empty level (every component
// independent or only one component) collapses to a single
// helmfile.yaml — no sub-helmfile sequencing because there's no
// ordering work to do.
func TestGenerate_SingleLevelCollapse(t *testing.T) {
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

	// helmfile.yaml must be a leaf, not a TopHelmfile with helmfiles: list.
	data, err := os.ReadFile(filepath.Join(outputDir, fileHelmfile))
	if err != nil {
		t.Fatalf("read helmfile.yaml: %v", err)
	}
	var top TopHelmfile
	if err := yaml.Unmarshal(data, &top); err != nil {
		t.Fatalf("parse helmfile.yaml as top-level doc: %v", err)
	}
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

	// No level-*.yaml should exist.
	assertLevelSubHelmfilesAbsent(t, outputDir, "single-level collapse path")
}

// TestGenerate_IndependentComponentsCollapse covers a recipe with
// multiple components but no inter-component dependencies. All
// components land at level 0; partition is single-level; layout
// collapses to one file.
func TestGenerate_IndependentComponentsCollapse(t *testing.T) {
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
	assertLevelSubHelmfilesAbsent(t, outputDir, "independent-components bundle")

	// Default (parallel): mutually independent components carry NO
	// cross-component needs:, so helmfile applies them concurrently.
	for _, r := range readHelmfileReleases(t, outputDir) {
		if len(r.Needs) != 0 {
			t.Errorf("release %q has needs %v; independent components must not chain in default (parallel) mode", r.Name, r.Needs)
		}
	}
}

// TestGenerate_SerialChainsIndependentComponents is the --serial counterpart:
// the same two independent components are chained into a single linear apply
// order (cert-manager -> gpu-operator) so they deploy one at a time.
func TestGenerate_SerialChainsIndependentComponents(t *testing.T) {
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
		Serial:  true,
	}
	outputDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), testGenerateTimeout)
	defer cancel()
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	releases := readHelmfileReleases(t, outputDir)
	byName := make(map[string]Release, len(releases))
	for _, r := range releases {
		byName[r.Name] = r
	}
	if got := byName["cert-manager"].Needs; len(got) != 0 {
		t.Errorf("cert-manager (chain head) needs = %v, want none", got)
	}
	// Different namespaces => the needs entry is namespace-qualified.
	if got := byName["gpu-operator"].Needs; len(got) != 1 || got[0] != "cert-manager/cert-manager" {
		t.Errorf("gpu-operator needs = %v, want [cert-manager/cert-manager] under --serial", got)
	}
}

// readHelmfileReleases parses the leaf helmfile.yaml in outputDir and returns
// its releases. Fails the test if the file is missing or is a top-level
// helmfiles: orchestration document — otherwise a stratified bundle would
// deserialize to an empty Releases slice (yaml.v3 ignores the unknown
// helmfiles: key) and make callers pass vacuously.
func readHelmfileReleases(t *testing.T, outputDir string) []Release {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, fileHelmfile))
	if err != nil {
		t.Fatalf("read helmfile.yaml: %v", err)
	}
	var top TopHelmfile
	if err := yaml.Unmarshal(data, &top); err != nil {
		t.Fatalf("parse helmfile.yaml: %v", err)
	}
	if len(top.Helmfiles) > 0 {
		t.Fatalf("readHelmfileReleases expects a leaf helmfile.yaml, got a top-level helmfiles: doc with %d entries", len(top.Helmfiles))
	}
	var doc Helmfile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse helmfile.yaml: %v", err)
	}
	return doc.Releases
}

// assertLevelSubHelmfilesAbsent fails the test if any level-*.yaml is
// present in outputDir. Used to pin the single-file-collapse branch.
// Checks level-0.yaml..level-9.yaml (well beyond any realistic depth).
func assertLevelSubHelmfilesAbsent(t *testing.T, outputDir, context string) {
	t.Helper()
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatalf("read outputDir %s: %v", outputDir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), fileLevelHelmfilePrefix) && strings.HasSuffix(e.Name(), ".yaml") {
			t.Errorf("unexpected %s present in %s (single-level collapse should not emit per-level files)",
				e.Name(), context)
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
