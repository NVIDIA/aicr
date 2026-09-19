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

package aicr

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/inventory"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// FromCluster is the UpgradeCheckRequest.From value that reads the `from`
// table off a live cluster rather than an artifact. It is a third kind of
// source beside a file path and a cm:// URI, not a mode flag: it answers the
// same question the artifact forms do, from the one place that knows what is
// actually installed.
const FromCluster = "cluster"

// UpgradeCheckRequest names the two sides of an upgrade check and the deployer
// its steps are scoped to.
type UpgradeCheckRequest struct {
	// From is the source artifact: a recipe file, a cm:// ConfigMap URI, or a
	// bundle directory (recognized by the recipe.yaml at its root).
	// FromCluster reads the installed inventory off the cluster instead.
	From string

	// To is the target artifact, in the same forms as From except
	// FromCluster. Empty re-resolves From's own criteria against this binary's
	// registry, which answers "am I behind, and does catching up hurt?" rather
	// than "is this move safe?", and is rejected when From is the cluster,
	// which carries no criteria to re-resolve.
	To string

	// Deployer scopes the rendered steps. Required whenever any result carries
	// a manual or blocked verdict; see upgrade.RequiresDeployer for why it
	// cannot be inferred. A cluster read requires it unconditionally and
	// earlier, because the release names it maps encode the deployer.
	Deployer string

	// Kubeconfig is the cluster both the cm:// artifact form and the cluster
	// read resolve through, and is resolved once for every client either
	// builds: a second authentication path could land on a different context,
	// and a report assembled from two clusters is a confident wrong answer.
	Kubeconfig string

	// ScanAtRisk additionally reports the objects the crossed transition
	// records name that carry no deployer ownership marker.
	//
	// It is an axis of its own rather than a consequence of From, per ADR-021
	// Decision 5: the scan needs a cluster wherever the `from` table came
	// from, so comparing two bundles while scanning a live cluster is a
	// legitimate combination. A FromCluster run implies it.
	//
	// Three-valued because of that implication, which a bool cannot argue
	// with: nil leaves the implication in force, a pointer to true asks for
	// the scan whatever the source, and a pointer to false suppresses it even
	// for a cluster read. An explicit refusal wins, and the report then
	// accounts for the empty section with NotScannedDeclined rather than
	// reporting a cluster that was there as one never offered.
	ScanAtRisk *bool
}

// UpgradeReport is the report UpgradeCheck returns. It is a transparent alias
// of upgrade.Report rather than a restatement of it: the report is already a
// projection built for consumers, so copying it here would add a second shape
// to keep in step with the first for no gain.
type UpgradeReport = upgrade.Report

// WriteUpgradeReportTable writes a human-readable upgrade-check table.
//
// Re-exported so pkg/cli renders the report without importing pkg/upgrade,
// mirroring WriteSnapshotDiffTable. The rendering itself stays beside the
// report shape and its goldens.
func WriteUpgradeReportTable(w io.Writer, report *UpgradeReport) error {
	if w == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "upgrade report table writer is required (got nil)")
	}
	if report == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "upgrade report is required (got nil)")
	}
	return upgrade.WriteTable(w, report)
}

