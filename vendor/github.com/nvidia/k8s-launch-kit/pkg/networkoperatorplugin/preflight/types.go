// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package preflight

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Stable codes for each check. Used to key Result lookups in callers (e.g.
// the HTML report renders one section per code) and to power structured
// error messages out of `l8k deploy`.
const (
	CodeHelmChartVersion     = "HELM_CHART_VERSION"
	CodeHelmValues           = "HELM_VALUES"
	CodeStrayCRs             = "STRAY_CRS"
	CodeNCPComponentVersions = "NCP_COMPONENT_VERSIONS"
)

// Result captures the outcome of a single preflight check. A Result with
// Skipped=true means the check could not be performed (missing inputs,
// missing cluster state) — the caller should not treat this as failure.
// Mismatches is the per-path diff list; empty when the check passed.
type Result struct {
	// Name is a human-readable label for log lines and the HTML report
	// (e.g., "Helm chart version", "NicClusterPolicy component versions").
	Name string `json:"name"`
	// Code is the stable identifier for this check kind. One of the
	// Code* constants above.
	Code string `json:"code"`
	// Skipped is true when the check couldn't run (missing input,
	// release not found, etc.). Skipped checks do NOT fail the verdict.
	Skipped bool `json:"skipped,omitempty"`
	// Reason holds the human-readable explanation for Skipped, or a
	// short headline for a successful/failed check.
	Reason string `json:"reason,omitempty"`
	// Mismatches lists every divergence the check found. Empty when
	// the check passed.
	Mismatches []Mismatch `json:"mismatches,omitempty"`
}

// Passed reports whether the check ran successfully with no mismatches.
// A Skipped check is NOT considered passed — it returned no opinion.
func (r Result) Passed() bool {
	return !r.Skipped && len(r.Mismatches) == 0
}

// Failed reports whether the check ran and found at least one mismatch.
// Mirrors Passed() but more readable at call sites that gate exit codes.
func (r Result) Failed() bool {
	return !r.Skipped && len(r.Mismatches) > 0
}

// Mismatch is one divergence between expected (what l8k would produce) and
// actual (what the cluster reports). For helm checks Path is a dotted
// values path; for stray-CRs and component-versions it's a
// Kind/[Namespace/]Name identifier.
type Mismatch struct {
	Path     string      `json:"path"`
	Expected interface{} `json:"expected,omitempty"`
	Actual   interface{} `json:"actual,omitempty"`
	// Detail is an optional extra line shown in reports and error
	// messages — used by stray-CR rows to record the GVK, by component
	// checks to record whether the field came from componentVersion or
	// docaDriver.version, etc.
	Detail string `json:"detail,omitempty"`
}

// String renders a one-line human-readable summary, suitable for log output.
//
// When both Expected and Actual are nil the Mismatch is a presence-only
// check (e.g. CheckStrayCRs: a resource exists in the cluster that
// shouldn't), and only Path + Detail are rendered. When at least one is
// set this is a value-diff (chart-version, helm-values, NCP component
// versions) and we render the explicit expected/actual pair.
func (m Mismatch) String() string {
	if m.Expected == nil && m.Actual == nil {
		if m.Detail != "" {
			return fmt.Sprintf("%s (%s)", m.Path, m.Detail)
		}
		return m.Path
	}
	if m.Detail != "" {
		return fmt.Sprintf("%s: expected=%v actual=%v (%s)", m.Path, m.Expected, m.Actual, m.Detail)
	}
	return fmt.Sprintf("%s: expected=%v actual=%v", m.Path, m.Expected, m.Actual)
}

// ObjectRef identifies a Kubernetes object — what `l8k generate` rendered
// (so the stray-CR check can subtract it from cluster state) and what the
// stray-CR remediation deletes when --overwrite-existing is set.
type ObjectRef struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

// Inputs bundles every piece of state the checks might need. The caller
// (deploy.go or validate.go) is responsible for pre-resolving:
//
//   - catalog-derived expectations (chart version, component version, etc.) —
//     looked up once from the embedded networkoperatorplugin release catalog;
//   - generated artefacts on disk (values.yaml content + the list of rendered
//     manifest object refs).
//
// Empty / nil fields are tolerated: each check decides whether it has enough
// to run (and returns Skipped otherwise).
type Inputs struct {
	// KubeClient is the controller-runtime client used by checks that
	// read CRs from the cluster (stray-CRs, NCP components). Required
	// for those checks; the helm checks use RestConfig instead.
	KubeClient client.Client

	// RestConfig is required by the Helm Go SDK checks. The package
	// constructs its own action.Configuration internally.
	RestConfig *rest.Config

	// OperatorNamespace is where the network-operator chart is
	// installed and where namespaced CRs are enumerated.
	OperatorNamespace string

	// HelmReleaseName is the helm release the check looks up. Defaults
	// to "network-operator" when empty.
	HelmReleaseName string

	// SelectedRelease is the catalog key (e.g. "26.4"). Surfaced in
	// human-readable messages; the actual expected versions are
	// supplied via the Expected* fields below.
	SelectedRelease string
	// ExpectedChartVersion is the chart version (without "v" prefix)
	// the deployed release should report — derived from
	// networkOperator.Version in the catalog.
	ExpectedChartVersion string
	// ExpectedAppVersion is the chart appVersion the deployed release
	// should report — typically the catalog's networkOperator.Version
	// with the "v" prefix kept.
	ExpectedAppVersion string
	// ExpectedComponentVersion is the per-component image tag (e.g.
	// "network-operator-v26.4.0-beta.9") used by the NCP check.
	ExpectedComponentVersion string
	// ExpectedDOCAVersion is the catalog's docaDriver.version (e.g.
	// "doca3.4.0-26.04-0.8.4.0-0"), used by the NCP check's ofedDriver
	// row.
	ExpectedDOCAVersion string

	// GeneratedValuesYAML is the rendered values.yaml content (from
	// <deployment-files>/values.yaml). Empty when the chart is managed
	// out of band — the helm-values check soft-skips in that case.
	GeneratedValuesYAML []byte

	// GeneratedManifests is the list of object refs l8k just rendered
	// (or would render). Used by the stray-CRs check to subtract
	// "ours" from cluster state. The caller derives this by reading
	// <deployment-files>/*.yaml and decoding each document.
	GeneratedManifests []ObjectRef
}
