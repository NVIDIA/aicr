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
	"slices"
	"testing"
)

func TestTopographRegistryEntry(t *testing.T) {
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), "")
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		t.Fatalf("failed to load component registry: %v", err)
	}
	topograph := registry.Get("topograph")
	if topograph == nil {
		t.Fatal("registry has no topograph component")
	}
	slinky := registry.Get("slinky-topograph")
	if slinky == nil {
		t.Fatal("registry has no slinky-topograph component")
	}
	if topograph.Helm != slinky.Helm {
		t.Errorf("topograph helm = %+v, want the slinky-topograph pin %+v", topograph.Helm, slinky.Helm)
	}

	for _, path := range []string{
		"components/topograph/values.yaml",
		"checks/topograph/health-check.yaml",
	} {
		if _, readErr := provider.ReadFile(context.Background(), path); readErr != nil {
			t.Errorf("%s is not readable from embedded data: %v", path, readErr)
		}
	}
}

func TestKueueTASLeaves(t *testing.T) {
	tests := []struct {
		name         string
		criteria     Criteria
		wantProvider string
		wantTopology string
	}{
		{
			name: "kind",
			criteria: Criteria{
				Service: CriteriaServiceKind, Accelerator: CriteriaAcceleratorH100,
				Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKueue,
			},
			wantProvider: "dra",
			wantTopology: "components/kueue/manifests/topology-accelerator.yaml",
		},
		{
			name: "gke",
			criteria: Criteria{
				Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100,
				Intent: CriteriaIntentTraining, OS: CriteriaOSCOS, Platform: CriteriaPlatformKueue,
			},
			wantProvider: "gcp",
			wantTopology: "components/kueue/manifests/topology-tiers-3.yaml",
		},
		{
			name: "eks",
			criteria: Criteria{
				Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
				Intent: CriteriaIntentTraining, OS: CriteriaOSUbuntu, Platform: CriteriaPlatformKueue,
			},
			wantProvider: "aws",
			wantTopology: "components/kueue/manifests/topology-tiers-3.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crit := tt.criteria
			result, err := NewBuilder().BuildFromCriteria(t.Context(), &crit)
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}

			topograph := result.GetComponentRef("topograph")
			if topograph == nil {
				t.Fatal("recipe has no topograph component")
			}
			provider, _ := topograph.Overrides["provider"].(map[string]any)
			if got := provider["name"]; got != tt.wantProvider {
				t.Errorf("topograph provider = %v, want %q", got, tt.wantProvider)
			}

			kueue := result.GetComponentRef("kueue")
			if kueue == nil {
				t.Fatal("recipe has no kueue component")
			}
			if !slices.Contains(kueue.DependencyRefs, "topograph") {
				t.Errorf("kueue dependencyRefs = %v, want topograph", kueue.DependencyRefs)
			}
			wantManifests := []string{
				tt.wantTopology,
				"components/kueue/manifests/tas-flavor.yaml",
				"components/kueue/manifests/cluster-queue-tas.yaml",
				"components/kueue/manifests/local-queue.yaml",
			}
			if !slices.Equal(kueue.ManifestFiles, wantManifests) {
				t.Errorf("kueue manifestFiles = %v, want %v", kueue.ManifestFiles, wantManifests)
			}
			if kueue.HealthCheckAsserts == "" {
				t.Error("kueue healthCheckAsserts is empty; the registry check pins default-flavor")
			}

			order := result.DeploymentOrder
			if slices.Index(order, "topograph") >= slices.Index(order, "kueue") {
				t.Errorf("deploymentOrder = %v, want topograph before kueue", order)
			}
		})
	}
}

// TestKueueTASOptIn guards the opt-in contract: recipes without
// platform=kueue must not pick up kueue or topograph.
func TestKueueTASOptIn(t *testing.T) {
	result, err := NewBuilder().BuildFromCriteria(t.Context(), &Criteria{
		Service: CriteriaServiceKind, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining,
	})
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	for _, name := range []string{"kueue", "topograph"} {
		if result.GetComponentRef(name) != nil {
			t.Errorf("recipe without platform=kueue unexpectedly includes %s", name)
		}
	}
}
