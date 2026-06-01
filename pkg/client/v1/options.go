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

package aicr

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// Option configures a Client.
type Option func(*Client)

// ValidateOption configures a validation run launched via
// Client.ValidateState. It is a facade-owned functional option type:
// each WithValidation* factory below captures its argument into an
// internal validateConfig, and Client.ValidateState translates the
// captured config into pkg/validator options at call time.
//
// The wrap insulates the facade's semver contract from pkg/validator's
// own evolving Option signature. Adding a field to pkg/validator's
// Validator struct, renaming validator.WithXxx, or changing the
// validator.Option function signature can all be absorbed inside the
// translation function without breaking facade consumers.
type ValidateOption func(*validateConfig)

// validateConfig is the internal capture struct populated by the
// WithValidation* factories. Pointer fields distinguish "unset" (nil,
// inherit validator default) from "set to zero/false/empty" (non-nil
// pointer whose value happens to be the zero). Slice/map fields use
// nil to mean unset because the empty slice has different semantics
// (e.g., "no tolerations at all" vs "no override; use default").
type validateConfig struct {
	namespace        *string
	runID            *string
	cleanup          *bool
	imagePullSecrets []string
	noCluster        *bool
	tolerations      []corev1.Toleration
	// tolerationsSet records that WithValidationTolerations was called,
	// even with a nil/empty slice. nil tolerations cannot itself signal
	// "explicitly override" because nil is also the zero value — but the CLI
	// must ALWAYS override the validator's default tolerate-all ("*") so it
	// doesn't ship AICR_TOLERATIONS. When tolerationsSet is true the option
	// is emitted regardless of nil-ness; when false (controllers that never
	// call the option) the validator keeps its default.
	tolerationsSet bool
	nodeSelector   map[string]string

	// timeout opts into a facade-level deadline for ValidateState. nil means
	// "use the default (ValidationOperationTimeout)" — the controller path.
	// A non-nil *0 means "no facade cap; run under the caller's ctx as-is"
	// (the CLI path, where per-validator timeouts govern). A non-nil >0 sets
	// that explicit cap. Pointer-wrapped so the zero (0 = uncapped) is
	// distinguishable from unset (nil = default).
	timeout *time.Duration

	// phases selects which validation phases run. nil/empty means
	// "run all phases" (validator.PhaseOrder) — the prior behavior.
	// Unlike the pointer fields above, an empty slice is treated the
	// same as nil (run all), because there is no meaningful "run zero
	// phases" request: a caller wanting a subset names that subset.
	phases []Phase

	// commit, imageRegistryOverride, and imageTagOverride mirror the
	// validator.WithCommit / WithImageRegistryOverride / WithImageTagOverride
	// options. These are string values whose validator-side handling already
	// treats empty as "unset" (commit only influences dev-image SHA
	// resolution; the two overrides only apply when non-empty), so an empty
	// string here is the natural "unset" sentinel — no pointer wrapper is
	// needed to disambiguate it from a meaningful zero value.
	commit                string
	imageRegistryOverride string
	imageTagOverride      string
}

// buildValidateConfig replays each WithValidation* option into a fresh
// validateConfig. Client.ValidateState reads BOTH the derived
// []validator.Option (via validateOptionsFromConfig) AND the configured
// phases from this single options pass — the phases don't map to a
// validator.Option (they're a parameter to ValidatePhases), so they
// can't be expressed in the option slice.
func buildValidateConfig(opts []ValidateOption) *validateConfig {
	cfg := &validateConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	return cfg
}

// validateOptionsFromConfig translates an already-built validateConfig into
// the pkg/validator option slice. Called from Client.ValidateState AFTER it
// releases Client.mu — the translation is pure (no Client state read) and
// runs once per call, so a future field added to validator.With* is one edit
// here and zero edits on the facade surface. phases is intentionally NOT
// translated here — it is passed directly to ValidatePhases by the caller.
func validateOptionsFromConfig(cfg *validateConfig) []validator.Option {
	out := make([]validator.Option, 0, 10)
	if cfg.namespace != nil {
		out = append(out, validator.WithNamespace(*cfg.namespace))
	}
	if cfg.runID != nil {
		out = append(out, validator.WithRunID(*cfg.runID))
	}
	if cfg.cleanup != nil {
		out = append(out, validator.WithCleanup(*cfg.cleanup))
	}
	if cfg.imagePullSecrets != nil {
		out = append(out, validator.WithImagePullSecrets(cfg.imagePullSecrets))
	}
	if cfg.noCluster != nil {
		out = append(out, validator.WithNoCluster(*cfg.noCluster))
	}
	if cfg.tolerationsSet {
		// Emit even on a nil/empty slice: an explicit override must clear the
		// validator's default tolerate-all ("*") so AICR_TOLERATIONS isn't
		// sent. Gating on non-nil would silently drop the CLI's intentional
		// "no tolerations" override back to the default.
		out = append(out, validator.WithTolerations(cfg.tolerations))
	}
	if cfg.nodeSelector != nil {
		out = append(out, validator.WithNodeSelector(cfg.nodeSelector))
	}
	if cfg.commit != "" {
		out = append(out, validator.WithCommit(cfg.commit))
	}
	if cfg.imageRegistryOverride != "" {
		out = append(out, validator.WithImageRegistryOverride(cfg.imageRegistryOverride))
	}
	if cfg.imageTagOverride != "" {
		out = append(out, validator.WithImageTagOverride(cfg.imageTagOverride))
	}
	return out
}

