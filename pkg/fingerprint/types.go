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

package fingerprint

// Dimension is a single fingerprint dimension. Forward-compatible with
// ADR-007 V2 extensions: signals[] and confidence are reserved for the
// follow-on per-signal provenance work and would be added as additional
// optional fields without a schema break.
type Dimension struct {
	// Value is the resolved dimension value, normalized to the
	// matching recipe.Criteria enum where applicable (e.g. "eks",
	// "h100", "ubuntu"). Empty string means the dimension was not
	// captured.
	Value string `json:"value" yaml:"value"`

	// Source names the collector signal that produced Value, for
	// audit and debugging (e.g. "k8s.node.provider"). Empty when the
	// dimension was not captured.
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
}

// IntDimension is a fingerprint dimension whose resolved value is an
// integer (e.g., node count).
type IntDimension struct {
	// Value is the resolved integer value. Zero indicates the
	// dimension was not captured.
	Value int `json:"value" yaml:"value"`

	// Source names the collector signal that produced Value.
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
}

// OSDimension extends Dimension with the raw OS version string. Value
// is the criteria-aligned OS kind (ubuntu, rhel, cos, amazonlinux,
// talos); Version carries the unmodified VERSION_ID from
// /etc/os-release for audit purposes (recipe.Criteria has no version
// field, so Version does not participate in Match).
type OSDimension struct {
	Value   string `json:"value" yaml:"value"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	Source  string `json:"source,omitempty" yaml:"source,omitempty"`
}

// Fingerprint is the structured cluster identity at snapshot time. Per
// ADR-007 it is the input the verifier uses to confirm a recipe's
// criteria matched the cluster on which validate ran.
type Fingerprint struct {
	// Service is the Kubernetes service / cloud platform
	// (eks, gke, aks, oke, kind, lke). Sourced from
	// k8s.node.provider (parsed from spec.providerID).
	Service Dimension `json:"service" yaml:"service"`

	// Accelerator is the GPU SKU (h100, gb200, b200, a100, l40,
	// rtx-pro-6000). Parsed from gpu.smi.gpu.model.
	Accelerator Dimension `json:"accelerator" yaml:"accelerator"`

	// OS is the worker node operating system, with the raw
	// VERSION_ID retained for audit. Sourced from
	// os.release.{ID,VERSION_ID}.
	OS OSDimension `json:"os" yaml:"os"`

	// K8sVersion is the Kubernetes server version with the leading
	// "v" stripped (e.g., "1.33.4"). Sourced from k8s.server.version.
	K8sVersion Dimension `json:"k8sVersion" yaml:"k8sVersion"`

	// NodeCount is the total number of cluster nodes. Sourced from
	// nodeTopology.summary.node-count.
	NodeCount IntDimension `json:"nodeCount" yaml:"nodeCount"`
}

// DimensionMatch is the three-way per-dimension outcome of Match.
//
// "unknown" means the criteria specifies a value but the fingerprint
// could not determine it (e.g., recipe intent or platform are
// recipe-author choices not detectable from cluster state). Unknowns
// surface in MatchResult.PerDimension for human review without
// counting as a contradiction in the overall MatchResult.Matched flag.
type DimensionMatch string

// DimensionMatch enumerated values.
const (
	DimensionMatched    DimensionMatch = "matched"
	DimensionMismatched DimensionMatch = "mismatched"
	DimensionUnknown    DimensionMatch = "unknown"
)

// DimensionDiff is a single criteria dimension's comparison outcome.
type DimensionDiff struct {
	// RecipeRequires is the criteria value the recipe declares, or
	// empty / "any" when the recipe is generic in this dimension.
	RecipeRequires string `json:"recipeRequires,omitempty" yaml:"recipeRequires,omitempty"`

	// FingerprintProvides is the value the fingerprint resolved for
	// this dimension. Empty when the dimension was not captured.
	FingerprintProvides string `json:"fingerprintProvides,omitempty" yaml:"fingerprintProvides,omitempty"`

	// Match is the three-way outcome.
	Match DimensionMatch `json:"match" yaml:"match"`
}

// MatchResult is the structured outcome of Fingerprint.Match.
//
// Matched is true when no dimension is Mismatched. Unknown dimensions
// (e.g., criteria.intent / criteria.platform when the fingerprint does
// not capture them) do not flip Matched to false: they surface in
// PerDimension for the maintainer to evaluate, but the fingerprint
// itself cannot disprove a match it does not capture.
type MatchResult struct {
	Matched      bool                     `json:"matched" yaml:"matched"`
	PerDimension map[string]DimensionDiff `json:"perDimension" yaml:"perDimension"`
}

// CriteriaInput is the recipe-side input to Fingerprint.Match. It is a
// flat string view of the criteria dimensions a recipe declares.
//
// This package intentionally does not import pkg/recipe.Criteria
// directly: pkg/snapshotter holds a *Fingerprint field, and
// pkg/recipe imports pkg/snapshotter for ExtractCriteriaFromSnapshot,
// so importing pkg/recipe here would close an import cycle. Callers
// adapt their *recipe.Criteria with recipe.ToFingerprintInput.
type CriteriaInput struct {
	Service     string
	Accelerator string
	Intent      string
	OS          string
	Platform    string
	Nodes       int
}
