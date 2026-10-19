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
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestPrecompiledDriverExcludesGDRCopy asserts that no resolved recipe enables
// both driver.usePrecompiled and gdrcopy.enabled in gpu-operator's merged
// values. It reads merged values because gdrcopy.enabled comes from
// components/gpu-operator/values.yaml, not from the overlay that turns on
// precompiled drivers, and it resolves each intent an overlay leaves unset
// because rke2-sles composes with rke2-training and rke2-inference at query
// time.
func TestPrecompiledDriverExcludesGDRCopy(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	precompiled := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		type query struct {
			criteria    Criteria
			synthesized bool
		}
		queries := []query{{criteria: *overlay.Spec.Criteria}}
		if intent := overlay.Spec.Criteria.Intent; intent == "" || intent == CriteriaIntentAny {
			for _, i := range []CriteriaIntentType{CriteriaIntentTraining, CriteriaIntentInference} {
				q := *overlay.Spec.Criteria
				q.Intent = i
				queries = append(queries, query{criteria: q, synthesized: true})
			}
		}

		// Set by the overlay's own query, which runs first and serially.
		ownPrecompiled := false
		for _, q := range queries {
			t.Run(name+"/"+q.criteria.String(), func(t *testing.T) {
				result, err := store.BuildRecipeResult(ctx, &q.criteria)
				// An intent added above may name a combination the catalog
				// does not offer, unless the overlay itself uses precompiled
				// drivers: its intents are the combinations this test exists
				// to check, so they must resolve.
				if q.synthesized && !ownPrecompiled && stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Skipf("catalog offers no such combination: %v", err)
				}
				if err != nil {
					t.Fatalf("BuildRecipeResult: %v", err)
				}
				op := result.GetComponentRef("gpu-operator")
				if op == nil || !op.IsEnabled() {
					return
				}
				values, err := result.GetValuesForComponentWithContext(ctx, "gpu-operator")
				if err != nil {
					t.Fatalf("GetValuesForComponent(gpu-operator): %v", err)
				}
				if !boolAtPath(values, false, "driver", "usePrecompiled") {
					return
				}
				if !q.synthesized {
					ownPrecompiled = true
				}
				precompiled++
				if boolAtPath(values, false, "gdrcopy", "enabled") {
					t.Errorf("gpu-operator sets driver.usePrecompiled: true with gdrcopy.enabled: true; " +
						"GPU Operator creates no driver DaemonSet for that pair. Set gdrcopy.enabled: false " +
						"in the overlay that turns on precompiled drivers")
				}
			})
		}
	}

	if precompiled == 0 {
		t.Fatal("no resolved recipe sets driver.usePrecompiled: true, so this test checked nothing; " +
			"if the values path moved, update it here")
	}
}
