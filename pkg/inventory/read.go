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

package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
)

// helmStatusUninstalled is the one release status this package treats as not
// installed. `helm uninstall --keep-history` leaves a complete record behind,
// and reporting it would put a deliberately-removed component on the `from`
// side at its last version, manufacturing a verdict for an upgrade nobody is
// performing. Every other status — failed, uninstalling, the pending-* three —
// describes resources that really are on the cluster, and a failed release at
// a known version is exactly when the upgrade question matters most.
const helmStatusUninstalled = "uninstalled"

// Deployer names the deployer that installed the bundle being compared.
//
// It is supplied by the caller rather than detected, because the cluster does
// not record it and the transform is not invertible: a release named
// gpu-operator-gpu-operator is flux's gpu-operator installed into namespace
// gpu-operator, or helm's component of that literal name, and nothing on the
// record distinguishes them.
//
// Mirrors config.DeployerType, declared here rather than imported for the
// reason pkg/upgrade's canonicalDeployers gives; read_test.go carries that
// import and fails on drift.
type Deployer string

// The deployers a bundle can be installed with, and the only values
// Options.Deployer accepts.
const (
	DeployerHelm       Deployer = "helm"
	DeployerHelmfile   Deployer = "helmfile"
	DeployerFlux       Deployer = "flux"
	DeployerArgoCD     Deployer = "argocd"
	DeployerArgoCDHelm Deployer = "argocd-helm"
)

// allDeployers is every deployer this package knows a name transform for.
var allDeployers = []Deployer{
	DeployerHelm,
	DeployerHelmfile,
	DeployerFlux,
	DeployerArgoCD,
	DeployerArgoCDHelm,
}

// Component is what the mapping layer needs to know about a registry entry.
// It is a local type for the reason upgrade.Component is: this package must
// not import pkg/recipe. recipe.InventoryComponents is the adapter.
type Component struct {
	// Name is the registry component name.
	Name string

	// Namespace is the component's helm.defaultNamespace. Flux composes it
	// into the release name, and it is what disambiguates an Argo suffix
	// match and two tenants' installs of one chart.
	Namespace string

	// HasUpstreamChart reports helm.defaultVersion being set, which is the
	// guard on the chart-version fallback: a component with no upstream chart
	// is carried by an AICR-authored wrapper whose chart version is the
	// wrapper's own and never the payload's.
	HasUpstreamChart bool
}

// Options is one inventory read.
type Options struct {
	// Kubeconfig is the path to read both clients from. Empty uses the
	// ambient resolution pkg/k8s/client applies.
	Kubeconfig string

	// Deployer is how the bundle being compared was installed.
	Deployer Deployer

	// Components is the set the read answers for.
	Components []Component
}

// Result is the installed inventory: the version table an upgrade comparison
// consumes, and an account of what produced it.
type Result struct {
	// Versions maps component name to installed version. A component absent
	// from the map is not installed. A component present with an empty value
	// is installed at a version nothing in the cluster records comparably;
	// upgrade.Match already reports that as VerdictUnversioned, so there is
	// no sentinel to invent.
	Versions map[string]string

	// Source is what was read and what was skipped.
	Source SourceInfo
}

// SourceInfo accounts for both readers separately. The two sets of counts are
// in different units and must never be summed: the Helm reader counts storage
// records, so one release with ten retained revisions contributes ten, while
// the Argo reader counts Applications, of which a component has one.
type SourceInfo struct {
	Helm HelmInfo
	Argo ArgoInfo
}

// HelmInfo accounts for the Helm storage records.
type HelmInfo struct {
	// Records is every storage object the read examined, across both drivers
	// and including the ones belonging to no component. It is the denominator
	// the other counts are read against.
	Records int

	// Unattributed is records carrying no release name, which cannot be
	// attributed even to a guess.
	Unattributed int

	// Unreadable is releases matched loosely whose records could not be read,
	// counted once per release however many revisions were unreadable.
	Unreadable int

	// Uninstalled is releases excluded because `helm uninstall
	// --keep-history` left the record behind.
	Uninstalled int

	// StampedUnmatched is records carrying an AICR stamp annotation that
	// matched no component under this deployer's name transform. It is the
	// detector for a broken mapping: AICR wrote those records, so anything
	// above zero means this package no longer recognizes its own output.
	StampedUnmatched int
}

// ArgoInfo accounts for the Argo CD Applications.
//
// It has no Uninstalled or StampedUnmatched counterpart, and they are absent
// rather than present-and-always-zero. An Application carries no release
// status, so there is nothing to exclude on, and the generated Application
// carries no AICR annotations either — the stamp lives in the wrapper
// Chart.yaml the Application points at, which this never reads. The Helm-side
// mapping detector therefore has no Argo equivalent today.
type ArgoInfo struct {
	// Applications is every Application the read examined, including the ones
	// belonging to no component.
	Applications int

	// Unattributed is Applications carrying no name.
	Unattributed int

	// Unreadable is Applications matched loosely that could not be projected.
	Unreadable int
}

