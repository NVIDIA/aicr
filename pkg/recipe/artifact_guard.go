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
	"path"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// CheckSelection enforces coverage only for selected, enabled Helm components.
// Exceptions waive missing matches, never active-target participation or identity.
func (p *ArtifactProvider) CheckSelection(ctx context.Context, refs []ComponentRef) error {
	matches := map[string]bool{}
	for _, item := range p.inventory.Artifacts {
		matches[item.Name] = true
	}
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "check artifact coverage")
		}
		if !ref.IsEnabled() || ref.Type != ComponentTypeHelm {
			continue
		}
		chartCovered := false
		for name, binding := range p.bindings.Charts {
			if binding.Component == ref.Name && matches[name] {
				chartCovered = true
			}
		}
		covered := chartCovered
		for name, binding := range p.bindings.Images {
			for _, target := range binding.Targets {
				if target.Component != ref.Name {
					continue
				}
				if !matches[name] {
					covered = false
					continue
				}
				if ref.ValuesFile == "" || (target.ValuesFile != ref.ValuesFile && target.ValuesFile != BaseValuesFile(ref.Name)) {
					return artifact.Invalid("artifact-target-invalid", "bound values file does not participate for "+ref.Name+": "+target.ValuesFile)
				}
			}
		}
		if !covered && !slices.Contains(p.provenance.AllowCatalogPins, ref.Name) {
			return artifact.Invalid("artifact-coverage-required", "selected Helm component is not fully covered: "+ref.Name+"; supply its chart and declared images or allowCatalogPins")
		}
	}
	return nil
}

// CheckIdentities compares final values and chart coordinates with active bindings.
// A nil values map resolves the provider's values and the refs' inline overrides.
func (p *ArtifactProvider) CheckIdentities(ctx context.Context, refs []ComponentRef, values map[string]map[string]any, dynamicPaths map[string][]string) error {
	selected := map[string]ComponentRef{}
	for _, ref := range refs {
		if ref.IsEnabled() {
			selected[ref.Name] = ref
		}
	}
	for _, applied := range p.provenance.Applied {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "guard bound artifact identities")
		}
		identity := applied.Artifact
		if applied.Chart != nil {
			if ref, ok := selected[applied.Chart.Component]; ok {
				repository, chart := path.Split(identity.Reference)
				repository = strings.TrimSuffix(repository, "/")
				if ref.Type != ComponentTypeHelm || ref.Source != repository || ref.EffectiveChart() != chart || ref.Version != identity.Version {
					return artifact.Invalid("artifact-identity-conflict", "chart identity differs from binding for "+ref.Name)
				}
			}
		}
		if applied.Image == nil {
			continue
		}
		for _, target := range applied.Image.Targets {
			ref, ok := selected[target.Component]
			if !ok {
				continue
			}
			candidate := values[target.Component]
			if values == nil {
				var err error
				candidate, err = GetComponentValuesWithContext(ctx, p, &ref)
				if err != nil {
					return err
				}
			}
			expected := []struct{ pointer, value string }{
				{target.Write.Reference, strings.TrimPrefix(identity.Reference, "oci://") + "@" + identity.ManifestDigest},
				{target.Write.Repository, strings.TrimPrefix(identity.Reference, "oci://")},
				{target.Write.Digest, identity.ManifestDigest}, {target.Write.ClearTag, ""},
			}
			for _, entry := range expected {
				if entry.pointer == "" {
					continue
				}
				parts, err := artifact.Pointer(entry.pointer)
				if err != nil {
					return err
				}
				for _, dynamic := range dynamicPaths[target.Component] {
					other := strings.Split(dynamic, ".")
					common := min(len(parts), len(other))
					if slices.Equal(parts[:common], other[:common]) {
						return artifact.Invalid("artifact-identity-conflict", "dynamic value overlaps bound image identity for "+target.Component+": "+dynamic)
					}
				}
				current := candidate
				for _, part := range parts[:len(parts)-1] {
					child, ok := current[part].(map[string]any)
					if !ok {
						return artifact.Invalid("artifact-identity-conflict", "bound image parent changed for "+target.Component+": "+entry.pointer)
					}
					current = child
				}
				actual, ok := current[parts[len(parts)-1]].(string)
				if !ok || actual != entry.value {
					return artifact.Invalid("artifact-identity-conflict", "image identity differs from binding for "+target.Component+": "+entry.pointer)
				}
			}
		}
	}
	return nil
}

// ArtifactProvenance returns the retained composition record without shared slices.
func (p *ArtifactProvider) ArtifactProvenance() *artifact.Provenance { return p.provenance.Clone() }

// CheckArtifactInputs validates a saved composition before attaching a provider.
func CheckArtifactInputs(record *artifact.Provenance, provider DataProvider) error {
	composed, active := provider.(*ArtifactProvider)
	if record == nil {
		if active {
			return artifact.Invalid("artifact-provenance-required", "saved recipe has no artifact provenance; re-resolve it under the chosen inventory")
		}
		return nil
	}
	if !active {
		return artifact.Invalid("artifact-inputs-required", "artifact-bearing recipe requires its original inventory, bindings, catalog and exceptions")
	}
	recorded, err := canonicalArtifactDigest(record)
	if err != nil {
		return err
	}
	expected, err := canonicalArtifactDigest(&composed.provenance)
	if err != nil {
		return err
	}
	if recorded != expected {
		return artifact.Invalid("artifact-inputs-mismatch", "saved recipe provenance differs from the supplied artifact inputs; restore the original inputs or re-resolve")
	}
	return nil
}

// ValidateArtifactLock guards execution after final values and identity inheritance.
func (r *RecipeResult) ValidateArtifactLock(ctx context.Context, refs []ComponentRef, values map[string]map[string]any, dynamicPaths map[string][]string) error {
	if r == nil {
		return nil
	}
	if err := CheckArtifactInputs(r.Metadata.Artifacts, r.provider); err != nil {
		return err
	}
	if composed, ok := r.provider.(*ArtifactProvider); ok {
		if err := composed.CheckSelection(ctx, refs); err != nil {
			return err
		}
		return composed.CheckIdentities(ctx, refs, values, dynamicPaths)
	}
	return nil
}