// WithValidationNamespace sets the Kubernetes namespace where
// validation Jobs run. Default: "aicr-validation".
func WithValidationNamespace(namespace string) ValidateOption {
	return func(c *validateConfig) { c.namespace = &namespace }
}

// WithValidationRunID overrides the auto-generated identifier shared
// across the Jobs and resources produced by a single validation run.
// Use this to make repeated runs in the same namespace
// distinguishable (e.g., a controller's reconcile-key suffix).
func WithValidationRunID(runID string) ValidateOption {
	return func(c *validateConfig) { c.runID = &runID }
}

// WithValidationCleanup controls whether validator-emitted Jobs,
// ConfigMaps, and RBAC are deleted at the end of the run. Default:
// true. Set to false to leave artifacts behind for post-mortem
// inspection.
func WithValidationCleanup(cleanup bool) ValidateOption {
	return func(c *validateConfig) { c.cleanup = &cleanup }
}

// WithValidationImagePullSecrets sets imagePullSecrets on the
// validator pods. Use this when the validator images live in a
// private registry whose credentials live in a Secret in the
// validation namespace.
//
// The input is defensively copied; a caller that mutates the slice
// after this returns won't race with ValidateState reading it on a
// goroutine. nil-in maps to nil stored (preserves the "unset" sentinel
// downstream), empty-in maps to an empty-non-nil copy.
func WithValidationImagePullSecrets(secrets []string) ValidateOption {
	return func(c *validateConfig) {
		c.imagePullSecrets = cloneStringSlice(secrets)
	}
}

// WithValidationNoCluster enables dry-run mode: no Kubernetes
// resources are created, all checks report as "skipped - no-cluster
// mode (test mode)". Constraints are still evaluated inline (they
// don't need cluster access). Use this for unit tests that exercise
// the facade surface without a live cluster.
func WithValidationNoCluster(noCluster bool) ValidateOption {
	return func(c *validateConfig) { c.noCluster = &noCluster }
}

// WithValidationTolerations passes tolerations through to the
// validation workload pods (e.g. NCCL benchmark pods). Does NOT
// affect the orchestrator Job itself, which runs with
// snapshotter.DefaultTolerations.
//
// The input is defensively copied; mutation after this returns won't
// race with downstream serialization on a validator goroutine.
func WithValidationTolerations(tolerations []corev1.Toleration) ValidateOption {
	return func(c *validateConfig) {
		// Mark set even for a nil/empty slice so validateOptionsFromConfig
		// always emits validator.WithTolerations — clearing the validator's
		// default tolerate-all is an explicit override, not a no-op.
		c.tolerationsSet = true
		c.tolerations = cloneTolerations(tolerations)
	}
}

// WithValidationTimeout opts into a facade-level deadline for the
// ValidateState run. By default (option unset) ValidateState wraps the
// caller's context with defaults.ValidationOperationTimeout (75m), which
// suits controllers that pass an unbounded context. Pass a positive
// duration to set an explicit cap, or 0 to impose NO facade cap — the run
// then proceeds under the caller's context unchanged so per-validator
// timeouts (e.g. the 65m inference-perf check) govern. The CLI validate
// command passes 0 so an all-phase run isn't cut short by a fixed cap.
func WithValidationTimeout(d time.Duration) ValidateOption {
	return func(c *validateConfig) { c.timeout = &d }
}

// WithValidationNodeSelector passes a node selector through to the
// validation workload pods. Use when GPU nodes carry non-standard
// labels and the platform-default selector wouldn't match. Does NOT
// affect the orchestrator Job itself.
//
// The input is defensively copied; without this, a caller mutating the
// map after handing off would race with the validator's map iteration
// (potential "concurrent map iteration and map write" panic in
// serializeNodeSelector).
func WithValidationNodeSelector(nodeSelector map[string]string) ValidateOption {
	return func(c *validateConfig) {
		c.nodeSelector = cloneStringMap(nodeSelector)
	}
}

// WithValidationPhases restricts the run to the named phases, in the
// order given. Valid values are PhaseDeployment, PhasePerformance, and
// PhaseConformance. When omitted (or called with no phases), all phases
// run in their canonical order — the default behavior. ValidateState
// rejects any unrecognized phase value with ErrCodeInvalidRequest before
// touching the cluster, so a typo cannot silently produce an empty run.
//
// The input is defensively copied so a caller mutating the slice after
// this returns won't race with ValidateState reading it.
func WithValidationPhases(phases ...Phase) ValidateOption {
	return func(c *validateConfig) {
		if phases == nil {
			c.phases = nil
			return
		}
		c.phases = append([]Phase(nil), phases...)
	}
}

