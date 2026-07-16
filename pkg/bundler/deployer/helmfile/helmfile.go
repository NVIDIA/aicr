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

package helmfile

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	// fileLevelHelmfilePrefix is prepended to each per-level sub-helmfile
	// in the stratified layout (issue #914). Level N's sub-helmfile is
	// "level-N.yaml". Operators reading the bundle see the dependency
	// depth as the filename.
	fileLevelHelmfilePrefix = "level-"

	// fileHelmfile is the top-level orchestration document emitted by this
	// deployer. The name matches helmfile's default discovery so operators
	// can run `helmfile apply` from the bundle directory with no `-f` flag.
	fileHelmfile = "helmfile.yaml"
	// fileReadme is the user-facing apply/diff/destroy walkthrough.
	fileReadme = "README.md"
)

//go:embed templates/README.md.tmpl
var readmeTemplate string

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates a helmfile.yaml release graph + per-component chart
// folders from a configured recipe. Operators run `helmfile apply` /
// `helmfile diff` / `helmfile destroy` against the bundle to drive
// rollouts.
//
// Configure with the required fields, then call Generate.
type Generator struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component name → Helm values. The values map
	// is split between values.yaml and cluster-values.yaml by localformat
	// per DynamicValues.
	ComponentValues map[string]map[string]any

	// Version is the bundler version (rendered into README.md header).
	Version string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// ComponentPreManifests maps component name → manifest path → bytes
	// for manifests that apply BEFORE each component's primary chart.
	// Forwarded to localformat.Options.ComponentPreManifests.
	ComponentPreManifests map[string]map[string][]byte

	// ComponentPostManifests maps component name → manifest path → bytes
	// for manifests that apply AFTER each component's primary chart.
	// Forwarded to localformat.Options.ComponentPostManifests.
	ComponentPostManifests map[string]map[string][]byte

	// DataFiles lists additional file paths (relative to output dir) to
	// include in checksum generation.
	DataFiles []string

	// DynamicValues maps component names to their dynamic value paths.
	// localformat splits these into cluster-values.yaml; the helmfile
	// deployer references that file in the release's values: list so
	// operators can fill it in before `helmfile apply`.
	DynamicValues map[string][]string

	// VendorCharts pulls upstream Helm chart bytes into the bundle at
	// generation time so the resulting artifact is air-gap deployable.
	// Off by default. See pkg/bundler/deployer/localformat for the
	// vendoring shape (single wrapped folder per Helm component, with
	// charts/<chart>-<version>.tgz adjacent to a wrapper Chart.yaml).
	VendorCharts bool

	// Serial chains every release to its immediate predecessor via needs:,
	// forming one linear apply chain instead of the default per-component
	// chaining (which lets independent components in a sub-helmfile apply
	// concurrently). Off by default. Operator escape hatch wired from
	// --serial; see config.Serial and buildHelmfile.
	Serial bool

	// vendorRecords is populated by Generate when VendorCharts is on.
	// Captured here so provenance.yaml can be written after component
	// generation without re-threading the slice through every helper.
	vendorRecords []localformat.VendorRecord
}

