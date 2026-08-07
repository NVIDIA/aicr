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

// ncclVariantDisplayName returns a human-readable, non-stuttering name for the
// variant suitable for skip messages and logs.
func ncclVariantDisplayName(variant ncclVariant) string {
	switch variant {
	case variantNET:
		return "NET"
	case variantNVLS:
		return "NVLS"
	case variantDefault:
		return "default"
	default:
		return string(variant)
	}
}

func checkNCCLAllReduceBWVariant(ctx *validators.Context, variant ncclVariant) error {
	name := constraintNameForVariant(variant)
	constraint, found := findPerformanceConstraint(ctx, name)
	if !found {
		// Genuine recipe-driven inapplicability: no run of this variant was
		// requested. A standalone/no-recipe validator run (nil ValidationInput)
		// also lands here — performanceConstraints returns nil, so found is
		// false — which is the #1327 boundary: fail-closed only fires once the
		// recipe actually declares the constraint (see classification below).
		return validators.Skip(fmt.Sprintf("no %s constraint in recipe", name))
	}

	actual, passed, err := validateNcclAllReduceBw(ctx, constraint, variant)
	return classifyNCCLAllReduceBWResult(name, constraint, actual, passed, err)
}

// classifyNCCLAllReduceBWResult turns the inner validateNcclAllReduceBw outcome
// into a check verdict, applying the #2122 applicability contract to the
// "skipped"-prefixed strings the inner function folds an inapplicable outcome
// into (see the skipMsg* constants in nccl_all_reduce_bw_constraint.go). It is
// pure apart from evidence written to stdout on the pass path, so the
// fail-closed classification is unit-testable without a live cluster.
//
// Reaching this function means the recipe declared the nccl-all-reduce-bw*
// performance constraint — a standalone/no-recipe run has no such constraint
// and already returned Skip in the caller (the #1327 boundary), so every
// classification below is evaluated in the "recipe declares the capability"
// column of the applicability contract.
func classifyNCCLAllReduceBWResult(name string, constraint recipe.Constraint, actual string, passed bool, err error) error {
	if err != nil {
		// Preserve a specific inner code (InvalidRequest / Timeout /
		// Unauthorized / NotFound / ...) instead of flattening every inner
		// failure to Internal; only an uncoded error falls back to Internal.
		// The previous errors.Wrap(ErrCodeInternal, ...) force-set the code and
		// masked, e.g., a preflight RBAC denial (Unauthorized) or the TrainJob
		// admission timeout the inner function classifies deliberately.
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "NCCL All Reduce bandwidth check failed")
	}

	// #2122 fail-closed: a DECLARED-but-unavailable prerequisite must BLOCK, not
	// Skip. The recipe asked to benchmark the East-West fabric, which is
	// meaningless below two GPU nodes; the inner check folds that into
	// skipMsgNCCLFewNodes. Treating it as a Skip is the false-PASS this contract
	// forbids — an under-provisioned or half-drained cluster would silently pass
	// conformance. The GPU-node List already succeeded cleanly to reach this
	// string (a Forbidden / timeout / transport failure returns via the err!=nil
	// path above and never folds into a skip string), so the >=2-node
	// prerequisite is cleanly ABSENT → ErrCodeNotFound, mirroring the
	// clean-NotFound row of the capability contract.
	if actual == skipMsgNCCLFewNodes {
		return errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("recipe declares the %s NCCL benchmark but the cluster has fewer than 2 GPU nodes required for the East-West fabric test — provision at least 2 GPU nodes or remove the constraint", name))
	}

	// The remaining "skipped" reasons are functions of the recipe criteria alone
	// (nil ValidationInput, an unsupported service+accelerator combination, or a
	// benchmark profile that does not implement this variant). They are
	// deterministic regardless of cluster state, so no broken or unauthorized
	// cluster can turn them into a false pass — they are genuinely inapplicable
	// and remain Skips.
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
