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
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler"
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

// UpgradeCheck compares two artifacts against the ADR-021 transition records
// this binary's registry references, and returns the report.
//
// The recipe and bundle input forms share this one code path: a bundle is read
// through the recipe.yaml it embeds, never by fingerprinting deployer-specific
// layout files. Only helm bundles embed one today, so the bundle form reaches
// no further than that until NVIDIA/aicr#2753 lands; the other four deployers
// fail with an explicit error rather than being misread.
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
//
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

	results := upgrade.Match(set, componentVersions(from), componentVersions(to))
	if req.Deployer == "" && upgrade.RequiresDeployer(results) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"a deployer is required: at least one component needs operator steps, and steps differ per deployer. "+
				"Set --deployer (SDK: UpgradeCheckRequest.Deployer) to one of: "+
				strings.Join(config.GetDeployerTypes(), ", "))
	}

	return upgrade.NewReport(results, upgrade.ReportOptions{
		From:     req.From,
		To:       req.To,
		Deployer: req.Deployer,
	}), nil
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

// componentVersions projects a resolved recipe onto the component-to-version
// table the matcher compares. Unversioned components are carried rather than
// dropped: the matcher classifies them as unversioned, which is a reportable
// blind spot, while dropping them would read as a component removal.
func componentVersions(r *RecipeResult) map[string]string {
	if r == nil {
		return nil
	}
	table := make(map[string]string, len(r.Components))
	for _, c := range r.Components {
		table[c.Name] = c.Version
	}
	return table
}
