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
//	  templates/*.yaml                    # raw manifests with post-install hooks (mixed only)
//	  install.sh                          # same install-local-helm.sh.tmpl as #662 wrappers
//
// Helm at install time finds the dependencies: entry in Chart.yaml,
// resolves it from charts/ (empty repository: signals "use the adjacent
// tarball"), and merges values.yaml into the subchart's value space
// because the values are nested under the subchart name.

package localformat

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
)

// writeVendoredHelmFolder pulls the upstream chart bytes via puller and
// emits a single wrapped local-helm folder. Returns the Folder manifest
// (Files relative to outputDir) and the VendorRecord for the audit log.
//
// idx is the NNN- prefix index. ctx threads through the puller call;
// puller is REQUIRED to be non-nil — caller picks the implementation.
//
// For mixed components (recipe-side raw manifests present), each
// manifest doc is rendered via manifest.Render (so chart-aware
// templating still resolves) and then mutated by injectPostInstallHooks
// so it deploys after the vendored subchart's resources.
func writeVendoredHelmFolder(
	ctx context.Context,
	outputDir, dir string,
	idx int,
	c Component,
	manifests map[string][]byte,
	puller ChartPuller,
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
	chartYAML, err := renderWrapperChartYAML(c.Name, c.Name, subchart, deployer.NormalizeVersionWithDefault(c.Version))
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

	// 5. Mixed components: emit recipe-side raw manifests as templates/
	//    with post-install hook annotations. Pure Helm (no manifests)
	//    skips the templates/ directory entirely.
	templateRelPaths, err := writeMixedManifests(ctx, folderDir, dir, c, manifests)
	if err != nil {
		return Folder{}, VendorRecord{}, err
	}

	// 6. install.sh — reuse the same template as #662's local-helm wrappers.
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
	files := make([]string, 0, 5+len(templateRelPaths))
	files = append(files,
		filepath.Join(dir, "Chart.yaml"),
		filepath.Join(dir, "charts", tarball),
		filepath.Join(dir, "values.yaml"),
		filepath.Join(dir, "cluster-values.yaml"),
	)
	files = append(files, templateRelPaths...)
	files = append(files, filepath.Join(dir, "install.sh"))

	// VendorRecord carries the audit fields plus context (folder name).
	// Force-canonicalize TarballName to the value we actually wrote
	// under charts/ so provenance.yaml can never point at a file that
	// doesn't exist on disk, even if a future puller returns the two
	// inconsistently.
	rec.Name = c.Name
	rec.TarballName = tarball

	return Folder{
		Index:  idx,
		Dir:    dir,
		Kind:   KindLocalHelm,
		Name:   c.Name,
		Parent: c.Name,
		Files:  files,
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

// writeMixedManifests renders recipe-side raw manifests through the
// usual manifest.Render pipeline and then injects post-install hook
// annotations so they deploy after the vendored subchart. Returns the
// list of written file paths relative to outputDir, suitable for
// inclusion in the Folder.Files slice.
//
// Returns an empty slice if there are no manifests (pure-Helm vendored
// component).
//
// ctx is checked at the top of each per-manifest iteration so a
// caller-initiated cancel aborts promptly even for large manifest sets.
func writeMixedManifests(ctx context.Context, folderDir, dir string, c Component, manifests map[string][]byte) ([]string, error) {
	if len(manifests) == 0 {
		return nil, nil
	}

	templatesDir, err := deployer.SafeJoin(folderDir, "templates")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"templates dir path unsafe", err)
	}
	if err = os.MkdirAll(templatesDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create templates dir for %s", dir), err)
	}

	renderInput := renderInputForVendored(c)

	sortedPaths := make([]string, 0, len(manifests))
	for p := range manifests {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	seen := make(map[string]string, len(sortedPaths))
	out := make([]string, 0, len(sortedPaths))
	for _, p := range sortedPaths {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled during manifest write", err)
		}
		baseName := filepath.Base(p)
		if prev, ok := seen[baseName]; ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("manifest basename collision in component %q: %q and %q both resolve to %q",
					c.Name, prev, p, baseName))
		}
		seen[baseName] = p

		rendered, rerr := manifest.Render(manifests[p], renderInput)
		if rerr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("render manifest %s for %s", p, c.Name), rerr)
		}
		if !hasYAMLObjects(rendered) {
			slog.Debug("skipping empty manifest after render",
				"component", c.Name, "manifest", baseName)
			continue
		}

		hooked, herr := injectPostInstallHooks(rendered)
		if herr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("inject post-install hook for %s in %s", baseName, c.Name), herr)
		}

		outPath, jerr := deployer.SafeJoin(templatesDir, baseName)
		if jerr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("template file path unsafe: %s", baseName), jerr)
		}
		if werr := writeFile(outPath, hooked, 0o644); werr != nil {
			return nil, werr
		}
		out = append(out, filepath.Join(dir, "templates", baseName))
	}
	return out, nil
}

// renderInputForVendored is the same shape as renderInputFor but uses
// the upstream chart name (post-vendoring) so manifest.Render's
// .Chart.Name reference points at the vendored subchart, not the
// wrapper. Callers in the non-vendored path should keep using
// renderInputFor.
func renderInputForVendored(c Component) manifest.RenderInput {
	chart := c.ChartName
	if chart == "" {
		chart = c.Name
	}
	return manifest.RenderInput{
		ComponentName: c.Name,
		Namespace:     c.Namespace,
		ChartName:     chart,
		ChartVersion:  deployer.NormalizeVersionWithDefault(c.Version),
		Values:        component.DeepCopyMap(c.Values),
	}
}
