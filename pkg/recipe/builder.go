// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"time"

	"github.com/NVIDIA/eidos/pkg/defaults"
	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
)

// ConstraintEvalResult represents the result of evaluating a single constraint.
// This mirrors the result from pkg/validator to avoid circular imports.
type ConstraintEvalResult struct {
	// Passed indicates if the constraint was satisfied.
	Passed bool

	// Actual is the actual value extracted from the snapshot.
	Actual string

	// Error contains the error if evaluation failed (e.g., value not found).
	Error error
}

// ConstraintEvaluatorFunc is a function type for evaluating constraints.
// It takes a constraint and returns the evaluation result.
// This function type allows the recipe package to use constraint evaluation
// from the validator package without creating a circular dependency.
type ConstraintEvaluatorFunc func(constraint Constraint) ConstraintEvalResult

// Option is a functional option for configuring Builder instances.
type Option func(*Builder)

// WithVersion returns an Option that sets the Builder version string.
// The version is included in recipe metadata for tracking purposes.
func WithVersion(version string) Option {
	return func(b *Builder) {
		b.Version = version
	}
}

// WithAllowLists returns an Option that sets criteria allowlists for the Builder.
// When allowlists are configured, the Builder will reject criteria values that
// are not in the allowed list. This is used by the API server to restrict
// which criteria values can be requested.
func WithAllowLists(al *AllowLists) Option {
	return func(b *Builder) {
		b.AllowLists = al
	}
}

// NewBuilder creates a new Builder instance with the provided functional options.
func NewBuilder(opts ...Option) *Builder {
	b := &Builder{}

	for _, opt := range opts {
		opt(b)
	}

	return b
}

// Builder constructs RecipeResult payloads based on Criteria specifications.
// It loads recipe metadata, applies matching overlays, and generates
// tailored configuration recipes.
type Builder struct {
	Version    string
	AllowLists *AllowLists
}

// BuildFromCriteria creates a RecipeResult payload for the provided criteria.
// It loads the metadata store, applies matching overlays, and returns
// a RecipeResult with merged components and computed deployment order.
func (b *Builder) BuildFromCriteria(ctx context.Context, c *Criteria) (*RecipeResult, error) {
	return b.buildWithStore(ctx, c, func(store *MetadataStore, buildCtx context.Context) (*RecipeResult, error) {
		return store.BuildRecipeResult(buildCtx, c)
	})
}

// BuildFromCriteriaWithEvaluator creates a RecipeResult payload for the provided criteria,
// filtering overlays based on constraint evaluation against snapshot data.
//
// When an evaluator function is provided:
//   - Overlays that match by criteria but fail constraint evaluation are excluded
//   - Constraint warnings are included in the result metadata for visibility
//   - Only overlays whose constraints pass (or have no constraints) are merged
//
// The evaluator function is typically created by wrapping validator.EvaluateConstraint
// with the snapshot data.
func (b *Builder) BuildFromCriteriaWithEvaluator(ctx context.Context, c *Criteria, evaluator ConstraintEvaluatorFunc) (*RecipeResult, error) {
	return b.buildWithStore(ctx, c, func(store *MetadataStore, buildCtx context.Context) (*RecipeResult, error) {
		return store.BuildRecipeResultWithEvaluator(buildCtx, c, evaluator)
	})
}

func (b *Builder) buildWithStore(ctx context.Context, c *Criteria, buildFn func(*MetadataStore, context.Context) (*RecipeResult, error)) (*RecipeResult, error) {
	if c == nil {
		return nil, eidoserrors.New(eidoserrors.ErrCodeInvalidRequest, "criteria cannot be nil")
	}

	buildCtx, cancel := context.WithTimeout(ctx, defaults.RecipeBuildTimeout)
	defer cancel()

	if err := buildCtx.Err(); err != nil {
		return nil, eidoserrors.WrapWithContext(
			eidoserrors.ErrCodeTimeout,
			"recipe build context cancelled during initialization",
			err,
			map[string]any{
				"stage": "initialization",
			},
		)
	}

	start := time.Now()
	defer func() {
		recipeBuiltDuration.Observe(time.Since(start).Seconds())
	}()

	store, err := loadMetadataStore(buildCtx)
	if err != nil {
		return nil, eidoserrors.WrapWithContext(
			eidoserrors.ErrCodeInternal,
			"failed to load metadata store",
			err,
			map[string]any{
				"stage": "metadata_load",
			},
		)
	}

	result, err := buildFn(store, buildCtx)
	if err != nil {
		return nil, err
	}

	if b.Version != "" {
		result.Metadata.Version = b.Version
	}

	return result, nil
}
