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

package aicrjob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Bundle layout identity at AICR v1.0.0 (pkg/bundler/bundleinfo).
const (
	BundleInfoKind       = "BundleInfo"
	BundleInfoAPIVersion = "aicr.run/v1"
	BundleInfoFile       = "bundle-info.yaml"
	// HelmDeployer is the engine-neutral layout the operator renders.
	HelmDeployer = "helm"
)

// Suffixes AICR reserves for releases it injects around a component. Each
// injected release names its parent component; the suffix is its phase.
var injectedPhases = []struct {
	suffix string
	phase  plan.Phase
}{
	{"-pre", plan.PhasePre},
	{"-post", plan.PhasePost},
	{"-readiness", plan.PhaseReadiness},
}

// BundleInfo is the subset of AICR's bundle-root build record the operator
// reads. Fields AICR adds later are ignored.
type BundleInfo struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   BundleMetadata `json:"metadata"`
	Build      BundleBuild    `json:"build"`
	Layout     BundleLayout   `json:"layout"`
}

// BundleMetadata identifies the AICR release that rendered the bundle.
type BundleMetadata struct {
	Version string `json:"version"`
}

// BundleBuild records how the bundle was rendered.
type BundleBuild struct {
	Deployer string         `json:"deployer"`
	Recipe   BundleRecipe   `json:"recipe"`
	Settings BundleSettings `json:"settings"`
}

// BundleRecipe binds the record to the recipe file beside it.
type BundleRecipe struct {
	Path    string `json:"path"`
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
}

// BundleSettings are the effective render settings the operator checks.
type BundleSettings struct {
	ReadinessHooks bool `json:"readinessHooks"`
	// Components is a component filter; a filtered bundle is not a plan.
	Components []string `json:"components,omitempty"`
}

// BundleLayout lists the releases in normative deployment order.
type BundleLayout struct {
	Entrypoint string          `json:"entrypoint"`
	Releases   []BundleRelease `json:"releases"`
}

// BundleRelease is one Helm release the bundle installs.
type BundleRelease struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Namespace string `json:"namespace,omitempty"`
	Path      string `json:"path"`
	// Phase is inferred from Name and Component; it is not in the record.
	Phase plan.Phase `json:"-"`
}

// bundleInfoRequired are the record's sections the operator cannot work without.
var bundleInfoRequired = withHeader("build", "layout")

// ParseBundleInfo decodes bundle-info.yaml from a bundle the operator
// rendered and applies the contract checks: identity, the version gate, the
// engine-neutral layout with readiness gates and no component filter, paths
// inside the bundle, and a phase for every release.
func ParseBundleInfo(data []byte, declaredVersion string) (*BundleInfo, error) {
	doc, err := yamlToJSON(data, BundleInfoFile)
	if err != nil {
		return nil, err
	}
	if err = requireFields(doc, BundleInfoFile, bundleInfoRequired); err != nil {
		return nil, err
	}
	b := &BundleInfo{}
	if err = json.Unmarshal(doc, b); err != nil {
		return nil, unsupported("decode %s: %v", BundleInfoFile, err)
	}
	if b.Kind != BundleInfoKind || b.APIVersion != BundleInfoAPIVersion {
		return nil, unsupported("unsupported artifact %s %s", b.APIVersion, b.Kind)
	}
	if err = CheckArtifactVersion(declaredVersion, b.Metadata.Version); err != nil {
		return nil, err
	}
	switch {
	case b.Build.Deployer != HelmDeployer:
		return nil, unsupported("bundle was rendered for deployer %q, want %q", b.Build.Deployer, HelmDeployer)
	case !b.Build.Settings.ReadinessHooks:
		return nil, unsupported("bundle was rendered without readiness gates")
	case len(b.Build.Settings.Components) > 0:
		return nil, unsupported("bundle was rendered for a subset of components %v", b.Build.Settings.Components)
	case b.Build.Recipe.Digest == "" || b.Layout.Entrypoint == "":
		return nil, unsupported("%s is missing build.recipe.digest or layout.entrypoint", BundleInfoFile)
	}
	for _, p := range []string{b.Build.Recipe.Path, b.Layout.Entrypoint} {
		if !filepath.IsLocal(p) {
			return nil, unsupported("bundle path %q is not inside the bundle", p)
		}
	}
	return b, b.inferPhases()
}

