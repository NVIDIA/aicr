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
	"testing"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestGPUOperatorLeavesTolerateNVSentinelReleaseTaint requires every leaf that
// resolves gpu-operator to carry the base overlay's daemonsets.tolerations entry
// for NVSentinel's release taint (Exists/NoSchedule), or to opt out with an
// explicit empty list, which only kind leaves may do. Overlay lists replace on
// inheritance, so a leaf that sets its own list drops the entry silently (#3357).
func TestGPUOperatorLeavesTolerateNVSentinelReleaseTaint(t *testing.T) {
	const key = "nvsentinel.dgxc.nvidia.com/external-remediation"
	const path = "daemonsets.tolerations"

	leaves, err := recipe.ResolveLeaves(context.Background(), recipe.ResolveLeavesOptions{Version: "gpu-operator-release-toleration-guard"})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	var guarded []string
	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result == nil {
			continue // TestCatalogParityGolden reports resolution failures
		}
		ref := leaf.Result.GetComponentRef("gpu-operator")
		if ref == nil || !ref.IsEnabled() {
			continue
		}
		guarded = append(guarded, leaf.Entry.Name)
		raw, ok := component.GetValueByPath(ref.Overrides, path)
		if !ok {
			t.Errorf("%s resolves gpu-operator without %s; the base overlay entry was lost", leaf.Entry.Name, path)
			continue
		}
		list, ok := raw.([]any)
		if !ok {
			t.Errorf("%s: %s is %T, want a list", leaf.Entry.Name, path, raw)
			continue
		}
		if len(list) == 0 {
			// kind.yaml opts out on purpose; an opt-out anywhere else is a decision, not drift.
			if leaf.Entry.Criteria == nil || leaf.Entry.Criteria.Service != recipe.CriteriaServiceKind {
				t.Errorf("%s opts out of gpu-operator %s; only kind leaves do", leaf.Entry.Name, path)
			}
			continue
		}
		found := false
		for _, item := range list {
			if m, ok := item.(map[string]any); ok && m["key"] == key && m["operator"] == "Exists" && m["effect"] == "NoSchedule" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s sets its own %s without the %s Exists/NoSchedule toleration; restate it or opt out with []", leaf.Entry.Name, path, key)
		}
	}
	if len(guarded) < 20 {
		t.Fatalf("guard matched %d gpu-operator leaves %v, want at least 20; the leaf filter is wrong", len(guarded), guarded)
	}
}
