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

package validations

import (
	"context"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ValidationFunc is the signature for validation check functions.
// Parameters:
//   - ctx: Context for cancellation/timeout
//   - componentName: Name of the component being validated
//   - recipeResult: The recipe result containing component refs and criteria
//   - bundlerConfig: The bundler configuration (for accessing flags like workload-selector)
//   - conditions: Conditions from the validation config (e.g., {"intent": ["training"]} or {"intent": ["training", "inference"]})
//
// Returns:
//   - warnings: List of warning messages (non-blocking)
//   - errors: List of error messages (blocking)
type ValidationFunc func(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) (warnings []string, errors []error)
