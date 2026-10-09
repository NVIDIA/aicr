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

package fleet

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

//go:embed templates/README.md.tmpl
var readmeTemplate string

const (
	fileFleetYAML   = "fleet.yaml"
	fileFleetIgnore = ".fleetignore"
	fileGitRepo     = "gitrepo.yaml"
	fileReadme      = "README.md"

	fileValues        = "values.yaml"
	fileClusterValues = "cluster-values.yaml"

	// ModeGitRepo emits a fleet.yaml per folder and a GitRepo (default).
	ModeGitRepo = config.FleetModeGitRepo

	// ModeHelmOp emits one HelmOp per folder in a single helmops.yaml.
	ModeHelmOp = config.FleetModeHelmOp

	// DefaultAppName names the GitRepo and prefixes every bundle name.
	DefaultAppName = "aicr"

	// DefaultNamespace is the Fleet workspace the GitRepo is applied to.
	DefaultNamespace = config.DefaultFleetNamespace

	// localNamespace is the Fleet workspace for the Rancher local cluster.
	localNamespace = "fleet-local"

	defaultRepoURL        = "https://github.com/YOUR_ORG/YOUR_REPO.git"
	defaultTargetRevision = "main"

	// BundleLabel marks every generated bundle and is the cluster label the
	// GitRepo selects on.
	BundleLabel = "aicr.nvidia.com/bundle"

	// maxBundleNameLen is the label-value limit: Fleet copies every bundle
	// name into the fleet.cattle.io/bundle-name label of its
	// BundleDeployments, so a longer name cannot be deployed. An explicit
	// fleet.yaml name is used verbatim, never shortened.
	maxBundleNameLen = validation.LabelValueMaxLength
)

// ignoredFiles are localformat files Fleet has no use for. install.sh,
// upstream.env and apply-crds.sh drive the helm deployer's scripts; Fleet
// reads the same coordinates from fleet.yaml and cannot run the CRD step.
var ignoredFiles = []string{"install.sh", "upstream.env", "apply-crds.sh"}

// FleetYAML is the subset of the fleet.yaml schema this deployer emits.
type FleetYAML struct {
	Name             string            `yaml:"name"`
	DefaultNamespace string            `yaml:"defaultNamespace,omitempty"`
	Labels           map[string]string `yaml:"labels,omitempty"`
	Helm             HelmOptions       `yaml:"helm"`
	DependsOn        []DependsOn       `yaml:"dependsOn,omitempty"`
}

// HelmOptions is the fleet.yaml helm block.
type HelmOptions struct {
	ReleaseName       string   `yaml:"releaseName"`
	Repo              string   `yaml:"repo,omitempty"`
	Chart             string   `yaml:"chart,omitempty"`
	Version           string   `yaml:"version,omitempty"`
	ValuesFiles       []string `yaml:"valuesFiles,omitempty"`
	DisablePreProcess bool     `yaml:"disablePreProcess,omitempty"`
}

// DependsOn references another Fleet bundle by name.
type DependsOn struct {
	Name string `yaml:"name"`
}

// GitRepo is the fleet.cattle.io/v1alpha1 GitRepo emitted at the bundle root.
type GitRepo struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   ObjectMeta  `yaml:"metadata"`
	Spec       GitRepoSpec `yaml:"spec"`
}

// ObjectMeta is the metadata subset the GitRepo needs.
type ObjectMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// GitRepoSpec is the GitRepo spec subset this deployer emits.
type GitRepoSpec struct {
	Repo    string   `yaml:"repo"`
	Branch  string   `yaml:"branch"`
	Paths   []string `yaml:"paths"`
	Targets []Target `yaml:"targets"`
}

// Target selects the clusters a GitRepo deploys to.
type Target struct {
	ClusterName     string           `yaml:"clusterName,omitempty"`
	ClusterSelector *ClusterSelector `yaml:"clusterSelector,omitempty"`
}

// ClusterSelector is a label selector over Fleet clusters.
type ClusterSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type readmeComponent struct {
	Bundle    string
	Release   string
	Namespace string
	Version   string
}

