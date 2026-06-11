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
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// Phase identifies a single validation phase. Facade-owned so the
// stable surface does not propagate pkg/validator type-shape changes.
// Values match pkg/validator/v1 constants verbatim for direct
// wire compatibility.
type Phase string

// Validation phases — string values match pkg/validator/v1 so wire
// round-trips between facade and validator are byte-identical.
const (
	PhaseDeployment  Phase = "deployment"
	PhasePerformance Phase = "performance"
	PhaseConformance Phase = "conformance"
)

// ReportSummary is the high-level pass/fail count breakdown of a
// validation phase's CTRF report. Facade-owned (not aliased to
// ctrf.Summary); fields mirror the CTRF spec summary contract.
type ReportSummary struct {
	Tests   int
	Passed  int
	Failed  int
	Skipped int
	Pending int
	Other   int
}

// PhaseResult is the outcome of running all validators in a single
// phase. Facade-owned. Summary holds the CTRF count breakdown for the
// common pass/fail check; RawReport carries the marshaled CTRF JSON
// for callers needing per-test detail; Report is the typed CTRF
// report retained for in-tree consumers that merge per-phase reports
// via ctrf.MergeReports.
type PhaseResult struct {
	Phase     Phase
	Status    string
	Duration  time.Duration
	Summary   ReportSummary
	RawReport []byte
	Report    *ctrf.Report
}

// Snapshot is the captured cluster-state artifact returned by
// Client.CollectSnapshot. Facade-owned so the stable surface does
// not propagate pkg/snapshotter type-shape changes. APIVersion / Kind /
// CapturedAt are the high-level identifying metadata; the full
// measurement payload is held in an unexported internal field for
// zero-copy round-trip through ValidateState. Consumers needing
// measurement-level inspection import pkg/snapshotter directly.
type Snapshot struct {
	APIVersion string
	Kind       string
	CapturedAt time.Time

	// internal holds the upstream pkg/snapshotter.Snapshot so the
	// facade can re-pass the snapshot to ValidateState without
	// reserializing. Tests that construct &Snapshot{} have internal == nil;
	// the translation helpers reconstruct a minimal pkg/snapshotter.Snapshot
	// from the public fields in that case.
	internal *snapshotter.Snapshot
}

// AgentConfig is the deployment-time configuration for the snapshot-
// collection Job passed to Client.CollectSnapshot. Facade-owned;
// field-for-field mirror of pkg/snapshotter.AgentConfig. Tolerations
// keep k8s.io/api/core/v1.Toleration since kubernetes/api is itself
// stable.
type AgentConfig struct {
	Kubeconfig         string
	Namespace          string
	Image              string
	ImagePullSecrets   []string
	JobName            string
	ServiceAccountName string
	NodeSelector       map[string]string
	Tolerations        []corev1.Toleration
	Timeout            time.Duration
	Cleanup            bool
	Output             string
	Debug              bool
	Privileged         bool
	RequireGPU         bool
	RuntimeClassName   string
	TemplatePath       string
	MaxNodesPerEntry   int
	OS                 string
	Requests           corev1.ResourceList
	Limits             corev1.ResourceList
}

// Criteria is the facade-owned, semver-stable shape of a recipe-resolution
// query. Mirrors pkg/recipe.Criteria field-for-field with the enum-typed
// pkg/recipe values projected to plain strings so the facade contract does
// not pin consumers to pkg/recipe's enum identifiers (an internal enum
// rename or addition stays internal). Construct one directly or wrap an
// upstream pkg/recipe.Criteria via WrapCriteria.
//
// Field meanings match the pkg/recipe.Criteria documentation:
//   - Service: Kubernetes service flavor (eks/gke/aks/oke/kind/lke/bcm).
//   - Accelerator: GPU model identifier (h100/h200/b200/gb200/gb300/a100/l40/rtx-pro-6000).
//   - Intent: workload intent (training/inference).
//   - OS: worker-node OS (ubuntu/rhel/cos/amazonlinux/talos).
//   - Platform: framework overlay (dynamo/kubeflow/nim/runai/slurm).
//   - Nodes: worker-node count hint (0 = unspecified).
//
// Empty string is the "unspecified" sentinel for every field except Nodes,
// where 0 plays that role. A non-empty string that the registry does not
// recognize is rejected at resolve time with ErrCodeInvalidRequest.
type Criteria struct {
	Service     string
	Accelerator string
	Intent      string
	OS          string
	Platform    string
	Nodes       int
}

