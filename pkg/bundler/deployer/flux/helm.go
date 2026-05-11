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

package flux

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	k8syaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// HelmReleaseData carries per-component data for the helmrelease.yaml template.
type HelmReleaseData struct {
	Name            string
	TargetNamespace string
	Chart           string
	Version         string
	SourceKind      string // "HelmRepository" or "GitRepository"
	SourceName      string
	DependsOn       []DependsOnRef
	ValuesYAML      string // Pre-rendered, indented YAML for spec.values
}

// HelmRepoSourceData carries data for HelmRepository source CRs.
type HelmRepoSourceData struct {
	Name  string
	URL   string
	IsOCI bool
}

// generateHelmComponent writes the HelmRelease with inline values for a Helm component.
func (g *Generator) generateHelmComponent(ref recipe.ComponentRef, compDir string,
	dependsOn []DependsOnRef, helmSources map[string]*HelmRepoSourceData, output *deployer.Output) error {

	sName := helmSourceName(ref.Source, helmSources)

	// Marshal values to YAML (2-space indent) and indent 4 spaces for embedding under spec.values.
	var valuesYAML string
	values := g.ComponentValues[ref.Name]
	if len(values) > 0 {
		yamlBytes, marshalErr := k8syaml.Marshal(values)
		if marshalErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to marshal values for %s", ref.Name), marshalErr)
		}
		valuesYAML = "    " + strings.ReplaceAll(strings.TrimRight(string(yamlBytes), "\n"), "\n", "\n    ")
	}

	// OCI tags are literal — helm push preserves the recipe's "v1.3.0"
	// verbatim, so stripping the v prefix produces a tag that does not
	// exist in the registry. HTTPS Helm repos use index.yaml with SemVer
	// matching, so normalize only there.
	version := ref.Version
	if !strings.HasPrefix(ref.Source, "oci://") {
		version = deployer.NormalizeVersion(version)
	}

	data := HelmReleaseData{
		Name:            ref.Name,
		TargetNamespace: ref.Namespace,
		Chart:           ref.Chart,
		Version:         version,
		SourceKind:      "HelmRepository",
		SourceName:      sName,
		DependsOn:       dependsOn,
		ValuesYAML:      valuesYAML,
	}

	return writeTemplate(output, helmReleaseTemplate, data, compDir, fileHelmRelease,
		fmt.Sprintf("failed to write %s for %s", fileHelmRelease, ref.Name))
}

// ChartData carries data for generating a local Chart.yaml.
type ChartData struct {
	Name    string
	Version string
}

// generateManifestHelmChart packages manifest templates as a local Helm chart
// in the Git repo and writes a HelmRelease CR pointing to it via GitRepository.
// Manifest files are Helm templates (with {{ .Values }}, {{ .Release }}, etc.)
// that Flux's Helm controller renders natively at deploy time.
func (g *Generator) generateManifestHelmChart(compName, dirName, namespace, compDir string,
	manifests map[string][]byte, gitSources map[string]*GitRepoSourceData,
	dependsOn []DependsOnRef, output *deployer.Output) error {

	// Create templates/ subdirectory for manifest files.
	templatesDir, err := deployer.SafeJoin(compDir, "templates")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(templatesDir, 0750); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create templates directory for %s", compName), err)
	}

	// Write manifest files into templates/ in sorted order for determinism.
	manifestNames := make([]string, 0, len(manifests))
	for name := range manifests {
		manifestNames = append(manifestNames, name)
	}
	sort.Strings(manifestNames)

	for _, name := range manifestNames {
		content := manifests[name]
		safeName := filepath.Base(name)
		filePath, joinErr := deployer.SafeJoin(templatesDir, safeName)
		if joinErr != nil {
			return joinErr
		}
		if err := os.WriteFile(filePath, content, 0600); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write template %s for %s", safeName, compName), err)
		}
		output.Files = append(output.Files, filePath)
		output.TotalSize += int64(len(content))
	}

	// Write Chart.yaml.
	if err := writeTemplate(output, chartTemplate, ChartData{Name: dirName, Version: "0.1.0"},
		compDir, fileChart,
		fmt.Sprintf("failed to write %s for %s", fileChart, compName)); err != nil {
		return err
	}

	// Marshal values for inline embedding in HelmRelease.
	var valuesYAML string
	values := g.ComponentValues[compName]
	if len(values) > 0 {
		yamlBytes, marshalErr := k8syaml.Marshal(values)
		if marshalErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to marshal values for %s", compName), marshalErr)
		}
		valuesYAML = "    " + strings.ReplaceAll(strings.TrimRight(string(yamlBytes), "\n"), "\n", "\n    ")
	}

	// Write HelmRelease CR pointing to this local chart via GitRepository.
	sName := gitSourceName(g.resolveRepoURL(), gitSources)
	data := HelmReleaseData{
		Name:            dirName,
		TargetNamespace: namespace,
		Chart:           "./" + dirName,
		SourceKind:      "GitRepository",
		SourceName:      sName,
		DependsOn:       dependsOn,
		ValuesYAML:      valuesYAML,
	}

	return writeTemplate(output, helmReleaseTemplate, data, compDir, fileHelmRelease,
		fmt.Sprintf("failed to write %s for %s", fileHelmRelease, compName))
}
