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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
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

// renderGolden maps each leaf overlay or opt-in fixture name to its rendered
// files, keyed by bundle-relative path with the SHA-256 of the file's contents
// as the value.
type renderGolden map[string]map[string]string

// stockRenderOptIn renders one leaf a second time with a recipe-time or
// bundle-time opt-in applied. Its digests land in the golden under name,
// beside the leaf's default-path entry.
type stockRenderOptIn struct {
	name string
	leaf string
	// overlay and build re-resolve the leaf with these additions. Nil for both
	// reuses the leaf's stock resolution.
	overlay *overlayAddition
	build   []recipe.BuildOption
	opts    []config.Option
	// covers names the opt-in components this fixture exists for. The fixture
	// fails unless each of them renders, so a catalog change that drops a
	// prerequisite cannot turn it into a silent copy of the leaf.
	covers []string
}

// overlayAddition is a user-authored overlay that inherits a stock leaf and
// adds mixins and componentRefs to it.
type overlayAddition struct {
	mixins        []string
	componentRefs []recipe.ComponentRef
}

// stockRenderOptIns lists the opt-in renders the golden digests. Stock leaves
// pin only the default path, so a component that only a user overlay, a recipe
// option, or a bundle flag selects renders nothing in them and needs an entry
// here to be covered.
var stockRenderOptIns = []stockRenderOptIn{
	{
		name:   "h100-eks-ubuntu-training-kubeflow+dra-eviction-node-label",
		leaf:   "h100-eks-ubuntu-training-kubeflow",
		opts:   []config.Option{config.WithDRAEvictionNodeLabel(config.DefaultDRAEvictionNodeLabel())},
		covers: []string{draNodeLabelerComponentName},
	},
	{
		name:   "h100-eks-ubuntu-training-slurm+slurm-accounting-aicr-provided",
		leaf:   "h100-eks-ubuntu-training-slurm",
		build:  []recipe.BuildOption{recipe.WithAccountingMode(recipe.AccountingModeAICRProvided)},
		covers: []string{"mariadb-operator-crds", "mariadb-operator", "slurm-accounting-mariadb"},
	},
	{
		name: "h100-eks-ubuntu-training-kubeflow+user-overlay-components",
		leaf: "h100-eks-ubuntu-training-kubeflow",
		overlay: &overlayAddition{
			mixins: []string{"npd"},
			componentRefs: []recipe.ComponentRef{
				{
					Name:           "nvcre",
					Type:           recipe.ComponentTypeHelm,
					ValuesFile:     "components/nvcre/values.yaml",
					DependencyRefs: []string{"kubeflow-trainer"},
				},
				{Name: "gatekeeper", Type: recipe.ComponentTypeHelm},
			},
		},
		covers: []string{"node-problem-detector", "nvcre", "gatekeeper"},
	},
	{
		name: "h100-eks-ubuntu-training-slurm+nvsentinel-slinky-drainer",
		leaf: "h100-eks-ubuntu-training-slurm",
		overlay: &overlayAddition{
			mixins: []string{"nvsentinel-quarantine", "nvsentinel-slinky-drainer"},
		},
		// slinky-drainer rejects a slurmd selector that no label key separates
		// from the Slurm control pods' system selector.
		opts:   []config.Option{config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"})},
		covers: []string{"nvsentinel-mongodb", "psmdb-operator", "slinky-drainer-crds", "slinky-drainer"},
	},
}

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
// Stock leaves pin only the default render path. A component that only a user
// overlay selects, such as nvcre, or that is gated behind a recipe option,
// such as the Slurm accounting MariaDB stack behind --slurm-accounting-mode,
// or behind a bundle flag, such as dra-node-labeler behind
// --dra-eviction-node-label, renders nothing in any of them and is covered
// only through an entry in stockRenderOptIns. The test fails when a registry
// component renders in no entry at all.
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

	got := make(renderGolden, len(leaves)+len(stockRenderOptIns))
	resolved := make(map[string]recipe.ResolvedLeaf, len(leaves))
	for _, leaf := range leaves {
		name := leaf.Entry.Name
		if leaf.Err != nil {
			// Resolution failures are TestCatalogParityGolden's business;
			// this golden has nothing to render for such a leaf.
			t.Errorf("leaf %q failed to resolve: %v", name, leaf.Err)
			continue
		}
		resolved[name] = leaf
		files, renderErr := renderLeafFileDigests(ctx, t, leaf.Result)
		if renderErr != nil {
			t.Errorf("leaf %q failed to render: %v", name, renderErr)
			got[name] = renderErrorLeaf()
			continue
		}
		got[name] = files
	}

	for _, fx := range stockRenderOptIns {
		leaf, ok := resolved[fx.leaf]
		if !ok {
			t.Errorf("opt-in fixture %q: leaf %q did not resolve (renamed or removed?)", fx.name, fx.leaf)
			continue
		}
		rr := leaf.Result
		if fx.overlay != nil || fx.build != nil {
			var resolveErr error
			rr, resolveErr = resolveOptInLeaf(ctx, t, leaf.Entry, fx)
			if resolveErr != nil {
				t.Errorf("opt-in fixture %q failed to resolve: %v", fx.name, resolveErr)
				got[fx.name] = renderErrorLeaf()
				continue
			}
		}
		files, renderErr := renderLeafFileDigests(ctx, t, rr, fx.opts...)
		if renderErr != nil {
			t.Errorf("opt-in fixture %q failed to render: %v", fx.name, renderErr)
			got[fx.name] = renderErrorLeaf()
			continue
		}
		for _, component := range fx.covers {
			if !rendersComponent(files, component) {
				t.Errorf("opt-in fixture %q does not render %s, a component it exists to cover", fx.name, component)
			}
		}
		got[fx.name] = files
	}

	if missing := unrenderedRegistryComponents(t, got); len(missing) > 0 {
		t.Errorf("registry components rendered by no leaf or opt-in fixture: %v. "+
			"Add a stockRenderOptIns entry that selects them.", missing)
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
			t.Errorf("leaf %q is not in the golden (new overlay or opt-in fixture?). Regenerate deliberately.", d.Leaf)
		case driftLeafRemoved:
			t.Errorf("golden leaf %q is no longer produced (overlay or opt-in fixture removed?). Regenerate deliberately.", d.Leaf)
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
// exact values are irrelevant. That they never vary is what matters. extra is
// applied after them and carries an opt-in fixture's flags.
func renderLeafFileDigests(ctx context.Context, t *testing.T, rr *recipe.RecipeResult, extra ...config.Option) (map[string]string, error) {
	t.Helper()

	opts := []config.Option{
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
	}

	b, err := New(WithConfig(config.NewConfig(append(opts, extra...)...)))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "new bundler")
	}

	outputDir := t.TempDir()
	if _, err := b.Make(ctx, rr, outputDir); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "make")
	}
	return digestTreeFiles(outputDir)
}

