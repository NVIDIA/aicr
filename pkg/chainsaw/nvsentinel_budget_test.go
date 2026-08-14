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

package chainsaw

import (
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nvsentinelAssertBudgetCeiling is the maximum authored assert budget for
// the nvsentinel health check. The check waits on DaemonSets that sit at
// 0 desired FOREVER on a cluster in the issue #2175 broken state — the
// exact state the check exists to catch — so a generous budget does not
// buy readiness there, it buys a stall. Verified live (issue #2186): a
// 5m budget pushed the expected-resources Job past its activeDeadline,
// the pod was deleted, and the operator saw "pod not found"
// (status=other) instead of the assert diff naming the DaemonSets. The
// in-process executor supports exactly one timeout knob (the Test-level
// spec.timeouts.assert caps the whole check; per-step and per-operation
// timeouts are not honored — see runChainsawTestInProcess), so bounding
// the file's budget is the only way to keep the failure a clean assert
// diff.
const nvsentinelAssertBudgetCeiling = 2 * time.Minute

// TestNVSentinelHealthCheckAssertBudgetBounded pins the nvsentinel
// check's authored assert budget below nvsentinelAssertBudgetCeiling so
// a future edit back to the 5m the sibling checks use — reasonable-
// looking in isolation — cannot silently reintroduce the #2186 stall.
func TestNVSentinelHealthCheckAssertBudgetBounded(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}
	comp := registry.Get("nvsentinel")
	if comp == nil {
		t.Fatal("nvsentinel missing from component registry")
	}
	assertFile := comp.HealthCheck.AssertFile
	if assertFile == "" {
		t.Fatal("nvsentinel registry entry has no healthCheck.assertFile")
	}
	data, err := provider.ReadFile(context.Background(), assertFile)
	if err != nil {
		t.Fatalf("failed to read %q: %v", assertFile, err)
	}
	tests, err := decodeTests(string(data))
	if err != nil {
		t.Fatalf("failed to decode %q: %v", assertFile, err)
	}
	if len(tests) == 0 {
		t.Fatalf("no chainsaw Test documents in %q", assertFile)
	}

	// The executor takes the FIRST document that declares
	// spec.timeouts.assert as the budget for the whole check, so that is
	// the value to pin. A file that stops declaring one falls back to
	// defaults.ChainsawAssertTimeout (6m) — worse than 5m — so absence
	// fails too.
	for i := range tests {
		spec := tests[i].Spec
		if spec.Timeouts == nil || spec.Timeouts.Assert == nil || spec.Timeouts.Assert.Duration <= 0 {
			continue
		}
		authored := spec.Timeouts.Assert.Duration
		if authored > nvsentinelAssertBudgetCeiling {
			t.Errorf("nvsentinel health check authors assert budget %v, want <= %v: "+
				"the driver-labeled DaemonSets it waits on never become ready on a "+
				"cluster in the #2175 broken state, and a stalled assert pushes the "+
				"expected-resources Job past its activeDeadline into the #2186 "+
				"\"pod not found\" failure mode instead of a clean assert diff",
				authored, nvsentinelAssertBudgetCeiling)
		}
		return
	}
	t.Errorf("nvsentinel health check declares no spec.timeouts.assert; without one the "+
		"whole check falls back to defaults.ChainsawAssertTimeout — author a budget <= %v",
		nvsentinelAssertBudgetCeiling)
}
