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

package localformat

import (
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
)

// hasYAMLObjects returns true if content contains at least one YAML object
// (a non-comment, non-blank, non-separator line). Used to skip writing
// fully-conditional manifests that rendered to nothing once values were
// applied. Mirrors the helper that lived in the old helm deployer.
func hasYAMLObjects(content []byte) bool {
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}
		return true
	}
	return false
}

//go:embed templates/install-local-helm.sh.tmpl templates/chart.yaml.tmpl
var localHelmTemplates embed.FS

var (
	localHelmInstallTmpl = template.Must(
		template.ParseFS(localHelmTemplates, "templates/install-local-helm.sh.tmpl"),
	)
	localHelmChartTmpl = template.Must(
		template.ParseFS(localHelmTemplates, "templates/chart.yaml.tmpl"),
	)
)

// writeLocalHelmFolder writes Chart.yaml + templates/* + values.yaml +
// cluster-values.yaml + install.sh into outputDir/dir. name is the folder
// and release name ("<name>" for primary, "<name>-post" for injected mixed);
// parent is the originating component name (== name for primary). manifests
// is the per-path rendered bytes map for this folder; may be empty (a
// manifest-only component with no manifests still yields a valid empty
// templates/ directory). Returns the Folder manifest with Files listed in
// deterministic order.
func writeLocalHelmFolder(
	outputDir, dir string, idx int, c Component,
	manifests map[string][]byte, renderInput manifest.RenderInput,
	name, parent string,
) (Folder, error) {

	folderDir, err := deployer.SafeJoin(outputDir, dir)
	if err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInvalidRequest, "folder path unsafe", err)
	}
	if err = os.MkdirAll(folderDir, 0o755); err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create folder %s", dir), err)
	}
	templatesDir, err := deployer.SafeJoin(folderDir, "templates")
	if err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInvalidRequest, "templates dir path unsafe", err)
	}
	if err = os.MkdirAll(templatesDir, 0o755); err != nil {
		return Folder{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("create templates dir for %s", dir), err)
	}

	// Chart.yaml
	chartData := struct {
		Name   string
		Parent string
	}{name, parent}
	if err = renderTemplateToFile(localHelmChartTmpl, chartData, folderDir, "Chart.yaml", 0o644); err != nil {
		return Folder{}, err
	}

	// values.yaml + cluster-values.yaml
	if err = writeValueFiles(folderDir, c); err != nil {
		return Folder{}, err
	}

	// templates/* from manifests (sorted for determinism; basename-collision is
	// an error so two manifests with the same file name cannot silently overwrite).
	sortedPaths := make([]string, 0, len(manifests))
	for p := range manifests {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)

	seen := make(map[string]string, len(sortedPaths))
	templateRelPaths := make([]string, 0, len(sortedPaths))
	for _, p := range sortedPaths {
		baseName := filepath.Base(p)
		if prev, ok := seen[baseName]; ok {
			return Folder{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("manifest basename collision in component %q: %q and %q both resolve to %q",
					c.Name, prev, p, baseName))
		}
		seen[baseName] = p

		rendered, rerr := manifest.Render(manifests[p], renderInput)
		if rerr != nil {
			return Folder{}, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("render manifest %s for %s", p, c.Name), rerr)
		}
		// Skip writing if the rendered output has no YAML objects (only
		// comments / blanks / separators) — typical for fully-conditional
		// manifests when the relevant value was set false at bundle time.
		// Mirrors the OLD helm deployer's hasYAMLObjects check.
		if !hasYAMLObjects(rendered) {
			slog.Debug("skipping empty manifest after render",
				"component", c.Name, "manifest", baseName)
			continue
		}
		outPath, jerr := deployer.SafeJoin(templatesDir, baseName)
		if jerr != nil {
			return Folder{}, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("template file path unsafe: %s", baseName), jerr)
		}
		if werr := writeFile(outPath, rendered, 0o644); werr != nil {
			return Folder{}, werr
		}
		templateRelPaths = append(templateRelPaths, filepath.Join(dir, "templates", baseName))
	}

	// install.sh
	installData := struct {
		Name      string
		Namespace string
	}{name, c.Namespace}
	if err = renderTemplateToFile(localHelmInstallTmpl, installData, folderDir, "install.sh", 0o755); err != nil {
		return Folder{}, err
	}

	files := make([]string, 0, 3+len(templateRelPaths)+1)
	files = append(files,
		filepath.Join(dir, "Chart.yaml"),
		filepath.Join(dir, "values.yaml"),
		filepath.Join(dir, "cluster-values.yaml"),
	)
	files = append(files, templateRelPaths...)
	files = append(files, filepath.Join(dir, "install.sh"))

	return Folder{
		Index:  idx,
		Dir:    dir,
		Kind:   KindLocalHelm,
		Name:   name,
		Parent: parent,
		Files:  files,
	}, nil
}
