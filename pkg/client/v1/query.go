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
func SelectFromRecipe(r *Recipe, selector string) (any, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil recipe")
	}
	hydrated, err := recipe.HydrateResult(r)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "hydrate recipe", err)
	}
	return recipe.Select(hydrated, selector)
}