// Read reads the installed inventory from the cluster opts names.
//
// Both clients are built from one resolved kubeconfig, which is the point: a
// second authentication path could resolve a different context, and an
// inventory assembled from two clusters is a confident wrong answer.
func Read(ctx context.Context, opts Options) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}

	typed, restConfig, err := k8sclient.GetKubeClientWithConfig(opts.Kubeconfig)
	if err != nil {
		return Result{}, err
	}
	dyn, err := k8sclient.NewDynamicClientForConfig(restConfig)
	if err != nil {
		return Result{}, err
	}

	return read(ctx, typed, dyn, opts)
}

// read is Read with the clients supplied.
func read(ctx context.Context, typed kubernetes.Interface, dyn dynamic.Interface, opts Options) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}

	names := make([]string, 0, len(opts.Components))
	for _, c := range opts.Components {
		names = append(names, c.Name)
	}
	within := newScope(names...)

	helmRead, err := helmReleases(ctx, typed, within)
	if err != nil {
		return Result{}, err
	}
	argoRead, err := argoApplications(ctx, dyn, within)
	if err != nil {
		return Result{}, err
	}

	return combine(opts.Deployer, opts.Components, helmRead, argoRead)
}

// validate refuses a request that cannot be answered, before any cluster is
// contacted. An unknown deployer is the dangerous one: no record would match
// its transform, and the read would report a fully-deployed cluster as having
// nothing installed.
func (o Options) validate() error {
	known := false
	for _, d := range allDeployers {
		if o.Deployer == d {
			known = true

			break
		}
	}
	if !known {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("the installed inventory was requested for deployer %q, whose release naming this build "+
				"does not know; pass one of %s", o.Deployer, strings.Join(deployerNames(), ", ")))
	}

	if len(o.Components) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			"the installed inventory was requested for no components, so it could only report that nothing is "+
				"installed; pass the components being compared")
	}

	seen := make(map[string]struct{}, len(o.Components))
	for _, c := range o.Components {
		if c.Name == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				"the installed inventory was requested for a component with no name")
		}
		if _, duplicate := seen[c.Name]; duplicate {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q was requested twice, and the two entries could name different "+
					"namespaces", c.Name))
		}
		seen[c.Name] = struct{}{}
	}

	return nil
}

func deployerNames() []string {
	names := make([]string, 0, len(allDeployers))
	for _, d := range allDeployers {
		names = append(names, string(d))
	}

	return names
}

// matchKind is how a record name reads as a component's.
//
// The order is the precedence: a primary record beats an injected one, so a
// component genuinely named "<x>-post" claims a record of that name rather
// than ceding it to a non-existent "<x>".
type matchKind int

const (
	matchNone matchKind = iota
	matchInjected
	matchPrimary
)

// combine maps both readers' records onto one version table.
//
// Helm answers first and Argo fills only what it left unanswered. Under
// argocd-helm both readers see the same cluster — the app-of-apps is itself a
// Helm release — but the per-component Applications are the per-component
// truth, so Argo may only add components rather than restate them. Presence in
// the table is what counts as an answer, including an empty version: that is
// the Helm side saying it found the release and could not version it, which is
// a finding rather than a gap.
func combine(d Deployer, comps []Component, helmRead, argoRead inventoryRead) (Result, error) {
	versions, helmCounts, err := installedVersions(d, comps, helmRead.Releases)
	if err != nil {
		return Result{}, err
	}
	argoVersions, _, err := installedVersions(d, comps, argoRead.Releases)
	if err != nil {
		return Result{}, err
	}
	for name, version := range argoVersions {
		if _, answered := versions[name]; !answered {
			versions[name] = version
		}
	}

	return Result{
		Versions: versions,
		Source: SourceInfo{
			Helm: HelmInfo{
				Records:          helmRead.Records,
				Unattributed:     helmRead.Unattributed,
				Unreadable:       helmRead.Unreadable,
				Uninstalled:      helmCounts.uninstalled,
				StampedUnmatched: helmCounts.stampedUnmatched,
			},
			Argo: ArgoInfo{
				Applications: argoRead.Records,
				Unattributed: argoRead.Unattributed,
				Unreadable:   argoRead.Unreadable,
			},
		},
	}, nil
}

// mappingCounts is what the mapping dropped, as opposed to what the readers
// did. Argo records can produce neither: they carry no status and no
// annotations. See ArgoInfo.
type mappingCounts struct {
	uninstalled      int
	stampedUnmatched int
}

