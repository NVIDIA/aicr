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

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// Snapshot is the captured cluster-state artifact returned by
// Client.CollectSnapshot. It is a transparent alias of
// pkg/snapshotter.Snapshot.
//
// Stability: this symbol inherits pkg/snapshotter's Public (evolving)
// tier rather than the facade's stable tier — field-shape changes in
// the source struct propagate without notice. See
// docs/integrator/public-api.md "Facade type aliases" and
// NVIDIA/aicr#1078 (replacing the alias with a facade-owned wrapper).
//
// A Snapshot embeds an api header (apiVersion, kind, metadata) and a
// Measurements slice, one entry per collector that ran.
type Snapshot = snapshotter.Snapshot

// AgentConfig is the deployment-time configuration for the snapshot-
// collection Job. Re-exported from pkg/snapshotter.AgentConfig.
//
// Stability: inherits pkg/snapshotter's Public (evolving) tier — see
// Snapshot's godoc and docs/integrator/public-api.md.
//
// At minimum, callers set Kubeconfig (path or empty for in-cluster),
// Namespace, Image, and ServiceAccountName. Everything else is
// optional and falls back to package defaults.
type AgentConfig = snapshotter.AgentConfig

// PhaseResult is the outcome of running all validators in a single
// phase. Re-exported from pkg/validator.PhaseResult; see that type's
// documentation for the full field reference (Phase, Status, Report,
// Duration).
//
// Stability: inherits pkg/validator's Public (evolving) tier — see
// docs/integrator/public-api.md "Facade type aliases".
type PhaseResult = validator.PhaseResult

// Phase identifies a single validation phase. Re-exported from
// pkg/validator.Phase. The supported values are PhaseDeployment,
// PhasePerformance, and PhaseConformance — see the constants below.
//
// Stability: inherits pkg/validator's Public (evolving) tier — see
// docs/integrator/public-api.md "Facade type aliases".
type Phase = validator.Phase

// Validation phases, re-exported as facade constants so consumers
// don't need to import pkg/validator to filter by phase. Same
// Public (evolving) tier as Phase above.
const (
	PhaseDeployment  = validator.PhaseDeployment
	PhasePerformance = validator.PhasePerformance
	PhaseConformance = validator.PhaseConformance
)

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