// Generate emits helmfile.yaml + per-component chart folders into outputDir.
// Per-component folder content (Chart.yaml, values.yaml, templates/*,
// upstream.env) is delegated to pkg/bundler/deployer/localformat. The
// helmfile deployer owns only the top-level orchestration: helmfile.yaml +
// README.md (and provenance.yaml / checksums.txt when configured).
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context canceled before generation", err)
	}

	output := &deployer.Output{Files: make([]string, 0)}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	// Sort components by deployment order and build the localformat input.
	sortedRefs := deployer.SortComponentRefsByDeploymentOrder(
		g.RecipeResult.ComponentRefs, g.RecipeResult.DeploymentOrder)
	lfComponents, namespaceByComponent := toLocalformatComponents(sortedRefs, g.ComponentValues, g.DynamicValues)

	writeResult, err := localformat.Write(ctx, localformat.Options{
		OutputDir:              outputDir,
		Components:             lfComponents,
		ComponentPreManifests:  g.ComponentPreManifests,
		ComponentPostManifests: g.ComponentPostManifests,
		VendorCharts:           g.VendorCharts,
	})
	if err != nil {
		return nil, err
	}
	g.vendorRecords = writeResult.VendoredCharts
	for _, f := range writeResult.Folders {
		for _, rel := range f.Files {
			abs, joinErr := deployer.SafeJoin(outputDir, rel)
			if joinErr != nil {
				// SafeJoin already returns a coded StructuredError
				// (ErrCodeInvalidRequest); preserve it.
				return nil, errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("path from localformat escapes outputDir: %s", rel))
			}
			output.Files = append(output.Files, abs)
			if info, statErr := os.Stat(abs); statErr == nil {
				output.TotalSize += info.Size()
			}
		}
	}

	// Emit either the single-file helmfile.yaml or the stratified
	// helmfiles: layout depending on whether the dependency DAG
	// produces more than one level (issue #914).
	stratified, err := g.writeHelmfileLayout(outputDir, output, writeResult.Folders, sortedRefs, namespaceByComponent)
	if err != nil {
		return nil, err
	}

	// README.md
	readmePath, readmeSize, err := writeReadme(outputDir, g.Version, sortedRefs, len(g.DynamicValues) > 0, g.VendorCharts, stratified)
	if err != nil {
		return nil, err
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// External data files (checksum coverage).
	if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
		return nil, err
	}

	// provenance.yaml for vendored bundles. Written before checksums so
	// the audit file is itself checksummed.
	if len(g.vendorRecords) > 0 {
		provPath, provSize, provErr := localformat.WriteProvenance(ctx, outputDir, g.vendorRecords)
		if provErr != nil {
			// WriteProvenance returns coded StructuredErrors;
			// preserve the code rather than overwriting with Internal.
			return nil, errors.PropagateOrWrap(provErr, errors.ErrCodeInternal,
				"failed to generate provenance.yaml")
		}
		output.Files = append(output.Files, provPath)
		output.TotalSize += provSize
	}

	if g.IncludeChecksums {
		if err := checksum.WriteChecksums(ctx, outputDir, output); err != nil {
			return nil, err
		}
	}

	output.Duration = time.Since(start)
	output.DeploymentSteps = []string{
		fmt.Sprintf("cd %s", outputDir),
		"helmfile diff   # preview changes",
		"helmfile apply  # install or upgrade",
	}
	notes := []string{
		"Requires the helmfile CLI on $PATH (see README.md for installation).",
	}
	if len(g.DynamicValues) > 0 {
		notes = append(notes,
			"Per-component cluster-values.yaml files have been generated. Edit them before `helmfile apply` to customize per-cluster settings.")
	}
	if len(g.vendorRecords) > 0 {
		notes = append(notes,
			"This bundle contains vendored Helm charts. No upstream registry access is required at deploy time. See provenance.yaml for chart provenance details.")
	}
	output.DeploymentNotes = notes

	slog.Debug("helmfile bundle generated",
		"components", len(sortedRefs),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)
	return output, nil
}

