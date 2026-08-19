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
	"sort"
	"testing"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
)

// Vacuity floors for the catalog-derived sweep below. They are deliberately
// well under the real counts (45 leaves across 10 service values at the time
// of writing) — they exist to catch a ListCatalog that returns nothing or a
// catalog that silently collapses, not to track catalog growth.
const (
	minCatalogLeaves   = 25
	minCatalogServices = 8
)

// TestBundleComponentsSucceedsForEveryCatalogLeaf pins the contract that
// makes running the NVSentinel gates on the values-only path safe: every
// recipe the embedded catalog ships must resolve values that already satisfy
// them.
//
// Client.BundleComponents is the values-only SDK path. It produces no
// deployable artifact and exposes no --set channel, so it passes a nil
// bundler config. The two NVSentinel gates used to no-op on that nil config
// precisely because their remedy was a bundle-time flag; #2181 removed the
// exemption once the recipes started carrying the values themselves.
//
// The exemption and the recipe data are therefore two halves of one contract,
// and removing the first without completing the second strands a platform
// with no expressible fix: the gate's message tells the caller to pass a
// --set that this API cannot accept. That is exactly what happened to Kind —
// its recipe sets gpu-operator driver.enabled=false but was left without
// labeler.assumeDriverInstalled, so BundleComponents returned INVALID_REQUEST
// for every Kind recipe until the overlay supplied the value.
//
// The sweep is DERIVED FROM THE CATALOG rather than hardcoded. An earlier
// revision enumerated a fixed platform list, which reproduced in miniature
// the very failure mode this guard exists to catch: the list omitted bcm, lke
// and ocp, so a service could be added — or an existing overlay could start
// disabling the driver — without the guard noticing. Walking ListCatalog
// means a new service or leaf is covered the moment it ships, with no list to
// maintain.
func TestBundleComponentsSucceedsForEveryCatalogLeaf(t *testing.T) {
	t.Parallel()

	client, err := aicr.NewClient(aicr.WithRecipeSource(aicr.EmbeddedSource()))
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	entries, err := client.ListCatalog(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListCatalog() error = %v", err)
	}

	leaves := make([]aicr.CatalogEntry, 0, len(entries))
	services := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsLeaf {
			continue
		}
		leaves = append(leaves, entry)
		services[entry.Criteria.Service] = struct{}{}
	}

	// Vacuity controls: a catalog that returned nothing, or collapsed to a
	// couple of services, would let every subtest below pass while
	// exercising almost nothing.
	if len(leaves) < minCatalogLeaves {
		t.Fatalf("catalog has %d leaves, want at least %d — the sweep would be near-vacuous",
			len(leaves), minCatalogLeaves)
	}
	if len(services) < minCatalogServices {
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Fatalf("catalog leaves span %d service values %v, want at least %d",
			len(services), names, minCatalogServices)
	}

	// Counts how many leaves actually put nvsentinel in front of the gates.
	// A leaf without it is legitimate, but if NO leaf carries it the sweep
	// proves nothing about the gates: both skip on an absent component ref.
	sentinelSeen := make(chan string, len(leaves))

	for _, leaf := range leaves {
		t.Run(leaf.Name, func(t *testing.T) {
			t.Parallel()

			criteria := leaf.Criteria
			resolved, err := client.ResolveRecipeFromCriteria(t.Context(), &criteria)
			if err != nil {
				t.Fatalf("ResolveRecipeFromCriteria(%+v) error = %v", criteria, err)
			}

			components, err := client.BundleComponents(t.Context(), resolved)
			if err != nil {
				t.Fatalf("BundleComponents() error = %v\n"+
					"  The values-only SDK path has no --set channel, so a gate that fires here\n"+
					"  leaves the caller no expressible fix. If this is the NVSentinel driver-label\n"+
					"  or RuntimeClass gate, this recipe is missing a value that #2181 requires it\n"+
					"  to carry.", err)
			}
			if len(components) == 0 {
				t.Fatal("BundleComponents() returned no components — this leaf proves nothing")
			}

			// BundleComponents returns only the components the bundle
			// would carry, so presence here means the gates had a subject.
			for i := range components {
				if components[i].Component.Name == "nvsentinel" {
					sentinelSeen <- leaf.Name
					break
				}
			}
		})
	}

	t.Cleanup(func() {
		close(sentinelSeen)
		if len(sentinelSeen) == 0 {
			t.Errorf("no catalog leaf resolved an enabled nvsentinel component — " +
				"both NVSentinel gates skip on a nil component ref, so this sweep " +
				"would pass without ever exercising them")
		}
	})
}
