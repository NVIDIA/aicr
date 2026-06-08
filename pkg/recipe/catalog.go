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

import "sort"

// CatalogEntry describes a single overlay entry in the recipe catalog.
//
// IsLeaf is true when the overlay is a leaf — no other overlay in the
// catalog lists this one as its spec.base. Leaf overlays are the most
// specific recipes for a given criteria combination; intermediate overlays
// (which exist to share constraints across multiple leaves) are not leaves.
//
// Source reflects where the overlay data came from: "embedded" for the
// built-in OSS overlays, "external" for overlays loaded via --data.
type CatalogEntry struct {
	Name     string    `json:"name"     yaml:"name"`
	Criteria *Criteria `json:"criteria" yaml:"criteria"`
	IsLeaf   bool      `json:"is_leaf"  yaml:"is_leaf"`
	Source   string    `json:"source"   yaml:"source"`
}

// ListCatalog returns catalog entries for overlays in the store that have
// non-nil Criteria, optionally narrowed by filter. Overlays without a
// Criteria block (e.g. the base recipe) are silently excluded.
//
// IsLeaf is set to true for leaf overlays — those that are not the
// base of any other overlay in the catalog.
//
// When filter is non-nil, only overlays whose criteria satisfy every
// explicitly-set filter dimension are returned. The filter uses simple
// equality: a filter dimension set to "any" or "" places no constraint
// on that dimension, while a specific value (e.g., "eks") restricts
// results to overlays whose criteria carry exactly that value.
//
// Entries are returned in ascending name order for deterministic output.
func (s *MetadataStore) ListCatalog(filter *Criteria) []CatalogEntry {
	// Identify ancestor overlays: any overlay listed as spec.base of
	// another overlay is not a leaf.
	ancestors := make(map[string]bool, len(s.Overlays))
	for _, overlay := range s.Overlays {
		if overlay.Spec.Base != "" && overlay.Spec.Base != baseRecipeName {
			ancestors[overlay.Spec.Base] = true
		}
	}

	entries := make([]CatalogEntry, 0, len(s.Overlays))
	for name, overlay := range s.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		if filter != nil && !matchesCatalogFilter(overlay.Spec.Criteria, filter) {
			continue
		}

		source := sourceEmbedded
		if src, ok := s.OverlaySources[name]; ok {
			source = src
		}

		critCopy := *overlay.Spec.Criteria
		entries = append(entries, CatalogEntry{
			Name:     name,
			Criteria: &critCopy,
			IsLeaf:   !ancestors[name],
			Source:   source,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	return entries
}

// matchesCatalogFilter reports whether overlayCriteria satisfies every
// explicitly-set dimension of filter. A filter dimension that is empty or
// "any" places no constraint. This is a simple equality check — unlike the
// asymmetric Criteria.Matches used for recipe resolution, this predicate
// asks "does this overlay carry the values I asked for?" without wildcard
// promotion, so --accelerator h100 returns overlays explicitly specifying
// h100, not overlays where accelerator=any.
func matchesCatalogFilter(overlayCriteria *Criteria, filter *Criteria) bool {
	if filter == nil {
		return true
	}
	if filter.Service != "" && filter.Service != CriteriaServiceAny {
		if overlayCriteria.Service != filter.Service {
			return false
		}
	}
	if filter.Accelerator != "" && filter.Accelerator != CriteriaAcceleratorAny {
		if overlayCriteria.Accelerator != filter.Accelerator {
			return false
		}
	}
	if filter.Intent != "" && filter.Intent != CriteriaIntentAny {
		if overlayCriteria.Intent != filter.Intent {
			return false
		}
	}
	if filter.OS != "" && filter.OS != CriteriaOSAny {
		if overlayCriteria.OS != filter.OS {
			return false
		}
	}
	if filter.Platform != "" && filter.Platform != CriteriaPlatformAny {
		if overlayCriteria.Platform != filter.Platform {
			return false
		}
	}
	return true
}