// writeHelmfileLayout decides between the single-file layout and the
// stratified multi-helmfiles layout (issue #914), writes the appropriate
// files to outputDir, threads the file metadata into output, and reports
// whether the split layout was used so the README renderer can include
// the helmfiles: explainer.
//
// Selection is driven by dependency depth (recipe.ComponentRefsTopological
// Levels). A bundle whose dependency DAG produces a single level (every
// component independent, or only one component) emits a single
// helmfile.yaml. A bundle producing N > 1 levels emits one sub-helmfile
// per level — `level-0.yaml`, `level-1.yaml`, ... — plus a top-level
// `helmfile.yaml` whose `helmfiles:` list references them in order.
// Sub-helmfiles are processed sequentially by `helmfile`, so by the
// time level K diffs, every release in levels 0…K-1 has fully applied
// (and any CRD they install is registered in the cluster's REST mapper).
// This removes any need for the bundler to know which charts ship CRDs
// — the dependency edges already encode it.
func (g *Generator) writeHelmfileLayout(
	outputDir string,
	output *deployer.Output,
	folders []localformat.Folder,
	sortedRefs []recipe.ComponentRef,
	namespaceByComponent map[string]string,
) (bool, error) {

	flags, err := componentFlagsByName(sortedRefs, g.RecipeResult.DataProvider())
	if err != nil {
		return false, errors.Wrap(errors.ErrCodeInternal,
			"failed to load registry for component flags", err)
	}
	levels, err := recipe.ComponentRefsTopologicalLevels(sortedRefs)
	if err != nil {
		return false, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to compute dependency levels")
	}
	folderLevels := splitFoldersByLevel(folders, levels)

	// buildHelmfile returns pkg/errors StructuredError values (e.g.
	// ErrCodeInvalidRequest for unsupported folder kinds); use
	// PropagateOrWrap so those codes survive rather than being
	// overwritten with ErrCodeInternal.
	writeDoc := func(folders []localformat.Folder, name string) error {
		doc, buildErr := buildHelmfile(folders, namespaceByComponent, g.DynamicValues, flags, g.Serial)
		if buildErr != nil {
			return errors.PropagateOrWrap(buildErr, errors.ErrCodeInternal,
				fmt.Sprintf("failed to build %s", name))
		}
		path, size, writeErr := writeHelmfileYAMLAs(outputDir, doc, name)
		if writeErr != nil {
			return writeErr
		}
		output.Files = append(output.Files, path)
		output.TotalSize += size
		return nil
	}

	// Collapse to single file when the DAG has at most one non-empty
	// level (every component is independent, or only one component
	// exists). Skipping the sub-helmfile sequencing avoids the extra
	// file count and helmfile-process overhead when there's no
	// ordering work to do.
	nonEmpty := 0
	for _, l := range folderLevels {
		if len(l) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty <= 1 {
		return false, writeDoc(folders, fileHelmfile)
	}

	subPaths := make([]string, 0, len(folderLevels))
	for i, levelFolders := range folderLevels {
		if len(levelFolders) == 0 {
			continue
		}
		name := fmt.Sprintf("%s%d.yaml", fileLevelHelmfilePrefix, i)
		if err := writeDoc(levelFolders, name); err != nil {
			return false, err
		}
		subPaths = append(subPaths, name)
	}

	topPath, topSize, topErr := writeTopHelmfile(outputDir, subPaths)
	if topErr != nil {
		return false, topErr
	}
	output.Files = append(output.Files, topPath)
	output.TotalSize += topSize
	return true, nil
}

// toLocalformatComponents maps the recipe ComponentRefs (already sorted by
// deployment order) to the per-component inputs consumed by
// localformat.Write, and returns a namespaceByComponent lookup that
// buildHelmfile uses to set release namespaces.
func toLocalformatComponents(
	refs []recipe.ComponentRef,
	values map[string]map[string]any,
	dynamic map[string][]string,
) ([]localformat.Component, map[string]string) {

	out := make([]localformat.Component, 0, len(refs))
	ns := make(map[string]string, len(refs))
	for _, ref := range refs {
		chartName := ref.EffectiveChart()
		out = append(out, localformat.Component{
			Name:         ref.Name,
			Namespace:    ref.Namespace,
			Repository:   ref.Source,
			ChartName:    chartName,
			Version:      ref.Version,
			IsOCI:        strings.HasPrefix(ref.Source, "oci://"),
			Tag:          ref.Tag,
			Path:         ref.Path,
			Values:       values[ref.Name],
			DynamicPaths: dynamic[ref.Name],
		})
		ns[ref.Name] = ref.Namespace
	}
	return out, ns
}

// writeHelmfileYAMLAs marshals doc to outputDir/<filename>. Marshals
// directly to YAML (not via text/template) so field order is anchored to
// the Helmfile struct and round-trip-stable across runs. The filename is
// parameterized so the same writer powers the single-file layout
// (helmfile.yaml) and the split layout's sub-helmfiles (level-0.yaml,
// ... level-N.yaml).
func writeHelmfileYAMLAs(outputDir string, doc Helmfile, filename string) (string, int64, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("encode %s", filename), err)
	}
	if err := enc.Close(); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("close %s encoder", filename), err)
	}

	path, err := deployer.SafeJoin(outputDir, filename)
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write %s", filename), err)
	}
	return path, int64(buf.Len()), nil
}

// writeTopHelmfile emits the stratified-layout top-level helmfile.yaml
// with a helmfiles: list referencing each per-level sub-helmfile in
// dependency order (level-0.yaml first, level-1.yaml next, ...). The
// body is intentionally minimal — repositories and helmDefaults live
// in the sub-files so each layer can declare only what it needs.
func writeTopHelmfile(outputDir string, subPaths []string) (string, int64, error) {
	refs := make([]SubHelmfileRef, 0, len(subPaths))
	for _, p := range subPaths {
		refs = append(refs, SubHelmfileRef{Path: p})
	}
	doc := TopHelmfile{Helmfiles: refs}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "encode helmfile.yaml", err)
	}
	if err := enc.Close(); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "close helmfile.yaml encoder", err)
	}

	path, err := deployer.SafeJoin(outputDir, fileHelmfile)
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "write helmfile.yaml", err)
	}
	return path, int64(buf.Len()), nil
}

