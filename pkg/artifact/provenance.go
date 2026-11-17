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
	if p.CRDOwnership != nil {
		policy := *p.CRDOwnership
		policy.Changed = slices.Clone(policy.Changed)
		policy.Owners = slices.Clone(policy.Owners)
		result.CRDOwnership = &policy
	}
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
	if err := validateCRDOwnership(p); err != nil {
		return err
	}
	return Account(inventory, bindings)
}

func validateCRDOwnership(p *Provenance) error {
	policy := p.CRDOwnership
	if policy == nil {
		return Invalid("artifact-inputs-mismatch", "artifact provenance lacks crdOwnership; re-resolve the recipe")
	}
	if policy.Changed == nil || policy.Owners == nil {
		return invalid("provenance requires crdOwnership and its arrays")
	}
	for _, names := range [][]string{policy.Changed, policy.Owners} {
		for i, name := range names {
			if !validName(name) || (i > 0 && names[i-1] >= name) {
				return invalid("crdOwnership names must be valid, sorted and unique")
			}
		}
	}
	charts := map[string]string{}
	for _, entry := range p.Applied {
		if entry.Chart != nil {
			charts[entry.Chart.Component] = entry.Chart.CRDOwnership
		}
	}
	unretained := false
	for _, name := range policy.Changed {
		retention, ok := charts[name]
		if !ok {
			return invalid("crdOwnership changed component requires an applied chart")
		}
		unretained = unretained || retention != CRDOwnershipRetain
	}
	switch policy.Policy {
	case CRDPolicyUnchanged:
		if len(policy.Changed) != 0 {
			return invalid("unchanged crdOwnership requires no changed charts")
		}
	case CRDPolicyRetained:
		if len(policy.Changed) == 0 || unretained {
			return invalid("retained crdOwnership requires every changed chart to retain")
		}
	case CRDPolicyDisabled:
		if !unretained {
			return invalid("disabled crdOwnership requires a changed chart without retain")
		}
	default:
		return invalid("invalid crdOwnership policy")
	}
	return nil
}
