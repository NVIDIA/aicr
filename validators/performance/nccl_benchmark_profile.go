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
	"slices"
	"sort"
	"strings"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
)

// perfConstraintNCCLBenchmarkProfile names the optional recipe performance
// constraint that opts a recipe into an embedded NCCL benchmark profile
// regardless of its own criteria. External `--data` recipes (a new
// criteria.service value, or an existing service extended to a new
// accelerator/OS) have no entry in the compiled supportedNCCLCombinations
// matrix, so without this constraint the NCCL checks skip — see
// NVIDIA/aicr#1703. Like the inference-perf inputs (inference-model,
// inference-concurrency-per-gpu) it carries a bare value, not a comparator
// expression:
//
//	constraints:
//	  - name: nccl-benchmark-profile
//	    value: gb200/eks
//
// The value is "{accelerator}/{service}", naming an embedded benchmark
// template family (the testdata/{accelerator}/{service} layout). Resolution
// is recipe-only — there is deliberately no env tier, so a profile is always
// recorded in the recipe that certified the cluster.
const perfConstraintNCCLBenchmarkProfile = "nccl-benchmark-profile"

// ncclBenchmarkTarget is the resolved (accelerator, service) pair an NCCL
// check runs as. Template selection, service-specific fabric plumbing (EFA
// and GKE NIC discovery, worker scheduling defaults), and preflight
// applicability all key off the target. Cluster-facing node identification
// (the GFD gpu.product filter in narrowByAccelerator) deliberately does NOT —
// it keeps using the recipe's own criteria accelerator, so a profile naming
// gb200 never filters a cluster of a newer, unmatched accelerator down to
// zero nodes.
type ncclBenchmarkTarget struct {
	accelerator recipe.CriteriaAcceleratorType
	service     recipe.CriteriaServiceType

	// fromProfile records whether the target came from an explicit
	// nccl-benchmark-profile constraint rather than the recipe's criteria.
	// Selects the more specific skip message when a variant is not covered.
	fromProfile bool
}

func (t ncclBenchmarkTarget) String() string {
	return string(t.accelerator) + "/" + string(t.service)
}

// resolveNCCLBenchmarkProfile reads the optional nccl-benchmark-profile
// performance constraint. Returns (nil, nil) when the constraint is absent or
// blank. A malformed value or an "{accelerator}/{service}" pair that no
// variant implements fails closed with ErrCodeInvalidRequest — a typo'd
// profile must abort the check rather than silently skip, which is the exact
// failure mode the constraint exists to eliminate.
func resolveNCCLBenchmarkProfile(ctx *validators.Context) (*ncclBenchmarkTarget, error) {
	c, ok := findPerformanceConstraint(ctx, perfConstraintNCCLBenchmarkProfile)
	if !ok {
		return nil, nil //nolint:nilnil // nil signals "no profile declared" — callers fall back to criteria
	}
	raw := strings.TrimSpace(c.Value)
	if raw == "" {
		return nil, nil //nolint:nilnil // blank value means "no profile declared", same as absent
	}

	parts := strings.Split(raw, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: must be \"{accelerator}/{service}\" (e.g. \"gb200/eks\")",
				perfConstraintNCCLBenchmarkProfile, raw))
	}

	// Normalize like criteria values: criteria parsing lowercases its input,
	// and the testdata template directories are lowercase.
	target := ncclBenchmarkTarget{
		accelerator: recipe.CriteriaAcceleratorType(strings.ToLower(strings.TrimSpace(parts[0]))),
		service:     recipe.CriteriaServiceType(strings.ToLower(strings.TrimSpace(parts[1]))),
		fromProfile: true,
	}

	if !benchmarkProfileKnown(target) {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unknown %s=%q: available profiles: %s",
				perfConstraintNCCLBenchmarkProfile, raw, strings.Join(knownBenchmarkProfiles(), ", ")))
	}

	return &target, nil
}

// benchmarkProfileKnown reports whether at least one NCCL variant implements
// the target's (accelerator, service) pair. Profiles are restricted to pairs
// present in the compiled matrix so the wiring guarantee enforced by
// TestSupportedNCCLCombinationsHaveRuntimeTemplates — advertised tuple ⇒
// parseable runtime template — extends to every profile a recipe can name.
//
// Note: RoCE NET applicability is service-keyed and accelerator-agnostic
// (roceNETSupportedServices), so a service present only there — with no
// accelerator-keyed matrix tuple — could not be named by any profile. Every
// RoCE service today also has matrix tuples; TestRoCEServicesHaveMatrixTuples
// guards that invariant, and extending this check to consult
// roceNETSupportedServices is the fix if a RoCE-only service is ever added.
func benchmarkProfileKnown(target ncclBenchmarkTarget) bool {
	for _, byService := range supportedNCCLCombinations {
		if slices.Contains(byService[target.service], target.accelerator) {
			return true
		}
	}
	return false
}

// knownBenchmarkProfiles returns the sorted, de-duplicated set of
// "{accelerator}/{service}" pairs implemented by any variant. Used only in
// the unknown-profile diagnostic so the operator sees the valid inputs
// instead of hunting through validator source.
func knownBenchmarkProfiles() []string {
	seen := map[string]struct{}{}
	for _, byService := range supportedNCCLCombinations {
		for service, accelerators := range byService {
			for _, accelerator := range accelerators {
				seen[string(accelerator)+"/"+string(service)] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ncclCombinationSupported reports whether the target (accelerator, service)
// pair implements the given variant on the given fabric. RoCE NET support is
// fabric-keyed and accelerator-agnostic (roceNETSupportedServices); every
// other combination is keyed through the per-variant compiled matrix.
func ncclCombinationSupported(variant ncclVariant, fabric ncclFabricType, target ncclBenchmarkTarget) bool {
	if fabric == fabricRoCE && variant == variantNET {
		// RoCE NET is fabric-keyed and accelerator-agnostic — supported on any
		// service with a testdata/roce/{service} template. Only NET has a RoCE
		// path; NVLS (NVLink/IMEX) is fabric-independent and uses the normal
		// accelerator-keyed combinations below.
		return roceNETSupportedServices[target.service]
	}
	return slices.Contains(supportedNCCLCombinations[variant][target.service], target.accelerator)
}
