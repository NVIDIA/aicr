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

package recipe

import (
	"context"
	"testing"
)

// TestAKSDriverOnlyProfileTuple pins the COMPLETE coordinated AKS driver-only
// gpu-operator profile on the resolved values of every AKS leaf, not just the
// driver.enabled flag. Without this, a regression that re-enables the toolkit,
// changes operator.runtimeClass, or re-enables GDRCopy would pass the
// driver-focused auto-detect tests. The DRA driver-root half of the profile
// (nvidiaDriverRoot=/ with driver.enabled=false) is enforced separately by
// TestDriverRootLockstep on effective values including overlay overrides.
func TestAKSDriverOnlyProfileTuple(t *testing.T) {
	ctx := context.Background()
	leaves, err := ResolveLeaves(ctx, ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	sawAKS := false
	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result == nil || leaf.Entry.Criteria == nil {
			continue
		}
		if leaf.Entry.Criteria.Service != CriteriaServiceAKS {
			continue
		}
		if leaf.Result.GetComponentRef("gpu-operator") == nil {
			continue
		}
		sawAKS = true
		t.Run(leaf.Entry.Name, func(t *testing.T) {
			values, verr := leaf.Result.GetValuesForComponentWithContext(ctx, "gpu-operator")
			if verr != nil {
				t.Fatalf("GetValuesForComponentWithContext(gpu-operator): %v", verr)
			}

			assertBool := func(want bool, path ...string) {
				got := valueAtPath[bool](t, values, path...)
				if got != want {
					t.Errorf("gpu-operator %v = %v, want %v", path, got, want)
				}
			}
			assertBool(false, "driver", "enabled")
			assertBool(false, "toolkit", "enabled")
			assertBool(false, "gdrcopy", "enabled")

			runtimeClass := valueAtPath[string](t, values, "operator", "runtimeClass")
			if runtimeClass != "nvidia-container-runtime" {
				t.Errorf("gpu-operator operator.runtimeClass = %q, want nvidia-container-runtime", runtimeClass)
			}
		})
	}
	if !sawAKS {
		t.Error("no AKS leaf with a gpu-operator ref was resolved")
	}
}
