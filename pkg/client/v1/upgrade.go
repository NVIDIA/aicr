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
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// UpgradeCheckRequest names the two sides of an upgrade check and the deployer
// its steps are scoped to.
type UpgradeCheckRequest struct {
	// From is the source artifact: a recipe file, a cm:// ConfigMap URI, or a
	// bundle directory (recognized by the recipe.yaml at its root).
	From string

	// To is the target artifact, in the same forms as From. Empty re-resolves
	// From's own criteria against this binary's registry, which answers "am I
	// behind, and does catching up hurt?" rather than "is this move safe?".
	To string

	// Deployer scopes the rendered steps. Required whenever any result carries
	// a manual or blocked verdict; see upgrade.RequiresDeployer for why it
	// cannot be inferred.
	Deployer string

	// Kubeconfig is honored only for cm:// artifact paths.
	Kubeconfig string
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
// Errors:
//   - ErrCodeInvalidRequest when the Client is nil or closed, ctx is nil, From
//     is empty, a bundle directory holds no recipe.yaml, To is omitted for an
//     artifact carrying no criteria, or a manual or blocked result needs a
//     Deployer that was not supplied.
//   - Loader, resolver and record errors propagate with their own codes.
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

	fromPath, err := artifactRecipePath(req.From)
	if err != nil {
		return nil, err
	}
	from, err := c.LoadRecipe(ctx, fromPath, req.Kubeconfig)
	if err != nil {
		return nil, err
	}

	to, err := c.upgradeCheckTarget(ctx, req, from)
	if err != nil {
		return nil, err
	}

	// Snapshot the per-Client provider under the read lock so a concurrent
	// Close can't race the read; Add to inflight under the lock so Close's
	// drain observes the increment. Same protocol as LoadRecipe.
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	set, comps, err := recipe.LoadUpgradeRecords(ctx, dp)
	if err != nil {
		return nil, err
	}
	if err := set.Validate(comps); err != nil {
		return nil, err
	}

	fromNames, toNames, skipped, nameErr := c.objectNames(ctx, req, to)
	if nameErr != nil {
		return nil, nameErr
	}

	results := upgrade.MatchIdentities(set,
		componentIdentities(from, fromNames), componentIdentities(to, toNames))
	if req.Deployer == "" && upgrade.RequiresDeployer(results) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"a deployer is required: at least one component needs operator steps, and steps differ per deployer. "+
				"Set --deployer (SDK: UpgradeCheckRequest.Deployer) to one of: "+
				strings.Join(config.GetDeployerTypes(), ", "))
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

	return upgrade.NewReport(results, upgrade.ReportOptions{
		From:                req.From,
		To:                  req.To,
		Deployer:            deployer,
		ObjectNamesCompared: skipped == "",
		ObjectNamesSkipped:  skipped,
	}), nil
}

// objectNames resolves the object-name tables for both sides, or reports why
// it could not.
//
// The source side governs. A resolved recipe records valuesFile as a PATH, so
// reading a recipe file's merged values would read whichever data THIS binary
// ships rather than what the artifact deployed — for two recipe files that
// compares today's values against themselves, and the drop this axis exists to
// catch would be invisible. Only a bundle wrote the merged values down.
//
// When either side cannot supply them, both are discarded. A table on one side
// and nothing on the other would read as every component having just acquired
// the names it has always had.
//
// A target that is not a bundle is the one case that needs no bundle: a recipe
// file, or no --to at all, asks what would deploy NOW, and the current
// provider answers exactly that. A target that IS a bundle is read as one,
// and an unreadable bundle skips rather than falling back — resolving that
// artifact's values against this binary would answer a question nobody asked.
func (c *Client) objectNames(
	ctx context.Context, req UpgradeCheckRequest, to *RecipeResult,
) (fromNames, toNames map[string]map[string]string, skipped string, err error) {

	fromNames, reason, err := bundleObjectNames(ctx, req.From)
	if err != nil {
		return nil, nil, "", err
	}
	if reason != "" {
		return nil, nil, reason, nil
	}

	if _, targetIsBundle := bundleDirectory(req.To); targetIsBundle {
		toNames, reason, err = bundleObjectNames(ctx, req.To)
		if err != nil {
			return nil, nil, "", err
		}
		if reason != "" {
			return nil, nil, reason, nil
		}
		return fromNames, toNames, "", nil
	}

	toNames, err = resolvedObjectNames(ctx, to)
	if err != nil {
		// Withdraw the axis rather than the whole run. The target's values
		// come from THIS binary's data, and a recipe from another release can
		// name a values file this one does not ship — a component's values
		// file being renamed between releases is enough. Every other
		// unreadable-artifact path here degrades to a reason and still reports
		// the version axis; failing the command outright would make a check
		// that used to work stop working over an axis it never had.
		return nil, nil, fmt.Sprintf(
			"the target's values could not be resolved against this binary's data (%v), so the "+
				"object names it would deploy with are unknown", err), nil
	}
	return fromNames, toNames, "", nil
}

