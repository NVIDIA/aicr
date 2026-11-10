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
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelLabelerReadsNoResourceSlices fails when a device-count class
// in the values a bundle ships reads ResourceSlices. From NVSentinel v1.30.0
// the labeler then starts a resource.k8s.io/v1 informer and skips every node
// reconcile until it syncs; v1 is served from Kubernetes 1.34, and recipes
// support 1.32 and 1.33, where it never does.
func TestNVSentinelLabelerReadsNoResourceSlices(t *testing.T) {
	store, err := recipe.LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}

	tests := []struct {
		name   string
		mixins []string
	}{
		{"base chain", []string{""}},
		{"remediation step", []string{"nvsentinel-remediation"}},
		{"observability", []string{"nvsentinel-observability"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := nvsentinelRenderedValues(t, store, tt.mixins...)
			labeler, _ := values["labeler"].(map[string]any)
			counts, _ := labeler["expectedDeviceCounts"].(map[string]any)
			classes, _ := counts["classes"].([]any)
			if len(classes) == 0 {
				t.Fatal("labeler.expectedDeviceCounts.classes is unset, so the chart's default class, " +
					"which reads ResourceSlices, applies")
			}
			// The list replaces the chart's, so a class left out stops being tracked.
			names := map[string]bool{}
			for _, raw := range classes {
				class, _ := raw.(map[string]any)
				name, _ := class["name"].(string)
				names[name] = true
			}
			for _, want := range []string{"gpu", "nic"} {
				if !names[want] {
					t.Errorf("class %q is missing; the chart enables it by default and this list replaces the default", want)
				}
			}
			for i, raw := range classes {
				class, _ := raw.(map[string]any)
				if _, ok := class["sources"]; ok {
					t.Errorf("class %d sets sources; a source can read ResourceSlices", i)
				}
				for _, key := range []string{"currentExpression", "groupingExpression"} {
					if expr, _ := class[key].(string); strings.Contains(expr, "resourceSlices") {
						t.Errorf("class %d %s reads resourceSlices", i, key)
					}
				}
				if expr, _ := class["currentExpression"].(string); strings.TrimSpace(expr) == "" {
					t.Errorf("class %d has no currentExpression", i)
				}
			}
		})
	}
}
