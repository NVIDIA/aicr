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
	// RetainNonLeaf opts a non-leaf catalog entry back into the enumeration.
	// Nil (the default) keeps the leaf-only behavior.
	//
	// It exists so a coordinate that carries published validation evidence is
	// not silently dropped the moment a platform sibling is added beneath it
	// — adding e.g. a `-kubeflow` leaf turns the plain training overlay into a
	// non-leaf, and without this the evidence-backed coordinate would vanish
	// from every leaf-only consumer (NVIDIA/aicr#2564). The predicate is
	// supplied by the caller rather than read here so pkg/recipe stays free of
	// a dependency on the evidence/presence manifest.
	RetainNonLeaf func(entry CatalogEntry) bool
	// BuildOptionsForCriteria supplies per-leaf generation-time selections,
	// applied to each leaf's initial build. The h100 GKE kubeflow leaf fails
	// closed without the TCPXO interface mapping; when the hook does not supply
	// one, that leaf is retried once with the fixed introspection value. Nil
	// means every leaf gets the default build plus that one retry.
	BuildOptionsForCriteria func(*Criteria) []BuildOption
}

func alwaysSatisfiedEvaluator(Constraint) ConstraintEvalResult {
	return ConstraintEvalResult{Passed: true}
}

// buildOptionsForCriteria nil-safely resolves the per-leaf build options.
func buildOptionsForCriteria(fn func(*Criteria) []BuildOption, c *Criteria) []BuildOption {
	if fn == nil {
		return nil
	}
	return fn(c)
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
		if !entry.IsLeaf && (opts.RetainNonLeaf == nil || !opts.RetainNonLeaf(entry)) {
			continue
		}
		result, buildErr := builder.BuildFromCriteriaWithEvaluator(ctx, entry.Criteria, alwaysSatisfiedEvaluator,
			buildOptionsForCriteria(opts.BuildOptionsForCriteria, entry.Criteria)...)
		if IsMissingGKETCPXOInterfaces(buildErr) {
			// The one catalog family with a required typed input: retry with
			// the fixed introspection mapping so enumeration tooling covers
			// this leaf without a cluster to name real networks for. A leaf
			// that does not ship the runtime never reaches this branch — its
			// plain build succeeded — so external catalogs that shadow the
			// family without the runtime are unaffected.
			result, buildErr = builder.BuildFromCriteriaWithEvaluator(ctx, entry.Criteria, alwaysSatisfiedEvaluator,
				WithGKETCPXOInterfaces(GKETCPXOIntrospectionInterfaces()))
		}
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
