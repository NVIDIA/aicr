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

import "testing"

// TestNVCRERegisteredWithoutOverlay keeps the public CRE chart installable
// from the registry without making it part of any shipped recipe. Overlay
// attachment is pinned separately by TestH100EKSTrainingCREStaysOptIn.
func TestNVCRERegisteredWithoutOverlay(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	var found bool
	for _, comp := range registry.Components {
		if comp.Name != "nvcre" {
			continue
		}
		found = true
		if comp.Helm.DefaultChart != "cluster-readiness-engine" {
			t.Errorf("nvcre helm chart = %q, want cluster-readiness-engine", comp.Helm.DefaultChart)
		}
		if comp.HealthCheck.AssertFile != "checks/nvcre/health-check.yaml" {
			t.Errorf("nvcre assertFile = %q, want checks/nvcre/health-check.yaml", comp.HealthCheck.AssertFile)
		}
		break
	}
	if !found {
		t.Fatal("nvcre is missing from recipes/registry.yaml")
	}
}