// bundleObjectNames reads the object names a bundle recorded, returning the
// reason it could not when ref is not a bundle that wrote them down.
func bundleObjectNames(ctx context.Context, ref string) (map[string]map[string]string, string, error) {
	dir, isBundle := bundleDirectory(ref)
	if !isBundle {
		return nil, "a recipe file records its values by reference, not by value, so it does not " +
			"state the object names it deployed with. Compare against the bundle directory instead", nil
	}

	values, err := bundleinfo.ReadReleaseValues(ctx, dir)
	if err != nil {
		// A bundle predating build-record stamping has no bundle-info.yaml to
		// locate its values through. That is a real state to report, not a
		// failure: the rest of the check is still worth running.
		if stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
			return nil, "the bundle at " + ref + " has no " + bundleinfo.FileName +
				", so the values it installed cannot be located. It was built before build-record " +
				"stamping; rebuild it with a current AICR to include this axis", nil
		}
		return nil, "", err
	}
	return projectObjectNames(values), "", nil
}

// resolvedObjectNames projects a resolved recipe's merged values, read through
// the provider the result is bound to.
func resolvedObjectNames(ctx context.Context, r *RecipeResult) (map[string]map[string]string, error) {
	// Empty rather than nil: a facade result carrying no internal recipe
	// states no object names, which is a table with no entries, not a failure
	// to build one.
	return internalObjectNames(ctx, r.Resolved())
}

// internalObjectNames resolves every ref's merged values and projects the
// object names out of them. Disabled refs are included: the caller keys into
// the result by name, and building two different tables for the two callers
// would be a way for them to disagree.
func internalObjectNames(
	ctx context.Context, internal *recipe.RecipeResult,
) (map[string]map[string]string, error) {

	if internal == nil {
		return map[string]map[string]string{}, nil
	}
	names := make(map[string]map[string]string, len(internal.ComponentRefs))
	for i := range internal.ComponentRefs {
		component := internal.ComponentRefs[i].Name
		merged, err := internal.GetValuesForComponentWithContext(ctx, component)
		if err != nil {
			// The adapter already returns structured errors with the right code.
			return nil, err
		}
		names[component] = recipe.ObjectNameValues(merged)
	}
	return names, nil
}

// projectObjectNames narrows merged values to the keys that name objects.
//
// A component that pins none keeps an entry holding an empty map, which is a
// different statement from having no entry at all: the first says the artifact
// deployed this component and it pinned nothing, the second that the artifact
// says nothing about it. Inheritance turns on exactly that difference — the
// first is a name the current values must not introduce, the second a first
// deploy.
func projectObjectNames(values map[string]map[string]any) map[string]map[string]string {
	names := make(map[string]map[string]string, len(values))
	for component, merged := range values {
		names[component] = recipe.ObjectNameValues(merged)
	}
	return names
}

// bundleDirectory reports whether ref names a bundle directory, which is the
// only artifact form carrying merged values. It mirrors artifactRecipePath's
// recognition rule: a directory holding the recipe the bundler embedded.
func bundleDirectory(ref string) (string, bool) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" || strings.HasPrefix(trimmed, serializer.ConfigMapURIScheme) {
		return "", false
	}
	info, err := os.Stat(ref)
	if err != nil || !info.IsDir() {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(ref, bundler.RecipeFileName)); err != nil {
		return "", false
	}
	return ref, true
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
// Object names ride along for the same reason, one step further out: they live
// in the merged values rather than on the ref, and they name the objects the
// chart owns. Dropping a component's fullnameOverride renames every one of
// them, which Helm applies as delete-and-recreate. objectNames supplies the
// table, or nil where the artifacts could not state it.
func componentIdentities(
	r *RecipeResult, objectNames map[string]map[string]string,
) map[string]upgrade.Identity {

	if r == nil {
		return nil
	}
	table := make(map[string]upgrade.Identity, len(r.Components))
	for _, c := range r.Components {
		version := c.Version
		if version == "" {
			version = c.Tag
		}
		table[c.Name] = upgrade.Identity{
			Version:     version,
			Namespace:   c.Namespace,
			ObjectNames: objectNames[c.Name],
		}
	}
	return table
}