// AllowLists fences which criteria values the resolve path accepts on a
// Client constructed via WithAllowLists. Facade-owned; the typed-enum
// fields on pkg/recipe.AllowLists project to plain string slices so the
// facade does not propagate pkg/recipe's enum identifiers across the
// semver boundary. A nil receiver, or an AllowLists whose slices are all
// empty, accepts every value (the documented "no fencing" mode). An "any"
// value on a Criteria field is always accepted regardless of the
// allowlist, matching the pkg/recipe behavior.
type AllowLists struct {
	// Accelerators is the set of accepted accelerator identifiers
	// (e.g., "h100", "b200"). Empty = accept all.
	Accelerators []string

	// Services is the set of accepted service identifiers
	// (e.g., "eks", "gke"). Empty = accept all.
	Services []string

	// Intents is the set of accepted intent identifiers
	// (e.g., "training", "inference"). Empty = accept all.
	Intents []string

	// OSTypes is the set of accepted OS identifiers
	// (e.g., "ubuntu", "rhel"). Empty = accept all.
	OSTypes []string
}

// CriteriaRegistry is the per-DataProvider set of valid criteria values,
// returned by Client.CriteriaRegistry so CLI/library callers parse and
// validate criteria against the SAME provider the Client resolves with.
//
// Intentionally kept as a transparent alias of pkg/recipe.CriteriaRegistry
// rather than wrapped into a facade-owned type, for two reasons:
//
//  1. The registry is behavior-rich (ParseService/ParseAccelerator/...,
//     SetStrict, Values, AllAcceleratorTypes, etc.) — wrapping it would
//     require translating every method through, with no semver win because
//     these methods are already used to construct pkg/recipe.Criteria
//     instances in CLI / API call paths.
//  2. The registry carries mutable shared state (strict mode, registered
//     values) keyed by per-Client DataProvider identity. A facade wrapper
//     would either copy state (breaking the per-Client identity coupling)
//     or hold a pointer (no isolation win over the alias).
//
// External callers receive the same pkg/recipe.CriteriaRegistry the
// Client's resolve path uses. If the underlying API evolves, this alias
// is the single canary; the facade can absorb it by hand-writing a wrapper
// then.
type CriteriaRegistry = recipe.CriteriaRegistry

// RecipeRequest is the stable external request shape. The Client
// translates this into pkg/recipe.Criteria.
type RecipeRequest struct {
	// Service is the target Kubernetes service identifier, e.g.
	// "eks", "gke", "aks", "oke", "kind", "lke", or "any". Mapped
	// to pkg/recipe CriteriaService. Note that this is the K8s
	// FLAVOR (eks vs gke), not the cloud vendor (aws vs gcp);
	// callers that think in cloud-vendor terms must map first
	// (aws→eks, gcp→gke, etc.).
	Service string

	// Region is the cloud region. Informational only — not part of
	// pkg/recipe.Criteria today; captured on the request so consumers
	// can audit the call without a separate field.
	Region string

	// Accelerator is the GPU model identifier, e.g. "h100", "b200".
	Accelerator string

	// Nodes is the worker-node count hint. Mapped to CriteriaNodes.
	// Note that this is the NUMBER OF NODES, not the number of
	// accelerators — a 64-GPU cluster on 8-GPU nodes has Nodes=8.
	// Zero means "unspecified, AICR picks the default-sized recipe."
	// Negative values are rejected with ErrCodeInvalidRequest.
	Nodes int32

	// Intent is the workload intent. Mapped to CriteriaIntent.
	// Supported values are defined by pkg/recipe.GetCriteriaIntentTypes
	// — today "training" and "inference".
	Intent string

	// OS is the worker-node operating system. Mapped to CriteriaOS.
	// Supported values: "ubuntu", "rhel", "cos", "amazonlinux".
	// Empty means "unspecified" — recipe resolution will not select
	// OS-pinned leaf overlays (e.g., h100-eks-ubuntu-training,
	// h100-gke-cos-training) and will fall back to the OS-agnostic
	// ancestor. Set this when the cluster's OS is known so OS-specific
	// constraints and mixins (kernel version, driver tuning) are
	// included.
	OS string

	// Platform is the workload platform overlay. Mapped to
	// CriteriaPlatform. Supported values are defined by
	// pkg/recipe.GetCriteriaPlatformTypes — today "", "any", "dynamo",
	// "kubeflow", "nim".
	Platform string

	// PinnedName reserves space for future pinned-recipe support.
	// Currently rejected with ErrCodeUnavailable; set the criteria
	// fields above instead.
	PinnedName string

	// PinnedVersion reserves space for future pinned-recipe support.
	// Currently rejected with ErrCodeUnavailable.
	PinnedVersion string
}

