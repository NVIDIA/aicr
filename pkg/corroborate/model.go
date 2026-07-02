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

package corroborate

// SchemaVersion is the emitted dashboard JSON schema identifier (Contract 4).
// v1 splits the v0 prototype's inlined per-source history: index.json keeps the
// latest-per-signer grid with baked consensus, and the heavy time-series moves
// to series/<recipe>.json.
const SchemaVersion = "aicr-corroboration/v1"

// Outbound header targets, baked into index.json meta so the renderer stays
// data-driven (a maintainer flips one literal here, not the template). An empty
// value renders nothing (the renderer fails soft). LinkInstall is a shell
// command (copied to the clipboard), not a URL; LinkDocs/LinkGitHub are http(s)
// links opened in a new tab.
const (
	LinkGitHub  = "https://github.com/NVIDIA/aicr"
	LinkDocs    = "https://docs.aicr.run/"
	LinkInstall = "curl -sfL https://get.aicr.run | bash"
)

// Index is the boot payload (index.json): the facet value sets, the source
// catalog, and the CSP-first navigation tree (groups -> dashboards -> tabs)
// with baked-in consensus. The static renderer fetches this on load and needs
// no further request to draw the catalog and per-recipe grids.
type Index struct {
	// Schema is always SchemaVersion.
	Schema string `json:"schema"`

	// Meta is presentation metadata (outbound links, summary counts, and the
	// deterministic generated-at stamp). Additive; never feeds consensus.
	Meta Meta `json:"meta"`

	// Criteria holds the facet dropdown values per axis (service, accelerator,
	// os, intent, platform), ordered by the criteria registry's canonical
	// order and filtered to values actually present in the data.
	Criteria map[string][]string `json:"criteria"`

	// Sources maps each signer-id-hash to its display source record.
	Sources map[string]Source `json:"sources"`

	// Groups is the CSP-first catalog tree, ordered by service.
	Groups []Group `json:"groups"`
}

// Meta is presentation metadata for the renderer header and landing summary.
// Purely additive — none of it feeds the consensus math.
type Meta struct {
	// Links are the outbound header navigation targets.
	Links Links `json:"links"`

	// Counts are the landing summary tallies.
	Counts Counts `json:"counts"`

	// GeneratedAt is the newest run AttestedAt rendered "YYYY-MM-DD HH:MM UTC",
	// or "" (omitted) when no run carried a parseable AttestedAt. Derived from
	// evidence, never the publish clock, so it stays byte-reproducible.
	GeneratedAt string `json:"generatedAt,omitempty"`
}

// Links are the outbound header navigation targets. An empty value hides that
// link in the renderer.
type Links struct {
	Install string `json:"install"`
	Docs    string `json:"docs"`
	GitHub  string `json:"github"`
}

// Counts are the landing-page summary tallies (validated recipes, CSPs, and
// distinct signing sources).
type Counts struct {
	Recipes int `json:"recipes"`
	CSPs    int `json:"csps"`
	Sources int `json:"sources"`
}

// Source is one signer's public catalog record, keyed in Index.Sources by its
// signer-id-hash.
type Source struct {
	// Label is the human-readable source name.
	Label string `json:"label"`

	// Class is the derived source class: first-party | community | partner.
	Class string `json:"class"`

	// Allowlisted reports whether the source carries corroboration weight.
	// A false value renders as a zero-weight "reported" dot.
	Allowlisted bool `json:"allowlisted"`

	// SignerID is the verified OIDC identity (the human-auditable count key).
	SignerID string `json:"signerId"`
}

// Group is one service's subtree (group = service in the locked taxonomy).
type Group struct {
	Service    string      `json:"service"`
	Dashboards []Dashboard `json:"dashboards"`
}

// Dashboard is one accelerator-os pairing within a service.
type Dashboard struct {
	Accelerator string `json:"accelerator"`
	OS          string `json:"os"`
	Tabs        []Tab  `json:"tabs"`
}

