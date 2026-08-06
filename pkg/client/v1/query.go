// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// SelectFromRecipeWithContext hydrates a resolved recipe and extracts a
// dot-path selector (e.g. "components.gpu-operator.values.driver.version").
// An empty selector returns the entire hydrated structure. Mirrors
// `aicr query`, and is the implementation both the CLI query command and
// the REST query handler run.
//
// Hydration reads each component's values through the DataProvider bound to
// the recipe, so ctx bounds real I/O: a canceled or expired context aborts
// the hydration rather than running to completion.
//
// The recipe must carry internal pkg/recipe state — obtain one from
// Client.ResolveRecipe, Client.LoadRecipe, Client.AdoptRecipe, or
// WrapResolved. A facade RecipeResult constructed any other way is rejected
// with ErrCodeInvalidRequest.
//
// # Error contract
//
// The OUTERMOST structured error code distinguishes the two failure stages,
// so a caller (e.g. an HTTP handler mapping to a status code) can shape its
// response without a parallel hydrate+select implementation:
//
//   - ErrCodeNotFound — the selector path does not exist in the hydrated
//     recipe. This code is only ever produced by the selection stage.
//   - ErrCodeInvalidRequest — ctx or r is nil, or r carries no internal state.
//   - Anything else (ErrCodeInternal, ErrCodeTimeout, ...) — hydration failed.
//     Hydration never surfaces ErrCodeNotFound as the outermost code:
//     pkg/recipe.HydrateResultWithContext wraps every per-component values
//     failure as ErrCodeInternal, so a missing values file cannot be mistaken
//     for a missing selector path.
//
// Inspect the outermost code with stderrors.As, NOT stderrors.Is — Is walks
// the wrap chain and would match an ErrCodeNotFound cause nested inside a
// hydration failure.
func SelectFromRecipeWithContext(ctx context.Context, r *RecipeResult, selector string) (any, error) {
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil recipe")
	}
	internal := r.Resolved()
	if internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"RecipeResult has no internal recipe state — call Client.ResolveRecipe, LoadRecipe, or AdoptRecipe, or wrap one with WrapResolved, to obtain a queryable RecipeResult")
	}
	hydrated, err := recipe.HydrateResultWithContext(ctx, internal)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "hydrate recipe")
	}
	return recipe.Select(hydrated, selector)
}

// SelectFromRecipe is the context-less form of SelectFromRecipeWithContext,
// kept for source compatibility. It derives a defaults.FileReadTimeout-bounded
// context so hydration's values reads stay bounded, but the caller cannot
// cancel them.
//
// Prefer SelectFromRecipeWithContext wherever a context.Context is available.
func SelectFromRecipe(r *RecipeResult, selector string) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return SelectFromRecipeWithContext(ctx, r, selector)
}
