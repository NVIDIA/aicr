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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	k8syaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/component"
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
	ValuesFrom      []ValuesFromRef // ConfigMap references for dynamic values
	ValuesYAML      string          // Pre-rendered, indented YAML for spec.values
}

// ValuesFromRef is a Flux valuesFrom reference to a ConfigMap.
type ValuesFromRef struct {
	Name string
}

// ConfigMapData carries data for the configmap-values.yaml template.
type ConfigMapData struct {
	Name       string
	ValuesYAML string // Indented YAML for the data.values.yaml field
}

// valueSplit carries the results of splitting component values into static
// (inline spec.values) and dynamic (ConfigMap) maps.
type valueSplit struct {
	static  map[string]any
	dynamic map[string]any
}

// splitDynamicPaths deep-copies values and moves the named dot-paths into a
// separate dynamic map. Paths not present in values are still added to the
// dynamic map with an empty-string value so the ConfigMap carries the full set
// of dynamic keys for operators to fill in at install time.
//
// This is the same algorithm as localformat/writer.go's splitDynamicPaths,
// duplicated here to avoid an import cycle (deployer → component → checksum → deployer).
func splitDynamicPaths(values map[string]any, dynamicPaths []string) valueSplit {
	static := component.DeepCopyMap(values)
	dynamic := make(map[string]any)
	for _, path := range dynamicPaths {
		val, found := component.GetValueByPath(static, path)
		if found {
			component.RemoveValueByPath(static, path)
		} else {
			val = ""
		}
		component.SetValueByPath(dynamic, path, val)
	}
	return valueSplit{static: static, dynamic: dynamic}
}

// HelmRepoSourceData carries data for HelmRepository source CRs.
type HelmRepoSourceData struct {
	Name  string
	URL   string
	IsOCI bool
}

