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

// Package recipe provides recipe building and matching functionality.
package recipe

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"gopkg.in/yaml.v3"
)

// RecipeMetadataKind is the kind value for RecipeMetadata resources.
const RecipeMetadataKind = "RecipeMetadata"

// RecipeResultKind is the kind value for RecipeResult resources.
const RecipeResultKind = "RecipeResult"

// RecipeAPIVersion is the API version for recipe metadata and result resources.
// It aliases the canonical header.GroupVersion (single source of truth).
const RecipeAPIVersion = header.GroupVersion

// ComponentType represents the type of component deployment.
type ComponentType string

// ComponentType constants for supported deployment types.
const (
	ComponentTypeHelm      ComponentType = "Helm"
	ComponentTypeKustomize ComponentType = "Kustomize"
)

// Constraint represents a deployment constraint/assumption.
type Constraint struct {
	// Name is the constraint identifier (e.g., "k8s", "worker-os").
	Name string `json:"name" yaml:"name"`

	// Value is the constraint expression (e.g., ">= 1.30", "ubuntu").
	Value string `json:"value" yaml:"value"`

	// Severity indicates the constraint severity ("error" or "warning").
	Severity string `json:"severity,omitempty" yaml:"severity,omitempty"`

	// Remediation provides actionable guidance for fixing failed constraints.
	Remediation string `json:"remediation,omitempty" yaml:"remediation,omitempty"`

	// Unit specifies the unit for numeric constraints (e.g., "GB/s").
	Unit string `json:"unit,omitempty" yaml:"unit,omitempty"`
}

// ComponentRef represents a reference to a deployable component.
type ComponentRef struct {
	// Name is the unique identifier for this component.
	Name string `json:"name" yaml:"name"`

	// Namespace is the Kubernetes namespace for deploying this component.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Chart is the Helm chart name (e.g., "gpu-operator").
	Chart string `json:"chart,omitempty" yaml:"chart,omitempty"`

	// Type is the deployment type (Helm, Kustomize).
	Type ComponentType `json:"type" yaml:"type"`

	// Source is the repository URL or OCI reference.
	Source string `json:"source" yaml:"source"`

	// Version is the chart/component version (for Helm).
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// Tag is the image/resource tag (for Kustomize).
	Tag string `json:"tag,omitempty" yaml:"tag,omitempty"`

	// ValuesFile is the path to the values file (relative to data directory).
	ValuesFile string `json:"valuesFile,omitempty" yaml:"valuesFile,omitempty"`

	// Overrides contains inline values that override those from ValuesFile.
	// Merge order: base values → ValuesFile → Overrides (highest precedence).
	Overrides map[string]any `json:"overrides,omitempty" yaml:"overrides,omitempty"`

	// Patches is a list of patch files (intended for Kustomize). NOT CURRENTLY
	// APPLIED by any deployer — an enabled ref that declares patches is rejected
	// by ValidateCoherence (disabled refs are skipped) rather than silently
	// producing an unpatched bundle. See #1588 (implement application or drop
	// the field).
	Patches []string `json:"patches,omitempty" yaml:"patches,omitempty"`

	// DependencyRefs is a list of component names this component depends on.
	DependencyRefs []string `json:"dependencyRefs,omitempty" yaml:"dependencyRefs,omitempty"`

	// ManifestFiles lists manifest files to include in the component bundle.
	// Paths are relative to the data directory.
	// Example: ["components/network-operator/manifests/nfd-network-rule.yaml"]
	ManifestFiles []string `json:"manifestFiles,omitempty" yaml:"manifestFiles,omitempty"`

	// PreManifestFiles lists manifest files that must be bundled and applied
	// BEFORE the component's primary chart. Paths are relative to the data
	// directory; ".." segments are rejected at load time (external data
	// directories enforce a path-traversal check during file registration,
	// and embed.FS refuses any read that resolves outside its root), so a
	// recipe cannot read arbitrary files outside the embedded/external data
	// root. Used for resources the chart depends on (e.g. a Namespace with
	// PSS labels that the chart's pods need to land in). Bundler emits
	// these as a "<name>-pre" local-helm folder at sync-wave N-1 (Argo) or
	// install step N-1 (Helm); the primary chart lands at wave N; existing
	// ManifestFiles still land at wave N+1 as before.
	PreManifestFiles []string `json:"preManifestFiles,omitempty" yaml:"preManifestFiles,omitempty"`

	// Path is the path within the repository to the kustomization (for Kustomize).
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// Cleanup indicates whether to uninstall this component after validation.
	// Used for validation infrastructure components (e.g., nccl-doctor).
	Cleanup bool `json:"cleanup,omitempty" yaml:"cleanup,omitempty"`

	// ExpectedResources lists Kubernetes resources that should exist after deployment.
	// Used by deployment phase validation to verify component health.
	ExpectedResources []ExpectedResource `json:"expectedResources,omitempty" yaml:"expectedResources,omitempty"`

	// HealthCheckAsserts contains raw Chainsaw-style assert YAML loaded from the
	// registry's healthCheck.assertFile via the DataProvider. When non-empty, the
	// expected-resources check runs Chainsaw CLI to evaluate assertions instead of
	// the default auto-discovery + typed replica checks.
	HealthCheckAsserts string `json:"healthCheckAsserts,omitempty" yaml:"healthCheckAsserts,omitempty"`

	// HealthCheckSkip suppresses hydration of the registry-declared
	// healthCheck.assertFile for this component. Set by a leaf overlay (or an
	// external --data overlay) as the rollback path for a regressing upstream
	// check: when true, ApplyRegistryDefaults leaves HealthCheckAsserts empty
	// even if the registry declares an assertFile for this component. Merge
	// semantics mirror Cleanup — set-if-true; descendants can opt in but not
	// opt out without re-declaring the assert content inline via
	// HealthCheckAsserts.
	HealthCheckSkip bool `json:"healthCheckSkip,omitempty" yaml:"healthCheckSkip,omitempty"`
}

// IsEnabled returns whether this component is enabled for deployment.
// A component is disabled when its Overrides map contains enabled: false.
// Components without an explicit enabled override are enabled by default.
func (c ComponentRef) IsEnabled() bool {
	v, ok := c.Overrides["enabled"]
	if !ok {
		return true
	}
	enabled, ok := v.(bool)
	if !ok {
		slog.Warn("overrides.enabled is not a bool, treating component as enabled",
			"component", c.Name, "value", v)
		return true
	}
	return enabled
}