type readmeData struct {
	BundlerVersion string
	AppName        string
	Namespace      string
	IsLocal        bool
	IsHelmOp       bool
	BundleLabel    string
	HasDynamic     bool
	HasVendored    bool
	CRDOwners      []string
	Components     []readmeComponent
	UpgradeNotice  string
}

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates a Fleet GitRepo bundle from recipe results.
// Configure it with the required fields, then call Generate.
type Generator struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component names to their values.
	ComponentValues map[string]map[string]any

	// Version is the bundler version.
	Version string

	// AppName names the GitRepo and prefixes every bundle name.
	// Defaults to DefaultAppName.
	AppName string

	// Namespace is the Fleet workspace the GitRepo is applied to.
	// Defaults to DefaultNamespace.
	Namespace string

	// Mode selects the output shape: ModeGitRepo (default) or ModeHelmOp.
	Mode string

	// RepoURL is the Git repository the bundle is pushed to.
	RepoURL string

	// TargetRevision is the branch Fleet tracks. Defaults to main.
	TargetRevision string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// ComponentPreManifests and ComponentPostManifests are forwarded to
	// localformat, which emits them as injected -pre / -post folders.
	ComponentPreManifests  map[string]map[string][]byte
	ComponentPostManifests map[string]map[string][]byte

	// DataFiles lists additional file paths (relative to output dir) to
	// include in checksum generation.
	DataFiles []string

	// DynamicValues maps component names to their dynamic value paths,
	// which localformat moves into cluster-values.yaml.
	DynamicValues map[string][]string

	// VendorCharts pulls upstream chart bytes into the bundle.
	VendorCharts bool

	// Puller fetches upstream chart bytes when VendorCharts is set. nil
	// resolves to localformat's default *CLIChartPuller; tests inject a stub
	// here.
	Puller localformat.ChartPuller

	// UpgradeNotice is inserted verbatim before the README deployment section.
	UpgradeNotice string
}

func (g *Generator) appName() string {
	if g.AppName == "" {
		return DefaultAppName
	}
	return g.AppName
}

func (g *Generator) mode() string {
	if g.Mode == "" {
		return ModeGitRepo
	}
	return g.Mode
}

func (g *Generator) namespace() string {
	if g.Namespace == "" {
		return DefaultNamespace
	}
	return g.Namespace
}

func (g *Generator) repoURL() string {
	if g.RepoURL == "" {
		return defaultRepoURL
	}
	return g.RepoURL
}

func (g *Generator) targetRevision() string {
	if g.TargetRevision == "" {
		return defaultTargetRevision
	}
	return g.TargetRevision
}

