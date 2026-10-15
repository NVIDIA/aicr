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

package terraform

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Bundle file names.
const (
	fileMain     = "main.tf"
	fileVersions = "versions.tf"
	fileVars     = "variables.tf"
	fileOutputs  = "outputs.tf"
	fileTfvars   = "terraform.tfvars.example"
	fileReadme   = "README.md"

	// moduleDir is the generic component module every module call shares,
	// relative to the bundle root.
	moduleDir = "modules/component"

	// Values file names written by localformat into each folder.
	fileValues        = "values.yaml"
	fileClusterValues = "cluster-values.yaml"
)

//go:embed templates/main.tf.tmpl
var mainTemplate string

//go:embed templates/versions.tf.tmpl
var versionsTemplate string

//go:embed templates/variables.tf.tmpl
var variablesTemplate string

//go:embed templates/outputs.tf.tmpl
var outputsTemplate string

//go:embed templates/tfvars.example.tmpl
var tfvarsTemplate string

//go:embed templates/module-main.tf.tmpl
var moduleMainTemplate string

//go:embed templates/module-variables.tf.tmpl
var moduleVariablesTemplate string

//go:embed templates/module-outputs.tf.tmpl
var moduleOutputsTemplate string

//go:embed templates/README.md.tmpl
var readmeTemplate string

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates a Terraform module from recipe results: a root module, or
// a child module when ChildModule is set.
// Configure it with the required fields, then call Generate.
type Generator struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component name → Helm values. The values map
	// is split between values.yaml and cluster-values.yaml by localformat
	// per DynamicValues.
	ComponentValues map[string]map[string]any

	// Version is the bundler version (rendered into every generated file's
	// header and into README.md).
	Version string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// UpgradeNotice is upgrade.WriteNotice output, inserted verbatim into
	// README.md ahead of the deploy steps when the recipe carries a
	// transition the operator must act on before upgrading. Empty otherwise.
	UpgradeNotice string

	// ComponentPreManifests maps component name → manifest path → bytes
	// for manifests that apply BEFORE each component's primary chart.
	ComponentPreManifests map[string]map[string][]byte

	// ComponentPostManifests maps component name → manifest path → bytes
	// for manifests that apply AFTER each component's primary chart.
	ComponentPostManifests map[string]map[string][]byte

	// ComponentReadiness maps component name → manifest path → bytes for
	// the readiness gate chart emitted after each component's primary
	// chart. Populated when --readiness-hooks is set; empty otherwise.
	//
	// Unlike flux and helmfile, this deployer can honor the gate without
	// extra wiring: the gate arrives as one more folder and becomes the
	// last slot in the component's module, which blocks until the Job
	// completes. The slot hardcodes wait, so an async override cannot
	// disarm the gate.
	ComponentReadiness map[string]map[string][]byte

	// DataFiles lists additional file paths (relative to output dir) to
	// include in checksum generation.
	DataFiles []string

	// DynamicValues maps component names to their dynamic value paths.
	// localformat splits these into cluster-values.yaml; this deployer
	// adds that file to the module call's values_files so operators can
	// fill it in before `terraform apply`.
	DynamicValues map[string][]string

	// VendorCharts pulls upstream Helm chart bytes into the bundle at
	// generation time so the resulting artifact is air-gap deployable.
	// Off by default.
	VendorCharts bool

	// Puller fetches upstream chart bytes when VendorCharts is set. nil
	// resolves to localformat's default *CLIChartPuller; tests inject a
	// stub here so the vendored layout can be rendered without a helm
	// binary.
	Puller localformat.ChartPuller

	// ClusterRollover emits a terraform_data keyed on var.cluster_keepers
	// in each component module, with a replace_triggered_by pointing at
	// it, so replacing the cluster replaces every release instead of
	// leaving state describing objects that no longer exist.
	//
	// Off by default: the keepers are whatever the operator says identifies
	// the cluster — a CA certificate, a provider id, an endpoint that is
	// unique per cluster — and nothing in the bundle can choose them. Both
	// forms require the variable: a default would contain nothing, and the
	// first real value would then replace every release.
	ClusterRollover bool

	// ChildModule emits the bundle as a CHILD module rather than a root
	// module: no provider configuration, and no cluster-connection
	// variables to feed one. The calling configuration declares and
	// configures the helm provider, and the bundle's module calls inherit
	// it the way any child module does.
	//
	// This is the shape to use when the cluster is declared in the same
	// configuration as the bundle. A module that carries its own provider
	// block is a legacy construct: Terraform still accepts it but
	// disallows count/for_each/depends_on on the call, which is exactly
	// what a caller needs to sequence the bundle behind the cluster.
	ChildModule bool

	// Serial replaces the declared dependency graph with a single linear
	// chain, so releases apply strictly one at a time regardless of the
	// recipe's actual edges. Off by default; wired from --serial.
	Serial bool

	// vendorRecords is populated by Generate when VendorCharts is on, so
	// provenance.yaml can be written after component generation.
	vendorRecords []localformat.VendorRecord
}

