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
	corev1 "k8s.io/api/core/v1"

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
	nodeSelector     map[string]string
}

// applyValidateOptions builds the []validator.Option slice from a
// validateConfig populated by the WithValidation* factories. Called
// from Client.ValidateState AFTER it releases Client.mu — the
// translation is pure (no Client state read) and runs once per call,
// so a future field added to validator.With* is one edit here and
// zero edits on the facade surface.
func applyValidateOptions(opts []ValidateOption) []validator.Option {
	cfg := &validateConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	out := make([]validator.Option, 0, 7)
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
	if cfg.tolerations != nil {
		out = append(out, validator.WithTolerations(cfg.tolerations))
	}
	if cfg.nodeSelector != nil {
		out = append(out, validator.WithNodeSelector(cfg.nodeSelector))
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
		c.tolerations = cloneTolerations(tolerations)
	}
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

// cloneStringSlice returns a shallow copy of s, preserving the
// nil-vs-empty distinction. A nil input maps to nil out (so the
// "unset" sentinel survives through to applyValidateOptions, which
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
// applyValidateOptions checks for nil to decide whether to emit the
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
)

// recipeSource is the internal representation passed to the Client.
type recipeSource struct {
	kind     sourceKind
	registry string
	tag      string
	path     string
}
