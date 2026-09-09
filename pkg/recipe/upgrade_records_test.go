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
	"testing"

	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// TestRealUpgradeRecordsWellFormed runs the ADR-021 well-formedness rules over
// the committed recipes/upgrades/ tree.
//
// It is inert while no component sets upgrades.file, and becomes live the
// moment one does. It asserts no verdict — only that whatever was authored is
// well-formed — so "make the test green" cannot be satisfied by writing safe.
func TestRealUpgradeRecordsWellFormed(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	provider := defaultEmbeddedProvider

	var comps []upgrade.Component
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
	t.Logf("checking %d component(s) with upgrade records", len(comps))

	set, err := upgrade.Load(context.Background(), provider, comps)
	if err != nil {
		t.Fatalf("loading upgrade records: %v", err)
	}
	if err := set.Validate(comps); err != nil {
		t.Fatalf("upgrade records are not well-formed: %v", err)
	}
}

func pinnedVersionFor(c *ComponentConfig) string {
	if c.Helm.DefaultVersion != "" {
		return c.Helm.DefaultVersion
	}
	return c.Kustomize.DefaultTag
}