// Generate writes the localformat folders, a fleet.yaml and .fleetignore per
// folder, the root gitrepo.yaml, and README.md.
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "context canceled before generation")
	}
	if errs := validation.IsDNS1123Label(g.appName()); len(errs) > 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Fleet app name %q: %s", g.appName(), strings.Join(errs, "; ")))
	}
	if err := config.ValidateFleetName("Fleet namespace", g.namespace()); err != nil {
		return nil, err
	}

	switch g.mode() {
	case ModeGitRepo:
	case ModeHelmOp:
		if g.VendorCharts {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"vendored charts are local charts, which --fleet-mode helmop cannot deploy; use --fleet-mode gitrepo")
		}
		// Reject before writing anything, so a refused bundle leaves no
		// half-written tree behind.
		if local := localChartComponents(g.RecipeResult.ComponentRefs, g.ComponentPreManifests, g.ComponentPostManifests); len(local) > 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("components %s need local charts (raw manifests or kustomize), which --fleet-mode helmop "+
					"cannot deploy because a HelmOp needs a chart repository; use --fleet-mode gitrepo",
					strings.Join(local, ", ")))
		}
	default:
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid Fleet mode %q: must be %q or %q", g.Mode, ModeGitRepo, ModeHelmOp))
	}

	// Like the HelmOp checks above, reject before writing anything.
	for _, release := range plannedReleases(g.RecipeResult.ComponentRefs, g.ComponentPreManifests, g.ComponentPostManifests) {
		if err := checkBundleNameLen(bundleName(g.appName(), release)); err != nil {
			return nil, err
		}
	}

	output := &deployer.Output{Files: make([]string, 0)}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	sortedRefs := deployer.SortComponentRefsByDeploymentOrder(
		g.RecipeResult.ComponentRefs, g.RecipeResult.DeploymentOrder)

	// Fleet installs charts through Helm, which never updates crds/ on
	// upgrade, and it cannot run the apply-crds.sh step the helm deployer
	// emits for CRD-owning components. Name those components so the
	// operator knows which upgrades need the manual CRD step.
	crdOwners, err := deployer.ResolveCRDOwners(ctx, g.RecipeResult.DataProvider(), sortedRefs)
	if err != nil {
		return nil, err
	}
	var crdOwnerNames []string
	for _, ref := range sortedRefs {
		if crdOwners[ref.Name] {
			crdOwnerNames = append(crdOwnerNames, ref.Name)
		}
	}

	writeResult, err := localformat.Write(ctx, localformat.Options{
		OutputDir:              outputDir,
		Components:             toLocalformatComponents(sortedRefs, g.ComponentValues, g.DynamicValues),
		AICRVersion:            g.Version,
		ComponentPreManifests:  g.ComponentPreManifests,
		ComponentPostManifests: g.ComponentPostManifests,
		VendorCharts:           g.VendorCharts,
		Puller:                 g.Puller,
	})
	if err != nil {
		return nil, err
	}
	output.Releases = writeResult.Releases()
	for _, f := range writeResult.Folders {
		for _, rel := range f.Files {
			if addErr := addFile(output, outputDir, rel); addErr != nil {
				return nil, addErr
			}
		}
	}

	docs, err := buildFleetYAMLs(writeResult.Folders, g.appName())
	if err != nil {
		return nil, err
	}
	if g.mode() == ModeHelmOp {
		err = g.writeHelmOpLayout(outputDir, output, writeResult.Folders, docs)
	} else {
		err = g.writeGitRepoLayout(outputDir, output, writeResult.Folders, docs)
	}
	if err != nil {
		return nil, err
	}

	if readmeErr := g.writeReadme(outputDir, writeResult.Folders, docs, sortedRefs, crdOwnerNames, len(writeResult.VendoredCharts) > 0); readmeErr != nil {
		return nil, readmeErr
	}
	if addErr := addFile(output, outputDir, fileReadme); addErr != nil {
		return nil, addErr
	}

	if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
		return nil, err
	}

	if len(writeResult.VendoredCharts) > 0 {
		provPath, provSize, provErr := localformat.WriteProvenance(ctx, outputDir, writeResult.VendoredCharts)
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
	g.finalizeOutput(output, crdOwnerNames, len(writeResult.VendoredCharts) > 0)

	slog.Debug("fleet bundle generated",
		"components", len(sortedRefs),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)
	return output, nil
}

// finalizeOutput records the deployment source, the steps a user runs, and
// the notes that depend on the bundle's mode and content.
func (g *Generator) finalizeOutput(output *deployer.Output, crdOwnerNames []string, hasVendored bool) {
	if g.mode() == ModeHelmOp {
		// A HelmOp bundle names no Git source; only the app name appears.
		output.Source = deployer.Source{AppName: g.appName()}
		output.DeploymentSteps = []string{"kubectl apply -f " + fileHelmOps}
	} else {
		output.Source = deployer.Source{
			RepoURL:        g.repoURL(),
			TargetRevision: g.targetRevision(),
			AppName:        g.appName(),
		}
		output.DeploymentSteps = []string{
			"Push this bundle to " + g.repoURL() + " (branch " + g.targetRevision() + ")",
			"kubectl apply -f " + fileGitRepo,
		}
	}
	if g.namespace() != localNamespace {
		output.DeploymentSteps = append(output.DeploymentSteps,
			fmt.Sprintf("kubectl label clusters.fleet.cattle.io -n %s <cluster> %s=%s", g.namespace(), BundleLabel, g.appName()))
	}
	output.DeploymentSteps = append(output.DeploymentSteps,
		fmt.Sprintf("kubectl get bundles -n %s -l %s=%s", g.namespace(), BundleLabel, g.appName()))
	notes := []string{
		"Requires Rancher Fleet; " + output.Entrypoint + " is applied to the Rancher management cluster.",
	}
	if len(crdOwnerNames) > 0 {
		notes = append(notes, fmt.Sprintf(
			"Fleet does not upgrade CRDs. Before upgrading %s, apply the new chart's CRDs manually "+
				"(see README.md).", strings.Join(crdOwnerNames, ", ")))
	}
	if len(g.DynamicValues) > 0 {
		if g.mode() == ModeHelmOp {
			notes = append(notes,
				"Dynamic values are inlined in helmops.yaml. Edit them under each HelmOp's spec.helm.values before applying.")
		} else {
			notes = append(notes,
				"Per-component cluster-values.yaml files have been generated. Edit them before pushing to customize per-cluster settings.")
		}
	}
	if hasVendored {
		notes = append(notes,
			"This bundle contains vendored Helm charts. Fleet Bundles are stored in etcd; check that each folder stays well under the ~1MiB object limit.")
	}
	output.DeploymentNotes = notes
}

