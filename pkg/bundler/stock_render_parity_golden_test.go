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

package bundler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	stockRenderGoldenPath = "testdata/stock_render_golden.yaml"

	// stockRenderVersion pins both the recipe builder version and the bundler
	// version so the digests are a pure function of the catalog and the render
	// logic, not of the release the test happens to run on.
	stockRenderVersion = "stock-render-golden"

	// renderErrorSentinel stands in for a leaf that resolved but failed to
	// render, so a leaf flipping between renderable and erroring flips the
	// golden rather than dropping out of the comparison. It is stored as the
	// leaf's only entry, keyed and valued by the sentinel.
	renderErrorSentinel = "render-error"

	// stockRenderBudget bounds the ENTIRE render loop, not one leaf.
	//
	// A per-leaf cap does not bound the test: 45 leaves times a 60s cap is 45
	// minutes, well past the 10m -timeout in .settings.yaml, so a wedged
	// bundler would blow the package timeout and produce a panic stack instead
	// of a named test failure. One loop-wide budget bounds the worst case at a
	// known value no matter how the catalog grows. Measured runtime is ~12s
	// without -race, so this is roughly 25x headroom.
	stockRenderBudget = 5 * time.Minute
)

// renderGolden maps each leaf overlay name to its rendered files: bundle
// relative path to SHA-256 of the file's contents.
type renderGolden map[string]map[string]string

// TestStockRenderParityGolden pins the rendered bundle bytes of every leaf
// recipe in the embedded catalog, one digest per rendered file.
//
// Companion to TestCatalogParityGolden in pkg/recipe, and NOT redundant with
// it. Resolution records which components a recipe selects and which values
// FILE each one points at; rendering reads that file's CONTENT, applies the
// registry's scheduling paths, and lays out per-deployer artifacts. A change
// to recipes/components/<name>/values.yaml therefore moves this golden while
// leaving the resolution golden untouched. Together the two cover #2240's
// "resolved and rendered bytes remain unchanged".
//
// The golden is keyed per file rather than collapsed to one digest per leaf
// so its diff names what moved: a shared template edit shows up as the same
// path changing under every leaf that renders it, while a values change shows
// up under the leaves that select that component (#2810). Paths keep their
// install-order prefix because a reordering is a real bundle change.
//
// Hermetic: the helm deployer emits values and manifests that reference the
// upstream chart by coordinates. Chart bytes are only fetched on the
// --vendor-charts path, which this test does not take, so no network or helm
// binary is involved. Vendored rendering is covered separately by the
// localformat writer tests, which inject a stub ChartPuller.
//
// Regenerate deliberately, and only when a bundle change is intended:
//
//	AICR_UPDATE_GOLDEN=1 go test ./pkg/bundler/ -run TestStockRenderParityGolden
func TestStockRenderParityGolden(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), stockRenderBudget)
	defer cancel()

	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{
		Version: stockRenderVersion,
	})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	if len(leaves) == 0 {
		t.Fatal("catalog resolved zero leaves; a golden over an empty set proves nothing")
	}

	got := make(renderGolden, len(leaves))
	for _, leaf := range leaves {
		name := leaf.Entry.Name
		if leaf.Err != nil {
			// Resolution failures are TestCatalogParityGolden's business;
			// this golden has nothing to render for such a leaf.
			t.Errorf("leaf %q failed to resolve: %v", name, leaf.Err)
			continue
		}
		files, renderErr := renderLeafFileDigests(ctx, t, leaf.Result)
		if renderErr != nil {
			t.Errorf("leaf %q failed to render: %v", name, renderErr)
			got[name] = renderErrorLeaf()
			continue
		}
		got[name] = files
	}

	if os.Getenv("AICR_UPDATE_GOLDEN") == "1" {
		if t.Failed() {
			t.Fatal("not writing golden: one or more leaves failed to resolve or render (see errors above)")
		}
		writeStockRenderGolden(t, got)
		t.Logf("golden updated: %d leaves", len(got))
		return
	}

	raw, err := os.ReadFile(stockRenderGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with AICR_UPDATE_GOLDEN=1 to create): %v", err)
	}
	want := renderGolden{}
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	fileDrift := false
	for _, d := range diffRenderGolden(want, got) {
		switch d.Kind {
		case driftLeafAdded:
			t.Errorf("leaf %q is not in the golden (new overlay?) — regenerate deliberately", d.Leaf)
		case driftLeafRemoved:
			t.Errorf("golden leaf %q is no longer produced (overlay removed?) — regenerate deliberately", d.Leaf)
		case driftRenderState:
			state := "renderable"
			if isRenderErrorLeaf(got[d.Leaf]) {
				state = "erroring"
			}
			t.Errorf("leaf %q changed render state: now %s", d.Leaf, state)
		default:
			fileDrift = true
			t.Errorf("leaf %q: %s %s", d.Leaf, d.Kind, d.Path)
		}
	}
	if fileDrift {
		t.Error("Rendered bytes changed for the files listed above. If this change was intended, " +
			"regenerate with AICR_UPDATE_GOLDEN=1 and justify the diff in the PR. If it was not, a change " +
			"meant to be scoped to one component has leaked into these bundles.")
	}
}