// RecipeResult is the stable external result shape.
type RecipeResult struct {
	// Name is a stable identifier derived from the resolved criteria.
	// Because AICR recipes are keyed by criteria (not by a standalone
	// name), this field is the criteria string representation rather
	// than an independent label.
	Name string

	// Version is the recipe metadata version (set by the CLI that
	// generated the recipe data).
	Version string

	// TranslatedAt is the wall-clock time the facade completed the
	// translation of the internal RecipeResult into this shape. This
	// is NOT the time the underlying recipe was built — AICR's
	// internal RecipeResult currently carries no build timestamp.
	TranslatedAt time.Time

	// Components lists the deployable components in the recipe.
	Components []ComponentRef

	// internal holds the upstream pkg/recipe.RecipeResult so
	// BundleComponents can call its GetValuesForComponent /
	// component-ref helpers without re-resolving the recipe.
	// Lowercase = unexported = invisible to consumers; the only
	// way to populate it is via Client.ResolveRecipe.
	//
	// Lifetime: bound to the Client that produced this
	// RecipeResult — callers MUST NOT cache RecipeResults across a
	// Close. If the Client is Closed, internal's underlying
	// DataProvider may have been evicted; BundleComponents
	// re-checks via the Client's own state.
	internal *recipe.RecipeResult

	// owner identifies the Client that produced this RecipeResult.
	// Set by Client.ResolveRecipe; checked by BundleComponents and
	// ValidateState to reject cross-client misuse — passing a
	// RecipeResult produced by Client A to Client B's bundle/validate
	// methods would silently mix A's component refs with B's
	// DataProvider reads, producing the wrong Helm values or
	// supplemental manifests without an error. Pointer identity is
	// the token: zero-cost, naturally unique, and unforgeable from
	// outside the package because the field is unexported.
	owner *Client
}

// Resolved returns the complete underlying recipe (the full
// pkg/recipe.RecipeResult) that this result wraps. The facade RecipeResult
// exposes only Name/Version/Components; callers that need constraints,
// validation config, deployment order, or metadata (e.g. evidence emission)
// use this. Returns nil if the result was not produced by the Client.
//
// Lifetime: the returned pointer is borrowed from the facade RecipeResult.
// Do not mutate; do not retain past the facade RecipeResult's lifetime.
// Marshal/serialize first if persistence is needed.
func (r *RecipeResult) Resolved() *recipe.RecipeResult {
	if r == nil {
		return nil
	}
	return r.internal
}

