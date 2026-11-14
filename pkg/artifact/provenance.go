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

package artifact

import "slices"

// Clone returns an independent provenance record, including nested targets.
func (p *Provenance) Clone() *Provenance {
	if p == nil {
		return nil
	}
	result := *p
	result.AllowCatalogPins = slices.Clone(p.AllowCatalogPins)
	result.Applied = slices.Clone(p.Applied)
	for i := range result.Applied {
		value := &result.Applied[i]
		value.Artifact = cloneIdentity(value.Artifact)
		if value.Chart != nil {
			chart := *value.Chart
			value.Chart = &chart
		}
		if value.Image != nil {
			image := *value.Image
			image.Targets = slices.Clone(image.Targets)
			for j := range image.Targets {
				image.Targets[j].Platform = clonePlatform(image.Targets[j].Platform)
			}
			value.Image = &image
		}
	}
	result.Ignored = slices.Clone(p.Ignored)
	for i := range result.Ignored {
		result.Ignored[i].Artifact = cloneIdentity(result.Ignored[i].Artifact)
	}
	return &result
}

func cloneIdentity(value Identity) Identity {
	value.Platform = clonePlatform(value.Platform)
	return value
}
func clonePlatform(value *Platform) *Platform {
	if value == nil {
		return nil
	}
	result := *value
	if value.Variant != nil {
		variant := *value.Variant
		result.Variant = &variant
	}
	return &result
}

// ValidateProvenance validates recorded metadata without requiring execution inputs.
func ValidateProvenance(p *Provenance) error {
	if p == nil {
		return nil
	}
	if !digestPattern.MatchString(p.InventoryDigest) || !digestPattern.MatchString(p.BindingsDigest) || !digestPattern.MatchString(p.BaseCatalogDigest) || p.Verification != "metadata-only" {
		return invalid("invalid artifact provenance digests or verification")
	}
	if p.AllowCatalogPins == nil || p.Applied == nil || p.Ignored == nil {
		return invalid("provenance requires exception and artifact arrays")
	}
	for i, name := range p.AllowCatalogPins {
		if !validName(name) || (i > 0 && p.AllowCatalogPins[i-1] >= name) {
			return invalid("provenance exceptions must be valid, sorted and unique")
		}
	}
	bindings := EmptyBindings()
	inventory := &Inventory{}
	for _, entry := range p.Applied {
		if err := validateIdentity(entry.Artifact); err != nil {
			return err
		}
		if (entry.Chart == nil) == (entry.Image == nil) {
			return invalid("applied artifact requires exactly one binding")
		}
		if entry.Chart != nil {
			bindings.Charts[entry.Artifact.Name] = *entry.Chart
		} else {
			bindings.Images[entry.Artifact.Name] = *entry.Image
		}
		inventory.Artifacts = append(inventory.Artifacts, entry.Artifact)
	}
	for _, entry := range p.Ignored {
		if err := validateIdentity(entry.Artifact); err != nil {
			return err
		}
		bindings.IgnoredArtifacts[entry.Artifact.Name] = Ignored{Reason: entry.Reason}
		inventory.Artifacts = append(inventory.Artifacts, entry.Artifact)
	}
	seen := map[string]bool{}
	for _, entry := range inventory.Artifacts {
		if !validName(entry.Name) || seen[entry.Name] {
			return invalid("invalid or duplicate recorded artifact name")
		}
		seen[entry.Name] = true
	}
	if err := ValidateBindings(bindings); err != nil {
		return err
	}
	return Account(inventory, bindings)
}
