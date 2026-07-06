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
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ResolvedLeaf pairs a leaf catalog entry with its hermetic resolution outcome.
// Result is nil when Err is non-nil; Err carries a per-leaf build failure and is
// NOT fatal to the enumeration, so callers grade it however they choose.
type ResolvedLeaf struct {
	Entry  CatalogEntry
	Result *RecipeResult
	Err    error
}

// ResolveLeavesOptions configures ResolveLeaves.
type ResolveLeavesOptions struct {
	// Provider is the DataProvider to enumerate and resolve against. Nil selects
	// the package-global embedded catalog (fully hermetic).
	Provider DataProvider
	// Version stamps the recipe builder version used during resolution.
	Version string
	// Filter narrows enumeration to leaves matching every set criteria dimension.
	// Nil enumerates all leaf combos.
	Filter *Criteria
}

// alwaysSatisfiedEvaluator reports every constraint satisfied, so the
// constraint-aware resolution path runs offline: no overlay is excluded and no
// snapshot measurement is consulted. This exercises the merge/compose machinery
// (populating merged Constraints and Metadata) without cluster or snapshot state.
func alwaysSatisfiedEvaluator(Constraint) ConstraintEvalResult {
	return ConstraintEvalResult{Passed: true}
}

// ResolveLeaves enumerates every leaf overlay in the catalog and resolves each
// one hermetically via a single shared builder (satisfied-evaluator path — no
// snapshot, no cluster, no network). Results are sorted by criteria string then
// leaf name for deterministic output. It fails loud if ctx is canceled
// mid-catalog so a partial run is never mistaken for a complete one; per-leaf
// build errors are returned in ResolvedLeaf.Err (not fatal).
func ResolveLeaves(ctx context.Context, opts ResolveLeavesOptions) ([]ResolvedLeaf, error) {
	store, err := LoadMetadataStoreFor(ctx, opts.Provider)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load recipe catalog")
	}

	builder := NewBuilder(
		WithVersion(opts.Version),
		WithDataProvider(opts.Provider),
	)

	// Detect cancellation even when the (possibly filtered) catalog yields no
	// entries; the in-loop check below never runs in that case, so without this
	// a canceled context could return an empty slice with a nil error.
	if cerr := ctx.Err(); cerr != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout,
			"catalog resolution canceled before enumerating the catalog", cerr)
	}

	var leaves []ResolvedLeaf
	for _, entry := range store.ListCatalog(opts.Filter) {
		if cerr := ctx.Err(); cerr != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"catalog resolution canceled before completing the catalog", cerr)
		}
		if !entry.IsLeaf {
			continue
		}
		result, buildErr := builder.BuildFromCriteriaWithEvaluator(ctx, entry.Criteria, alwaysSatisfiedEvaluator)
		leaves = append(leaves, ResolvedLeaf{Entry: entry, Result: result, Err: buildErr})
	}

	sort.Slice(leaves, func(i, j int) bool {
		ci, cj := leaves[i].Entry.Criteria.String(), leaves[j].Entry.Criteria.String()
		if ci != cj {
			return ci < cj
		}
		return leaves[i].Entry.Name < leaves[j].Entry.Name
	})
	return leaves, nil
}