// buildFleetYAMLs returns one fleet.yaml document per folder, in folder
// order. Each folder depends on its predecessor, so Fleet reconciles the
// bundle in install order.
func buildFleetYAMLs(folders []localformat.Folder, appName string) ([]FleetYAML, error) {
	docs := make([]FleetYAML, 0, len(folders))
	prevBundle := ""
	for _, f := range folders {
		name := bundleName(appName, f.Name)
		// Generate checks the planned names before writing; this catches a
		// folder localformat added that plannedReleases does not know about.
		if err := checkBundleNameLen(name); err != nil {
			return nil, err
		}
		doc := FleetYAML{
			Name:             name,
			DefaultNamespace: f.Namespace,
			Labels:           map[string]string{BundleLabel: appName},
			Helm: HelmOptions{
				ReleaseName:       f.Name,
				ValuesFiles:       valuesFilesFor(f),
				DisablePreProcess: true,
			},
		}
		switch f.Kind {
		case localformat.KindUpstreamHelm:
			if f.Upstream == nil {
				return nil, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("folder %s is KindUpstreamHelm but Upstream is nil", f.Dir))
			}
			doc.Helm.Version = f.Upstream.Version
			if strings.HasPrefix(f.Upstream.Repo, "oci://") {
				// Fleet expects OCI charts as a full reference in helm.repo.
				doc.Helm.Repo = strings.TrimSuffix(f.Upstream.Repo, "/") + "/" + f.Upstream.Chart
			} else {
				doc.Helm.Repo = f.Upstream.Repo
				doc.Helm.Chart = f.Upstream.Chart
			}
		case localformat.KindLocalHelm:
			// The folder holds Chart.yaml; Fleet deploys it as a local chart.
			// Fleet parses helm.valuesFiles only when helm.chart or helm.repo
			// is set, yet always drops those files from the Bundle, so an
			// empty chart would lose every value. "." keeps the folder as the
			// chart source: a chart path that exists on disk with no repo is
			// read locally, never downloaded.
			doc.Helm.Chart = "."
		default:
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unsupported folder kind %v for %s", f.Kind, f.Dir))
		}
		if prevBundle != "" {
			doc.DependsOn = []DependsOn{{Name: prevBundle}}
		}
		docs = append(docs, doc)
		prevBundle = name
	}
	return docs, nil
}

// buildGitRepo returns the GitRepo listing every folder in install order.
func (g *Generator) buildGitRepo(folders []localformat.Folder) GitRepo {
	paths := make([]string, 0, len(folders))
	for _, f := range folders {
		paths = append(paths, f.Dir)
	}
	return GitRepo{
		APIVersion: "fleet.cattle.io/v1alpha1",
		Kind:       "GitRepo",
		Metadata:   ObjectMeta{Name: g.appName(), Namespace: g.namespace()},
		Spec: GitRepoSpec{
			Repo:    g.repoURL(),
			Branch:  g.targetRevision(),
			Paths:   paths,
			Targets: g.targets(),
		},
	}
}

// targets opts clusters in by label, or selects the Rancher local cluster in
// the fleet-local workspace.
func (g *Generator) targets() []Target {
	if g.namespace() == localNamespace {
		return []Target{{ClusterName: "local"}}
	}
	return []Target{{ClusterSelector: &ClusterSelector{
		MatchLabels: map[string]string{BundleLabel: g.appName()},
	}}}
}