// WithValidationCommit sets the git commit SHA threaded into the
// validator (validator.WithCommit). Used to resolve dev-build validator
// images to SHA-tagged images. An empty string is the "unset" sentinel —
// no validator option is emitted, matching the validator's own behavior
// where an empty commit influences nothing.
func WithValidationCommit(commit string) ValidateOption {
	return func(c *validateConfig) { c.commit = commit }
}

// WithValidationImageRegistryOverride overrides the registry prefix on
// validator container images (validator.WithImageRegistryOverride), e.g.
// to point at a local registry mirror. Empty means "no override" — the
// validator keeps its default registry.
func WithValidationImageRegistryOverride(registry string) ValidateOption {
	return func(c *validateConfig) { c.imageRegistryOverride = registry }
}

// WithValidationImageTagOverride overrides the tag on every validator
// container image (validator.WithImageTagOverride), intended for
// feature-branch dev builds whose commit SHA has no published image.
// Empty means "no override" — the validator keeps its resolved tag.
func WithValidationImageTagOverride(tag string) ValidateOption {
	return func(c *validateConfig) { c.imageTagOverride = tag }
}

// cloneStringSlice returns a shallow copy of s, preserving the
// nil-vs-empty distinction. A nil input maps to nil out (so the
// "unset" sentinel survives through to validateOptionsFromConfig, which
// skips the validator option entirely on nil); an empty non-nil input
// maps to an empty non-nil copy (preserving caller intent for "no
// secrets" / "no tolerations" overrides).
func cloneStringSlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// cloneTolerations is the corev1.Toleration analog of
// cloneStringSlice. corev1.Toleration is a value type so a shallow
// copy is enough — its fields are scalars and one string.
func cloneTolerations(t []corev1.Toleration) []corev1.Toleration {
	if t == nil {
		return nil
	}
	out := make([]corev1.Toleration, len(t))
	copy(out, t)
	return out
}

// cloneStringMap returns a shallow copy of m, preserving the
// nil-vs-empty distinction. Same rationale as cloneStringSlice;
// validateOptionsFromConfig checks for nil to decide whether to emit the
// validator.With* call at all.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// RecipeSourceOption identifies where recipes are sourced from.
type RecipeSourceOption struct {
	internal recipeSource
}

// WithRecipeSource sets the recipe source on the Client. Construct the
// argument with OCISource or FilesystemSource.
func WithRecipeSource(s RecipeSourceOption) Option {
	return func(c *Client) {
		c.source = s.internal
	}
}

// WithVersion sets the version string stamped into resolved recipe
// metadata (RecipeResult.Metadata.Version). Threaded through to the
// underlying recipe.Builder via recipe.WithVersion. Typically the
// consuming binary's build version.
func WithVersion(version string) Option {
	return func(c *Client) { c.version = version }
}

// WithAllowLists fences which criteria values the Client's resolve path
// accepts. A resolve whose criteria fall outside the allowlist is rejected
// before the recipe is built. Pass nil (or omit the option) to allow all
// values. Construct an AllowLists directly or via ParseAllowListsFromEnv.
func WithAllowLists(al *AllowLists) Option {
	return func(c *Client) { c.allowLists = al }
}

// ParseAllowListsFromEnv builds an AllowLists from the AICR_ALLOWED_*
// environment variables (AICR_ALLOWED_ACCELERATORS, AICR_ALLOWED_SERVICES,
// AICR_ALLOWED_INTENTS, AICR_ALLOWED_OS). Returns nil when none are set —
// WithAllowLists treats a nil AllowLists as allow-all. Pass the result to
// WithAllowLists.
func ParseAllowListsFromEnv() (*AllowLists, error) {
	internal, err := recipe.ParseAllowListsFromEnv()
	if err != nil {
		return nil, err
	}
	return WrapAllowLists(internal), nil
}

// OCISource describes an OCI registry containing AICR recipes.
//
// The tag is optional; if empty, "latest" is assumed by the downstream
// loader.
func OCISource(registry, tag string) RecipeSourceOption {
	return RecipeSourceOption{
		internal: recipeSource{
			kind:     sourceKindOCI,
			registry: registry,
			tag:      tag,
		},
	}
}

// EmbeddedSource uses only AICR's built-in (embedded) recipe data, no overlay.
func EmbeddedSource() RecipeSourceOption {
	return RecipeSourceOption{internal: recipeSource{kind: sourceKindEmbedded}}
}

// FilesystemSource describes a local filesystem path containing AICR
// recipes.
func FilesystemSource(path string) RecipeSourceOption {
	return RecipeSourceOption{
		internal: recipeSource{
			kind: sourceKindFilesystem,
			path: path,
		},
	}
}

// sourceKind is an unexported enum for recipe source variants.
type sourceKind int

const (
	sourceKindUnset sourceKind = iota
	sourceKindOCI
	sourceKindFilesystem
	sourceKindEmbedded
)

// recipeSource is the internal representation passed to the Client.
type recipeSource struct {
	kind     sourceKind
	registry string
	tag      string
	path     string
}
