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

// TestPrecompiledDriverExcludesGDRCopy asserts that no resolved recipe turns
// on both driver.usePrecompiled and gdrcopy.enabled in gpu-operator's merged
// values. GPU Operator rejects the pair ("GDRCopy is not supported along with
// pre-compiled NVIDIA drivers") and returns before creating the driver
// DaemonSet, so no GPU node gets a driver. gdrcopy.enabled: true comes from
// components/gpu-operator/values.yaml rather than from the overlay that turns
// on precompiled drivers, which is why the check reads merged values.
//
// An overlay whose criteria leave intent unset is also resolved once per
// intent: rke2-sles composes with rke2-training and rke2-inference at query
// time, and no single overlay holds those combinations.
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

		for _, q := range queries {
			t.Run(name+"/"+q.criteria.String(), func(t *testing.T) {
				result, err := store.BuildRecipeResult(ctx, &q.criteria)
				// An intent added above may name a combination the catalog
				// does not offer; the overlay's own criteria must resolve.
				if q.synthesized && stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
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
