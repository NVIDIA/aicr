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
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestResolveLeaves_EmbeddedCatalog(t *testing.T) {
	leaves, err := recipe.ResolveLeaves(context.Background(), recipe.ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	if len(leaves) == 0 {
		t.Fatal("expected leaves, got none")
	}
	for _, l := range leaves {
		if !l.Entry.IsLeaf {
			t.Errorf("non-leaf entry present: %s", l.Entry.Name)
		}
	}
	// Deterministic order: criteria string then leaf name.
	for i := 1; i < len(leaves); i++ {
		ka, kb := leaves[i-1].Entry.Criteria.String(), leaves[i].Entry.Criteria.String()
		if ka > kb || (ka == kb && leaves[i-1].Entry.Name > leaves[i].Entry.Name) {
			t.Errorf("not sorted at %d: %q/%s vs %q/%s", i, ka, leaves[i-1].Entry.Name, kb, leaves[i].Entry.Name)
		}
	}
	// A known leaf resolves with component refs.
	var found bool
	for _, l := range leaves {
		if l.Entry.Name == "h100-eks-ubuntu-inference-nim" {
			found = true
			if l.Err != nil {
				t.Errorf("h100-eks-ubuntu-inference-nim resolve err: %v", l.Err)
			}
			if l.Result == nil || len(l.Result.ComponentRefs) == 0 {
				t.Error("expected resolved componentRefs for h100-eks-ubuntu-inference-nim")
			}
		}
	}
	if !found {
		t.Error("h100-eks-ubuntu-inference-nim not found in catalog")
	}
}

func TestResolveLeaves_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{}); err == nil {
		t.Fatal("expected error on canceled context")
	}
}

// TestResolveLeaves_CanceledContextZeroMatchFilter guards the edge case where a
// filter matches no leaves: the empty-result path must not mask a canceled
// context by returning (nil, nil).
func TestResolveLeaves_CanceledContextZeroMatchFilter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	filter := &recipe.Criteria{Service: recipe.CriteriaServiceType("does-not-exist")}
	if _, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Filter: filter}); err == nil {
		t.Fatal("expected error on canceled context with a zero-match filter")
	}
}

// TestResolveLeaves_RetainNonLeaf asserts the opt-in escape hatch that keeps an
// evidence-backed coordinate enumerable after a platform sibling turns its
// overlay into a non-leaf (#2564). The control half matters as much as the
// test: the same overlay must be absent without the predicate, otherwise the
// assertion would pass for the wrong reason.
func TestResolveLeaves_RetainNonLeaf(t *testing.T) {
	// vr200-rke2-ubuntu-training became a non-leaf when
	// vr200-rke2-ubuntu-training-kubeflow was added, and it carries published
	// evidence (pkg/testgrid/presence.yaml lists rke2/vr200-ubuntu/training).
	const nonLeaf = "vr200-rke2-ubuntu-training"

	baseline, err := recipe.ResolveLeaves(context.Background(), recipe.ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves (leaf-only): %v", err)
	}
	if containsEntry(baseline, nonLeaf) {
		t.Fatalf("%s is a leaf today; pick another non-leaf overlay for this regression", nonLeaf)
	}

	retained, err := recipe.ResolveLeaves(context.Background(), recipe.ResolveLeavesOptions{
		RetainNonLeaf: func(entry recipe.CatalogEntry) bool { return entry.Name == nonLeaf },
	})
	if err != nil {
		t.Fatalf("ResolveLeaves (RetainNonLeaf): %v", err)
	}
	if !containsEntry(retained, nonLeaf) {
		t.Fatalf("RetainNonLeaf did not retain %s", nonLeaf)
	}
	if got, want := len(retained), len(baseline)+1; got != want {
		t.Errorf("retained %d entries, want %d — RetainNonLeaf must add exactly the opted-in entry", got, want)
	}
	for _, l := range retained {
		if l.Entry.Name != nonLeaf {
			continue
		}
		if l.Err != nil {
			t.Errorf("%s resolve err: %v", nonLeaf, l.Err)
		}
		if l.Result == nil || len(l.Result.ComponentRefs) == 0 {
			t.Errorf("expected resolved componentRefs for %s", nonLeaf)
		}
	}
}

func containsEntry(leaves []recipe.ResolvedLeaf, name string) bool {
	for _, l := range leaves {
		if l.Entry.Name == name {
			return true
		}
	}
	return false
}