// ApplyRegistryDefaults fills in ComponentRef fields from ComponentConfig defaults.
// This applies registry defaults for fields that are not already set in the ComponentRef.
func (ref *ComponentRef) ApplyRegistryDefaults(config *ComponentConfig) {
	if config == nil {
		return
	}

	// Set type from config if not already set
	if ref.Type == "" {
		ref.Type = config.GetType()
	}

	switch ref.Type {
	case ComponentTypeHelm:
		// Apply Helm defaults
		if ref.Source == "" && config.Helm.DefaultRepository != "" {
			ref.Source = config.Helm.DefaultRepository
		}
		if ref.Version == "" && config.Helm.DefaultVersion != "" {
			ref.Version = config.Helm.DefaultVersion
		}
		if ref.Namespace == "" && config.Helm.DefaultNamespace != "" {
			ref.Namespace = config.Helm.DefaultNamespace
		}
		if ref.Chart == "" && config.Helm.DefaultChart != "" {
			chart := config.Helm.DefaultChart
			if idx := strings.LastIndex(chart, "/"); idx >= 0 {
				chart = chart[idx+1:]
			}
			ref.Chart = chart
		}
	case ComponentTypeKustomize:
		// Apply Kustomize defaults
		if ref.Source == "" && config.Kustomize.DefaultSource != "" {
			ref.Source = config.Kustomize.DefaultSource
		}
		if ref.Tag == "" && config.Kustomize.DefaultTag != "" {
			ref.Tag = config.Kustomize.DefaultTag
		}
		if ref.Path == "" && config.Kustomize.DefaultPath != "" {
			ref.Path = config.Kustomize.DefaultPath
		}
	}

	// healthCheck.assertFile hydration is NOT performed in this method.
	// ApplyRegistryDefaults runs per-ref against a registry config and has no
	// DataProvider in scope, but assertFile content lives on disk (or in an
	// external --data overlay) and must be loaded through the provider that
	// produced this result. Hydration is performed by hydrateHealthCheckAsserts
	// in metadata_store.go after the per-ref defaults pass, where the bound
	// DataProvider is available. See issue #1219.
}

// coherenceProblem reports why a resolved ComponentRef's deployment-shape
// fields are internally inconsistent, or "" if the ref is coherent. The rules
// mirror what the deployers enforce so an incoherent ref is rejected at
// resolution rather than silently deploying as a different type (or producing
// a signed attestation whose metadata does not match what deploys):
//
//   - the field-classifying deployers (localformat, and the Helm/Helmfile/ArgoCD
//     generators built on it) do not trust the declared Type — localformat
//     classifies any ref carrying a Tag or Path as Kustomize (see
//     pkg/bundler/deployer/localformat classify/write). The Flux generator
//     differs: it switches on the declared Type. So a Helm ref carrying a
//     tag/path builds as Kustomize under the field-classifiers but as Helm
//     under Flux — the same ref deploys differently by deployer — which is why
//     a Helm ref must not carry Kustomize fields. (An explicitly Kustomize ref,
//     conversely, is rejected outright by the Helm-only Flux generator — see
//     #1588.);
//   - a Kustomize ref needs a Path to build from;
//   - a Tag is only meaningful with a Source (git repo / OCI ref); and
//   - no deployer applies ComponentRef.Patches, so a ref that declares patch
//     files is rejected rather than silently producing an unpatched bundle
//     (see #1588).
//
// Keep these in lockstep with the localformat rules.
func (ref *ComponentRef) coherenceProblem() string {
	// Patches are unsupported for every deployment type: the field is carried
	// through resolution but no deployer applies it (localformat's Component has
	// no patches field). Fail closed on any type rather than drop it silently.
	if len(ref.Patches) > 0 {
		return fmt.Sprintf("component %q declares patches, but no deployer applies patch files; "+
			"remove `patches` (removing it does not change the generated bundle). See #1588.", ref.Name)
	}

	hasTag, hasPath := ref.Tag != "", ref.Path != ""
	// Match the type case-insensitively: the resolver and the OpenAPI examples
	// use the canonical ComponentType ("Helm"/"Kustomize"), but lowercase is
	// accepted as backward-compatible input from hand-authored recipes or older
	// clients, and the field-classifying deployers key off tag/path (while Flux
	// switches on Type) — so "helm" and "Helm" must be treated the same.
	switch {
	case strings.EqualFold(string(ref.Type), string(ComponentTypeHelm)):
		if hasTag || hasPath {
			return fmt.Sprintf("component %q is Helm but carries Kustomize field(s) (tag=%q, path=%q); "+
				"the field-classifying deployers would treat it as Kustomize while the Type-switching Flux "+
				"deployer would treat it as Helm, so the same ref deploys differently — remove the tag/path, "+
				"or convert it into a coherent Kustomize ref (which needs a path, and a source if it sets a tag)",
				ref.Name, ref.Tag, ref.Path)
		}
	case strings.EqualFold(string(ref.Type), string(ComponentTypeKustomize)):
		if !hasPath {
			return fmt.Sprintf("component %q is Kustomize but has no path; a path is required to build from", ref.Name)
		}
		if hasTag && ref.Source == "" {
			return fmt.Sprintf("component %q is Kustomize with tag %q but no source; a tag is only "+
				"meaningful with a git source", ref.Name, ref.Tag)
		}
		// A Kustomize ref wraps a single primary source; the deployers reject a
		// ref that also carries post-manifests (ManifestFiles). PreManifestFiles
		// are pre-injected separately and remain supported.
		if len(ref.ManifestFiles) > 0 {
			return fmt.Sprintf("component %q is Kustomize but also declares manifestFiles; a component may "+
				"declare either Kustomize (tag/path) or raw manifest files, not both", ref.Name)
		}
	default:
		// After ApplyRegistryDefaults every registry-backed ref has a supported
		// Type; an empty or unknown Type here means an externally-supplied ref
		// the registry did not populate. The field-classifying deployers key off
		// tag/path (ignoring this field) while Flux switches on it, so an
		// unsupported Type would deploy ambiguously or be rejected — fail closed
		// rather than silently accept it.
		return fmt.Sprintf("component %q has unsupported type %q; expected %q or %q",
			ref.Name, ref.Type, ComponentTypeHelm, ComponentTypeKustomize)
	}
	return ""
}