// inferPhases assigns each release its phase and checks the layout: unique
// names and paths (so at most one injected release per phase), exactly one
// primary release per component, and injected releases on the correct side
// of their primary.
func (b *BundleInfo) inferPhases() error {
	components := map[string]bool{}
	for i := range b.Layout.Releases {
		components[b.Layout.Releases[i].Component] = true
	}
	names, paths := map[string]bool{}, map[string]bool{}
	primary := map[string]bool{}
	for i := range b.Layout.Releases {
		r := &b.Layout.Releases[i]
		switch {
		case r.Name == "" || r.Component == "" || r.Path == "":
			return unsupported("release %d is missing name, component or path", i)
		case !filepath.IsLocal(r.Path):
			return unsupported("release %q path %q is not inside the bundle", r.Name, r.Path)
		case names[r.Name] || paths[r.Path]:
			return unsupported("release %q or path %q is repeated", r.Name, r.Path)
		}
		names[r.Name], paths[r.Path] = true, true
		if err := ambiguous(r.Component, components); err != nil {
			return err
		}
		phase, err := phaseOf(r)
		if err != nil {
			return err
		}
		r.Phase = phase
		seen := primary[r.Component]
		switch phase {
		case plan.PhasePrimary:
			if seen {
				return unsupported("component %q has more than one primary release", r.Component)
			}
			primary[r.Component] = true
		case plan.PhasePre:
			if seen {
				return unsupported("release %q follows its component's primary release", r.Name)
			}
		case plan.PhasePost, plan.PhaseReadiness:
			if !seen {
				return unsupported("release %q precedes its component's primary release", r.Name)
			}
		}
	}
	for c := range components {
		if !primary[c] {
			return unsupported("component %q has no primary release", c)
		}
	}
	return nil
}

// phaseOf infers a release's phase: a release named after its component is
// the primary release; one named after it with a reserved suffix is the
// injected release of that phase. Anything else is not understood.
func phaseOf(r *BundleRelease) (plan.Phase, error) {
	if r.Name == r.Component {
		return plan.PhasePrimary, nil
	}
	for _, p := range injectedPhases {
		if r.Name == r.Component+p.suffix {
			return p.phase, nil
		}
	}
	return "", unsupported("release %q of component %q has no recognizable phase", r.Name, r.Component)
}

// ambiguous rejects a component whose name is another component's name plus
// a reserved suffix: AICR documents that its deployers disagree on how such a
// release is attributed, so its phase cannot be inferred reliably.
func ambiguous(component string, components map[string]bool) error {
	for _, p := range injectedPhases {
		if base, ok := strings.CutSuffix(component, p.suffix); ok && components[base] {
			return unsupported("component %q collides with the %s release of %q", component, p.phase, base)
		}
	}
	return nil
}

// BundleRecipeJSON returns the recipe file a bundle carries as JSON, after
// checking it against the digest bundle-info.yaml records.
func (b *BundleInfo) BundleRecipeJSON(data []byte) ([]byte, error) {
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != b.Build.Recipe.Digest {
		return nil, unsupported("bundle recipe digest %s does not match %s", got, b.Build.Recipe.Digest)
	}
	return yamlToJSON(data, b.Build.Recipe.Path)
}

// ParseBundleRecipe decodes the recipe file a bundle carries and binds it to
// the record: its SHA-256 must equal build.recipe.digest. AICR writes this
// file after bundle-time filtering, so it, not the recipe Job's output, lists
// exactly the components the bundle renders. Every component must
// have a primary release, and primary releases follow the recipe's
// deployment order.
func (b *BundleInfo) ParseBundleRecipe(data []byte, declaredVersion string) (*RecipeResult, error) {
	doc, err := b.BundleRecipeJSON(data)
	if err != nil {
		return nil, err
	}
	r, err := ParseRecipeResult(doc, declaredVersion)
	if err != nil {
		return nil, err
	}
	var primaries []string
	for i := range b.Layout.Releases {
		if b.Layout.Releases[i].Phase == plan.PhasePrimary {
			primaries = append(primaries, b.Layout.Releases[i].Component)
		}
	}
	if strings.Join(primaries, ",") != strings.Join(r.DeploymentOrder, ",") {
		return nil, unsupported("bundle releases %v do not follow the recipe deployment order %v", primaries, r.DeploymentOrder)
	}
	return r, nil
}