// Generate emits the Terraform module (root or child), the shared component module, and
// the per-component folders into outputDir.
//
// Per-component folder content is delegated to
// pkg/bundler/deployer/localformat, the same writer the helm and helmfile
// deployers use. This deployer owns only the Terraform layer: main.tf and its
// siblings, modules/component/, and README.md.
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe result is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled before generation", err)
	}

	output := &deployer.Output{Files: make([]string, 0)}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	enabledRefs := filterEnabled(g.RecipeResult.ComponentRefs)
	sortedRefs := deployer.SortComponentRefsByDeploymentOrder(enabledRefs, g.RecipeResult.DeploymentOrder)

	for _, ref := range sortedRefs {
		if !deployer.IsSafePathComponent(ref.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unsafe component name: %q", ref.Name))
		}
		// These values reach main.tf, some of them in comments. A newline
		// would end a comment and hand the remainder to the HCL parser as
		// configuration, and an apply of that runs on the operator's host.
		// Recipe validation rejects only surrounding whitespace, so refuse
		// here, where the values are about to become HCL.
		for _, fv := range [][2]string{
			{"name", ref.Name}, {"namespace", ref.Namespace}, {"chart name", ref.Chart},
			{"chart version", ref.Version}, {"source", ref.Source},
		} {
			field, value := fv[0], fv[1]
			if hasControl(value) {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q: %s %q contains a control character", ref.Name, field, value))
			}
		}
		if strings.EqualFold(string(ref.Type), string(recipe.ComponentTypeKustomize)) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q is a Kustomize component; --deployer terraform supports Helm components only",
					ref.Name))
		}
	}

	// Fail closed on a bad dependency graph before projecting it onto
	// depends_on. Generate is exported, so a direct caller could hand-build a
	// cyclic RecipeResult; a cycle that reaches main.tf is a terraform init
	// error attributed to AICR's output rather than to its input. Validate the
	// UNFILTERED refs so the graph builder's own enabled-filtering can treat
	// an edge to a declared-but-disabled component as satisfied externally,
	// matching flux, argocd and helmfile. The levels are discarded — Terraform
	// renders the exact DAG.
	if _, levelErr := recipe.ComponentRefsTopologicalLevels(g.RecipeResult.ComponentRefs); levelErr != nil {
		return nil, errors.PropagateOrWrap(levelErr, errors.ErrCodeInternal,
			"failed to validate dependency graph")
	}

	lfComponents := toLocalformatComponents(sortedRefs, g.ComponentValues, g.DynamicValues)
	writeResult, err := localformat.Write(ctx, localformat.Options{
		OutputDir:              outputDir,
		Components:             lfComponents,
		AICRVersion:            g.Version,
		ComponentPreManifests:  g.ComponentPreManifests,
		ComponentPostManifests: g.ComponentPostManifests,
		ComponentReadiness:     g.ComponentReadiness,
		VendorCharts:           g.VendorCharts,
		Puller:                 g.Puller,
	})
	if err != nil {
		return nil, err
	}
	g.vendorRecords = writeResult.VendoredCharts

	// localformat's folders are helm's: each carries an install.sh that
	// deploy.sh and the e2e harnesses run, and the upstream.env it sources.
	// Nothing in a terraform bundle does — the module call carries the chart
	// coordinates and the values files — so they are removed rather than
	// shipped as a second, unmanaged way to install the release names
	// Terraform manages. Same treatment as argocd's stripUnusedHelmFiles.
	if stripErr := stripHelmOnlyFiles(outputDir, writeResult.Folders); stripErr != nil {
		return nil, stripErr
	}

	// main.tf is the entrypoint even in child-module form: a caller still
	// points its module source at the bundle root. Manifest is left empty on
	// every release because a terraform bundle declares its releases in
	// main.tf, not in a per-folder file.
	output.Entrypoint = fileMain
	output.Releases = writeResult.Releases()

	if recordErr := g.recordFolderFiles(outputDir, writeResult.Folders, output); recordErr != nil {
		return nil, recordErr
	}

	components, err := buildComponents(writeResult.Folders, sortedRefs, g.Serial)
	if err != nil {
		return nil, err
	}

	digests, err := chartDigests(outputDir, writeResult.Folders)
	if err != nil {
		return nil, err
	}

	data := g.bundleData(components, digests)
	if err := g.writeTerraform(outputDir, data, output); err != nil {
		return nil, err
	}

	if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
		return nil, err
	}

	// provenance.yaml for vendored bundles, before checksums so the audit
	// file is itself checksummed.
	if len(g.vendorRecords) > 0 {
		provPath, provSize, provErr := localformat.WriteProvenance(ctx, outputDir, g.vendorRecords)
		if provErr != nil {
			return nil, errors.PropagateOrWrap(provErr, errors.ErrCodeInternal,
				"failed to generate provenance.yaml")
		}
		output.Files = append(output.Files, provPath)
		output.TotalSize += provSize
		output.Provenance = localformat.ProvenanceFileName
	}

	if g.IncludeChecksums {
		if err := checksum.WriteChecksums(ctx, outputDir, output); err != nil {
			return nil, err
		}
	}

	output.Duration = time.Since(start)
	output.DeploymentSteps, output.DeploymentNotes = g.deploymentGuidance(outputDir)

	slog.Debug("terraform bundle generated",
		"components", len(components),
		"releases", len(writeResult.Folders),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)
	return output, nil
}

