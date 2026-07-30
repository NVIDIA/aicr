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
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
)

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

	// Profile is the effective declaration reachable from this overlay,
	// nil for an unprofiled entry. Only ListCatalogWithProfiles populates
	// it; ListCatalog leaves it nil because projection requires validating
	// every reachable declaration and a context for cancellation.
	Profile *ProfileSummary `json:"profile,omitempty" yaml:"profile,omitempty"`
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

// ListCatalogWithProfiles returns the catalog projection with each entry's
// effective inherited/co-matched profile declaration. It reuses the same
// pre-filter declaration resolver as recipe generation so discovery cannot
// disagree with selection, and fails without a partial result when ctx is
// canceled.
func (s *MetadataStore) ListCatalogWithProfiles(
	ctx context.Context,
	filter *Criteria,
) ([]CatalogEntry, error) {

	if s == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "metadata store is required")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(
			errors.ErrCodeTimeout, "catalog profile projection canceled before enumeration", ctxErr)
	}
	entries := s.ListCatalog(filter)
	resolvedByCriteria := make(map[Criteria]*effectiveProfileDeclaration)
	for i := range entries {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(
				errors.ErrCodeTimeout, "catalog profile projection canceled before completion", ctxErr)
		}
		key := *entries[i].Criteria
		effective, resolved := resolvedByCriteria[key]
		if !resolved {
			var err error
			effective, err = s.resolveProfileDeclaration(s.FindMatchingOverlays(entries[i].Criteria))
			if err != nil {
				return nil, err
			}
			resolvedByCriteria[key] = effective
		}
		if effective != nil {
			entries[i].Profile = profileSummary(effective.Declaration)
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.Wrap(
			errors.ErrCodeTimeout, "catalog profile projection canceled before completion", ctxErr)
	}
	return entries, nil
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