// installedVersions maps one reader's records onto component versions.
func installedVersions(d Deployer, comps []Component,
	records []installedRelease) (map[string]string, mappingCounts, error) {

	var counts mappingCounts
	candidates := make(map[int][]installedRelease, len(comps))
	for _, record := range records {
		index, kind := attributeRecord(d, comps, record)
		switch {
		case kind == matchNone:
			// Dropped: the overwhelming majority of a real cluster is
			// workloads this project knows nothing about. A stamp is what
			// makes one of them a finding, because AICR wrote it.
			if stamped(record) {
				counts.stampedUnmatched++
			}
		case kind == matchInjected:
			// Absorbed and silent. An injected folder's chart is
			// AICR-authored scaffolding stamped with the AICR release
			// version, never the component's payload version.
		case record.Source == sourceHelm && record.Status == helmStatusUninstalled:
			counts.uninstalled++
		default:
			candidates[index] = append(candidates[index], record)
		}
	}

	// Walked in component order rather than map order so that a cluster with
	// two ambiguous components reports the same one every run.
	versions := make(map[string]string, len(candidates))
	for i := range comps {
		found := candidates[i]
		if len(found) == 0 {
			continue
		}
		record, err := resolveInstall(comps[i], found)
		if err != nil {
			return nil, mappingCounts{}, err
		}
		versions[comps[i].Name] = versionFor(record, comps[i])
	}

	return versions, counts, nil
}

// resolveInstall picks the one record that is this component's install.
//
// A release name is unique within a namespace and not across the cluster, so
// two tenants can hold the same chart at different versions while the version
// table holds one entry per component. The component's own namespace is the
// tiebreak, and failing to break the tie is an error rather than a pick:
// guessing which tenant's install the operator meant is not this tool's
// decision, and the wrong guess is an upgrade verdict for a cluster nobody is
// upgrading.
//
// A single install answers whatever namespace it is in. The component's
// namespace is the registry default and a recipe may legitimately deploy
// elsewhere, so demanding it of an unambiguous install would fail a check that
// has nothing ambiguous about it.
//
// Ambiguity is not always a namespace conflict, which is why the message
// counts installs and namespaces separately and lists each namespace once. Two
// Argo CD Applications under different name prefixes point at one destination
// namespace, so that shape is two installs in one namespace; repeating the
// namespace per record, as this once did, asserted a conflict that does not
// exist and hid the names that are the actual discriminator.
func resolveInstall(c Component, records []installedRelease) (installedRelease, error) {
	if len(records) == 1 {
		return records[0], nil
	}

	var expected []installedRelease
	names := make([]string, 0, len(records))
	seen := make(map[string]struct{}, len(records))
	namespaces := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
		if _, duplicate := seen[record.Namespace]; !duplicate {
			seen[record.Namespace] = struct{}{}
			namespaces = append(namespaces, record.Namespace)
		}
		if record.Namespace == c.Namespace {
			expected = append(expected, record)
		}
	}
	if len(expected) == 1 {
		return expected[0], nil
	}
	sort.Strings(names)
	sort.Strings(namespaces)

	// len(records) is always above one here, so the plural never has to be
	// conditioned; the namespaces are listed rather than counted for the same
	// reason in reverse.
	return installedRelease{}, errors.NewWithContext(errors.ErrCodeConflict,
		fmt.Sprintf("component %q resolves to %d installs (%s) in namespaces (%s), and %d of them are in the %q "+
			"the registry declares, so which install to compare cannot be decided here",
			c.Name, len(records), strings.Join(names, ", "), strings.Join(namespaces, ", "),
			len(expected), c.Namespace),
		map[string]any{"component": c.Name, ctxKeyNamespace: c.Namespace})
}

// attributeRecord reports which component a record belongs to under d, as an
// index into comps, and how its name reads. It returns -1 for no match.
//
// The best match wins on the tier first and on the length of the matched
// component-side string second. Length matters only for the Argo rule, whose
// suffix match is not exclusive: a record ending in gpu-operator also ends in
// operator, and the longer name is the more specific reading.
func attributeRecord(d Deployer, comps []Component, record installedRelease) (int, matchKind) {
	best, bestKind, bestLen := -1, matchNone, -1
	for i := range comps {
		kind, matched := matchComponent(d, &comps[i], record)
		if kind == matchNone {
			continue
		}
		if kind < bestKind || (kind == bestKind && len(matched) <= bestLen) {
			continue
		}
		best, bestKind, bestLen = i, kind, len(matched)
	}

	return best, bestKind
}