// deploymentGuidance returns the printed steps and notes for the form. A child
// module has no provider block and no connection variables, so it is applied
// from the configuration that calls it; printing the root-module steps for it
// would apply with an unconfigured helm provider.
func (g *Generator) deploymentGuidance(outputDir string) (steps, notes []string) {
	var connection string
	if g.ChildModule {
		steps = []string{
			fmt.Sprintf("Call %s as a module from the configuration that owns the helm provider (see its README.md)", outputDir),
			"terraform init",
			"terraform plan",
			"terraform apply",
		}
		connection = "This bundle is a child module: it declares no provider and no cluster-connection variables, " +
			"and installs through the calling configuration's helm provider. Run init/plan/apply there, not in the bundle. " +
			"An upgrade is a regenerated bundle in a new directory; point the module source at it (see README.md)."
	} else {
		steps = []string{
			fmt.Sprintf("cd %s", outputDir),
			"terraform init",
			"terraform plan",
			"terraform apply",
		}
		connection = "The bundle does not create a cluster. It reads ~/.kube/config by default; " +
			"set kubeconfig_path/kube_context, or cluster_host and credentials, to point it elsewhere. " +
			"An upgrade is a regenerated bundle in a new directory: carry .terraform.lock.hcl, terraform.tfvars and the state across " +
			"(see README.md). Regenerating into this directory fails the bundle's closed-world check once Terraform has run in it."
	}
	notes = []string{
		connection,
		"depends_on carries the recipe's dependency graph exactly, so independent components apply concurrently. " +
			"Each component is one module call; its pre/post/readiness folders chain inside it.",
	}
	if len(g.DynamicValues) > 0 {
		notes = append(notes,
			"Per-component cluster-values.yaml files have been generated. Edit them before `terraform apply` to customize per-cluster settings.")
	}
	if len(g.vendorRecords) > 0 {
		notes = append(notes,
			"This bundle contains vendored Helm charts. No upstream registry access is required at deploy time. See provenance.yaml.")
	}
	if g.ClusterRollover {
		keepers := "Set cluster_keepers from the cluster resource itself — its CA certificate, id or " +
			"creation time — so that it changes when the cluster is rebuilt; a value a rebuild " +
			"keeps leaves the releases uncontained"
		if g.ChildModule {
			keepers += ". The variable is required: the module call must pass it."
		} else {
			keepers += ". The variable is required: set it in terraform.tfvars or with -var before the first apply."
		}
		notes = append(notes,
			"Cluster rollover is enabled: replacing the cluster replaces every release. "+keepers)
	}
	return steps, notes
}

