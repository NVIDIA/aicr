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

// Command bom renders every Helm chart in recipes/registry.yaml at its pinned
// version and emits a CycloneDX 1.6 JSON BOM plus a Markdown summary listing
// every container image AICR can deploy.
//
// Usage: bom -repo-root <path> -out-dir <path>
//
// Outputs:
//
//	<out-dir>/bom.cdx.json
//	<out-dir>/bom.md
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	defaultHelmTimeout = 90 * time.Second
	kindHelm           = "helm"
)

func main() {
	var (
		repoRoot    string
		outDir      string
		aicrVersion string
		skipHelm    bool
		strict      bool
	)
	flag.StringVar(&repoRoot, "repo-root", ".", "path to the AICR repository root")
	flag.StringVar(&outDir, "out-dir", "dist/bom", "directory to write bom.cdx.json and bom.md")
	flag.StringVar(&aicrVersion, "aicr-version", "dev", "AICR version label embedded in the BOM")
	flag.BoolVar(&skipHelm, "skip-helm", false, "skip helm template rendering (only walk embedded manifests)")
	flag.BoolVar(&strict, "strict", false, "fail if any component fails to render or is missing a pinned chart version")
	flag.Parse()

	if err := run(repoRoot, outDir, aicrVersion, skipHelm, strict); err != nil {
		fmt.Fprintln(os.Stderr, "bom:", err)
		os.Exit(1)
	}
}

func run(repoRoot, outDir, aicrVersion string, skipHelm, strict bool) error {
	registryPath := filepath.Join(repoRoot, "recipes", "registry.yaml")
	reg, err := loadRegistry(registryPath)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "load registry", err)
	}

	if mkErr := os.MkdirAll(outDir, 0o755); mkErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir out-dir", mkErr)
	}

	results := make([]bom.ComponentResult, 0, len(reg.Components))
	for _, c := range reg.Components {
		results = append(results, surveyComponent(repoRoot, c, skipHelm))
	}

	if strict {
		var hardErrs []string
		for _, r := range results {
			if r.Type == kindHelm && r.Version == "" {
				hardErrs = append(hardErrs, fmt.Sprintf("%s: chart version is not pinned", r.Name))
			}
			for _, w := range r.Warnings {
				hardErrs = append(hardErrs, r.Name+": "+w)
			}
		}
		if len(hardErrs) > 0 {
			sort.Strings(hardErrs)
			for _, e := range hardErrs {
				fmt.Fprintln(os.Stderr, "strict:", e)
			}
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("strict mode: %d issues", len(hardErrs)))
		}
	}

	doc := bom.BuildBOM(bom.Metadata{
		Name:        "aicr",
		Version:     aicrVersion,
		Description: "NVIDIA AI Cluster Runtime",
		ToolName:    "aicr-bom",
		ToolVersion: aicrVersion,
	}, results)

	jsonPath := filepath.Join(outDir, "bom.cdx.json")
	jf, err := os.Create(jsonPath) //nolint:gosec // outDir is operator-supplied
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create "+jsonPath, err)
	}
	enc := cdx.NewBOMEncoder(jf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	encErr := enc.EncodeVersion(doc, cdx.SpecVersion1_6)
	closeErr := jf.Close()
	if encErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "encode cyclonedx", encErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+jsonPath, closeErr)
	}

	mdPath := filepath.Join(outDir, "bom.md")
	mf, err := os.Create(mdPath) //nolint:gosec // outDir is operator-supplied
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create "+mdPath, err)
	}
	mdErr := bom.WriteMarkdown(mf, bom.Metadata{
		Name:        "aicr",
		Version:     aicrVersion,
		Description: "NVIDIA AI Cluster Runtime",
	}, results)
	closeErr = mf.Close()
	if mdErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "render markdown", mdErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+mdPath, closeErr)
	}

	totalImages := 0
	for _, r := range results {
		totalImages += len(r.Images)
	}
	fmt.Printf("bom: wrote %s and %s (%d components, %d image refs)\n",
		jsonPath, mdPath, len(results), totalImages)
	return nil
}

// renderHelmComponent shells out to `helm template` for c. The timeout
// context is scoped to this call so its associated timer is canceled before
// the manifests walk begins, regardless of how many components are surveyed.
func renderHelmComponent(repoRoot string, c component) ([]byte, []string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultHelmTimeout)
	defer cancel()

	valuesPath := componentValuesPath(repoRoot, c.Name)
	if _, err := os.Stat(valuesPath); err != nil {
		valuesPath = ""
	}

	var warnings []string
	out, err := renderChart(ctx, c, valuesPath)
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	return out, warnings
}

// surveyComponent renders the component's chart (if any) and walks its
// embedded manifests directory, returning the union of image refs.
func surveyComponent(repoRoot string, c component, skipHelm bool) bom.ComponentResult {
	res := bom.ComponentResult{
		Name:        c.Name,
		DisplayName: c.DisplayName,
		Type:        c.kind(),
		Repository:  c.Helm.DefaultRepository,
		Chart:       c.Helm.DefaultChart,
		Version:     c.Helm.DefaultVersion,
		Namespace:   c.Helm.DefaultNamespace,
		Pinned:      c.Helm.DefaultVersion != "",
	}

	images := map[string]struct{}{}

	if c.kind() == kindHelm && !skipHelm {
		out, warnings := renderHelmComponent(repoRoot, c)
		res.Warnings = append(res.Warnings, warnings...)
		if len(out) > 0 {
			imgs, parseErr := bom.ExtractImagesFromYAML(out)
			if parseErr != nil {
				res.Warnings = append(res.Warnings, "parse rendered yaml: "+parseErr.Error())
			}
			for _, i := range imgs {
				images[i] = struct{}{}
			}
		}
	}

	manifestsDir := componentManifestsDir(repoRoot, c.Name)
	if info, err := os.Stat(manifestsDir); err == nil && info.IsDir() {
		walkErr := filepath.WalkDir(manifestsDir, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			data, rerr := os.ReadFile(path) //nolint:gosec // path is bounded by manifestsDir under the repo root
			if rerr != nil {
				return rerr
			}
			imgs, perr := bom.ExtractImagesFromYAML(data)
			if perr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("parse %s: %v", path, perr))
				return nil
			}
			for _, i := range imgs {
				images[i] = struct{}{}
			}
			return nil
		})
		if walkErr != nil {
			res.Warnings = append(res.Warnings, "walk manifests: "+walkErr.Error())
		}
	}

	res.Images = make([]string, 0, len(images))
	for i := range images {
		res.Images = append(res.Images, i)
	}
	sort.Strings(res.Images)
	return res
}
