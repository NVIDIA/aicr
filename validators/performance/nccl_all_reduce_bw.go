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

package main

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

// checkNCCLAllReduceBW wraps the NCCL constraint validator as a CheckFunc.
// Finds the nccl-all-reduce-bw constraint from the recipe and delegates to
// validateNcclAllReduceBw (copied from v1).
func checkNCCLAllReduceBW(ctx *validators.Context) error {
	constraint, found := findPerformanceConstraint(ctx, "nccl-all-reduce-bw")
	if !found {
		return validators.Skip("no nccl-all-reduce-bw constraint in recipe")
	}

	actual, passed, err := validateNcclAllReduceBw(ctx, constraint)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "NCCL All Reduce bandwidth check failed", err)
	}

	// The inner function returns "skipped - ..." when the check is not applicable.
	if strings.HasPrefix(actual, "skipped") {
		return validators.Skip(actual)
	}

	fmt.Printf("NCCL All Reduce bandwidth: %s\n", actual)
	fmt.Printf("Constraint: %s → %v\n", constraint.Value, passed)

	if !passed {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("NCCL bandwidth %s does not satisfy constraint %q", actual, constraint.Value))
	}

	return nil
}

func findPerformanceConstraint(ctx *validators.Context, name string) (recipe.Constraint, bool) {
	if ctx.Recipe == nil || ctx.Recipe.Validation == nil || ctx.Recipe.Validation.Performance == nil {
		return recipe.Constraint{}, false
	}
	for _, c := range ctx.Recipe.Validation.Performance.Constraints {
		if c.Name == name {
			return c, true
		}
	}
	return recipe.Constraint{}, false
}