// Kinds of drift diffRenderGolden reports, from coarsest to finest.
const (
	driftLeafAdded   = "leaf added"
	driftLeafRemoved = "leaf removed"
	driftRenderState = "render state changed"
	driftFileAdded   = "file added"
	driftFileRemoved = "file removed"
	driftFileChanged = "file changed"
)

// renderDrift is one difference between the golden and the current render.
// Path is empty for leaf-level kinds.
type renderDrift struct {
	Leaf string
	Kind string
	Path string
}

// diffRenderGolden compares two goldens and reports every difference in a
// deterministic order: leaves ascending, then paths ascending within a leaf.
// A leaf that flipped between renderable and erroring reports a single
// render-state drift rather than every file as added or removed.
func diffRenderGolden(want, got renderGolden) []renderDrift {
	var out []renderDrift
	leaves := slices.Sorted(maps.Keys(want))
	for leaf := range got {
		if _, ok := want[leaf]; !ok {
			leaves = append(leaves, leaf)
		}
	}
	slices.Sort(leaves)

	for _, leaf := range leaves {
		w, inWant := want[leaf]
		g, inGot := got[leaf]
		switch {
		case !inWant:
			out = append(out, renderDrift{Leaf: leaf, Kind: driftLeafAdded})
			continue
		case !inGot:
			out = append(out, renderDrift{Leaf: leaf, Kind: driftLeafRemoved})
			continue
		case isRenderErrorLeaf(w) != isRenderErrorLeaf(g):
			out = append(out, renderDrift{Leaf: leaf, Kind: driftRenderState})
			continue
		}

		paths := slices.Sorted(maps.Keys(w))
		for p := range g {
			if _, ok := w[p]; !ok {
				paths = append(paths, p)
			}
		}
		slices.Sort(paths)
		for _, p := range paths {
			wd, inW := w[p]
			gd, inG := g[p]
			switch {
			case !inW:
				out = append(out, renderDrift{Leaf: leaf, Kind: driftFileAdded, Path: p})
			case !inG:
				out = append(out, renderDrift{Leaf: leaf, Kind: driftFileRemoved, Path: p})
			case wd != gd:
				out = append(out, renderDrift{Leaf: leaf, Kind: driftFileChanged, Path: p})
			}
		}
	}
	return out
}

func renderErrorLeaf() map[string]string {
	return map[string]string{renderErrorSentinel: renderErrorSentinel}
}

func isRenderErrorLeaf(files map[string]string) bool {
	return len(files) == 1 && files[renderErrorSentinel] == renderErrorSentinel
}

// renderLeafFileDigests bundles one resolved recipe and returns a digest per
// file in the emitted tree.
//
// The scheduling, storage, and node-count inputs below are fixed synthetic
// values, applied identically to every leaf. Several components' bundle
// contracts require them, and supplying them is what lets the golden cover the
// injection paths rather than only the components that need no input. Their
// exact values are irrelevant; that they never vary is what matters.
func renderLeafFileDigests(ctx context.Context, t *testing.T, rr *recipe.RecipeResult) (map[string]string, error) {
	t.Helper()

	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerHelm),
		config.WithVersion(stockRenderVersion),
		// Suppresses wall-clock timestamps and derives attestation
		// invocation IDs, without which two runs never agree.
		config.WithDeterministic(true),
		config.WithSystemNodeSelector(map[string]string{"nodeGroup": "system"}),
		config.WithAcceleratedNodeSelector(map[string]string{"nvidia.com/gpu.present": "true"}),
		config.WithAcceleratedNodeTolerations([]corev1.Toleration{{
			Key:      "nvidia.com/gpu",
			Operator: corev1.TolerationOpEqual,
			Value:    "present",
			Effect:   corev1.TaintEffectNoSchedule,
		}}),
		config.WithWorkloadSelector(map[string]string{"aicr.nvidia.com/parity-test": "true"}),
		config.WithStorageClass("aicr-parity-rwo"),
		config.WithSharedStorageClass("aicr-parity-rwx"),
		config.WithEstimatedNodeCount(3),
	)

	b, err := New(WithConfig(cfg))
	if err != nil {
		return nil, fmt.Errorf("new bundler: %w", err)
	}

	outputDir := t.TempDir()
	if _, err := b.Make(ctx, rr, outputDir); err != nil {
		return nil, fmt.Errorf("make: %w", err)
	}
	return digestTreeFiles(outputDir)
}