// optInOverlayName names the overlay an overlayAddition writes into its own
// data directory.
const optInOverlayName = "stock-render-opt-in"

// resolveOptInLeaf resolves one catalog leaf again, the same way the stock
// pass does, with fx's overlay additions and build options applied.
func resolveOptInLeaf(ctx context.Context, t *testing.T, entry recipe.CatalogEntry, fx stockRenderOptIn) (*recipe.RecipeResult, error) {
	t.Helper()
	opts := recipe.ResolveLeavesOptions{
		Version: stockRenderVersion,
		Filter:  entry.Criteria,
	}
	if fx.build != nil {
		opts.BuildOptionsForCriteria = func(*recipe.Criteria) []recipe.BuildOption {
			return fx.build
		}
	}
	name := entry.Name
	if fx.overlay != nil {
		opts.Provider = fx.overlay.dataProvider(t, entry)
		name = optInOverlayName
	}
	leaves, err := recipe.ResolveLeaves(ctx, opts)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "resolve leaves")
	}
	for _, leaf := range leaves {
		if leaf.Entry.Name == name {
			return leaf.Result, leaf.Err
		}
	}
	return nil, errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("leaf %q not found under the criteria of %q", name, entry.Name))
}

// dataProvider layers an overlay that inherits leaf and carries a's additions
// over the embedded catalog, the way `aicr recipe --data` layers a
// user-authored overlay. The overlay takes over as the leaf for leaf's
// criteria, and leaf's own file is read unchanged.
func (a *overlayAddition) dataProvider(t *testing.T, leaf recipe.CatalogEntry) recipe.DataProvider {
	t.Helper()
	overlay := recipe.RecipeMetadata{
		Spec: recipe.RecipeMetadataSpec{
			Base:          leaf.Name,
			Criteria:      leaf.Criteria,
			Mixins:        a.mixins,
			ComponentRefs: a.componentRefs,
		},
	}
	overlay.Kind = recipe.RecipeMetadataKind
	overlay.APIVersion = recipe.RecipeMetadataAPIVersion
	overlay.Metadata.Name = optInOverlayName
	// The layered provider requires a registry file. An empty one leaves the
	// embedded registry as is.
	registry := recipe.ComponentRegistry{
		APIVersion: recipe.ComponentRegistryAPIVersion,
		Kind:       recipe.ComponentRegistryKind,
	}

	dir := t.TempDir()
	writeYAMLFile(t, filepath.Join(dir, recipe.RegistryFileName), &registry)
	if err := os.Mkdir(filepath.Join(dir, "overlays"), 0o700); err != nil {
		t.Fatalf("create overlays dir: %v", err)
	}
	writeYAMLFile(t, filepath.Join(dir, "overlays", optInOverlayName+".yaml"), &overlay)
	provider, err := recipe.NewLayeredDataProvider(
		recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: dir},
	)
	if err != nil {
		t.Fatalf("layer opt-in overlay: %v", err)
	}
	t.Cleanup(func() { recipe.EvictCachedRegistry(provider) })
	return provider
}

func writeYAMLFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// unrenderedRegistryComponents returns the registry components that render in
// no entry of got.
func unrenderedRegistryComponents(t *testing.T, got renderGolden) []string {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("load component registry: %v", err)
	}
	var missing []string
	for _, name := range registry.Names() {
		rendered := false
		for _, files := range got {
			if rendersComponent(files, name) {
				rendered = true
				break
			}
		}
		if !rendered {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// rendersComponent reports whether any rendered path sits under the
// install-order directory of the named component ("NNN-<name>/").
func rendersComponent(files map[string]string, name string) bool {
	for p := range files {
		dir, _, _ := strings.Cut(p, "/")
		if _, component, ok := strings.Cut(dir, "-"); ok && component == name {
			return true
		}
	}
	return false
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
		return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf("bundle tree at %s is empty", root))
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
		"# the sha256 of that file's contents. Entries named <leaf>+<opt-in> render\n" +
		"# that leaf again with a recipe-time or bundle-time opt-in applied. A leaf\n" +
		"# that failed to render is recorded as the single entry\n" +
		"# `render-error: render-error`.\n")
	if err := os.WriteFile(stockRenderGoldenPath, append(header, raw...), 0o600); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

func TestRendersComponent(t *testing.T) {
	files := map[string]string{
		"014-nvidia-dra-driver-gpu/values.yaml": "aa",
		"013-kubeflow-trainer-post/install.sh":  "bb",
		"recipe.yaml":                           "cc",
	}
	tests := []struct {
		name      string
		component string
		want      bool
	}{
		{"exact component directory", "nvidia-dra-driver-gpu", true},
		{"post directory is its own component", "kubeflow-trainer-post", true},
		{"prefix of a rendered name does not match", "nvidia-dra-driver", false},
		{"parent of a post directory does not match", "kubeflow-trainer", false},
		{"absent component", "dra-node-labeler", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rendersComponent(files, tt.component); got != tt.want {
				t.Errorf("rendersComponent(%q) = %v, want %v", tt.component, got, tt.want)
			}
		})
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
