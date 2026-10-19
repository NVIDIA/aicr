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

package recipe_test

import (
	"context"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// Every leaf that resolves slinky-slurm on a GPU service must select
// slinky-slurm-gpu-access, or Slurm GPU access and isolation go unverified on
// it (#2756). Kind leaves run without task/cgroup and are exempt.
func TestGPUSlurmLeavesSelectGPUAccessCheck(t *testing.T) {
	const check = "slinky-slurm-gpu-access"
	ctx := context.Background()

	cat, err := catalog.LoadWithDataProvider(ctx, nil, "", "")
	if err != nil {
		t.Fatalf("load embedded validator catalog: %v", err)
	}
	if !slices.ContainsFunc(cat.Validators, func(e validatorv1.ValidatorEntry) bool {
		return e.Name == check && e.Phase == "conformance"
	}) {

		t.Fatalf("validator catalog has no conformance entry named %s", check)
	}

	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Version: "slurm-gpu-access-guard"})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	var guarded []string
	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result == nil {
			continue // TestCatalogParityGolden reports resolution failures
		}
		ref := leaf.Result.GetComponentRef("slinky-slurm")
		if ref == nil || !ref.IsEnabled() {
			continue
		}
		if leaf.Entry.Criteria != nil && leaf.Entry.Criteria.Service == recipe.CriteriaServiceKind {
			continue
		}
		guarded = append(guarded, leaf.Entry.Name)
		v := leaf.Result.Validation
		if v == nil || v.Conformance == nil || !slices.Contains(v.Conformance.Checks, check) {
			t.Errorf("%s resolves slinky-slurm on a GPU service but does not select %s", leaf.Entry.Name, check)
		}
	}
	if len(guarded) < 6 {
		t.Fatalf("guard matched %d GPU Slinky leaves %v, want at least 6; the leaf filter is wrong", len(guarded), guarded)
	}
}
