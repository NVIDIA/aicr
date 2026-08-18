// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr_test

import (
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// TestBundleComponentsSucceedsForEveryShippedPlatform pins the contract that
// makes running the NVSentinel gates on this path safe: every platform the
// embedded catalog ships must resolve values that already satisfy them.
//
// Client.BundleComponents is the values-only SDK path. It produces no
// deployable artifact and exposes no --set channel, so it passes a nil bundler
// config. The two NVSentinel gates used to no-op on that nil config precisely
// because their remedy was a bundle-time flag; #2181 removed the exemption
// once the recipes started carrying the values themselves.
//
// The exemption and the recipe data are therefore two halves of one contract,
// and removing the first without completing the second strands a platform with
// no expressible fix: the gate's message tells the caller to pass a --set that
// this API cannot accept. That is exactly what happened to Kind — its recipe
// sets gpu-operator driver.enabled=false but was left without
// labeler.assumeDriverInstalled, so BundleComponents returned INVALID_REQUEST
// for every Kind recipe until the overlay supplied the value.
//
// Enumerating the platforms here rather than testing one is the point: the
// regression was a platform omitted from the matrix, which a single-platform
// smoke test cannot catch. A new service added to the catalog should be added
// to this list.
func TestBundleComponentsSucceedsForEveryShippedPlatform(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	tests := []struct {
		name     string
		criteria *aicr.Criteria
	}{
		{"aks training", &aicr.Criteria{Service: "aks", Accelerator: "h100", OS: "ubuntu", Intent: "training"}},
		{"gke-cos training", &aicr.Criteria{Service: "gke", Accelerator: "h100", OS: "cos", Intent: "training"}},
		{"oke training", &aicr.Criteria{Service: "oke", Accelerator: "a100", OS: "ol", Intent: "training"}},
		{"eks training", &aicr.Criteria{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training"}},
		{"kind training", &aicr.Criteria{Service: "kind", Accelerator: "h100", Intent: "training"}},
		{"kind inference", &aicr.Criteria{Service: "kind", Accelerator: "h100", Intent: "inference"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolved, err := client.ResolveRecipeFromCriteria(t.Context(), tt.criteria)
			if err != nil {
				t.Fatalf("ResolveRecipeFromCriteria() error = %v", err)
			}

			components, err := client.BundleComponents(t.Context(), resolved)
			if err != nil {
				t.Fatalf("BundleComponents() error = %v\n"+
					"  The values-only SDK path has no --set channel, so a gate that fires here\n"+
					"  leaves the caller no expressible fix. If this is the NVSentinel driver-label\n"+
					"  or RuntimeClass gate, the recipe for this platform is missing a value that\n"+
					"  #2181 requires it to carry.", err)
			}

			// Control: an empty component set would satisfy the gates
			// vacuously, since every NVSentinel check keys off a resolved
			// nvsentinel component ref.
			if len(components) == 0 {
				t.Fatal("BundleComponents() returned no components — the check above would be vacuous")
			}
		})
	}
}