// componentFlags captures the registry-derived behavioral flags that
// affect how a component's release is rendered in the generated
// helmfile. HasSelfRefCRDs drives disableValidation: true on the
// primary release; ManifestsUseChartCRDs drives it on the injected
// -post wrapper release. The struct exists as a gathering point so
// future per-release knobs can be added without changing call
// signatures.
type componentFlags struct {
	HasSelfRefCRDs        bool
	ManifestsUseChartCRDs bool
}

// componentFlagsByName returns a per-component map of registry flags
// for the refs in this recipe. Loaded once per Generate so the
// release-rendering step does a single registry round-trip.
// Components not in the registry are absent from the map (treated as
// all-false).
//
// provider is the recipe-bound DataProvider whose registry is consulted;
// nil falls back to the deprecated process-global registry for callers
// that pre-date per-recipe binding.
func componentFlagsByName(refs []recipe.ComponentRef, provider recipe.DataProvider) (map[string]componentFlags, error) {
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return nil, err
	}
	out := make(map[string]componentFlags, len(refs))
	for _, ref := range refs {
		cfg := registry.Get(ref.Name)
		if cfg == nil {
			continue
		}
		out[ref.Name] = componentFlags{
			HasSelfRefCRDs:        cfg.HasSelfRefCRDs,
			ManifestsUseChartCRDs: cfg.ManifestsUseChartCRDs,
		}
	}
	return out, nil
}

// splitFoldersByLevel partitions the localformat folders into per-DAG-
// level groups, preserving the original folder order within each group.
// A folder's level is its parent component's level in levels (as
// produced by recipe.ComponentRefsTopologicalLevels). Auxiliary -pre
// and -post folders share the parent's level so the three travel
// together to the same sub-helmfile.
//
// Returns a slice of length len(levels). Each index i holds the folders
// whose parent component is in dependency level i. A level with no
// folders yields an empty slice at that index (callers skip empties
// when emitting sub-helmfiles).
//
// Folders whose parent component is not represented in levels (e.g.,
// injected wrapper folders for a component name that doesn't appear in
// the DAG) default to level 0. In a correctly-constructed bundle this
// shouldn't happen — levels covers every recipe component — but the
// fallback keeps the partition total and avoids silently dropping
// folders.
func splitFoldersByLevel(folders []localformat.Folder, levels [][]string) [][]localformat.Folder {
	levelOf := make(map[string]int, 0)
	for i, level := range levels {
		for _, name := range level {
			levelOf[name] = i
		}
	}
	out := make([][]localformat.Folder, len(levels))
	if len(out) == 0 {
		// No components produced any levels — defensive: emit one
		// level containing all folders so callers see a non-empty
		// partition.
		return [][]localformat.Folder{folders}
	}
	for _, f := range folders {
		idx, ok := levelOf[f.Parent]
		if !ok {
			idx = 0
		}
		out[idx] = append(out[idx], f)
	}
	return out
}

// readmeData is the template data for README.md generation.
type readmeData struct {
	BundlerVersion string
	HasDynamic     bool
	HasVendored    bool
	// HasSubHelmfiles is true when the bundle uses the stratified
	// multi-helmfiles layout (one level-N.yaml per DAG level,
	// referenced from helmfile.yaml's helmfiles: list). Drives a
	// short README note explaining the structure to operators.
	// Issue #914.
	HasSubHelmfiles bool
	Components      []readmeComponent
}

type readmeComponent struct {
	Name      string
	Namespace string
	Version   string
}

// writeReadme renders README.md from the embedded template.
func writeReadme(outputDir, version string, refs []recipe.ComponentRef, hasDynamic, hasVendored, hasSubHelmfiles bool) (string, int64, error) {
	data := readmeData{
		BundlerVersion:  deployer.NormalizeVersionWithDefault(version),
		HasDynamic:      hasDynamic,
		HasVendored:     hasVendored,
		HasSubHelmfiles: hasSubHelmfiles,
		Components:      make([]readmeComponent, 0, len(refs)),
	}
	for _, r := range refs {
		v := r.Version
		if v == "" {
			v = r.Tag
		}
		data.Components = append(data.Components, readmeComponent{
			Name: r.Name, Namespace: r.Namespace, Version: v,
		})
	}
	path, size, err := deployer.GenerateFromTemplate(readmeTemplate, data, outputDir, fileReadme)
	if err != nil {
		return "", 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to write README.md")
	}
	return path, size, nil
}
