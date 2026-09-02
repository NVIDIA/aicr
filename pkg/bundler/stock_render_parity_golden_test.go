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
	"os"
	"path/filepath"
	"sort"
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
	// version so the digest is a pure function of the catalog and the render
	// logic, not of the release the test happens to run on.
	stockRenderVersion = "stock-render-golden"

	// renderErrorSentinel stands in for a leaf that resolved but failed to
	// render, so a leaf flipping between renderable and erroring flips the
	// golden rather than dropping out of the comparison.
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

// TestStockRenderParityGolden pins the rendered bundle bytes of every leaf
// recipe in the embedded catalog.
//
// Companion to TestCatalogParityGolden in pkg/recipe, and NOT redundant with
// it. Resolution records which components a recipe selects and which values
// FILE each one points at; rendering reads that file's CONTENT, applies the
// registry's scheduling paths, and lays out per-deployer artifacts. A change
// to recipes/components/<name>/values.yaml therefore moves this golden while
// leaving the resolution golden untouched. Together the two cover #2240's
// "resolved and rendered bytes remain unchanged".
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

	got := make(map[string]string, len(leaves))
	for _, leaf := range leaves {
		name := leaf.Entry.Name
		if leaf.Err != nil {
			// Resolution failures are TestCatalogParityGolden's business;
			// this golden has nothing to render for such a leaf.
			t.Errorf("leaf %q failed to resolve: %v", name, leaf.Err)
			continue
		}
		digest, renderErr := renderLeafDigest(ctx, t, leaf.Result)
		if renderErr != nil {
			t.Errorf("leaf %q failed to render: %v", name, renderErr)
			got[name] = renderErrorSentinel
			continue
		}
		got[name] = digest
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
	want := map[string]string{}
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	for _, name := range sortedStringKeys(got) {
		w, ok := want[name]
		if !ok {
			t.Errorf("leaf %q is not in the golden (new overlay?) — regenerate deliberately", name)
			continue
		}
		if w != got[name] {
			t.Errorf("leaf %q rendered bytes changed: golden %s, now %s\n"+
				"If this change was intended, regenerate with AICR_UPDATE_GOLDEN=1 and justify the diff "+
				"in the PR. If it was not, a change meant to be scoped to one component has leaked into "+
				"this recipe's bundle.", name, w, got[name])
		}
	}
	for _, name := range sortedStringKeys(want) {
		if _, ok := got[name]; !ok {
			t.Errorf("golden leaf %q is no longer produced (overlay removed?) — regenerate deliberately", name)
		}
	}
}

// renderLeafDigest bundles one resolved recipe and returns a digest over the
// full emitted tree.
//
// The scheduling, storage, and node-count inputs below are fixed synthetic
// values, applied identically to every leaf. Several components' bundle
// contracts require them, and supplying them is what lets the golden cover the
// injection paths rather than only the components that need no input. Their
// exact values are irrelevant; that they never vary is what matters.
func renderLeafDigest(ctx context.Context, t *testing.T, rr *recipe.RecipeResult) (string, error) {
	t.Helper()

	cfg := config.NewConfig(
		config.WithDeployer(config.DeployerHelm),
		config.WithVersion(stockRenderVersion),
		// Suppresses wall-clock timestamps and derives attestation
		// invocation IDs, without which two runs never agree.
		config.WithDeterministic(true),
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
		return "", fmt.Errorf("new bundler: %w", err)
	}

	outputDir := t.TempDir()
	if _, err := b.Make(ctx, rr, outputDir); err != nil {
		return "", fmt.Errorf("make: %w", err)
	}
	return digestTree(outputDir)
}

// digestTree hashes a directory into one stable digest: the sorted set of
// relative paths, each paired with the SHA-256 of its contents. Paths are
// included so a file rename moves the digest even when total bytes are
// unchanged, and sorting removes the filesystem's walk-order from the result.
func digestTree(root string) (string, error) {
	type entry struct {
		path   string
		digest string
	}
	var entries []entry

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
		entries = append(entries, entry{path: filepath.ToSlash(rel), digest: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("bundle tree at %s is empty", root)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s %s\n", e.path, e.digest)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeStockRenderGolden(t *testing.T, got map[string]string) {
	t.Helper()
	raw, err := serializer.MarshalYAMLDeterministic(got)
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	header := []byte("# Generated by TestStockRenderParityGolden. Do not hand-edit.\n" +
		"# Regenerate: AICR_UPDATE_GOLDEN=1 go test ./pkg/bundler/ -run TestStockRenderParityGolden\n" +
		"#\n" +
		"# One entry per leaf overlay: a digest over its fully rendered helm-deployer\n" +
		"# bundle tree (sorted relative paths paired with per-file content hashes).\n")
	if err := os.WriteFile(stockRenderGoldenPath, append(header, raw...), 0o600); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