// matchComponent reports how record's name reads as component c under d, and
// the component-side string that matched.
//
// Each deployer's rule is the inverse of what that deployer writes. helm and
// helmfile install under the component's own name
// (localformat's install-local-helm.sh). Flux writes a HelmRelease with a name
// and a targetNamespace and never a releaseName, so helm-controller composes
// the release name from the two. Argo CD prepends a user-settable namePrefix
// to every child Application, which cannot be enumerated and need not end in a
// separator, so the match is a raw suffix — anchored by requiring the
// Application's destination namespace to be the component's, which is what
// keeps an unrelated workload whose name happens to end in a component's from
// being reported as that component.
func matchComponent(d Deployer, c *Component, record installedRelease) (matchKind, string) {
	switch d {
	case DeployerHelm, DeployerHelmfile:
		return matchName(record.Name, c.Name, false)
	case DeployerFlux:
		return matchName(record.Name, fluxReleaseName(c), false)
	case DeployerArgoCD, DeployerArgoCDHelm:
		if record.Namespace != c.Namespace {
			return matchNone, ""
		}

		return matchName(record.Name, c.Name, true)
	default:
		return matchNone, ""
	}
}

// fluxReleaseName is the release name helm-controller composes for a
// HelmRelease the flux deployer wrote. The deployer always sets
// spec.targetNamespace and never spec.releaseName, so this reproduces
// HelmRelease.GetReleaseName: "<targetNamespace>-<name>", falling back to the
// bare name when no target namespace is set.
func fluxReleaseName(c *Component) string {
	if c.Namespace == "" {
		return c.Name
	}

	return c.Namespace + nameSeparator + c.Name
}

// matchName reads a record name as want, or as one of want's injected folders.
// want is tested before any phase so a component whose own name ends in a
// phase is not stripped down to a component that does not exist.
func matchName(recordName, want string, suffix bool) (matchKind, string) {
	if want == "" {
		return matchNone, ""
	}
	if nameIs(recordName, want, suffix) {
		return matchPrimary, want
	}
	for _, phase := range injectedFolderPhases {
		if nameIs(recordName, want+nameSeparator+phase, suffix) {
			return matchInjected, want
		}
	}

	return matchNone, ""
}

func nameIs(recordName, want string, suffix bool) bool {
	if suffix {
		return strings.HasSuffix(recordName, want)
	}

	return recordName == want
}

// versionFor is the installed version of a component, per ADR-021 Decision 5
// plus the registry guard on the fallback.
//
// The guard is what makes a bundle generated before the stamp existed report
// nothing rather than reporting its wrapper's version as the payload's. A
// manifest-only or Kustomize component has no upstream chart, so the chart
// version of whatever carries it is by construction a wrapper version.
//
// The branch is on Source and not on the annotations being absent: an Argo
// Application carries none at all, which is a different fact from a chart
// whose metadata declared none, and only the latter licenses the fallback. A
// stamp present but empty answers empty rather than falling through, because
// falling through would report the wrapper version the stamp exists to
// replace.
func versionFor(record installedRelease, c Component) string {
	if record.Source == sourceHelm {
		if version, ok := record.Annotations[header.AnnotationComponentVersion]; ok {
			if stampsWrapperVersion(record, c) {
				return ""
			}

			return version
		}
	}
	if c.HasUpstreamChart {
		return record.ChartVersion
	}

	return ""
}

// stampsWrapperVersion reports a component-version stamp that carries the
// wrapper's own version rather than the payload's.
//
// The wrapper writer has no payload version to stamp for a component that pins
// neither a chart version nor a Kustomize tag, and stamps the AICR build
// version in its place — into generated-by as well, which is what makes the
// fallback detectable. Reading it as a payload version reports an unchanged
// component as changed, because the recipe side answers nothing at all for the
// same component.
//
// Equality alone would also catch a component genuinely pinned to whatever
// string AICR is released at. An upstream chart pin is what rules that out: it
// is precisely what the fallback lacks, so a component that has one is never
// the case this is looking for.
func stampsWrapperVersion(record installedRelease, c Component) bool {
	if c.HasUpstreamChart {
		return false
	}
	generatedBy, ok := record.Annotations[header.AnnotationGeneratedBy]

	return ok && generatedBy == record.Annotations[header.AnnotationComponentVersion]
}

// stamped reports a record AICR wrote. Either annotation counts: a wrapper
// carries both, and a record with one and not the other is still this
// project's output read through a mapping that no longer recognizes it.
func stamped(record installedRelease) bool {
	if record.Source != sourceHelm {
		return false
	}
	if _, ok := record.Annotations[header.AnnotationComponentVersion]; ok {
		return true
	}
	_, ok := record.Annotations[header.AnnotationGeneratedBy]

	return ok
}