// canonicalizeComponentTypes normalizes each ref's case-insensitively-matched
// Type to the canonical ComponentType constant ("helm" -> "Helm"), so registry
// defaulting (which switches on the exact constant) and the deployers (Flux
// rejects a lowercase "helm", ArgoCD-Helm mis-handles a lowercase "kustomize")
// all see a consistent value. Unknown types are left unchanged for the
// coherence check to reject. Call this at every boundary that produces a
// RecipeResult, before defaulting and before returning the result.
func canonicalizeComponentTypes(refs []ComponentRef) {
	for i := range refs {
		switch {
		case strings.EqualFold(string(refs[i].Type), string(ComponentTypeHelm)):
			refs[i].Type = ComponentTypeHelm
		case strings.EqualFold(string(refs[i].Type), string(ComponentTypeKustomize)):
			refs[i].Type = ComponentTypeKustomize
		}
	}
}

// backfillComponentTypes sets ref.Type from the registry component's type for
// each ENABLED ref that has no explicit type, using this result's bound
// DataProvider's registry. It mirrors what ApplyRegistryDefaults does on the
// resolve path, so a hand-authored or hydrated recipe that omits `type` —
// valid before #1584, since the deployers derive the type from the ref's
// fields — is not rejected by ValidateCoherence. It is the first step of
// PrepareAndValidate (the load and adopt boundaries do not run
// ApplyRegistryDefaults). Disabled refs are ignored (they are excluded from the
// bundle and skipped by ValidateCoherence, so a disabled type-less stub must
// not trigger a registry load), and the registry is not consulted at all when
// no enabled ref needs a type. Non-registry components are left untouched
// (their empty type still fails closed).
func (r *RecipeResult) backfillComponentTypes() error {
	if r == nil {
		return nil
	}
	// Only touch the registry if an ENABLED ref actually needs a type. This
	// avoids a spurious registry load (and its potential error) for the common
	// case where every ref already declares a type — and, critically, for a
	// disabled legacy stub with an empty type: ValidateCoherence skips disabled
	// refs, so back-filling one must not be able to fail the whole recipe on a
	// registry error when no enabled ref needs it.
	needsBackfill := false
	for i := range r.ComponentRefs {
		if r.ComponentRefs[i].Type == "" && r.ComponentRefs[i].IsEnabled() {
			needsBackfill = true
			break
		}
	}
	if !needsBackfill {
		return nil
	}
	// A type-less ref needs the registry to resolve its type; propagate a
	// load/parse/timeout failure as-is rather than swallowing it and letting
	// ValidateCoherence report a misleading, non-retryable "unsupported type".
	registry, err := GetComponentRegistryFor(r.provider)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry to back-fill component types")
	}
	for i := range r.ComponentRefs {
		if r.ComponentRefs[i].Type != "" || !r.ComponentRefs[i].IsEnabled() {
			continue
		}
		if cfg := registry.Get(r.ComponentRefs[i].Name); cfg != nil {
			r.ComponentRefs[i].Type = cfg.GetType()
		}
	}
	return nil
}

// PrepareAndValidate normalizes a RecipeResult's component refs and rejects
// incoherent ones, in the required order: back-fill missing types on enabled
// refs from the registry, canonicalize the type casing, then validate
// coherence (which itself only inspects enabled refs). Boundaries
// that produce a RecipeResult WITHOUT running ApplyRegistryDefaults — file load
// (LoadFromFileWithProvider) and external adoption (client adoptRecipe) — call
// this single method so the three steps cannot drift or be partially applied
// (e.g. validating before canonicalizing would reject legitimate lowercase
// types; skipping the back-fill would reject type-less registry refs). The
// resolve path (finalizeRecipeResult) instead back-fills via ApplyRegistryDefaults
// and canonicalizes before defaulting, so it calls ValidateCoherence directly.
func (r *RecipeResult) PrepareAndValidate() error {
	if r == nil {
		return nil
	}
	if err := r.backfillComponentTypes(); err != nil {
		return err
	}
	canonicalizeComponentTypes(r.ComponentRefs)
	return r.ValidateCoherence()
}

