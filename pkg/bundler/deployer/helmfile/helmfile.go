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

	// Build helmfile.yaml from the written folders. buildHelmfile returns
	// pkg/errors StructuredError values (e.g. ErrCodeInvalidRequest for
	// unsupported folder kinds); use PropagateOrWrap so those codes survive
	// rather than being overwritten with ErrCodeInternal.
	doc, err := buildHelmfile(writeResult.Folders, namespaceByComponent, g.DynamicValues)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to build helmfile document")
	}
	helmfilePath, helmfileSize, err := writeHelmfileYAML(outputDir, doc)
	if err != nil {
		return nil, err
	}
	output.Files = append(output.Files, helmfilePath)
	output.TotalSize += helmfileSize

	// README.md
	readmePath, readmeSize, err := writeReadme(outputDir, g.Version, sortedRefs, len(g.DynamicValues) > 0, g.VendorCharts)
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
		chartName := ref.Chart
		if chartName == "" {
			chartName = ref.Name
		}
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

// writeHelmfileYAML marshals doc to outputDir/helmfile.yaml. Marshals
// directly to YAML (not via text/template) so field order is anchored to
// the Helmfile struct and round-trip-stable across runs.
func writeHelmfileYAML(outputDir string, doc Helmfile) (string, int64, error) {
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

// readmeData is the template data for README.md generation.
type readmeData struct {
	BundlerVersion string
	HasDynamic     bool
	HasVendored    bool
	Components     []readmeComponent
}

type readmeComponent struct {
	Name      string
	Namespace string
	Version   string
}

// writeReadme renders README.md from the embedded template.
func writeReadme(outputDir, version string, refs []recipe.ComponentRef, hasDynamic, hasVendored bool) (string, int64, error) {
	data := readmeData{
		BundlerVersion: deployer.NormalizeVersionWithDefault(version),
		HasDynamic:     hasDynamic,
		HasVendored:    hasVendored,
		Components:     make([]readmeComponent, 0, len(refs)),
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