// helmOnlyFile reports whether a localformat folder file exists only for the
// helm deployer's shell path. install.sh is helm's standalone entry point and
// upstream.env is what it sources for the chart coordinates; the terraform
// module call carries chart, repository and version itself, so neither has a
// reader here. cluster-values.yaml stays: it is the --dynamic edit point that
// values_files lists.
func helmOnlyFile(base string) bool {
	return base == "install.sh" || base == "upstream.env"
}

// stripHelmOnlyFiles removes helm's shell files from every folder and rewrites
// folder.Files in place, so recordFolderFiles — and through it checksums.txt —
// sees only what survives in the bundle. Leaving them would be worse than
// clutter: install.sh installs the same release names Terraform manages,
// without --wait and on helm's default timeout, so running it by hand drifts
// the state Terraform holds.
func stripHelmOnlyFiles(outputDir string, folders []localformat.Folder) error {
	for i := range folders {
		kept := folders[i].Files[:0]
		for _, rel := range folders[i].Files {
			if !helmOnlyFile(filepath.Base(rel)) {
				kept = append(kept, rel)
				continue
			}
			abs, joinErr := deployer.SafeJoin(outputDir, rel)
			if joinErr != nil {
				return errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("path from localformat escapes outputDir: %s", rel))
			}
			if rmErr := os.Remove(abs); rmErr != nil && !os.IsNotExist(rmErr) {
				return errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to remove %s", rel), rmErr)
			}
		}
		folders[i].Files = kept
	}
	return nil
}

// recordFolderFiles adds every file localformat wrote to the output record, so
// checksum generation covers the whole bundle and not just the .tf layer.
func (g *Generator) recordFolderFiles(outputDir string, folders []localformat.Folder, output *deployer.Output) error {
	for _, f := range folders {
		for _, rel := range f.Files {
			abs, joinErr := deployer.SafeJoin(outputDir, rel)
			if joinErr != nil {
				return errors.PropagateOrWrap(joinErr, errors.ErrCodeInvalidRequest,
					fmt.Sprintf("path from localformat escapes outputDir: %s", rel))
			}
			output.Files = append(output.Files, abs)
			if info, statErr := os.Stat(abs); statErr == nil {
				output.TotalSize += info.Size()
			}
		}
	}
	return nil
}

// filterEnabled returns only the components that are enabled for deployment.
func filterEnabled(refs []recipe.ComponentRef) []recipe.ComponentRef {
	enabled := make([]recipe.ComponentRef, 0, len(refs))
	for _, ref := range refs {
		if ref.IsEnabled() {
			enabled = append(enabled, ref)
		}
	}
	return enabled
}

// toLocalformatComponents projects the recipe's component refs onto the
// localformat writer's input. Identical in shape to the helmfile deployer's
// conversion; the namespace map it also returns there is not needed here
// because Folder.Namespace already carries it.
func toLocalformatComponents(
	refs []recipe.ComponentRef,
	values map[string]map[string]any,
	dynamic map[string][]string,
) []localformat.Component {

	out := make([]localformat.Component, 0, len(refs))
	for _, ref := range refs {
		out = append(out, localformat.Component{
			Name:         ref.Name,
			Namespace:    ref.Namespace,
			Repository:   ref.Source,
			ChartName:    ref.EffectiveChart(),
			Version:      ref.Version,
			IsOCI:        strings.HasPrefix(ref.Source, "oci://"),
			Tag:          ref.Tag,
			Path:         ref.Path,
			Values:       values[ref.Name],
			DynamicPaths: dynamic[ref.Name],
		})
	}
	return out
}
