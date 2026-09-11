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

package recipe_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	catalogParityGoldenPath = "testdata/catalog_parity_golden.yaml"

	// catalogParityVersion pins the builder version stamped into every
	// resolved recipe. A real version string would change the digest on
	// every release and make the golden unmaintainable; the constant makes
	// the digest a pure function of the catalog data.
	catalogParityVersion = "catalog-parity-golden"

	// resolveErrorSentinel stands in for a leaf that failed to resolve, so a
	// leaf flipping between resolvable and erroring flips the golden instead
	// of silently dropping out of the comparison.
	resolveErrorSentinel = "resolve-error"
)

// TestCatalogParityGolden pins the resolved bytes of every leaf recipe in the
// embedded catalog.
//
// This is the "cost to existing users" gate. A registry entry, overlay, mixin,
// or values file that was only supposed to add an opt-in component must not
// perturb any recipe that does not declare it — and the only trustworthy way
// to know is byte comparison across the whole catalog.
//
// Why a committed golden rather than a diff against a base revision: a script
// that resolves the catalog with two binaries proves parity once, for the
// person who runs it, on the day they run it. This proves it on every PR, for
// every future change, without anyone remembering to. The one-time proof for
// the change that introduces the golden is "the golden did not move"; the
// durable proof is that it keeps not moving.
//
// Regenerate deliberately, and only when a recipe change is intended:
//
//	AICR_UPDATE_GOLDEN=1 go test ./pkg/recipe/ -run TestCatalogParityGolden
//
// Scope note: this covers recipe *resolution*. Rendered bundle bytes are
// pinned separately by TestStockRenderParityGolden in pkg/bundler, because
// rendering also depends on registry scheduling paths and bundler config that
// resolution never sees.
func TestCatalogParityGolden(t *testing.T) {
	ctx := context.Background()

	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{
		Version: catalogParityVersion,
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
		if _, dup := got[name]; dup {
			t.Fatalf("duplicate leaf name %q: the golden is keyed by name and cannot represent both", name)
		}
		if leaf.Err != nil {
			t.Errorf("leaf %q failed to resolve: %v", name, leaf.Err)
			got[name] = resolveErrorSentinel
			continue
		}
		raw, marshalErr := serializer.MarshalYAMLDeterministic(leaf.Result)
		if marshalErr != nil {
			t.Fatalf("leaf %q: marshal resolved recipe: %v", name, marshalErr)
		}
		sum := sha256.Sum256(raw)
		got[name] = hex.EncodeToString(sum[:])
	}

	if os.Getenv("AICR_UPDATE_GOLDEN") == "1" {
		if t.Failed() {
			t.Fatal("not writing golden: one or more leaves failed to resolve (see errors above)")
		}
		writeCatalogParityGolden(t, got)
		t.Logf("golden updated: %d leaves", len(got))
		return
	}

	raw, err := os.ReadFile(catalogParityGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run with AICR_UPDATE_GOLDEN=1 to create): %v", err)
	}
	want := map[string]string{}
	if err := yaml.Unmarshal(raw, &want); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	for _, name := range sortedKeys(got) {
		w, ok := want[name]
		if !ok {
			t.Errorf("leaf %q is not in the golden (new overlay?) — regenerate deliberately", name)
			continue
		}
		if w != got[name] {
			t.Errorf("leaf %q resolved bytes changed: golden %s, now %s\n"+
				"If this change was intended, regenerate with AICR_UPDATE_GOLDEN=1 and justify the diff "+
				"in the PR. If it was not, a change meant to be scoped to one component has leaked into "+
				"this recipe.", name, w, got[name])
		}
	}
	for _, name := range sortedKeys(want) {
		if _, ok := got[name]; !ok {
			t.Errorf("golden leaf %q is no longer produced (overlay removed?) — regenerate deliberately", name)
		}
	}
}

func writeCatalogParityGolden(t *testing.T, got map[string]string) {
	t.Helper()
	raw, err := serializer.MarshalYAMLDeterministic(got)
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	header := []byte("# Generated by TestCatalogParityGolden. Do not hand-edit.\n" +
		"# Regenerate: AICR_UPDATE_GOLDEN=1 go test ./pkg/recipe/ -run TestCatalogParityGolden\n" +
		"#\n" +
		"# One entry per leaf overlay: sha256 of its deterministically-marshalled\n" +
		"# resolved recipe. A moved digest means that recipe's resolved bytes changed.\n")
	if err := os.WriteFile(catalogParityGoldenPath, append(header, raw...), 0o600); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