// ValidateCoherence rejects enabled ComponentRefs whose deployment-shape fields
// are internally inconsistent (see coherenceProblem), aggregating every
// offender into one ErrCodeInvalidRequest so the author sees all problems at
// once. Disabled refs are skipped: they are excluded from the bundle, so their
// shape never reaches a deployer.
//
// It is invoked at every boundary that produces a RecipeResult — criteria
// resolution (finalizeRecipeResult), file load (LoadFromFileWithProvider), and
// external adoption (client adoptRecipe / POST /v1/bundle) — so an incoherent
// ref cannot slip in via a hand-authored or decoded hydrated recipe.
func (r *RecipeResult) ValidateCoherence() error {
	if r == nil {
		return nil
	}
	var problems []string
	for i := range r.ComponentRefs {
		if !r.ComponentRefs[i].IsEnabled() {
			continue
		}
		if p := r.ComponentRefs[i].coherenceProblem(); p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New(errors.ErrCodeInvalidRequest,
		"recipe has incoherent component ref(s): "+strings.Join(problems, "; "))
}

// ExpectedResource represents a Kubernetes resource that should exist after deployment.
type ExpectedResource struct {
	// Kind is the resource kind (e.g., "Deployment", "DaemonSet").
	Kind string `json:"kind" yaml:"kind"`

	// Name is the resource name.
	Name string `json:"name" yaml:"name"`

	// Namespace is the resource namespace (optional for cluster-scoped resources).
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// RecipeMetadataSpec contains the specification for a recipe.
type RecipeMetadataSpec struct {
	// Base is the name of the parent recipe to inherit from.
	// If empty, the recipe inherits from "base" (the root base.yaml).
	// This enables multi-level inheritance chains like:
	//   base → eks → eks-training → h100-eks-training
	Base string `json:"base,omitempty" yaml:"base,omitempty"`

	// Criteria defines when this recipe/overlay applies.
	// Only present in overlay files, not in base.
	Criteria *Criteria `json:"criteria,omitempty" yaml:"criteria,omitempty"`

	// Mixins is a list of mixin names to compose into this overlay.
	// Mixins are loaded from recipes/mixins/ and carry only constraints
	// and componentRefs. This field is loader metadata and is stripped
	// from the materialized recipe result.
	Mixins []string `json:"mixins,omitempty" yaml:"mixins,omitempty"`

	// Constraints are deployment assumptions/requirements.
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// ComponentRefs is the list of components to deploy.
	ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`

	// Validation defines multi-phase validation configuration.
	// Presence of a phase implies it is enabled.
	Validation *ValidationConfig `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// RecipeMixinKind is the kind value for mixin files.
const RecipeMixinKind = "RecipeMixin"

// RecipeMixin represents a composable fragment that carries only constraints
// and componentRefs. Mixins live in recipes/mixins/ and are referenced by
// overlay spec.mixins fields.
type RecipeMixin struct {
	Kind       string `json:"kind" yaml:"kind"`
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Metadata   struct {
		Name string `json:"name" yaml:"name"`
	} `json:"metadata" yaml:"metadata"`
	Spec struct {
		Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
		ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
	} `json:"spec" yaml:"spec"`
}

// RecipeMetadataHeader contains the Kubernetes-style header fields.
type RecipeMetadataHeader struct {
	// Kind is always "RecipeMetadata".
	Kind string `json:"kind" yaml:"kind"`

	// APIVersion is the API version (e.g., "aicr.run/v1alpha2").
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`

	// Metadata contains the name and other metadata.
	Metadata struct {
		Name string `json:"name" yaml:"name"`
	} `json:"metadata" yaml:"metadata"`
}

// RecipeMetadata represents a recipe definition (base or overlay).
type RecipeMetadata struct {
	RecipeMetadataHeader `json:",inline" yaml:",inline"`

	// Spec contains the recipe specification.
	Spec RecipeMetadataSpec `json:"spec" yaml:"spec"`
}

// ConstraintWarning represents a warning about an overlay that matched criteria
// but was excluded due to failing constraint validation against the snapshot.
type ConstraintWarning struct {
	// Overlay is the name of the overlay that was excluded.
	Overlay string `json:"overlay" yaml:"overlay"`

	// Constraint is the name of the constraint that failed.
	Constraint string `json:"constraint" yaml:"constraint"`

	// Expected is the expected constraint value.
	Expected string `json:"expected" yaml:"expected"`

	// Actual is the actual value from the snapshot (if found).
	Actual string `json:"actual,omitempty" yaml:"actual,omitempty"`

	// Reason explains why the constraint evaluation resulted in exclusion.
	Reason string `json:"reason" yaml:"reason"`
}

// ExcludedOverlayReason indicates why a matching overlay was dropped.
type ExcludedOverlayReason string

const (
	// ExcludedOverlayReasonConstraintFailed is used when an overlay's own
	// constraints fail pre-merge evaluation.
	ExcludedOverlayReasonConstraintFailed ExcludedOverlayReason = "constraint-failed"
	// ExcludedOverlayReasonMixinConstraintFailed is used when a candidate chain
	// is excluded during post-compose mixin constraint evaluation.
	ExcludedOverlayReasonMixinConstraintFailed ExcludedOverlayReason = "mixin-constraint-failed"
)

// ExcludedOverlay records a matching overlay that was excluded from the final
// recipe result, along with a machine-readable reason.
type ExcludedOverlay struct {
	// Name is the excluded overlay name.
	Name string `json:"name" yaml:"name"`

	// Reason identifies why the overlay was excluded.
	Reason ExcludedOverlayReason `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// UnmarshalYAML accepts both the legacy scalar string form:
//   - excludedOverlays: ["overlay-name"]
//
// and the current object form:
//   - excludedOverlays: [{name: overlay-name, reason: constraint-failed}]
func (e *ExcludedOverlay) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		e.Name = node.Value
		e.Reason = ""
		return nil
	}

	type rawExcludedOverlay ExcludedOverlay
	var raw rawExcludedOverlay
	if err := node.Decode(&raw); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to decode excluded overlay", err)
	}
	*e = ExcludedOverlay(raw)
	return nil
}

// UnmarshalJSON accepts both the legacy string form and the current object form.
func (e *ExcludedOverlay) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		e.Name = name
		e.Reason = ""
		return nil
	}

	type rawExcludedOverlay ExcludedOverlay
	var raw rawExcludedOverlay
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to decode excluded overlay JSON", err)
	}
	*e = ExcludedOverlay(raw)
	return nil
}

// RecipeResult represents the final merged recipe output.
type RecipeResult struct {
	// Kind is always "RecipeResult".
	Kind string `json:"kind" yaml:"kind"`

	// APIVersion is the API version.
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`

	// Metadata contains result metadata.
	Metadata struct {
		// Version is the recipe version (CLI version that generated this recipe).
		Version string `json:"version,omitempty" yaml:"version,omitempty"`

		// AppliedOverlays lists the overlay names in order of application.
		AppliedOverlays []string `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`

		// ExcludedOverlays lists overlays that matched criteria but were excluded
		// from the final recipe, along with the machine-readable exclusion reason.
		// Only populated when a snapshot is provided during recipe generation.
		ExcludedOverlays []ExcludedOverlay `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`

		// ConstraintWarnings contains details about why specific overlays were excluded.
		// Helps users understand why certain environment-specific configurations
		// were not applied and what would need to change to include them.
		ConstraintWarnings []ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
	} `json:"metadata" yaml:"metadata"`

	// Criteria is the input criteria used to generate this result.
	Criteria *Criteria `json:"criteria" yaml:"criteria"`

	// Constraints is the merged list of constraints.
	Constraints []Constraint `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// ComponentRefs is the merged list of components.
	ComponentRefs []ComponentRef `json:"componentRefs" yaml:"componentRefs"`

	// DeploymentOrder is the topologically sorted component names for deployment.
	// Components should be deployed in this order to satisfy dependencies.
	DeploymentOrder []string `json:"deploymentOrder" yaml:"deploymentOrder"`

	// Validation defines multi-phase validation configuration.
	// Inherited from recipe metadata during merging.
	Validation *ValidationConfig `json:"validation,omitempty" yaml:"validation,omitempty"`

	// provider is the DataProvider that produced this result. Threaded
	// through finalizeRecipeResult from the originating MetadataStore so
	// downstream consumers (e.g., GetValuesForComponent in Task 6) can
	// route file lookups through the same provider — preserving per-tenant
	// isolation even when the package-global DataProvider has since been
	// swapped. Nil when the result was built against the package-global
	// provider, in which case DataProvider() returns nil and callers fall
	// back to GetDataProvider().
	provider DataProvider

	// owner identifies the *Builder that produced this RecipeResult.
	// Stamped by Builder.buildWithStore (covers BuildFromCriteria and
	// BuildFromCriteriaWithEvaluator) using the Builder's pointer
	// identity — zero-cost, naturally unique, and unforgeable from
	// outside the package because the field is unexported.
	//
	// Consumers that hold a *Builder reference can call AssertOwnedBy
	// to refuse a RecipeResult produced by a different Builder before
	// reading values via GetValuesForComponent. This pushes the
	// cross-provider safety guard down from the aicr facade (where it
	// only protected facade callers) to the layer where the bug
	// actually lives — protecting direct pkg/recipe.Builder importers
	// the same way.
	//
	// Nil when the result was constructed outside Builder (e.g.,
	// LoadFromFile decoding a pre-hydrated RecipeResult file).
	// AssertOwnedBy treats nil owner as "no provenance" and rejects;
	// callers that legitimately load results externally should rebind
	// via BindDataProvider and consume read-only without going through
	// ownership-checked entry points.
	owner *Builder
}

