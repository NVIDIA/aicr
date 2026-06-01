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
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// SelectFromRecipe hydrates a resolved recipe and extracts a dot-path selector
// (e.g. "components.gpu-operator.values.driver.version"). An empty selector
// returns the entire hydrated structure. Mirrors `aicr query`.
//
// The recipe must have been produced by a Client (so its internal
// pkg/recipe.RecipeResult is populated). A facade RecipeResult constructed
// outside ResolveRecipe / LoadRecipe / AdoptRecipe is rejected with
// ErrCodeInvalidRequest.
func SelectFromRecipe(r *RecipeResult, selector string) (any, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil recipe")
	}
	internal := r.Resolved()
	if internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"RecipeResult has no internal recipe state — call Client.ResolveRecipe, LoadRecipe, or AdoptRecipe to obtain a queryable RecipeResult")
	}
	hydrated, err := recipe.HydrateResult(internal)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "hydrate recipe")
	}
	return recipe.Select(hydrated, selector)
}
