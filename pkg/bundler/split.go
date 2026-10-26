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

package bundler

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/bundleinfo"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/helm"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/canonical"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// SplitFileName is the audit file at the root of a class-partitioned bundle
// set (ADR-018). It records which resolved recipe and which partition
// produced the set; it is never a trust root and deployment never reads it.
const SplitFileName = "split.yaml"

// splitStagePrefix names the private staging directory a bundle set renders
// into before its one atomic move to the destination. It is a sibling of the
// destination so the move stays on one filesystem.
const splitStagePrefix = ".aicr-split-"

// BundleSplit is the schema of split.yaml.
//
// RecipeDigest identifies the resolved recipe, computed over the set-root
// recipe.yaml with the canonicalization recipe evidence uses, so it matches
// the subject of current recipe evidence. Together with Components it
// identifies the resolved recipe and its partition, not the rendered payload:
// bundle-time inputs (--set, typed overrides, scheduling settings) land in
// the extracted component values, not in recipe.yaml, so two invocations can
// share a RecipeDigest and still render different class directories. The
// rendered payload is identified by the root checksums.txt, which remains the
// attestation subject.
type BundleSplit struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`

	// RecipeDigest is "sha256:<hex>" of the set-root recipe.yaml under
	// evidence/canonical.SubjectDigestV3 (the digest recipe evidence uses).
	RecipeDigest string `json:"recipeDigest" yaml:"recipeDigest"`

	// ClassOrder lists the classes the set contains, in install order.
	// Teardown is the reverse.
	ClassOrder []string `json:"classOrder" yaml:"classOrder"`

	// Components maps every component in the set to its class.
	Components map[string]string `json:"components" yaml:"components"`
}

// splitHeader precedes the YAML document in split.yaml.
const splitHeader = `# AICR bundle set (ADR-018): audit metadata only.
#
# recipeDigest identifies the resolved recipe (recipe.yaml in this directory)
# and components records the class of each; together they identify the recipe
# and its partition, not the rendered payload. Bundle-time inputs such as --set
# do not change recipeDigest. The payload is identified by checksums.txt, which
# the attestation signs. Verify the set at this root and deploy the classes in
# classOrder; tear them down in reverse. Nothing reads this file at deploy time.
`

// classPlan is one class directory to render: the enabled components that
// belong to the class, in recipe order, with the induced deployment order.
type classPlan struct {
	class recipe.ComponentClass
	refs  []recipe.ComponentRef
	order []string
}

// splitPartition is a validated partition of the enabled components.
type splitPartition struct {
	// classes holds only classes with at least one enabled component, in
	// install order.
	classes []classPlan
	// classOf maps every enabled component to its class.
	classOf map[string]recipe.ComponentClass
}

func (p *splitPartition) classNames() []string {
	names := make([]string, 0, len(p.classes))
	for _, c := range p.classes {
		names = append(names, string(c.class))
	}
	return names
}

// accountingCohesionSet is the one v1 cohesion set: the AICR-provided Slurm
// accounting components (ADR-016) must land in one class.
var accountingCohesionSet = []string{
	"slinky-slurm", "mariadb-operator-crds", "mariadb-operator", "slurm-accounting-mariadb",
}

// planSplit validates the partition of the enabled components (ADR-018,
// Partition validation) and returns the per-class rendering plan. declared is
// the component set before recipe disables and bundle-time overrides were
// applied; enabled is what the bundle renders.
func planSplit(declared []recipe.ComponentRef, enabled *recipe.RecipeResult) (*splitPartition, error) {
	if enabled.Criteria != nil && enabled.Criteria.Service == recipe.CriteriaServiceOCP {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"--split does not support OCP recipes in this release (ADR-018)")
	}
	registry, err := recipe.GetComponentRegistryFor(enabled.DataProvider())
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load the component registry for --split")
	}

	classOf := make(map[string]recipe.ComponentClass, len(enabled.ComponentRefs))
	rankOf := make(map[string]int, len(enabled.ComponentRefs))
	for _, ref := range enabled.ComponentRefs {
		entry := registry.Get(ref.Name)
		if entry == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"--split requires every component to have a registry entry that places it in a class; "+
					"%q has none. Register it, for example through a --data registry entry that declares its class",
				ref.Name))
		}
		class := entry.GetClass()
		rank, ok := class.Rank()
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q has unknown class %q; known classes are %v", ref.Name, class, recipe.ComponentClasses))
		}
		classOf[ref.Name] = class
		rankOf[ref.Name] = rank
	}

	// Direction: a component may depend only on its own class or an earlier
	// one. A disabled or absent dependency is satisfied outside the recipe.
	for _, ref := range enabled.ComponentRefs {
		for _, depName := range ref.DependencyRefs {
			dep := enabled.GetComponentRef(depName)
			if dep == nil || !dep.IsEnabled() {
				continue
			}
			if rankOf[depName] > rankOf[ref.Name] {
				return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"--split rejects the dependency %s (%s) → %s (%s): a component may depend only on its own class or an earlier one (ADR-018)",
					ref.Name, classOf[ref.Name], depName, classOf[depName]))
			}
		}
	}

	// Cohesion: the accounting set travels together.
	var accountingClasses []string
	for _, name := range accountingCohesionSet {
		if class, ok := classOf[name]; ok && !slices.Contains(accountingClasses, string(class)) {
			accountingClasses = append(accountingClasses, string(class))
		}
	}
	if len(accountingClasses) > 1 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"--split rejects a partition that separates the Slurm accounting set %v across classes %v (ADR-016)",
			accountingCohesionSet, accountingClasses))
	}

	// All-disabled: a class the recipe declares but whose every component is
	// disabled is rejected; a class with no declared component is skipped.
	declaredByClass := make(map[recipe.ComponentClass]int)
	for _, ref := range declared {
		if entry := registry.Get(ref.Name); entry != nil {
			declaredByClass[entry.GetClass()]++
		}
	}
	enabledByClass := make(map[recipe.ComponentClass]int)
	for _, class := range classOf {
		enabledByClass[class]++
	}
	for _, class := range recipe.ComponentClasses {
		if declaredByClass[class] > 0 && enabledByClass[class] == 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"--split rejects an empty class: the recipe declares %d %s component(s) and all are disabled; "+
					"a split set renders every declared class", declaredByClass[class], class))
		}
	}

	partition := &splitPartition{classOf: classOf}
	for _, class := range recipe.ComponentClasses {
		if enabledByClass[class] == 0 {
			continue
		}
		plan := classPlan{class: class}
		for _, ref := range enabled.ComponentRefs {
			if classOf[ref.Name] == class {
				plan.refs = append(plan.refs, ref)
			}
		}
		for _, name := range enabled.DeploymentOrder {
			if classOf[name] == class {
				plan.order = append(plan.order, name)
			}
		}
		partition.classes = append(partition.classes, plan)
	}
	return partition, nil
}

// validateSplitHelmIdentities checks the Helm identities the whole set emits,
// before any class is rendered: per-class rendering would miss a collision
// between one class's injected -pre/-post/-readiness release and another
// class's component. Identities are (namespace, release) pairs, since a
// namespace may span classes.
func (b *DefaultBundler) validateSplitHelmIdentities(ctx context.Context, enabled *recipe.RecipeResult, componentValues map[string]map[string]any) error {
	rawPre, err := b.collectComponentPreManifests(ctx, enabled)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to collect component pre-manifests")
	}
	// Count only the -pre releases the writer emits: a pre-manifest that
	// renders no object injects no folder.
	lfComponents := make([]localformat.Component, 0, len(enabled.ComponentRefs))
	for _, ref := range enabled.ComponentRefs {
		lfComponents = append(lfComponents, localformat.Component{
			Name: ref.Name, Namespace: ref.Namespace, ChartName: ref.EffectiveChart(),
			Version: ref.Version, Values: componentValues[ref.Name],
		})
	}
	pre, err := localformat.FilterEmptyPreManifests(ctx, lfComponents, rawPre)
	if err != nil {
		return err
	}
	post, err := b.collectComponentManifests(ctx, enabled)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to collect component post-manifests")
	}
	readiness, err := b.collectComponentReadiness(ctx, enabled)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to collect component readiness gates")
	}

	type identity struct{ namespace, release string }
	owner := make(map[identity]string, len(enabled.ComponentRefs)*2)
	claim := func(ns, release, component string) error {
		id := identity{ns, release}
		if prev, taken := owner[id]; taken {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"--split rejects colliding Helm release identities: %q and %q both emit release %q in namespace %q",
				prev, component, release, ns))
		}
		owner[id] = component
		return nil
	}
	for _, ref := range enabled.ComponentRefs {
		if strings.HasSuffix(ref.Name, "-readiness") {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"component %q uses the reserved %q suffix", ref.Name, "-readiness"))
		}
		if err := claim(ref.Namespace, ref.Name, ref.Name); err != nil {
			return err
		}
		if len(pre[ref.Name]) > 0 {
			if err := claim(ref.Namespace, ref.Name+"-pre", ref.Name); err != nil {
				return err
			}
		}
		if ref.Source != "" && len(post[ref.Name]) > 0 {
			if err := claim(ref.Namespace, ref.Name+"-post", ref.Name); err != nil {
				return err
			}
		}
		if len(readiness[ref.Name]) > 0 {
			if err := claim(ref.Namespace, ref.Name+"-readiness", ref.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitContextFor describes one class directory for the helm deployer: the
// set's class order and what this class shares with an earlier class.
func splitContextFor(p *splitPartition, plan classPlan) *helm.SplitContext {
	ctxt := &helm.SplitContext{Class: string(plan.class), ClassOrder: p.classNames()}
	rank, _ := plan.class.Rank()

	earlierNamespaces := map[string]struct{}{}
	for _, earlier := range p.classes {
		if r, _ := earlier.class.Rank(); r >= rank {
			continue
		}
		for _, ref := range earlier.refs {
			earlierNamespaces[ref.Namespace] = struct{}{}
			for _, group := range helm.CRDGroupsInstalledBy(ref.Name) {
				if !slices.Contains(ctxt.SharedCRDGroups, group) {
					ctxt.SharedCRDGroups = append(ctxt.SharedCRDGroups, group)
				}
			}
		}
	}
	for _, ref := range plan.refs {
		if _, shared := earlierNamespaces[ref.Namespace]; shared && !slices.Contains(ctxt.SharedNamespaces, ref.Namespace) {
			ctxt.SharedNamespaces = append(ctxt.SharedNamespaces, ref.Namespace)
		}
	}
	slices.Sort(ctxt.SharedNamespaces)
	slices.Sort(ctxt.SharedCRDGroups)
	return ctxt
}

// prepareSplitDestination enforces the create-only publication contract: an
// explicit destination whose exact path is absent, under a parent that
// exists. It returns the absolute destination and its parent.
func prepareSplitDestination(dir string) (string, string, error) {
	if dir == "" || dir == "." {
		return "", "", errors.New(errors.ErrCodeInvalidRequest,
			"--split requires an explicit output path for the bundle set (the destination must not exist yet)")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", errors.Wrap(errors.ErrCodeInvalidRequest, "invalid output path", err)
	}
	abs = filepath.Clean(abs)
	if _, statErr := os.Lstat(abs); statErr == nil {
		return "", "", errors.New(errors.ErrCodeConflict, fmt.Sprintf(
			"--split publishes a new bundle set and does not replace an existing path: %s already exists", abs))
	} else if !stderrors.Is(statErr, os.ErrNotExist) {
		return "", "", errors.Wrap(errors.ErrCodeInternal, "failed to inspect the output path", statErr)
	}
	parent := filepath.Dir(abs)
	info, err := os.Stat(parent)
	if err != nil {
		return "", "", errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("the parent of the bundle set destination must exist: %s", parent), err)
	}
	if !info.IsDir() {
		return "", "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("the parent of the bundle set destination is not a directory: %s", parent))
	}
	return abs, parent, nil
}

// makeSplit renders a class-partitioned bundle set (ADR-018). It runs after
// every validation gate of Make, so the complete component set has already
// been validated once; here the partition is checked, each class is rendered
// into a private staging sibling of the destination, the set-root files and
// checksums are written and verified, and the set is published with one
// atomic rename to the absent destination. Any failure before that rename
// leaves the destination absent and removes the staging directory.
//
//nolint:funlen // one linear render-then-publish sequence; splitting it would scatter the cleanup contract.
func (b *DefaultBundler) makeSplit(
	ctx context.Context,
	declared []recipe.ComponentRef,
	enabled *recipe.RecipeResult,
	componentValues map[string]map[string]any,
	dir string,
	upgradeNotes []upgrade.BundleNote,
	start time.Time,
) (retOut *result.Output, retErr error) {

	dest, parent, err := prepareSplitDestination(dir)
	if err != nil {
		return nil, err
	}
	partition, err := planSplit(declared, enabled)
	if err != nil {
		return nil, err
	}
	if identityErr := b.validateSplitHelmIdentities(ctx, enabled, componentValues); identityErr != nil {
		return nil, identityErr
	}

	stage, err := os.MkdirTemp(parent, splitStagePrefix+filepath.Base(dest)+"-")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create the bundle set staging directory", err)
	}
	published := false
	defer func() {
		if published {
			return
		}
		if rmErr := os.RemoveAll(stage); rmErr != nil {
			retErr = stderrors.Join(retErr, errors.Wrap(errors.ErrCodeInternal,
				"failed to remove the bundle set staging directory", rmErr))
		}
	}()
	// MkdirTemp creates 0700; the published root gets the mode an ordinary
	// bundle directory has, so another owner can traverse the set.
	if chmodErr := os.Chmod(stage, 0o755); chmodErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to set the bundle set staging directory mode", chmodErr)
	}
	if rootErr := checksum.ValidateOutputRoot(ctx, stage); rootErr != nil {
		return nil, rootErr
	}

	dataFiles, err := b.copyDataFiles(stage, enabled.DataProvider())
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to copy external data files")
	}

	// Class READMEs sit one level below the set root, where the upgrade
	// guide is written, so their notice links ../UPGRADING.md.
	classNotice := ""
	if len(upgradeNotes) > 0 {
		var sb strings.Builder
		if noticeErr := upgrade.WriteNoticeAt(&sb, upgradeNotes, string(b.Config.Deployer()), "../"+upgrade.GuideFile); noticeErr != nil {
			return nil, noticeErr
		}
		classNotice = sb.String()
	}

	output := &deployer.Output{Files: make([]string, 0)}
	// infoOut feeds the set-root bundle-info.yaml: the first class's deploy.sh
	// is the entrypoint and every release path is prefixed with its class
	// directory, so readers that locate values through bundle-info.yaml
	// (upgrade-check, recipe --inherit-from) work on a set like on a bundle.
	infoOut := &deployer.Output{}
	var steps, notes []string
	for _, plan := range partition.classes {
		subset := *enabled
		subset.ComponentRefs = plan.refs
		subset.DeploymentOrder = plan.order
		subsetValues := make(map[string]map[string]any, len(plan.refs))
		for _, ref := range plan.refs {
			if v, ok := componentValues[ref.Name]; ok {
				subsetValues[ref.Name] = v
			}
		}
		d, buildErr := b.buildDeployer(ctx, &subset, subsetValues, nil, classNotice)
		if buildErr != nil {
			return nil, buildErr
		}
		gen, ok := d.(*helm.Generator)
		if !ok {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("--split rendered an unexpected deployer %T; only the helm deployer is supported", d))
		}
		gen.Split = splitContextFor(partition, plan)

		classDir := filepath.Join(stage, string(plan.class))
		classOut, genErr := gen.Generate(ctx, classDir)
		if genErr != nil {
			return nil, errors.PropagateOrWrap(genErr, errors.ErrCodeInternal,
				fmt.Sprintf("failed to generate the %s class", plan.class))
		}
		output.Files = append(output.Files, classOut.Files...)
		output.TotalSize += classOut.TotalSize
		if infoOut.Entrypoint == "" {
			infoOut.Entrypoint = filepath.ToSlash(filepath.Join(string(plan.class), classOut.Entrypoint))
		}
		for _, r := range classOut.Releases {
			r.Path = filepath.ToSlash(filepath.Join(string(plan.class), r.Path))
			if r.Manifest != "" {
				r.Manifest = filepath.ToSlash(filepath.Join(string(plan.class), r.Manifest))
			}
			infoOut.Releases = append(infoOut.Releases, r)
		}
		steps = append(steps,
			fmt.Sprintf("cd %s", filepath.Join(dest, string(plan.class))),
			"chmod +x deploy.sh",
			"./deploy.sh")
		for _, note := range classOut.DeploymentNotes {
			notes = append(notes, fmt.Sprintf("%s: %s", plan.class, note))
		}
	}

	if len(upgradeNotes) > 0 {
		guidePath, guideSize, guideErr := b.writeUpgradeGuide(stage, upgradeNotes)
		if guideErr != nil {
			return nil, guideErr
		}
		output.Files = append(output.Files, guidePath)
		output.TotalSize += guideSize
	}
	recipeSize, err := b.writeRecipeFile(enabled, stage)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to write recipe file")
	}
	recipePath := filepath.Join(stage, RecipeFileName)
	output.Files = append(output.Files, recipePath)
	output.TotalSize += recipeSize

	splitSize, err := writeSplitFile(stage, recipePath, partition)
	if err != nil {
		return nil, err
	}
	output.Files = append(output.Files, filepath.Join(stage, SplitFileName))
	output.TotalSize += splitSize

	rawDigest, digestErr := checksum.SHA256RawContext(ctx, recipePath)
	if digestErr != nil {
		return nil, errors.PropagateOrWrap(digestErr, errors.ErrCodeInternal, "failed to digest recipe for bundle info")
	}
	info := b.buildBundleInfo(enabled, infoOut, fmt.Sprintf("sha256:%x", rawDigest))
	infoSize, infoErr := bundleinfo.Write(ctx, stage, info)
	if infoErr != nil {
		return nil, errors.PropagateOrWrap(infoErr, errors.ErrCodeInternal, "failed to write bundle info")
	}
	output.Files = append(output.Files, filepath.Join(stage, bundleinfo.FileName))
	output.TotalSize += infoSize

	if addErr := output.AddDataFiles(stage, dataFiles); addErr != nil {
		return nil, addErr
	}
	if sumErr := checksum.WriteChecksums(ctx, stage, output); sumErr != nil {
		return nil, errors.PropagateOrWrap(sumErr, errors.ErrCodeInternal, "failed to finalize bundle set checksums")
	}
	if _, attestErr := b.attestBundleAt(ctx, stage, dest, dataFiles, enabled); attestErr != nil {
		return nil, attestErr
	}
	verifyOpts := checksum.InventoryOptions{AllowedMetadataPaths: attestation.BundleMetadataPaths()}
	_, inventory, _, err := checksum.ReadAndVerifyBundle(ctx, stage, verifyOpts)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed final bundle set inventory verification")
	}

	// Publish with one no-replace rename: the destination stays absent until
	// this syscall, and anything created there meanwhile, even an empty
	// directory, makes it fail. The stage is then removed and that content is
	// left untouched, so concurrent attempts have at most one winner.
	if renameErr := renameNoReplace(stage, dest); renameErr != nil {
		if destinationTaken(renameErr) {
			return nil, errors.Wrap(errors.ErrCodeConflict,
				fmt.Sprintf("failed to publish the bundle set to %s (the destination must be absent)", dest), renameErr)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to publish the bundle set to %s", dest), renameErr)
	}
	published = true

	files := make([]string, 0, len(inventory.RelativeFiles()))
	for _, rel := range inventory.RelativeFiles() {
		files = append(files, filepath.Join(dest, filepath.FromSlash(rel)))
	}
	resultType, deploymentType := deployerResultNames(b.Config.Deployer(), false)
	out := &result.Output{
		Results: []*result.Result{{
			Type:     resultType,
			Success:  true,
			Files:    files,
			Size:     inventory.TotalSize(),
			Duration: time.Since(start),
		}},
		Errors:        make([]result.BundleError, 0),
		TotalDuration: time.Since(start),
		TotalSize:     inventory.TotalSize(),
		TotalFiles:    len(files),
		OutputDir:     dest,
	}
	allNotes := upgrade.NoteLines(upgradeNotes)
	allNotes = append(allNotes, fmt.Sprintf(
		"Bundle set split by class (ADR-018): install %s; tear down in reverse. Verify the set at %s before deploying a class.",
		strings.Join(partition.classNames(), ", then "), dest))
	allNotes = append(allNotes, notes...)
	allNotes = append(allNotes, b.warnings...)
	out.Deployment = &result.DeploymentInfo{
		Type:  deploymentType + " set, split by class",
		Steps: steps,
		Notes: allNotes,
	}

	slog.Debug("bundle set generation complete",
		"classes", partition.classNames(),
		"files", len(files),
		"size_bytes", inventory.TotalSize(),
		"output_dir", dest,
	)
	return out, nil
}

// writeSplitFile writes split.yaml next to the set-root recipe.yaml.
func writeSplitFile(stage, recipePath string, p *splitPartition) (int64, error) {
	recipeData, err := readBoundedFile(recipePath, defaults.MaxRecipePOSTBytes)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to read the set-root recipe for its digest")
	}
	digest, err := canonical.SubjectDigestV3(recipeData)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to digest the set-root recipe")
	}
	doc := BundleSplit{
		APIVersion:   header.StableGroupVersion,
		Kind:         string(header.KindBundleSplit),
		RecipeDigest: "sha256:" + digest,
		ClassOrder:   p.classNames(),
		Components:   make(map[string]string, len(p.classOf)),
	}
	for name, class := range p.classOf {
		doc.Components[name] = string(class)
	}
	body, err := serializer.MarshalYAMLDeterministic(doc)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to serialize split.yaml")
	}
	data := append([]byte(splitHeader), body...)
	path, err := deployer.SafeJoin(stage, SplitFileName)
	if err != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "unsafe split.yaml path", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil { //nolint:gosec // path validated by SafeJoin
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to write split.yaml", err)
	}
	return int64(len(data)), nil
}

// destinationTaken reports whether a publish rename failed because something
// already occupies the destination, the one failure that is a conflict rather
// than an internal error.
func destinationTaken(err error) bool {
	return stderrors.Is(err, os.ErrExist) ||
		stderrors.Is(err, syscall.EEXIST) ||
		stderrors.Is(err, syscall.ENOTEMPTY) ||
		stderrors.Is(err, syscall.EISDIR)
}

// renameCheckThenMove is the two-step fallback for filesystems without a
// no-replace rename: it refuses an existing destination, then renames. The
// check and the move are separate syscalls, so an empty directory created
// between them could be replaced; it runs only where the single-syscall form
// is unavailable.
func renameCheckThenMove(oldpath, newpath string) error {
	if _, err := os.Lstat(newpath); err == nil {
		return os.ErrExist
	} else if !stderrors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(oldpath, newpath)
}