// DataProvider returns the DataProvider that produced this result, or nil
// when the result was built against the package-global provider. Nil-safe
// on the receiver so call sites can chain freely off a possibly-nil result.
func (r *RecipeResult) DataProvider() DataProvider {
	if r == nil {
		return nil
	}
	return r.provider
}

// Owner returns the *Builder that produced this RecipeResult, or nil when
// the result was constructed outside the Builder path (e.g., a recipe file
// loaded via LoadFromFile that was never re-built locally). Nil-safe on
// the receiver. Returned identity is for comparison only; callers should
// not mutate the Builder.
func (r *RecipeResult) Owner() *Builder {
	if r == nil {
		return nil
	}
	return r.owner
}

// AssertOwnedBy returns nil when this RecipeResult was produced by b, and
// ErrCodeInvalidRequest otherwise. The check uses pointer identity on the
// unexported owner field stamped by Builder.buildWithStore at build time.
//
// Use this from consumer entry points that hold a *Builder reference and
// want to refuse a RecipeResult produced elsewhere before reading values
// (e.g., calling GetValuesForComponent). Two Builders with different
// DataProviders would otherwise mix component refs from one provider with
// file reads from the other — the same bug class the facade-level
// assertOwns in pkg/client/v1 protects against, but enforced at the layer
// where the data lives so external pkg/recipe.Builder importers are
// covered too.
//
// A nil receiver returns nil (vacuously owned by anything) so the helper
// composes with chained nil-checks. A nil b argument returns
// ErrCodeInvalidRequest — callers must pass the Builder they want to
// assert against.
//
// A nil owner on a non-nil result is rejected: the result has no provenance
// and cannot prove it belongs to b. Callers that load results externally
// (e.g., recipe YAML from disk) and want to consume them must rebuild via
// Builder or skip the owner-checked entry points; BindDataProvider + the
// non-checked accessors remain available for that path.
func (r *RecipeResult) AssertOwnedBy(b *Builder) error {
	if r == nil {
		return nil
	}
	if b == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"AssertOwnedBy requires a non-nil *Builder")
	}
	if r.owner == nil {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"RecipeResult has no owner (constructed outside Builder); cross-builder operations are not permitted",
			map[string]any{
				"expectedOwner": fmt.Sprintf("%p", b),
				"actualOwner":   "<nil>",
			})
	}
	if r.owner != b {
		return errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"RecipeResult was produced by a different Builder; cross-builder operations are not permitted",
			map[string]any{
				"expectedOwner": fmt.Sprintf("%p", b),
				"actualOwner":   fmt.Sprintf("%p", r.owner),
			})
	}
	return nil
}

// BindDataProvider sets the DataProvider on a RecipeResult so downstream
// value/manifest/data-file reads route through dp rather than the package
// global. It is the exported binder the aicr.Client facade uses to adopt a
// RecipeResult decoded from an external source (e.g. a /v1/bundle POST body)
// onto the Client's own provider — the in-process equivalent of the
// rec.provider = dp binding loader.go performs for an already-hydrated file.
// Nil-safe on the receiver. A nil dp leaves the result on the package-global
// fallback (DataProvider() then returns nil), matching the pre-bind behavior.
func (r *RecipeResult) BindDataProvider(dp DataProvider) {
	if r == nil {
		return
	}
	r.provider = dp
}

// DeepCopy returns an independent copy of r with all exported fields
// deep-copied: the nested Metadata slices, Criteria, Constraints,
// ComponentRefs (including their map/slice fields), DeploymentOrder, and
// Validation config. The unexported provider is intentionally NOT copied —
// it is left nil so the caller can rebind it (e.g. the aicr.Client facade
// adopts a recipe by deep-copying first, then BindDataProvider on the copy,
// so binding never mutates caller-owned state). Nil-safe on the receiver.
//
// Used by the facade's AdoptRecipe path: a caller may reuse one *RecipeResult
// across multiple Clients, and binding a Client's provider must not leak into
// the caller's pointer or contaminate a sibling Client's binding.
func (r *RecipeResult) DeepCopy() *RecipeResult {
	if r == nil {
		return nil
	}
	out := &RecipeResult{
		Kind:       r.Kind,
		APIVersion: r.APIVersion,
		// provider intentionally left nil: BindDataProvider sets it on the copy.
		// owner intentionally left nil: the copy has no producer Builder, so
		// AssertOwnedBy rejects it. The facade's AdoptRecipe path rebinds
		// the provider but does not rebind owner — adopted recipes can be
		// read but not consumed via ownership-checked entry points.
	}

	// Metadata: scalar Version plus three slices.
	out.Metadata.Version = r.Metadata.Version
	if r.Metadata.AppliedOverlays != nil {
		out.Metadata.AppliedOverlays = make([]string, len(r.Metadata.AppliedOverlays))
		copy(out.Metadata.AppliedOverlays, r.Metadata.AppliedOverlays)
	}
	if r.Metadata.ExcludedOverlays != nil {
		out.Metadata.ExcludedOverlays = make([]ExcludedOverlay, len(r.Metadata.ExcludedOverlays))
		copy(out.Metadata.ExcludedOverlays, r.Metadata.ExcludedOverlays)
	}
	if r.Metadata.ConstraintWarnings != nil {
		out.Metadata.ConstraintWarnings = make([]ConstraintWarning, len(r.Metadata.ConstraintWarnings))
		copy(out.Metadata.ConstraintWarnings, r.Metadata.ConstraintWarnings)
	}

	// Criteria is all-scalar, so a value copy behind a fresh pointer is a
	// full deep copy.
	if r.Criteria != nil {
		c := *r.Criteria
		out.Criteria = &c
	}

	if r.Constraints != nil {
		out.Constraints = make([]Constraint, len(r.Constraints))
		copy(out.Constraints, r.Constraints)
	}

	if r.ComponentRefs != nil {
		out.ComponentRefs = make([]ComponentRef, len(r.ComponentRefs))
		for i := range r.ComponentRefs {
			out.ComponentRefs[i] = cloneComponentRef(r.ComponentRefs[i])
		}
	}

	if r.DeploymentOrder != nil {
		out.DeploymentOrder = make([]string, len(r.DeploymentOrder))
		copy(out.DeploymentOrder, r.DeploymentOrder)
	}

	out.Validation = cloneValidationConfig(r.Validation)

	return out
}

