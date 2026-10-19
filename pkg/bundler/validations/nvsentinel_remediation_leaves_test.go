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

package validations

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelRemediationResetJobOnEveryLeaf composes the remediation
// step's nvsentinel values onto every shipped leaf and runs the pipeline
// gate. The GPU reset Job's driver root and RuntimeClass come from each
// platform overlay, so a leaf whose overlay disagrees with its GPU Operator
// fails here rather than on a node. GKE's managed driver install has no
// working driver root and must be rejected.
func TestNVSentinelRemediationResetJobOnEveryLeaf(t *testing.T) {
	ctx := t.Context()
	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	mixin, ok := store.Mixins["nvsentinel-remediation"]
	if !ok {
		t.Fatal("nvsentinel-remediation mixin not present; check recipes/mixins/")
	}
	var stepOverrides map[string]any
	for _, c := range mixin.Spec.ComponentRefs {
		if c.Name == nvsentinelComponent {
			stepOverrides = c.Overrides
		}
	}
	if stepOverrides == nil {
		t.Fatal("nvsentinel-remediation has no nvsentinel componentRef")
	}

	var leaves []string
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria != nil {
			leaves = append(leaves, name)
		}
	}
	sort.Strings(leaves)

	checked := 0
	for _, name := range leaves {
		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, store.Overlays[name].Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult: %v", err)
			}
			idx := slices.IndexFunc(result.ComponentRefs, func(r recipe.ComponentRef) bool { return r.Name == nvsentinelComponent })
			if idx < 0 {
				t.Skip("leaf does not deploy nvsentinel")
			}
			overrides := map[string]any{}
			mergeValuesForRender(overrides, result.ComponentRefs[idx].Overrides)
			mergeValuesForRender(overrides, stepOverrides)
			result.ComponentRefs[idx].Overrides = overrides

			_, errs := CheckNVSentinelRemediationPipelineCoherent(ctx, nvsentinelComponent, result, config.NewConfig(), nil)
			var reset []string
			for _, e := range errs {
				if strings.Contains(e.Error(), "GPU reset") {
					reset = append(reset, e.Error())
				}
			}
			gke := result.Criteria != nil && result.Criteria.Service == recipe.CriteriaServiceGKE &&
				result.Criteria.OS == recipe.CriteriaOSCOS
			switch {
			case gke && (len(reset) != 1 || !strings.Contains(reset[0], "not supported with GKE's managed driver install")):
				t.Errorf("GKE COS leaf: reset findings = %q, want only the managed-driver rejection", reset)
			case !gke && len(reset) > 0:
				t.Errorf("reset Job incoherent with this leaf's GPU Operator: %q", reset)
			}
			checked++
		})
	}
	if checked == 0 {
		t.Fatal("no leaf deploys nvsentinel -- the overlay walk is broken")
	}
}