// UpgradeCheck compares two artifacts against the ADR-021 transition records
// this binary's registry references, and returns the report.
//
// The recipe and bundle input forms share this one code path: a bundle is read
// through the recipe.yaml it embeds, never by fingerprinting deployer-specific
// layout files. A directory without one is neither a recipe nor a bundle and is
// rejected rather than misread.
//
// Records are loaded and validated before matching. Both fail closed, because
// "a record exists and I could not read it" is not "no record exists", and a
// record the well-formedness rules would reject must not lend a component a
// verdict it cannot support.
//
// The operation adds no facade timeout of its own: the underlying LoadRecipe
// and record reads are each bounded, and the caller's context governs the whole.
//
// A FromCluster run reads the installed inventory instead of a `from`
// artifact. Records, matching and reporting are otherwise the same, with one
// asymmetry: the read states a version and no namespace, so the identity axis
// has nothing to compare and no relocation is reported against a cluster
// source. clusterIdentities says why the read cannot state one. A cluster that
// recognizes nothing is reported and never failed; the report's Source block
// is what separates an empty cluster from a kubeconfig on the wrong context.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client is nil or closed, ctx is nil, From
//     is empty, a bundle directory holds no recipe.yaml, To is omitted for an
//     artifact carrying no criteria or for a cluster read, a cluster read was
//     asked for without a Deployer, or a manual or blocked result needs a
//     Deployer that was not supplied.
//   - Loader, resolver, cluster-read and record errors propagate with their
//     own codes. The advisory at-risk scan is the one exception: its failure
//     is reported in the section it could not fill.
func (c *Client) UpgradeCheck(ctx context.Context, req UpgradeCheckRequest) (*UpgradeReport, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if req.From == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"a source artifact is required: set --from (SDK: UpgradeCheckRequest.From)")
	}

	// Validated here, not only in pkg/cli: an unrecognized name matches no
	// explicit step group, so it would silently collect the remainder group,
	// which was authored for the deployers nobody named. Rendering somebody
	// else's steps is the failure deployer-scoping exists to prevent, so an
	// unknown value is rejected rather than approximated.
	deployer := req.Deployer
	if deployer != "" {
		parsed, perr := config.ParseDeployerType(deployer)
		if perr != nil {
			return nil, perr
		}
		deployer = parsed.String()
	}

	fromCluster := req.From == FromCluster
	// Stricter than the RequiresDeployer rule below, and checked before any
	// I/O rather than after the verdicts are in: that rule asks whether steps
	// will be rendered, while this one is what makes the read possible at all.
	if fromCluster && deployer == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"a deployer is required to read the cluster: a release name encodes it (flux composes "+
				"<targetNamespace>-<name>, Argo CD prepends a user-settable prefix), so without one no "+
				"installed release maps to a component and the read could only report an empty cluster. "+
				"Set --deployer (SDK: UpgradeCheckRequest.Deployer) to one of: "+
				strings.Join(config.GetDeployerTypes(), ", "))
	}

	var from *RecipeResult
	if !fromCluster {
		fromPath, err := artifactRecipePath(req.From)
		if err != nil {
			return nil, err
		}
		from, err = c.LoadRecipe(ctx, fromPath, req.Kubeconfig)
		if err != nil {
			return nil, err
		}
	}

	to, err := c.upgradeCheckTarget(ctx, req, from)
	if err != nil {
		return nil, err
	}

	// Snapshot the per-Client provider under the read lock so a concurrent
	// Close can't race the read; Add to inflight under the lock so Close's
	// drain observes the increment. Same protocol as LoadRecipe. Every call
	// that takes the lock itself is already done above, so the increment
	// cannot outlive a Close waiting on it.
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	fromTable := componentIdentities(from)
	var source *upgrade.ReportSource
	if fromCluster {
		var versions map[string]string
		versions, source, err = c.clusterVersions(ctx, dp, req, deployer)
		if err != nil {
			return nil, err
		}
		fromTable = clusterIdentities(versions)
	}

	set, comps, err := recipe.LoadUpgradeRecords(ctx, dp)
	if err != nil {
		return nil, err
	}
	if err := set.Validate(comps); err != nil {
		return nil, err
	}

	results := upgrade.MatchIdentities(set, fromTable, componentIdentities(to))
	if deployer == "" && upgrade.RequiresDeployer(results) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"a deployer is required: at least one component needs operator steps, and steps differ per deployer. "+
				"Set --deployer (SDK: UpgradeCheckRequest.Deployer) to one of: "+
				strings.Join(config.GetDeployerTypes(), ", "))
	}

	// After the match and not before it: the scan looks only for the kinds the
	// crossed records name, and an upgrade that is not being made cannot put
	// anything at risk. Scanning the whole record set instead would list
	// objects no jump here goes near.
	scanRequested := req.ScanAtRisk != nil && *req.ScanAtRisk
	scanRefused := req.ScanAtRisk != nil && !*req.ScanAtRisk

	var atRisk *upgrade.AtRiskReport
	switch {
	case scanRefused:
		// Tested before the implication rather than after it: a refusal the
		// caller stated is the one thing FromCluster must not talk over. The
		// section is filled here instead of being left to NewReport's offline
		// default, which would report access that existed as access nobody had.
		atRisk = &upgrade.AtRiskReport{Reason: upgrade.NotScannedDeclined}
	case fromCluster || scanRequested:
		atRisk = c.upgradeCheckScan(ctx, req.Kubeconfig, upgrade.AffectedKinds(results))
	}

	return upgrade.NewReport(results, upgrade.ReportOptions{
		From:     req.From,
		To:       req.To,
		Deployer: deployer,
		Source:   source,
		AtRisk:   atRisk,
	}), nil
}