// Tab is one recipe (intent[-platform]). Its consensus is split per AICR
// version so that corroboration only counts agreement at the SAME version
// (cross-version agreement is not reproduction). Versions are newest-first;
// the overview/landing summarize the newest (Versions[0]).
type Tab struct {
	// Recipe is the overlay metadata.name (the series-file slug).
	Recipe string `json:"recipe"`

	// Coord is the full five-dimension criteria for display and facet
	// filtering (service, accelerator, os, intent, platform).
	Coord map[string]string `json:"coord"`

	// Versions holds one baked consensus grid per AICR version present in the
	// evidence, newest-first.
	Versions []TabVersion `json:"versions"`
}

// TabVersion is one recipe's baked consensus grid for a single AICR version.
type TabVersion struct {
	// AICRVer is the AICR version this grid's consensus was computed at.
	AICRVer string `json:"aicrVer"`

	// PhaseRollup maps each phase to its worst-first rollup state.
	PhaseRollup map[string]string `json:"phaseRollup"`

	// Tests is the per-row grid, ordered by PhaseOrder then CTRF name.
	Tests []Row `json:"tests"`
}

// Row is one CTRF check within a phase, with its baked consensus and the
// latest-per-signer results that carried it (pass/fail only; not-run signers
// are omitted and render as empty cells).
type Row struct {
	Phase     string   `json:"phase"`
	Name      string   `json:"name"`
	Consensus string   `json:"consensus"`
	Reported  int      `json:"reported"`
	Signers   []Latest `json:"signers"`
}

// Latest is one signer's latest in-scope result for a row in index.json. The
// full per-build history lives in series/<recipe>.json.
type Latest struct {
	// Src is the signer-id-hash (a key into Index.Sources).
	Src string `json:"src"`

	// Result is "pass" or "fail" (not-run signers are omitted from the grid).
	Result string `json:"result"`

	// AICRVer is the AICR version from the bundle predicate (a facet axis).
	AICRVer string `json:"aicrVer"`

	// K8sVer is the observed Kubernetes major.minor (a facet axis).
	K8sVer string `json:"k8sVer"`

	// When is the predicate AttestedAt rendered for display — never the
	// publish clock.
	When string `json:"when"`

	// Build is the run identifier.
	Build string `json:"build"`

	// EvidenceRef is the OCI ref of the signed bundle, for the drilldown link.
	EvidenceRef string `json:"evidenceRef"`
}

// Series is the lazy per-recipe payload (series/<recipe>.json): the heavy
// per-source x per-build history the renderer loads on a source-column
// drilldown.
type Series struct {
	// Recipe is the overlay metadata.name.
	Recipe string `json:"recipe"`

	// Builds maps each signer-id-hash to its build columns, newest first.
	Builds map[string][]SeriesBuild `json:"builds"`

	// Health maps each signer-id-hash to its derived run-health summary.
	Health map[string]SeriesHealth `json:"health"`
}

// SeriesBuild is one signer run rendered as a build column.
type SeriesBuild struct {
	ID          string `json:"id"`
	AICRVer     string `json:"aicrVer"`
	K8sVer      string `json:"k8sVer"`
	When        string `json:"when"`
	Newest      bool   `json:"newest"`
	EvidenceRef string `json:"evidenceRef"`

	// Results maps every CTRF name in the recipe's union test set to this
	// build's outcome: "pass", "fail", or "not-run".
	Results map[string]string `json:"results"`
}

// SeriesHealth is a signer's derived run-health summary for one recipe.
type SeriesHealth struct {
	// FlakePct is the percentage of build-to-build result transitions across
	// the recipe's union test set (0 when there is at most one build).
	FlakePct int `json:"flakePct"`

	// LastPassBuild is the newest build id in which every test this signer ran
	// passed, or "" when none.
	LastPassBuild string `json:"lastPassBuild"`

	// Builds is the number of build columns shown.
	Builds int `json:"builds"`
}
