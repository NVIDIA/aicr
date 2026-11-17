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

const kaiStalenessArg = "default-staleness-grace-period"

// TestKAIStalenessDisabledExactlyWithGrove pins the Dynamo platform chart's
// requirement (README, v1.5.0 upgrade notes): with Grove's kai-scheduler
// backend, Grove owns PodGroup termination timing, so an externally managed
// KAI must set scheduler.args.default-staleness-grace-period to "-1s"; without
// Grove, KAI keeps its own cleanup. Checked on the merged values, so the
// setting may come from a values file or inline overrides.
func TestKAIStalenessDisabledExactlyWithGrove(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	withGrove := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Errorf("resolving %s: %v", name, err)
			continue
		}
		if result.GetComponentRef("kai-scheduler") == nil {
			continue
		}
		values, err := result.GetValuesForComponentWithContext(ctx, "kai-scheduler")
		if err != nil {
			t.Errorf("%s: kai-scheduler values: %v", name, err)
			continue
		}
		var got any
		if scheduler, ok := values["scheduler"].(map[string]any); ok {
			if args, ok := scheduler["args"].(map[string]any); ok {
				got = args[kaiStalenessArg]
			}
		}
		_, hasGrove := findComponentRefByName(result.ComponentRefs, "grove")
		switch {
		case hasGrove:
			withGrove++
			if got != "-1s" {
				t.Errorf("%s ships grove but kai-scheduler scheduler.args.%s = %v, want \"-1s\"", name, kaiStalenessArg, got)
			}
		case got != nil:
			t.Errorf("%s does not ship grove but sets kai-scheduler scheduler.args.%s = %v; KAI-only recipes keep its default cleanup", name, kaiStalenessArg, got)
		}
	}
	if withGrove == 0 {
		t.Fatal("no grove leaves were resolved -- the overlay walker is broken")
	}
}