// cloneComponentRef returns a deep copy of a ComponentRef, allocating
// independent backing storage for every map/slice field so a mutation
// through the copy can't reach the source.
func cloneComponentRef(ref ComponentRef) ComponentRef {
	out := ref // copies scalars and the (to-be-replaced) reference fields
	if ref.Overrides != nil {
		// serializer.DeepCopyAnyMap recurses into nested map[string]any/[]any
		// so a mutation through the copy can't reach the source's nested
		// values (a shallow key-copy would share those nested containers).
		out.Overrides = serializer.DeepCopyAnyMap(ref.Overrides)
	}
	if ref.Patches != nil {
		out.Patches = make([]string, len(ref.Patches))
		copy(out.Patches, ref.Patches)
	}
	if ref.DependencyRefs != nil {
		out.DependencyRefs = make([]string, len(ref.DependencyRefs))
		copy(out.DependencyRefs, ref.DependencyRefs)
	}
	if ref.ManifestFiles != nil {
		out.ManifestFiles = make([]string, len(ref.ManifestFiles))
		copy(out.ManifestFiles, ref.ManifestFiles)
	}
	if ref.PreManifestFiles != nil {
		out.PreManifestFiles = make([]string, len(ref.PreManifestFiles))
		copy(out.PreManifestFiles, ref.PreManifestFiles)
	}
	if ref.ExpectedResources != nil {
		out.ExpectedResources = make([]ExpectedResource, len(ref.ExpectedResources))
		copy(out.ExpectedResources, ref.ExpectedResources)
	}
	return out
}

// Merge merges another RecipeMetadataSpec into this one.
// The other spec takes precedence for conflicts.
func (s *RecipeMetadataSpec) Merge(other *RecipeMetadataSpec) {
	if other == nil {
		return
	}

	// Merge constraints - other takes precedence for same name
	constraintMap := make(map[string]Constraint)
	for _, c := range s.Constraints {
		constraintMap[c.Name] = c
	}
	for _, c := range other.Constraints {
		constraintMap[c.Name] = c
	}
	s.Constraints = make([]Constraint, 0, len(constraintMap))
	for _, c := range constraintMap {
		s.Constraints = append(s.Constraints, c)
	}
	// Sort constraints by name for deterministic output
	sort.Slice(s.Constraints, func(i, j int) bool {
		return s.Constraints[i].Name < s.Constraints[j].Name
	})

	// Merge componentRefs - overlay fields take precedence, but inherit missing from base
	componentMap := make(map[string]ComponentRef)
	for _, c := range s.ComponentRefs {
		componentMap[c.Name] = c
	}
	for _, overlay := range other.ComponentRefs {
		if base, exists := componentMap[overlay.Name]; exists {
			// Merge overlay into base - overlay takes precedence for non-empty fields
			componentMap[overlay.Name] = mergeComponentRef(base, overlay)
		} else {
			// New component from overlay
			componentMap[overlay.Name] = overlay
		}
	}
	s.ComponentRefs = make([]ComponentRef, 0, len(componentMap))
	for _, c := range componentMap {
		s.ComponentRefs = append(s.ComponentRefs, c)
	}
	// Sort components by name for deterministic output
	sort.Slice(s.ComponentRefs, func(i, j int) bool {
		return s.ComponentRefs[i].Name < s.ComponentRefs[j].Name
	})

	// Merge validation config. Each phase merges field-by-field so leaf
	// overlays can add or override checks/constraints without restating the
	// entire inherited block. See issue #1000 for the rationale — the prior
	// per-phase pointer replace was the only list-shaped field with replace
	// semantics, and it silently dropped inherited checks when a descendant
	// declared its own phase block. Phase pointers are still cloned (not
	// aliased) when the destination's phase is nil so successive merges
	// cannot mutate the source's cached ValidationConfig.
	if other.Validation != nil {
		if s.Validation == nil {
			s.Validation = cloneValidationConfig(other.Validation)
		} else {
			s.Validation.Readiness = mergeValidationPhase(s.Validation.Readiness, other.Validation.Readiness)
			s.Validation.Deployment = mergeValidationPhase(s.Validation.Deployment, other.Validation.Deployment)
			s.Validation.Performance = mergeValidationPhase(s.Validation.Performance, other.Validation.Performance)
			s.Validation.Conformance = mergeValidationPhase(s.Validation.Conformance, other.Validation.Conformance)
		}
	}

	// Accumulate mixins (deduplicated, preserving order).
	// Both leaf and intermediate overlays can declare mixins. When an
	// intermediate overlay (e.g., eks-inference) declares a mixin, it is
	// accumulated into all descendants during inheritance chain merging.
	if len(other.Mixins) > 0 {
		seen := make(map[string]bool)
		for _, m := range s.Mixins {
			seen[m] = true
		}
		for _, m := range other.Mixins {
			if !seen[m] {
				s.Mixins = append(s.Mixins, m)
				seen[m] = true
			}
		}
	}
}