// ComponentBundle is the resolved deployable artifact for one
// recipe component. The slice returned by Client.BundleComponents
// mirrors RecipeResult.Components 1:1 — same order, same length —
// so callers can correlate by index when threading bundles back
// through their own state.
//
// Component identity (Name, Kind, Version) duplicates the matching
// RecipeRef so callers passing bundles around without the original
// RecipeResult retain enough context to dispatch on kind.
//
// HelmValues vs Manifests population — read carefully, the rule is
// per-Kind, not "exactly one":
//
//   - Helm components: HelmValues holds YAML-encoded values that
//     downstream consumers pass to `helm install --values`.
//     Manifests MAY ALSO be non-nil when the recipe attaches
//     supplemental manifest files to the Helm component (e.g.,
//     gpu-operator's overlay attaches a dcgm-exporter manifest;
//     h100-gke-cos-training attaches gke-nccl-tcpxo manifests).
//     Downstream consumers should apply Manifests alongside the
//     Helm release. Skipping Manifests on a Helm component will
//     silently drop those resources.
//   - Kustomize / raw-manifest components: Manifests holds the
//     rendered manifest bytes. HelmValues is nil.
//   - Components with neither (rare — a recipe component with no
//     valuesFile, no overrides, and no manifestFiles): both fields
//     are nil; the component is still listed for ordering / status
//     purposes.
type ComponentBundle struct {
	// Component is the matching ComponentRef from the recipe.
	Component ComponentRef

	// HelmValues are YAML-encoded Helm values, or nil for
	// non-Helm components.
	HelmValues []byte

	// Manifests are rendered manifest bytes. Non-nil for
	// Kustomize components, and also non-nil for Helm components
	// whose recipe attaches supplemental manifestFiles. nil when
	// the component has no manifest files of its own.
	Manifests []byte
}

// CatalogSource constants for CatalogEntry.Source comparisons.
const (
	// CatalogSourceEmbedded is the Source value for built-in OSS overlays.
	CatalogSourceEmbedded = recipe.CatalogSourceEmbedded

	// CatalogSourceExternal is the Source value for overlays loaded via --data.
	CatalogSourceExternal = recipe.CatalogSourceExternal
)

// CatalogEntry describes one overlay in the recipe catalog, returned by
// Client.ListCatalog.
//
// IsLeaf is true when the overlay is a leaf — no other overlay in the
// catalog lists this one as its spec.base. Leaf overlays are the most
// specific recipes for a given criteria combination.
//
// Source is one of CatalogSourceEmbedded or CatalogSourceExternal.
type CatalogEntry struct {
	// Name is the overlay name, e.g. "h100-eks-ubuntu-training".
	Name string `json:"name" yaml:"name"`

	// Criteria is the set of dimensions this overlay targets.
	Criteria Criteria `json:"criteria" yaml:"criteria"`

	// IsLeaf is true when this overlay is a catalog leaf (no other
	// overlay inherits from it).
	IsLeaf bool `json:"is_leaf" yaml:"is_leaf"`

	// Source is the data provenance: "embedded" or "external".
	Source string `json:"source" yaml:"source"`
}

// ComponentRef identifies a deployable recipe component.
//
// The Name/Chart distinction matters: Name is AICR's identifier
// (e.g. "nfd"), while Chart is the Helm chart name (e.g.
// "node-feature-discovery"). Most components have Name == Chart,
// but the registry's helm.defaultChart override allows them to
// differ. Consumers building Helm Releases must use Chart, not
// Name, as spec.forProvider.chart.name.
type ComponentRef struct {
	// Name is the component identifier, e.g. "gpu-operator".
	Name string

	// Kind is the deployment kind, e.g. "Helm" or "Kustomize".
	Kind string

	// Version is the component chart/manifest version.
	Version string

	// Source is the upstream artifact location: a Helm chart
	// repository URL for Helm components (e.g.
	// "https://helm.ngc.nvidia.com/nvidia"), or a Kustomize source
	// repo for Kustomize components. Empty when the recipe
	// registry leaves it unset.
	Source string

	// Chart is the Helm chart name as it appears in the upstream
	// repository (e.g. "gpu-operator"). Empty for non-Helm
	// components. Defaults to Name when the registry leaves it
	// unset.
	Chart string

	// Namespace is the install namespace recommended by the recipe
	// (e.g. "gpu-operator"). Consumers SHOULD honor it so the
	// deployed layout matches what AICR validation expects to find.
	// Empty when the recipe leaves it unset.
	Namespace string
}