// writeGitRepoLayout writes a fleet.yaml and .fleetignore into every folder
// and the root gitrepo.yaml.
func (g *Generator) writeGitRepoLayout(outputDir string, output *deployer.Output,
	folders []localformat.Folder, docs []FleetYAML) error {

	output.Entrypoint = fileGitRepo
	for i, f := range folders {
		fleetPath := path.Join(f.Dir, fileFleetYAML)
		if err := writeYAML(outputDir, fleetPath, docs[i]); err != nil {
			return err
		}
		ignorePath := path.Join(f.Dir, fileFleetIgnore)
		if err := writeFile(outputDir, ignorePath, []byte(strings.Join(ignoredFiles, "\n")+"\n")); err != nil {
			return err
		}
		for _, rel := range []string{fleetPath, ignorePath} {
			if err := addFile(output, outputDir, rel); err != nil {
				return err
			}
		}
		output.Releases[i].Manifest = fleetPath
	}
	if err := writeYAML(outputDir, fileGitRepo, g.buildGitRepo(folders)); err != nil {
		return err
	}
	return addFile(output, outputDir, fileGitRepo)
}

func bundleName(appName, release string) string {
	return appName + "-" + release
}

func checkBundleNameLen(name string) error {
	if len(name) > maxBundleNameLen {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("Fleet bundle name %q exceeds %d characters; use a shorter --app-name", name, maxBundleNameLen))
	}
	return nil
}

// plannedReleases lists the release (folder) names localformat writes for
// refs: each component, plus <name>-pre when it has pre-phase manifests and
// <name>-post when a chart-backed component has post-phase manifests.
func plannedReleases(refs []recipe.ComponentRef, pre, post map[string]map[string][]byte) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.Name)
		if len(pre[ref.Name]) > 0 {
			out = append(out, ref.Name+"-pre")
		}
		if len(post[ref.Name]) > 0 && ref.Source != "" {
			out = append(out, ref.Name+"-post")
		}
	}
	return out
}

// valuesFilesFor lists the values files localformat wrote for f, relative
// to the folder. Fleet fails on a missing values file, so only files that
// exist in f.Files are referenced.
func valuesFilesFor(f localformat.Folder) []string {
	written := make(map[string]bool, len(f.Files))
	for _, rel := range f.Files {
		if path.Dir(rel) == f.Dir {
			written[path.Base(rel)] = true
		}
	}
	var out []string
	for _, name := range []string{fileValues, fileClusterValues} {
		if written[name] {
			out = append(out, name)
		}
	}
	return out
}

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

func (g *Generator) writeReadme(outputDir string, folders []localformat.Folder, docs []FleetYAML,
	refs []recipe.ComponentRef, crdOwners []string, hasVendored bool) error {

	versions := make(map[string]string, len(refs))
	for _, r := range refs {
		v := r.Version
		if v == "" {
			v = r.Tag
		}
		versions[r.Name] = v
	}
	data := readmeData{
		BundlerVersion: deployer.NormalizeVersionWithDefault(g.Version),
		AppName:        g.appName(),
		Namespace:      g.namespace(),
		IsLocal:        g.namespace() == localNamespace,
		IsHelmOp:       g.mode() == ModeHelmOp,
		BundleLabel:    BundleLabel,
		HasDynamic:     len(g.DynamicValues) > 0,
		HasVendored:    hasVendored,
		CRDOwners:      crdOwners,
		Components:     make([]readmeComponent, 0, len(folders)),
		UpgradeNotice:  g.UpgradeNotice,
	}
	for i, f := range folders {
		data.Components = append(data.Components, readmeComponent{
			Bundle:    docs[i].Name,
			Release:   f.Name,
			Namespace: f.Namespace,
			Version:   versions[f.Parent],
		})
	}
	if _, _, err := deployer.GenerateFromTemplate(readmeTemplate, data, outputDir, fileReadme); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to write README.md")
	}
	return nil
}

func writeYAML(outputDir, rel string, doc any) error {
	var buf bytes.Buffer
	buf.WriteString("# Generated by AICR\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("encode %s", rel), err)
	}
	if err := enc.Close(); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("close %s encoder", rel), err)
	}
	return writeFile(outputDir, rel, buf.Bytes())
}

func writeFile(outputDir, rel string, content []byte) error {
	abs, err := deployer.SafeJoin(outputDir, rel)
	if err != nil {
		return err
	}
	if err := os.WriteFile(abs, content, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("write %s", rel), err)
	}
	return nil
}

func addFile(output *deployer.Output, outputDir, rel string) error {
	abs, err := deployer.SafeJoin(outputDir, rel)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			fmt.Sprintf("path escapes outputDir: %s", rel))
	}
	output.Files = append(output.Files, abs)
	if info, statErr := os.Stat(abs); statErr == nil {
		output.TotalSize += info.Size()
	}
	return nil
}