// mergeComponentRef merges overlay into base, with overlay taking precedence
// for non-empty fields. Empty/zero fields in overlay inherit from base.
func mergeComponentRef(base, overlay ComponentRef) ComponentRef {
	result := base // Start with base values

	// Namespace: overlay takes precedence if set
	if overlay.Namespace != "" {
		result.Namespace = overlay.Namespace
	}

	// Chart: overlay takes precedence if set
	if overlay.Chart != "" {
		result.Chart = overlay.Chart
	}

	// Type: overlay takes precedence if set
	if overlay.Type != "" {
		result.Type = overlay.Type
	}

	// Source: overlay takes precedence if set
	if overlay.Source != "" {
		result.Source = overlay.Source
	}

	// Version: overlay takes precedence if set
	if overlay.Version != "" {
		result.Version = overlay.Version
	}

	// Tag: overlay takes precedence if set
	if overlay.Tag != "" {
		result.Tag = overlay.Tag
	}

	// ValuesFile: overlay takes precedence if set
	if overlay.ValuesFile != "" {
		result.ValuesFile = overlay.ValuesFile
	}

	// Overrides: deep-merge maps, overlay takes precedence
	if len(overlay.Overrides) > 0 {
		if result.Overrides == nil {
			result.Overrides = make(map[string]any)
		}
		deepMergeMap(result.Overrides, overlay.Overrides)
	}

	// Patches: overlay replaces if set
	if len(overlay.Patches) > 0 {
		result.Patches = overlay.Patches
	}

	// DependencyRefs: additive merge (base + overlay, deduplicated)
	if len(overlay.DependencyRefs) > 0 {
		seen := make(map[string]bool)
		for _, d := range result.DependencyRefs {
			seen[d] = true
		}
		for _, d := range overlay.DependencyRefs {
			if !seen[d] {
				result.DependencyRefs = append(result.DependencyRefs, d)
				seen[d] = true
			}
		}
	}

	// ManifestFiles: additive merge (base + overlay, deduplicated)
	if len(overlay.ManifestFiles) > 0 {
		seen := make(map[string]bool)
		for _, f := range result.ManifestFiles {
			seen[f] = true
		}
		for _, f := range overlay.ManifestFiles {
			if !seen[f] {
				result.ManifestFiles = append(result.ManifestFiles, f)
				seen[f] = true
			}
		}
	}

	// PreManifestFiles: additive merge (base + overlay, deduplicated)
	if len(overlay.PreManifestFiles) > 0 {
		seen := make(map[string]bool)
		for _, f := range result.PreManifestFiles {
			seen[f] = true
		}
		for _, f := range overlay.PreManifestFiles {
			if !seen[f] {
				result.PreManifestFiles = append(result.PreManifestFiles, f)
				seen[f] = true
			}
		}
	}

	// Path: overlay takes precedence if set (for Kustomize)
	if overlay.Path != "" {
		result.Path = overlay.Path
	}

	// Cleanup: overlay takes precedence if true
	if overlay.Cleanup {
		result.Cleanup = overlay.Cleanup
	}

	// ExpectedResources: overlay replaces if set
	if len(overlay.ExpectedResources) > 0 {
		result.ExpectedResources = overlay.ExpectedResources
	}

	// HealthCheckAsserts: overlay takes precedence if set
	if overlay.HealthCheckAsserts != "" {
		result.HealthCheckAsserts = overlay.HealthCheckAsserts
	}

	// HealthCheckSkip: overlay takes precedence if true (set-if-true,
	// mirroring Cleanup). A descendant cannot opt back in to hydration
	// once an ancestor has opted out; the explicit re-enable path is to
	// declare HealthCheckAsserts inline in the descendant overlay.
	if overlay.HealthCheckSkip {
		result.HealthCheckSkip = true
	}

	return result
}

// ValidateDependencies validates that all dependencyRefs reference existing components.
// Returns an error if any dependency is missing or if there are circular dependencies.
func (s *RecipeMetadataSpec) ValidateDependencies() error {
	// Build a set of known component names
	known := make(map[string]bool)
	for _, c := range s.ComponentRefs {
		known[c.Name] = true
	}

	// Check all dependencyRefs point to known components
	for _, c := range s.ComponentRefs {
		for _, dep := range c.DependencyRefs {
			if !known[dep] {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("component %q references unknown dependency %q", c.Name, dep))
			}
		}
	}

	// Check for circular dependencies
	if err := s.detectCycles(); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "dependency validation failed", err)
	}

	return nil
}

// detectCycles uses DFS to detect circular dependencies.
func (s *RecipeMetadataSpec) detectCycles() error {
	// Build adjacency list
	deps := make(map[string][]string)
	for _, c := range s.ComponentRefs {
		deps[c.Name] = c.DependencyRefs
	}

	// Track visited nodes and recursion stack
	visited := make(map[string]bool)
	recStack := make(map[string]bool)
	var path []string

	var dfs func(node string) error
	dfs = func(node string) error {
		visited[node] = true
		recStack[node] = true
		path = append(path, node)

		for _, neighbor := range deps[node] {
			if !visited[neighbor] {
				if err := dfs(neighbor); err != nil {
					return err
				}
			} else if recStack[neighbor] {
				// Found a cycle - build the cycle path
				cycleStart := -1
				for i, n := range path {
					if n == neighbor {
						cycleStart = i
						break
					}
				}
				// Build cycle path: copy to avoid modifying original path slice
				cyclePath := make([]string, len(path)-cycleStart+1)
				copy(cyclePath, path[cycleStart:])
				cyclePath[len(cyclePath)-1] = neighbor
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("circular dependency detected: %v", cyclePath))
			}
		}

		path = path[:len(path)-1]
		recStack[node] = false
		return nil
	}

	// Run DFS from each unvisited node
	for _, c := range s.ComponentRefs {
		if !visited[c.Name] {
			if err := dfs(c.Name); err != nil {
				return err
			}
		}
	}

	return nil
}