// clusterVersions reads the installed inventory as the `from` table, and
// accounts for what produced it.
//
// A read that recognizes nothing is not an error. Every row then reads
// "added", and the Source block this returns beside the table is the only
// thing separating a bare cluster from a mapping that no longer matches what
// AICR installed.
func (c *Client) clusterVersions(ctx context.Context, dp recipe.DataProvider, req UpgradeCheckRequest,
	deployer string) (map[string]string, *upgrade.ReportSource, error) {

	registry, err := recipe.GetComponentRegistryFor(dp)
	if err != nil {
		return nil, nil, err
	}
	result, err := c.deps.readInventory(ctx, inventory.Options{
		Kubeconfig: req.Kubeconfig,
		Deployer:   inventory.Deployer(deployer),
		Components: recipe.InventoryComponents(registry),
	})
	if err != nil {
		return nil, nil, err
	}

	return result.Versions,
		reportSourceFrom(result.Source, k8sclient.ResolveKubeconfigPath(req.Kubeconfig), len(result.Versions)),
		nil
}

// clusterIdentities is a cluster read restated as the matcher's `from` table.
//
// It states a version and no namespace, which is not an omission the read
// could fill. The read attributes a release to a component by name, and for
// flux and Argo CD that name is composed from the registry's namespace: a
// namespace is an input to the attribution rather than a fact recovered from
// it, and a release that moved out of the registry's namespace is not matched
// at all under those deployers. Only helm and helmfile match on the bare name,
// so a namespace stated here would come from two deployers of five and be
// silently absent under the rest, which is a worse report than none.
//
// upgrade.identityChanges treats an unstated field as a fact the artifact did
// not carry rather than a move to the default, so this reports no relocation
// instead of reporting a wrong one.
func clusterIdentities(versions map[string]string) map[string]upgrade.Identity {
	if versions == nil {
		return nil
	}
	table := make(map[string]upgrade.Identity, len(versions))
	for name, version := range versions {
		table[name] = upgrade.Identity{Version: version}
	}

	return table
}

// reportSourceFrom restates a read's account of itself for the report.
//
// The two readers keep their own shapes and their own field names, and their
// counts are never summed: the Helm side counts storage records, so one
// release with ten retained revisions contributes ten, while the Argo side
// counts Applications, of which a component has one.
//
// Matched is len(Versions) rather than a count of report rows, which cannot
// answer it: a component installed at the target version produces no row, so
// the rows do not distinguish "found, unchanged" from "never found".
//
// Context is left unset. The read recovers no single answer for it: a merged
// KUBECONFIG names several files and an in-cluster run names none. The
// renderer prints the gap as unknown rather than dropping the line.
func reportSourceFrom(info inventory.SourceInfo, kubeconfig string, matched int) *upgrade.ReportSource {
	return &upgrade.ReportSource{
		Kubeconfig: kubeconfig,
		Matched:    matched,
		Helm: upgrade.ReportSourceHelm{
			Records:          info.Helm.Records,
			Unattributed:     info.Helm.Unattributed,
			Unreadable:       info.Helm.Unreadable,
			Uninstalled:      info.Helm.Uninstalled,
			StampedUnmatched: info.Helm.StampedUnmatched,
		},
		Argo: upgrade.ReportSourceArgo{
			Applications: info.Argo.Applications,
			Unattributed: info.Argo.Unattributed,
			Unreadable:   info.Argo.Unreadable,
		},
	}
}

