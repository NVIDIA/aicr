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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
)

// Component is the per-component input for Write. Fields mirror the subset of
// pkg/bundler/deployer/helm.ComponentData that localformat needs.
type Component struct {
	Name      string
	Namespace string
	// Helm upstream ref (empty for manifest-only components)
	Repository string
	ChartName  string
	Version    string
	IsOCI      bool
	// Kustomize (empty for helm components)
	Tag  string
	Path string
	// Values hydrated by the component bundler
	Values       map[string]any
	DynamicPaths []string // paths moved from values.yaml into cluster-values.yaml
}

// Options configures Write.
type Options struct {
	OutputDir          string
	Components         []Component                  // ordered per DeploymentOrder
	ComponentManifests map[string]map[string][]byte // name → path → rendered bytes
}

// renderInputFor builds the per-component manifest.RenderInput. The Helm
// templates inside ComponentManifests reference ".Values[componentName]" and
// ".Release.Namespace" / ".Chart.{Name,Version}" — those all derive from the
// Component itself, so we construct it here rather than asking callers to
// pre-build N separate RenderInputs in lockstep with Components.
func renderInputFor(c Component) manifest.RenderInput {
	chart := c.ChartName
	if chart == "" {
		chart = c.Name
	}
	return manifest.RenderInput{
		ComponentName: c.Name,
		Namespace:     c.Namespace,
		ChartName:     chart,
		ChartVersion:  deployer.NormalizeVersionWithDefault(c.Version),
		Values:        c.Values,
	}
}

// Write emits the numbered folder layout. Deterministic and idempotent.
//
// Removes any pre-existing NNN-* folders under OutputDir before writing, so
// reusing the same --output across recipe regenerations does not leave stale
// component folders that the deployer's loop would later install. Top-level
// orchestration files (deploy.sh, undeploy.sh, README.md, attestation/) are
// left intact; only files under [0-9][0-9][0-9]-* are removed.
func Write(ctx context.Context, opts Options) ([]Folder, error) {
	// Honor cancellation before any filesystem mutation.
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
	}
	// Fail fast if the layout's three-digit prefix can't accommodate the
	// component count. Mixed components inject a second folder per
	// component, so the upper bound is 2*len(Components). The deploy/undeploy
	// templates glob [0-9][0-9][0-9]-*/, so a 4-digit prefix would be
	// silently skipped.
	if 2*len(opts.Components) > 999 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("too many components (%d): NNN- folder prefix supports at most 999 entries",
				len(opts.Components)))
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "create output dir", err)
	}
	if err := pruneStaleFolders(opts.OutputDir); err != nil {
		return nil, err
	}

	// Detect <name>-post collisions up front: if a recipe declares both a
	// mixed component "foo" (Helm + manifests) and a separate component
	// "foo-post", the injection rule would synthesize a second "foo-post"
	// folder/release that collides with the explicitly-declared one.
	declared := make(map[string]struct{}, len(opts.Components))
	for _, c := range opts.Components {
		declared[c.Name] = struct{}{}
	}
	for _, c := range opts.Components {
		if len(opts.ComponentManifests[c.Name]) == 0 {
			continue
		}
		// Mixed component (helm + manifests) → would inject "<name>-post".
		if c.Repository == "" {
			continue // manifest-only doesn't inject; already a single local-helm folder
		}
		if _, clash := declared[c.Name+"-post"]; clash {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q is mixed (helm + manifests) and would inject %q-post, but a component named %q-post is already declared in the recipe — rename one to avoid collision",
					c.Name, c.Name, c.Name))
		}
	}

	folders := make([]Folder, 0, len(opts.Components))
	idx := 1
	for _, c := range opts.Components {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
		}
		if !deployer.IsSafePathComponent(c.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q", c.Name))
		}

		// Reject kustomize + raw manifests: each recipe component must declare
		// EITHER kustomize (Tag/Path) OR raw manifests, not both. The bundle
		// shape can only wrap one primary source into the local chart.
		if (c.Tag != "" || c.Path != "") && len(opts.ComponentManifests[c.Name]) > 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q has both kustomize (Tag/Path) and raw manifests; use one", c.Name))
		}

		kind := classify(c, opts.ComponentManifests[c.Name])
		dir := fmt.Sprintf("%03d-%s", idx, c.Name)

		switch kind {
		case KindUpstreamHelm:
			f, err := writeUpstreamHelmFolder(opts.OutputDir, dir, idx, c)
			if err != nil {
				return nil, err
			}
			folders = append(folders, f)
			slog.Info("wrote local chart folder", "index", idx, "dir", dir, "kind", kind.String(), "parent", c.Name)
			idx++

			// Mixed component: upstream chart + raw manifests.
			// Emit an injected -post wrapped chart immediately after the primary so
			// raw manifests apply post-install (after helm has registered the chart's CRDs).
			// The "mixed" concept lives only here at the bundle layer — no recipe metadata involved.
			if manifests := opts.ComponentManifests[c.Name]; len(manifests) > 0 {
				postName := c.Name + "-post"
				postDir := fmt.Sprintf("%03d-%s", idx, postName)
				postFolder, postErr := writeLocalHelmFolder(
					opts.OutputDir, postDir, idx, c,
					manifests, renderInputFor(c),
					postName, c.Name,
				)
				if postErr != nil {
					return nil, postErr
				}
				folders = append(folders, postFolder)
				slog.Info("wrote local chart folder", "index", idx, "dir", postDir, "kind", KindLocalHelm.String(), "parent", c.Name)
				idx++
			}
		case KindLocalHelm:
			manifests := opts.ComponentManifests[c.Name]
			if c.Tag != "" || c.Path != "" {
				// Kustomize-typed: materialize the overlay output to a single
				// templates/manifest.yaml inside the wrapped chart.
				//
				// Path is required (kustomize needs somewhere to build from);
				// Tag is only meaningful with a git Repository. Reject the
				// incomplete combinations explicitly so a recipe author sees
				// the misconfiguration rather than a silent empty build.
				if c.Path == "" {
					return nil, errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("kustomize component %q has Tag but no Path; Path is required", c.Name))
				}
				if c.Tag != "" && c.Repository == "" {
					return nil, errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("kustomize component %q has Tag but no Repository; Tag is only meaningful with a git Repository", c.Name))
				}
				// Build target: git URL form for git-sourced kustomizations
				// (matches the original deploy.sh.tmpl convention), or local
				// filesystem path otherwise. Only append ?ref= when Tag is
				// non-empty — kustomize distinguishes `repo//path` (no ref,
				// HEAD) from `repo//path?ref=` (empty ref, error).
				target := c.Path
				if c.Repository != "" {
					target = fmt.Sprintf("%s//%s", c.Repository, c.Path)
					if c.Tag != "" {
						target += "?ref=" + c.Tag
					}
				}
				rendered, kerr := buildKustomize(ctx, target)
				if kerr != nil {
					return nil, kerr
				}
				manifests = map[string][]byte{"manifest.yaml": rendered}
			}
			f, err := writeLocalHelmFolder(opts.OutputDir, dir, idx, c,
				manifests, renderInputFor(c),
				c.Name, c.Name)
			if err != nil {
				return nil, err
			}
			folders = append(folders, f)
			slog.Info("wrote local chart folder", "index", idx, "dir", dir, "kind", kind.String(), "parent", c.Name)
			idx++
		}
	}
	return folders, nil
}