// TopologicalLevels returns components grouped into dependency-depth tiers
// (Kahn-style, level-by-level). Level i contains exactly the components
// whose longest dependency path from a root is i. All components within
// a level are mutually independent (no edges among them), so a deployer
// can install/diff them in parallel.
//
// Within each level, names are sorted alphabetically for determinism.
//
// Error semantics match TopologicalSort: missing or cyclic dependencies
// surface as ErrCodeInvalidRequest with "circular dependencies exist."
// (Same trade-off — a dependency on an undeclared component appears as
// a cycle because its in-degree never drains to zero.)
func (s *RecipeMetadataSpec) TopologicalLevels() ([][]string, error) {
	return ComponentRefsTopologicalLevels(s.ComponentRefs)
}

// ComponentRefsTopologicalLevels is the free-function form of
// RecipeMetadataSpec.TopologicalLevels — operates on a bare
// []ComponentRef slice. Callers that have refs but not a full
// RecipeMetadataSpec (e.g., the bundler post-resolution) use this.
func ComponentRefsTopologicalLevels(refs []ComponentRef) ([][]string, error) {
	declared, enabled := componentSets(refs)

	// Only enabled components are nodes; an edge pointing at a declared-but-
	// disabled (externally-provided) component is treated as satisfied, while
	// an edge to an undeclared component is retained so it still surfaces as a
	// cycle/missing-dependency error. See componentSets and TopologicalSort.
	inDegree := make(map[string]int, len(enabled))
	dependents := make(map[string][]string, len(enabled))
	for _, c := range refs {
		if _, ok := enabled[c.Name]; !ok {
			continue
		}
		degree := 0
		for _, dep := range c.DependencyRefs {
			if edgeSatisfiedExternally(dep, declared, enabled) {
				continue
			}
			degree++
			dependents[dep] = append(dependents[dep], c.Name)
		}
		inDegree[c.Name] = degree
	}

	// Seed level 0: components with no incoming edges.
	current := make([]string, 0, len(inDegree))
	for name, degree := range inDegree {
		if degree == 0 {
			current = append(current, name)
		}
	}
	sort.Strings(current)

	var levels [][]string
	processed := 0
	for len(current) > 0 {
		levels = append(levels, append([]string(nil), current...))
		processed += len(current)

		next := make([]string, 0)
		for _, node := range current {
			for _, dependent := range dependents[node] {
				inDegree[dependent]--
				if inDegree[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		sort.Strings(next)
		current = next
	}

	if processed != len(enabled) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"cannot determine deployment levels: circular dependencies exist")
	}
	return levels, nil
}

// TopologicalSort returns components in dependency order (dependencies first).
// Components with no dependencies come first, then components that depend only
// on already-listed components, etc.
func (s *RecipeMetadataSpec) TopologicalSort() ([]string, error) {
	declared, enabled := componentSets(s.ComponentRefs)

	// Only enabled components are nodes. A dependency edge pointing at a
	// declared-but-disabled component is treated as already satisfied (the
	// dependency is assumed to be provided externally, e.g. a CSP-managed
	// cert-manager) and excluded from the in-degree count. An edge to an
	// undeclared component is still counted so it surfaces as a cycle/missing
	// dependency error, matching the prior behavior. See componentSets.
	inDegree := make(map[string]int, len(enabled))
	dependents := make(map[string][]string) // dep -> list of components that depend on it
	for _, c := range s.ComponentRefs {
		if _, ok := enabled[c.Name]; !ok {
			continue
		}
		degree := 0
		for _, dep := range c.DependencyRefs {
			if edgeSatisfiedExternally(dep, declared, enabled) {
				continue
			}
			degree++
			dependents[dep] = append(dependents[dep], c.Name)
		}
		inDegree[c.Name] = degree
	}

	// Kahn's algorithm
	// https://www.geeksforgeeks.org/dsa/topological-sorting-indegree-based-solution/
	queue := make([]string, 0, len(inDegree))
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}
	// Sort queue for deterministic output
	sort.Strings(queue)

	result := make([]string, 0, len(enabled))
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		result = append(result, node)

		for _, dependent := range dependents[node] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
		sort.Strings(queue)
	}

	// Check if all enabled nodes were processed (no cycles)
	if len(result) != len(enabled) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "cannot determine deployment order: circular dependencies exist")
	}

	return result, nil
}

// componentSets returns two sets over refs: every declared component name,
// and the subset whose IsEnabled() reports true. Components disabled via
// overrides.enabled=false are excluded from dependency ordering: they are
// assumed to be provided externally (for example a CSP-managed cert-manager on
// OKE), so dependency edges pointing at them are treated as already satisfied
// rather than causing a false "circular dependencies" error. This mirrors the
// bundler, which filters disabled components before generating deployment
// artifacts. The declared set lets callers distinguish a disabled component
// (skip the edge) from an undeclared one (still an error).
func componentSets(refs []ComponentRef) (declared, enabled map[string]struct{}) {
	declared = make(map[string]struct{}, len(refs))
	enabled = make(map[string]struct{}, len(refs))
	for _, c := range refs {
		declared[c.Name] = struct{}{}
		if c.IsEnabled() {
			enabled[c.Name] = struct{}{}
		}
	}
	return declared, enabled
}

// edgeSatisfiedExternally reports whether a dependency edge pointing at dep
// should be dropped from ordering because dep is a declared-but-disabled
// component (assumed provided externally). Edges to enabled components are
// real, and edges to undeclared components are retained so they still surface
// as missing-dependency errors.
func edgeSatisfiedExternally(dep string, declared, enabled map[string]struct{}) bool {
	_, isDeclared := declared[dep]
	_, isEnabled := enabled[dep]
	return isDeclared && !isEnabled
}

// deepMergeMap copies all key-value pairs from src into dst. For keys whose
// values are nested maps in both src and dst, the merge recurses so that
// inner maps are not shared by reference between the two trees.
//
// Non-map values (scalars, slices) are deep-copied via serializer.DeepCopyAny
// so that dst never aliases src's []any values. Without this, a downstream
// mutation at an index of a toleration/env/args list would leak back into
// the cached source map.
func deepMergeMap(dst, src map[string]any) {
	for k, sv := range src {
		svMap, svIsMap := sv.(map[string]any)
		if !svIsMap {
			dst[k] = serializer.DeepCopyAny(sv)
			continue
		}
		dvMap, dvIsMap := dst[k].(map[string]any)
		if !dvIsMap {
			dst[k] = serializer.DeepCopyAnyMap(svMap)
			continue
		}
		deepMergeMap(dvMap, svMap)
	}
}