// digestTreeFiles hashes every file under root, keyed by slash-separated
// relative path, so a rename is visible as a removed and an added path even
// when total bytes are unchanged.
func digestTreeFiles(root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		content, readErr := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over a test temp dir.
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(content)
		files[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("bundle tree at %s is empty", root)
	}
	return files, nil
}

func writeStockRenderGolden(t *testing.T, got renderGolden) {
	t.Helper()
	raw, err := serializer.MarshalYAMLDeterministic(got)
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	header := []byte("# Generated by TestStockRenderParityGolden. Do not hand-edit.\n" +
		"# Regenerate: AICR_UPDATE_GOLDEN=1 go test ./pkg/bundler/ -run TestStockRenderParityGolden\n" +
		"#\n" +
		"# One entry per leaf overlay, mapping each file in its fully rendered\n" +
		"# helm-deployer bundle tree (relative path, install-order prefix kept) to\n" +
		"# the sha256 of that file's contents. A leaf that failed to render is\n" +
		"# recorded as the single entry `render-error: render-error`.\n")
	if err := os.WriteFile(stockRenderGoldenPath, append(header, raw...), 0o600); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

func TestDiffRenderGolden(t *testing.T) {
	base := renderGolden{
		"leaf-a": {"001-x/values.yaml": "aa", "001-x/deploy.sh": "bb"},
		"leaf-b": {"001-x/values.yaml": "cc"},
	}
	clone := func(mutate func(renderGolden)) renderGolden {
		g := renderGolden{}
		for leaf, files := range base {
			g[leaf] = maps.Clone(files)
		}
		mutate(g)
		return g
	}

	tests := []struct {
		name string
		got  renderGolden
		want []renderDrift
	}{
		{"identical", clone(func(renderGolden) {}), nil},
		{
			"file added",
			clone(func(g renderGolden) { g["leaf-a"]["001-x/new.yaml"] = "dd" }),
			[]renderDrift{{Leaf: "leaf-a", Kind: driftFileAdded, Path: "001-x/new.yaml"}},
		},
		{
			"file removed",
			clone(func(g renderGolden) { delete(g["leaf-a"], "001-x/deploy.sh") }),
			[]renderDrift{{Leaf: "leaf-a", Kind: driftFileRemoved, Path: "001-x/deploy.sh"}},
		},
		{
			"file changed in every leaf that renders it, reported per leaf in order",
			clone(func(g renderGolden) {
				g["leaf-b"]["001-x/values.yaml"] = "zz"
				g["leaf-a"]["001-x/values.yaml"] = "zz"
			}),
			[]renderDrift{
				{Leaf: "leaf-a", Kind: driftFileChanged, Path: "001-x/values.yaml"},
				{Leaf: "leaf-b", Kind: driftFileChanged, Path: "001-x/values.yaml"},
			},
		},
		{
			"leaf added",
			clone(func(g renderGolden) { g["leaf-c"] = map[string]string{"001-x/values.yaml": "ee"} }),
			[]renderDrift{{Leaf: "leaf-c", Kind: driftLeafAdded}},
		},
		{
			"leaf removed",
			clone(func(g renderGolden) { delete(g, "leaf-b") }),
			[]renderDrift{{Leaf: "leaf-b", Kind: driftLeafRemoved}},
		},
		{
			"leaf started erroring reports one render-state drift, not every file removed",
			clone(func(g renderGolden) { g["leaf-a"] = renderErrorLeaf() }),
			[]renderDrift{{Leaf: "leaf-a", Kind: driftRenderState}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diffRenderGolden(base, tt.got); !slices.Equal(got, tt.want) {
				t.Errorf("diffRenderGolden() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("leaf recovered from erroring reports one render-state drift", func(t *testing.T) {
		errWant := renderGolden{"leaf-a": renderErrorLeaf()}
		got := renderGolden{"leaf-a": {"001-x/values.yaml": "aa"}}
		want := []renderDrift{{Leaf: "leaf-a", Kind: driftRenderState}}
		if d := diffRenderGolden(errWant, got); !slices.Equal(d, want) {
			t.Errorf("diffRenderGolden() = %v, want %v", d, want)
		}
	})
}
