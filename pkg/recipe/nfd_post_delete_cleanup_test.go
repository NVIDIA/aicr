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

// TestNFDPostDeleteCleanupDisabled asserts that every overlay deploying nfd
// resolves postDeleteCleanup to false. The chart defaults it to true, which
// renders a post-delete Job that prunes every NFD-owned node label, including
// the ones the GPU Operator, Network Operator and DRA DaemonSets select on,
// so an unset value fails too.
func TestNFDPostDeleteCleanupDisabled(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	checked := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Fatalf("overlay %q: BuildRecipeResult: %v", name, err)
		}
		ref := result.GetComponentRef("nfd")
		if ref == nil || !ref.IsEnabled() {
			continue
		}
		checked++

		values, err := result.GetValuesForComponentWithContext(ctx, "nfd")
		if err != nil {
			t.Fatalf("overlay %q: GetValuesForComponent(nfd): %v", name, err)
		}
		if boolAtPath(values, true, "postDeleteCleanup") {
			t.Errorf("overlay %q: nfd postDeleteCleanup resolves to true (or unset, chart default true).\n"+
				"  Uninstalling nfd would run the chart's prune Job and strip the node labels\n"+
				"  that GPU, fabric and DRA DaemonSets schedule on. Set postDeleteCleanup: false.",
				name)
		}
	}

	if checked == 0 {
		t.Fatal("no overlay deploys an enabled nfd component; the check would be vacuous")
	}
	t.Logf("verified nfd postDeleteCleanup=false across %d overlays", checked)
}
