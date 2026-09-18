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

	"github.com/NVIDIA/aicr/pkg/inventory"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// UpgradeComponents projects the registry entries that reference an ADR-021
// transition record onto the shape pkg/upgrade reads, sorted by name so a
// caller's Load order does not depend on registry declaration order.
//
// Entries with no upgrades.file are omitted: pkg/upgrade skips them anyway, and
// carrying them would make Load's duplicate-name guard report on components
// that reference nothing.
func UpgradeComponents(registry *ComponentRegistry) []upgrade.Component {
	if registry == nil {
		return nil
	}
	comps := make([]upgrade.Component, 0, len(registry.Components))
	for i := range registry.Components {
		c := &registry.Components[i]
		if c.Upgrades.File == "" {
			continue
		}
		comps = append(comps, upgrade.Component{
			Name:          c.Name,
			File:          c.Upgrades.File,
			PinnedVersion: pinnedVersionFor(c),
		})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	return comps
}

// InventoryComponents projects every registry entry onto the facts
// pkg/inventory needs to attribute a cluster record to a component and read a
// version off it, sorted by name so a caller's output does not depend on
// registry declaration order.
//
// Unlike UpgradeComponents next door, nothing is filtered. A component with no
// transition record still has a release in the cluster, and the mapping layer
// counts the records that match no component as its signal that the mapping
// itself is broken — omitting the recordless entries here would turn that
// signal into noise.
func InventoryComponents(registry *ComponentRegistry) []inventory.Component {
	if registry == nil {
		return nil
	}
	comps := make([]inventory.Component, 0, len(registry.Components))
	for i := range registry.Components {
		c := &registry.Components[i]
		comps = append(comps, inventory.Component{
			Name:      c.Name,
			Namespace: c.Helm.DefaultNamespace,
			// Not pinnedVersionFor: a Kustomize defaultTag is the git ref of
			// content AICR wraps in a chart of its own, so the wrapper's chart
			// version is never the payload's and the fallback must not fire.
			HasUpstreamChart: c.Helm.DefaultVersion != "",
		})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	return comps
}

// LoadUpgradeRecords reads every transition record dp's registry references and
// returns the loaded set alongside the components it was built from.
//
// It does not validate. Loading and validating answer different questions (see
// the pkg/upgrade package doc), and the components are returned precisely so a
// caller that wants the well-formedness rules can pass them to Set.Validate.
func LoadUpgradeRecords(ctx context.Context, dp DataProvider) (upgrade.Set, []upgrade.Component, error) {
	registry, err := GetComponentRegistryFor(dp)
	if err != nil {
		return nil, nil, err
	}
	comps := UpgradeComponents(registry)
	if dp == nil {
		dp = defaultEmbeddedProvider
	}
	set, err := upgrade.Load(ctx, dp, comps)
	if err != nil {
		return nil, nil, err
	}
	return set, comps, nil
}

// pinnedVersionFor is the version a record's `to` ceiling is checked against:
// the chart version for a Helm component, the tag for a Kustomize one. A
// component declares one or the other, never both.
func pinnedVersionFor(c *ComponentConfig) string {
	if c.Helm.DefaultVersion != "" {
		return c.Helm.DefaultVersion
	}
	return c.Kustomize.DefaultTag
}
