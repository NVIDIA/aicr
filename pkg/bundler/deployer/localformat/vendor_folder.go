// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Vendored-chart folder writer. Used when --vendor-charts is on for
// any Helm-typed component (with or without raw manifests).
//
// On-disk layout:
//
//	NNN-<name>/
//	  Chart.yaml                          # wrapper, declares vendored subchart
//	  values.yaml                         # values nested under the subchart name
//	  cluster-values.yaml                 # dynamic values, also nested
//	  charts/<chart>-<version>.tgz        # vendored upstream tarball
//	  install.sh                          # same install-local-helm.sh.tmpl as #662 wrappers
//
// Mixed components (#1835) do not embed recipe-side manifests here.
// Write emits a separate NNN-<name>-post/ local-helm folder after this
// primary (same shape as the non-vendored path), so manifests are
// tracked Helm release members rather than fire-and-forget hooks.
//
// Helm at install time finds the dependencies: entry in Chart.yaml,
// resolves it from charts/ (empty repository: signals "use the adjacent
// tarball"), and merges values.yaml into the subchart's value space
// because the values are nested under the subchart name.

package localformat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// writeVendoredHelmFolder pulls the upstream chart bytes via puller and
// emits a single wrapped local-helm folder for the primary chart.
// Returns the Folder manifest (Files relative to outputDir) and the
// VendorRecord for the audit log.
//
// Recipe-side post manifests are NOT written here — the caller
// (Write) injects them via injectAuxiliaryFolder(phasePost) so they
// become a tracked <name>-post release (#1835).
//
// idx is the NNN- prefix index. ctx threads through the puller call;
// puller is REQUIRED to be non-nil — caller picks the implementation.
// stamp carries the two versions written into the wrapper Chart.yaml.
func writeVendoredHelmFolder(
	ctx context.Context,
	outputDir, dir string,
	idx int,
	c Component,
	puller ChartPuller,
	stamp chartStamp,
) (Folder, VendorRecord, error) {

	if puller == nil {
		return Folder{}, VendorRecord{}, errors.New(errors.ErrCodeInternal,
			"writeVendoredHelmFolder: puller is nil")
	}

	folderDir, err := deployer.SafeJoin(outputDir, dir)
	if err != nil {
		return Folder{}, VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			"folder path unsafe", err)
	}
	if err = os.MkdirAll(folderDir, 0o755); err != nil {
		return Folder{}, VendorRecord{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create folder %s", dir), err)
	}

	// 1. Pull upstream chart bytes. PropagateOrWrap preserves the
	// structured error code from CLIChartPuller (NOT_FOUND, UNAUTHORIZED,
	// UNAVAILABLE, ...) while wrapping any uncoded error from a future
	// puller implementation so the bundle layer doesn't leak raw exec
	// errors past this boundary.
	tgz, rec, tarball, pullErr := puller.Pull(ctx, c)
	if pullErr != nil {
		return Folder{}, VendorRecord{}, errors.PropagateOrWrap(
			pullErr,
			errors.ErrCodeInternal,
			fmt.Sprintf("pull vendored chart for component %q", c.Name),
		)
	}

	// 2. Write charts/<tarball>.
	chartsDir, err := deployer.SafeJoin(folderDir, "charts")
	if err != nil {
		return Folder{}, VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			"charts dir path unsafe", err)
	}
	if err = os.MkdirAll(chartsDir, 0o755); err != nil {
		return Folder{}, VendorRecord{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create charts dir for %s", dir), err)
	}
	tarballPath, err := deployer.SafeJoin(chartsDir, tarball)
	if err != nil {
		return Folder{}, VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("tarball path unsafe: %s", tarball), err)
	}
	if err = writeFile(tarballPath, tgz, 0o644); err != nil {
		return Folder{}, VendorRecord{}, err
	}

	// 3. Wrapper Chart.yaml.
	subchart := c.ChartName
	if subchart == "" {
		subchart = c.Name
	}
	chartYAML, err := renderWrapperChartYAML(wrapperSubchart{
		Name:         c.Name,
		Parent:       c.Name,
		ChartName:    subchart,
		ChartVersion: deployer.NormalizeVersionWithDefault(c.Version),
	}, stamp)
	if err != nil {
		return Folder{}, VendorRecord{}, err
	}
	chartPath, err := deployer.SafeJoin(folderDir, "Chart.yaml")
	if err != nil {
		return Folder{}, VendorRecord{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			"Chart.yaml path unsafe", err)
	}
	if err = writeFile(chartPath, chartYAML, 0o644); err != nil {
		return Folder{}, VendorRecord{}, err
	}

	// 4. values.yaml + cluster-values.yaml, nested under the subchart name.
	if err = writeNestedValueFiles(folderDir, c, subchart); err != nil {
		return Folder{}, VendorRecord{}, err
	}

	// 5. install.sh — reuse the same template as #662's local-helm wrappers.
	// Vendored Helm folders are always primary (pre-injection still flows
	// through writeLocalHelmFolder) and the upstream chart's own templates
	// do not declare a Namespace by recipe convention, so --create-namespace
	// is safe.
	installData := struct {
		Name            string
		Namespace       string
		CreateNamespace bool
	}{c.Name, c.Namespace, true}
	if err = renderTemplateToFile(localHelmInstallTmpl, installData, folderDir, "install.sh", 0o755); err != nil {
		return Folder{}, VendorRecord{}, err
	}

	// File list — deterministic order matching writeLocalHelmFolder.
	files := []string{
		filepath.Join(dir, "Chart.yaml"),
		filepath.Join(dir, "charts", tarball),
		filepath.Join(dir, "values.yaml"),
		filepath.Join(dir, "cluster-values.yaml"),
		filepath.Join(dir, "install.sh"),
	}

	// VendorRecord carries the audit fields plus context (folder name).
	// Force-canonicalize TarballName to the value we actually wrote
	// under charts/ so provenance.yaml can never point at a file that
	// doesn't exist on disk, even if a future puller returns the two
	// inconsistently.
	rec.Name = c.Name
	rec.TarballName = tarball

	return Folder{
		Index:     idx,
		Dir:       dir,
		Kind:      KindLocalHelm,
		Name:      c.Name,
		Namespace: c.Namespace,
		Parent:    c.Name,
		Files:     files,
		// Vendored folders wrap an upstream chart that AICR does not
		// render; the chart-owns-Namespace detection does not apply
		// here. Default to true to match the upstream-helm path.
		CreateNamespace: true,
		// Post manifests live in the injected <name>-post folder
		// (#1835), not in this primary wrapper.
		CarriesPostManifests: false,
	}, rec, nil
}

// writeNestedValueFiles writes values.yaml + cluster-values.yaml with
// the static and dynamic maps nested under subchart so Helm forwards
// them at install time. Mirrors writeValueFiles for the non-vendored
// path; extracted so the nesting transformation lives in one place.
func writeNestedValueFiles(folderDir string, c Component, subchart string) error {
	split := splitDynamicPaths(c.Values, c.DynamicPaths)

	staticNested := nestUnderSubchart(split.static, subchart)
	dynamicNested := nestUnderSubchart(split.dynamic, subchart)
	// nestUnderSubchart returns nil for empty maps so we don't emit
	// `<subchart>: {}`. WriteValuesFile is happy with nil — it writes
	// an empty document, matching the existing behavior.

	if _, _, err := deployer.WriteValuesFile(staticNested, folderDir, "values.yaml"); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write values.yaml for %s", c.Name), err)
	}
	if _, _, err := deployer.WriteValuesFile(dynamicNested, folderDir, "cluster-values.yaml"); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write cluster-values.yaml for %s", c.Name), err)
	}
	return nil
}
