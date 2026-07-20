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

package main

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
)

// checkNCCLAllReduceBW is the legacy CheckFunc that runs the provider-default
// NCCL all-reduce template with no transport assertion. Preserved so existing
// recipes keep working after the per-variant catalog entries were added.
func checkNCCLAllReduceBW(ctx *validators.Context) error {
	return checkNCCLAllReduceBWVariant(ctx, variantDefault)
}

// checkNCCLAllReduceBWNET runs the NET-transport variant (EFA on EKS, etc.)
// and asserts the NET fabric carried traffic.
func checkNCCLAllReduceBWNET(ctx *validators.Context) error {
	return checkNCCLAllReduceBWVariant(ctx, variantNET)
}

// checkNCCLAllReduceBWNVLS runs the NVLS/MNNVL-transport variant and asserts
// that NVLS initialized and carried traffic (fails loudly if the cluster's
// IMEX domain is broken and NCCL falls back to NET).
func checkNCCLAllReduceBWNVLS(ctx *validators.Context) error {
	return checkNCCLAllReduceBWVariant(ctx, variantNVLS)
}

// constraintNameForVariant returns the recipe constraint name that selects a
// given NCCL transport variant. Must match the entries in
// recipes/validators/catalog.yaml.
func constraintNameForVariant(variant ncclVariant) string {
	switch variant {
	case variantNET:
		return "nccl-all-reduce-bw-net"
	case variantNVLS:
		return "nccl-all-reduce-bw-nvls"
	case variantDefault:
		return checkNameNCCLAllReduceBW
	default:
		// Unknown values fall back to the legacy constraint name so existing
		// recipes keep validating after variant rollout.
		return checkNameNCCLAllReduceBW
	}
}

func checkNCCLAllReduceBWVariant(ctx *validators.Context, variant ncclVariant) error {
	name := constraintNameForVariant(variant)
	constraint, found := findPerformanceConstraint(ctx, name)
	if !found {
		return validators.Skip(fmt.Sprintf("no %s constraint in recipe", name))
	}

	actual, passed, err := validateNcclAllReduceBw(ctx, constraint, variant)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "NCCL All Reduce bandwidth check failed", err)
	}

	// The inner function returns "skipped - ..." when the check is not applicable.
	if strings.HasPrefix(actual, "skipped") {
		return validators.Skip(actual)
	}

	fmt.Printf("NCCL All Reduce bandwidth (%s): %s\n", name, actual)
	fmt.Printf("Constraint: %s → %v\n", constraint.Value, passed)

	if !passed {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("NCCL bandwidth %s does not satisfy constraint %q", actual, constraint.Value))
	}

	return nil
}

func findPerformanceConstraint(ctx *validators.Context, name string) (recipe.Constraint, bool) {
	return v1.FindConstraint(performanceConstraints(ctx), name)
}

// countPerformanceConstraint counts performance constraints with the given name.
func countPerformanceConstraint(ctx *validators.Context, name string) int {
	return v1.CountConstraint(performanceConstraints(ctx), name)
}

// performanceConstraints returns the recipe's performance-phase constraints in a
// nil-safe way. The lookup semantics (first-match, count) live once in
// pkg/validator/v1 so the pod and the orchestrator can't drift.
func performanceConstraints(ctx *validators.Context) []recipe.Constraint {
	if ctx.ValidationInput == nil || ctx.ValidationInput.Config.Performance == nil {
		return nil
	}
	return ctx.ValidationInput.Config.Performance.Constraints
}