// upgradeCheckScan runs the advisory at-risk scan, and returns a report
// whatever happens.
//
// A scan failure, such as an RBAC gap on a CRD or an apiserver that went
// away, fills the section's reason instead of aborting the run. The findings already stay
// out of the exit code per ADR-021 Decision 3, and an advisory feature taking
// down the comparison it annotates is that same trade made backwards.
func (c *Client) upgradeCheckScan(ctx context.Context, kubeconfig string,
	kinds []upgrade.ResourceKind) *upgrade.AtRiskReport {

	result, err := c.deps.scanAtRisk(ctx, inventory.AtRiskOptions{
		Kubeconfig: kubeconfig,
		Kinds:      atRiskKinds(kinds),
	})
	if err != nil {
		slog.Warn("at-risk scan failed; the upgrade comparison is unaffected", "error", err)

		return &upgrade.AtRiskReport{Reason: "the scan failed: " + err.Error()}
	}

	return atRiskReportFrom(result)
}

// atRiskKinds hands the crossed records' kinds to the scanner.
//
// Components travels with each kind rather than being dropped as scan-time
// noise: the object's identity says what might be lost and this says whose
// upgrade would do it, which is the whole actionability of the warning.
func atRiskKinds(kinds []upgrade.ResourceKind) []inventory.ResourceKind {
	if len(kinds) == 0 {
		return nil
	}
	out := make([]inventory.ResourceKind, len(kinds))
	for i, k := range kinds {
		// The two shapes differ only in their tags, so the conversion is
		// exact. A field added to either stops compiling here, which is the
		// point: whether the scan should carry it is a decision, not a
		// default.
		out[i] = inventory.ResourceKind(k)
	}

	return out
}

// atRiskReportFrom restates a completed scan.
//
// Scanned is set because the scan ran and not because it found anything: a
// scan that examined every object and found none at risk and a run that
// contacted no cluster both leave Findings empty, and those are opposite
// facts. A scan that had no kind to look for is the former: the crossed
// records name no resources, so there is nothing an upgrade here could
// disturb.
func atRiskReportFrom(result inventory.AtRiskResult) *upgrade.AtRiskReport {
	out := &upgrade.AtRiskReport{Scanned: true}
	for _, kind := range result.Kinds {
		out.Kinds = append(out.Kinds, upgrade.AtRiskKind{
			Group:      kind.Group,
			Kind:       kind.Kind,
			Components: kind.Components,
			Present:    kind.Present,
			Examined:   kind.Examined,
		})
	}
	for _, finding := range result.Findings {
		out.Findings = append(out.Findings, upgrade.AtRiskFinding{
			Group:      finding.Group,
			Kind:       finding.Kind,
			Components: finding.Components,
			Namespace:  finding.Namespace,
			Name:       finding.Name,
		})
	}

	return out
}