// generateHelmComponent writes the HelmRelease with inline values for a Helm
// component. When dynamic paths are configured, it splits values into static
// (inline) and dynamic (ConfigMap) and returns true to signal a ConfigMap was
// written alongside the HelmRelease.
func (g *Generator) generateHelmComponent(ref recipe.ComponentRef, compDir string,
	dependsOn []DependsOnRef, helmSources map[string]*HelmRepoSourceData, output *deployer.Output) (bool, error) {

	sName := helmSourceName(ref.Source, helmSources)
	values := g.ComponentValues[ref.Name]
	dynamicPaths := g.DynamicValues[ref.Name]

	var valuesFrom []ValuesFromRef
	var wroteConfigMap bool

	// Split dynamic paths into a ConfigMap when configured.
	if len(dynamicPaths) > 0 {
		split := splitDynamicPaths(values, dynamicPaths)
		values = split.static

		cmName, cmErr := writeConfigMap(ref.Name, split.dynamic, compDir, output)
		if cmErr != nil {
			return false, cmErr
		}
		valuesFrom = []ValuesFromRef{{Name: cmName}}
		wroteConfigMap = true
	}

	// Marshal values to YAML (2-space indent) and indent 4 spaces for embedding under spec.values.
	var valuesYAML string
	if len(values) > 0 {
		yamlBytes, marshalErr := k8syaml.Marshal(values)
		if marshalErr != nil {
			return false, errors.Wrap(errors.ErrCodeInternal,
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
		ValuesFrom:      valuesFrom,
		ValuesYAML:      valuesYAML,
	}

	if err := writeTemplate(output, helmReleaseTemplate, data, compDir, fileHelmRelease,
		fmt.Sprintf("failed to write %s for %s", fileHelmRelease, ref.Name)); err != nil {
		return false, err
	}
	return wroteConfigMap, nil
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
// Returns true when a ConfigMap was written for dynamic values.
func (g *Generator) generateManifestHelmChart(compName, dirName, namespace, compDir string,
	manifests map[string][]byte, gitSources map[string]*GitRepoSourceData,
	dependsOn []DependsOnRef, output *deployer.Output) (bool, error) {

	// Create templates/ subdirectory for manifest files.
	templatesDir, err := deployer.SafeJoin(compDir, "templates")
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(templatesDir, 0750); err != nil {
		return false, errors.Wrap(errors.ErrCodeInternal,
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
		safeName := filepath.Clean(name)
		filePath, joinErr := deployer.SafeJoin(templatesDir, safeName)
		if joinErr != nil {
			return false, joinErr
		}
		if err := os.MkdirAll(filepath.Dir(filePath), 0750); err != nil {
			return false, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create manifest subdirectory for %s/%s", compName, safeName), err)
		}
		if err := os.WriteFile(filePath, content, 0600); err != nil {
			return false, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write template %s for %s", safeName, compName), err)
		}
		output.Files = append(output.Files, filePath)
		output.TotalSize += int64(len(content))
	}

	// Write Chart.yaml.
	if err := writeTemplate(output, chartTemplate, ChartData{Name: dirName, Version: "0.1.0"},
		compDir, fileChart,
		fmt.Sprintf("failed to write %s for %s", fileChart, compName)); err != nil {
		return false, err
	}

	// Marshal values for inline embedding in HelmRelease.
	// Manifest templates access values via `index .Values "<compName>"`, so wrap
	// the flat values map under the component name key to match that expectation.
	values := g.ComponentValues[compName]
	dynamicPaths := g.DynamicValues[compName]

	var valuesFrom []ValuesFromRef
	var wroteConfigMap bool

	// Split dynamic paths into a ConfigMap when configured.
	// Dynamic paths are split before wrapping under the component name key,
	// then both halves are wrapped to match the template expectation.
	if len(dynamicPaths) > 0 {
		split := splitDynamicPaths(values, dynamicPaths)
		values = split.static

		// Wrap dynamic values under the component name key for manifest template compatibility.
		wrappedDynamic := map[string]any{compName: split.dynamic}
		cmName, cmErr := writeConfigMap(dirName, wrappedDynamic, compDir, output)
		if cmErr != nil {
			return false, cmErr
		}
		valuesFrom = []ValuesFromRef{{Name: cmName}}
		wroteConfigMap = true
	}

	var valuesYAML string
	if len(values) > 0 {
		wrapped := map[string]any{compName: values}
		yamlBytes, marshalErr := k8syaml.Marshal(wrapped)
		if marshalErr != nil {
			return false, errors.Wrap(errors.ErrCodeInternal,
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
		ValuesFrom:      valuesFrom,
		ValuesYAML:      valuesYAML,
	}

	if err := writeTemplate(output, helmReleaseTemplate, data, compDir, fileHelmRelease,
		fmt.Sprintf("failed to write %s for %s", fileHelmRelease, compName)); err != nil {
		return false, err
	}
	return wroteConfigMap, nil
}

// generateVendoredHelmComponent writes a vendored wrapper chart folder for a
// Helm component and generates a HelmRelease CR pointing to it via
// GitRepository. For mixed components (Helm + manifests), manifest templates
// are included in the wrapper with post-install hook annotations so they
// install after the subchart's resources.
// Returns (wroteConfigMap, vendorRecord, error).
func (g *Generator) generateVendoredHelmComponent(ctx context.Context, ref recipe.ComponentRef,
	compDir string, dependsOn []DependsOnRef,
	gitSources map[string]*GitRepoSourceData,
	puller localformat.ChartPuller,
	output *deployer.Output) (bool, localformat.VendorRecord, error) {

	chartName := ref.Chart
	if chartName == "" {
		chartName = ref.Name
	}

	// Build localformat.Component for the puller.
	lfc := localformat.Component{
		Name:       ref.Name,
		Repository: ref.Source,
		ChartName:  chartName,
		Version:    ref.Version,
		IsOCI:      strings.HasPrefix(ref.Source, "oci://"),
	}

	// Pull upstream chart tarball.
	tgz, rec, tarball, pullErr := puller.Pull(ctx, lfc)
	if pullErr != nil {
		return false, localformat.VendorRecord{}, errors.PropagateOrWrap(
			pullErr, errors.ErrCodeInternal,
			fmt.Sprintf("pull vendored chart for component %q", ref.Name))
	}

	// Write charts/<tarball>.tgz.
	chartsDir, err := deployer.SafeJoin(compDir, "charts")
	if err != nil {
		return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			"charts dir path unsafe", err)
	}
	if err = os.MkdirAll(chartsDir, 0750); err != nil {
		return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create charts dir for %s", ref.Name), err)
	}
	tarballPath, err := deployer.SafeJoin(chartsDir, tarball)
	if err != nil {
		return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("tarball path unsafe: %s", tarball), err)
	}
	if err = os.WriteFile(tarballPath, tgz, 0600); err != nil {
		return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write tarball for %s", ref.Name), err)
	}
	output.Files = append(output.Files, tarballPath)
	output.TotalSize += int64(len(tgz))

	// Write wrapper Chart.yaml declaring the upstream as a dependency.
	version := deployer.NormalizeVersionWithDefault(ref.Version)
	wrapperYAML, err := localformat.RenderWrapperChartYAML(ref.Name, ref.Name, chartName, version)
	if err != nil {
		return false, localformat.VendorRecord{}, err
	}
	chartPath, err := deployer.SafeJoin(compDir, fileChart)
	if err != nil {
		return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			"Chart.yaml path unsafe", err)
	}
	if err = os.WriteFile(chartPath, wrapperYAML, 0600); err != nil {
		return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write Chart.yaml for %s", ref.Name), err)
	}
	output.Files = append(output.Files, chartPath)
	output.TotalSize += int64(len(wrapperYAML))

	// Build values nested under the subchart name so the wrapper chart
	// forwards them to the vendored dependency at install time.
	values := g.ComponentValues[ref.Name]
	dynamicPaths := g.DynamicValues[ref.Name]

	var valuesFrom []ValuesFromRef
	var wroteConfigMap bool

	if len(dynamicPaths) > 0 {
		split := splitDynamicPaths(values, dynamicPaths)
		values = split.static

		wrappedDynamic := localformat.NestUnderSubchart(split.dynamic, chartName)
		cmName, cmErr := writeConfigMap(ref.Name, wrappedDynamic, compDir, output)
		if cmErr != nil {
			return false, localformat.VendorRecord{}, cmErr
		}
		valuesFrom = []ValuesFromRef{{Name: cmName}}
		wroteConfigMap = true
	}

	var valuesYAML string
	nestedValues := localformat.NestUnderSubchart(values, chartName)
	if len(nestedValues) > 0 {
		yamlBytes, marshalErr := k8syaml.Marshal(nestedValues)
		if marshalErr != nil {
			return false, localformat.VendorRecord{}, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to marshal values for %s", ref.Name), marshalErr)
		}
		valuesYAML = "    " + strings.ReplaceAll(strings.TrimRight(string(yamlBytes), "\n"), "\n", "\n    ")
	}

	// Write HelmRelease referencing GitRepository (vendored chart is local).
	sName := gitSourceName(g.resolveRepoURL(), gitSources)
	data := HelmReleaseData{
		Name:            ref.Name,
		TargetNamespace: ref.Namespace,
		Chart:           "./" + ref.Name,
		SourceKind:      "GitRepository",
		SourceName:      sName,
		DependsOn:       dependsOn,
		ValuesFrom:      valuesFrom,
		ValuesYAML:      valuesYAML,
	}

	if err := writeTemplate(output, helmReleaseTemplate, data, compDir, fileHelmRelease,
		fmt.Sprintf("failed to write %s for %s", fileHelmRelease, ref.Name)); err != nil {
		return false, localformat.VendorRecord{}, err
	}

	return wroteConfigMap, rec, nil
}

// writeConfigMap writes a ConfigMap YAML file containing the given dynamic
// values into compDir and returns the ConfigMap name for valuesFrom wiring.
func writeConfigMap(compName string, dynamicValues map[string]any, compDir string, output *deployer.Output) (string, error) {
	cmName := compName + "-values"

	var valuesYAML string
	if len(dynamicValues) > 0 {
		yamlBytes, err := k8syaml.Marshal(dynamicValues)
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to marshal dynamic values for %s", compName), err)
		}
		valuesYAML = "    " + strings.ReplaceAll(strings.TrimRight(string(yamlBytes), "\n"), "\n", "\n    ")
	}

	data := ConfigMapData{
		Name:       cmName,
		ValuesYAML: valuesYAML,
	}

	if err := writeTemplate(output, configMapTemplate, data, compDir, fileConfigMap,
		fmt.Sprintf("failed to write %s for %s", fileConfigMap, compName)); err != nil {
		return "", err
	}

	return cmName, nil
}