// valueSplit carries the results of splitting component values into static
// (values.yaml) and dynamic (cluster-values.yaml) maps.
type valueSplit struct {
	static  map[string]any
	dynamic map[string]any
}

// splitDynamicPaths deep-copies values and moves the named dot-paths into a
// separate dynamic map. Paths not present in values are still added to the
// dynamic map with an empty-string value so cluster-values.yaml carries the
// full set of dynamic keys for operators to fill in at install time.
//
// Unexported because lifting it into pkg/bundler/deployer would create an
// import cycle (deployer → component → checksum → deployer); keeping it
// in this leaf subpackage avoids that.
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

// classify determines the primary folder kind for a component.
func classify(c Component, manifests map[string][]byte) FolderKind {
	if c.Tag != "" || c.Path != "" {
		// Kustomize-typed — Task 9 adds actual kustomize build support.
		return KindLocalHelm
	}
	if c.Repository == "" && len(manifests) > 0 {
		return KindLocalHelm
	}
	return KindUpstreamHelm
}

// writeValueFiles writes values.yaml (static) and cluster-values.yaml (dynamic)
// into folderDir, splitting c.Values via c.DynamicPaths. Extracted because
// both per-folder writers (upstream-helm and local-helm) perform this
// identical dance; keeping it in one place preserves the single-source-of-
// truth guarantee around dynamic-values semantics.
func writeValueFiles(folderDir string, c Component) error {
	split := splitDynamicPaths(c.Values, c.DynamicPaths)
	if _, _, err := deployer.WriteValuesFile(split.static, folderDir, "values.yaml"); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write values.yaml for %s", c.Name), err)
	}
	if _, _, err := deployer.WriteValuesFile(split.dynamic, folderDir, "cluster-values.yaml"); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("write cluster-values.yaml for %s", c.Name), err)
	}
	return nil
}

// renderTemplateToFile executes tmpl against data, SafeJoin-checks the output
// path, and writes the rendered bytes with mode. Extracted because the
// render-then-write dance is repeated for install.sh in both writers and for
// Chart.yaml in the local-helm writer — three call sites, identical shape.
func renderTemplateToFile(tmpl *template.Template, data any,
	folderDir, filename string, mode os.FileMode,
) error {

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("render %s", filename), err)
	}
	outPath, err := deployer.SafeJoin(folderDir, filename)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s path unsafe", filename), err)
	}
	return writeFile(outPath, buf.Bytes(), mode)
}

// pruneStaleFolders removes pre-existing NNN-<name>/ directories under
// outputDir so a reused output directory cannot accumulate components from
// a previous recipe generation. Only directories matching the strict
// `[0-9][0-9][0-9]-*` pattern are removed; top-level orchestration files
// (deploy.sh, README.md, etc.) and any other directories are left alone.
func pruneStaleFolders(outputDir string) error {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(errors.ErrCodeInternal, "read output dir", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Must be NNN-<something>: 3 digits then a hyphen.
		if len(name) < 4 || name[3] != '-' {
			continue
		}
		ok := true
		for i := 0; i < 3; i++ {
			if name[i] < '0' || name[i] > '9' {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		full, joinErr := deployer.SafeJoin(outputDir, name)
		if joinErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "prune stale folder unsafe", joinErr)
		}
		if rmErr := os.RemoveAll(full); rmErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("remove stale folder %s", name), rmErr)
		}
	}
	return nil
}

// writeFile writes contents to path with the given mode, returning any
// error from write or close. Close errors on writable handles are captured
// so buffered-flush failures are not silently dropped (see CLAUDE.md rule).
// Returns StructuredErrors with ErrCodeInternal so callers can propagate
// with "return err" — no double-wrapping.
func writeFile(path string, contents []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("open %s", path), err)
	}
	_, writeErr := f.Write(contents)
	closeErr := f.Close()
	if writeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("write %s", path), writeErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("close %s", path), closeErr)
	}
	return nil
}