// upgradeCheckTarget resolves the `to` side, synthesizing it from the source's
// own criteria when the caller named no target.
//
// The synthesized target carries the source's profile selection. Re-resolving
// without it would answer a different question: an unprofiled resolve of
// profiled criteria yields a different component set, and the check would
// report that difference as components added and removed.
func (c *Client) upgradeCheckTarget(
	ctx context.Context,
	req UpgradeCheckRequest,
	from *RecipeResult,
) (*RecipeResult, error) {

	if req.To != "" {
		toPath, err := artifactRecipePath(req.To)
		if err != nil {
			return nil, err
		}
		return c.LoadRecipe(ctx, toPath, req.Kubeconfig)
	}

	if req.From == FromCluster {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"the cluster's installed inventory is a state, not a query, so there is nothing to re-resolve "+
				"against this binary's registry; name a target with --to (SDK: UpgradeCheckRequest.To)")
	}

	internal := from.Resolved()
	if internal == nil || internal.Criteria == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s carries no criteria, so there is nothing to re-resolve against this binary's registry; "+
				"name a target with --to (SDK: UpgradeCheckRequest.To)", req.From))
	}
	if p := from.SelectedProfile; p != nil && p.Name != "" && p.Value != "" {
		return c.ResolveRecipeFromCriteriaWithProfile(ctx, WrapCriteria(internal.Criteria), p.Name+"="+p.Value)
	}
	return c.ResolveRecipeFromCriteria(ctx, WrapCriteria(internal.Criteria))
}

// artifactRecipePath maps an artifact reference to the recipe document to read.
// A directory holding a bundle's embedded recipe resolves to that file;
// everything else, including a cm:// URI, is already the document.
func artifactRecipePath(ref string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(ref), serializer.ConfigMapURIScheme) {
		return ref, nil
	}
	info, err := os.Stat(ref)
	if err != nil {
		// Not resolvable here is not necessarily absent: let LoadRecipe
		// report against the path the caller actually gave.
		return ref, nil //nolint:nilerr // the loader owns the real diagnosis
	}
	if !info.IsDir() {
		return ref, nil
	}
	embedded := filepath.Join(ref, bundler.RecipeFileName)
	if _, err := os.Stat(embedded); err != nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s is a directory with no %s in it, so it is neither a recipe nor a bundle",
			ref, bundler.RecipeFileName))
	}
	return embedded, nil
}

// inheritedRecipePath maps a RecipeRequest.InheritFrom reference to the recipe
// document to read, reusing artifactRecipePath's file and bundle-directory
// forms but not its cm:// pass-through.
//
// cm:// is rejected before delegating so the unsupported form fails closed
// here rather than inside a loader that would try to reach a cluster for it;
// upgrade-check's --from keeps accepting the scheme (#2830).
//
// A reference that resolves to nothing is rejected here too. artifactRecipePath
// leaves that to the loader, which reports ErrCodeNotFound; for inheritance the
// distinction matters, because a silently absent prior artifact would resolve
// as a first deploy and relocate exactly the components this is meant to pin.
func inheritedRecipePath(ref string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(ref), serializer.ConfigMapURIScheme) {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"inherit-from does not support cm:// locations yet; pass a recipe file or a bundle directory")
	}
	if _, err := os.Stat(ref); err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"inherit-from %s is not readable as a recipe file or a bundle directory", ref), err)
	}
	return artifactRecipePath(ref)
}

// componentIdentities projects a resolved recipe onto the component-to-identity
// table the matcher compares. Unversioned components are carried rather than
// dropped: the matcher classifies them as unversioned, which is a reportable
// blind spot, while dropping them would read as a component removal.
//
// A Kustomize component pins Tag where a Helm one pins Version, and records
// apply to both (pinnedVersionFor checks a record's ceiling against whichever
// the registry declares). Reading Version alone would compare "" to "" for
// every Kustomize component, and the matcher skips equal pins, so a tag move
// would vanish from the report rather than be carried as unversioned.
//
// Namespace rides along because a recipe is regenerated from scratch on every
// AICR upgrade: a moved registry default relocates the install, and Helm cannot
// move a release between namespaces, so applying the new recipe installs a
// second copy beside the running one. A version-only projection reports that as
// no change at all.
func componentIdentities(r *RecipeResult) map[string]upgrade.Identity {
	if r == nil {
		return nil
	}
	table := make(map[string]upgrade.Identity, len(r.Components))
	for _, c := range r.Components {
		version := c.Version
		if version == "" {
			version = c.Tag
		}
		table[c.Name] = upgrade.Identity{Version: version, Namespace: c.Namespace}
	}
	return table
}
