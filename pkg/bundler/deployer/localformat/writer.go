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
	"io"
	"log/slog"
	"os"
	"text/template"

	stderrors "errors"

	"gopkg.in/yaml.v3"

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

// WriteResult is the typed return shape from Write. Callers consume
// Folders for per-component bookkeeping (checksums, output files) and
// VendoredCharts to emit provenance.yaml or other audit artifacts.
//
// Returned as a struct (rather than (slices..., error)) so future
// additions — e.g., per-folder warnings, partial-failure detail — do
// not break the call signature for every downstream consumer.
type WriteResult struct {
	// Folders is one entry per emitted NNN-<name>/ directory, in
	// deployment order. Files within each Folder are relative to the
	// Write call's OutputDir.
	Folders []Folder

	// VendoredCharts is non-empty only when Options.VendorCharts was
	// set; one record per upstream chart pulled into the bundle. Pass
	// directly to WriteProvenance to emit the audit log.
	VendoredCharts []VendorRecord
}

// Options configures Write.
type Options struct {
	OutputDir  string
	Components []Component // ordered per DeploymentOrder
	// ComponentPreManifests maps component name → manifest path → rendered
	// bytes for manifests that should apply BEFORE each component's primary
	// chart. Populated from ComponentRef.PreManifestFiles. The writer does
	// not yet emit pre-phase folders — Task 4 wires the pre-injection
	// branch; for now the map is threaded through but unread.
	ComponentPreManifests map[string]map[string][]byte
	// ComponentPostManifests maps component name → manifest path → rendered
	// bytes for manifests that should apply AFTER each component's primary
	// chart. Populated from ComponentRef.ManifestFiles. Drives the existing
	// -post injection for mixed components and the template contents for
	// manifest-only wrapped charts.
	ComponentPostManifests map[string]map[string][]byte

	// VendorCharts pulls upstream Helm chart bytes into each Helm-typed
	// component's folder at bundle time. When set, every Helm component
	// emits a single wrapped folder with charts/<chart>-<version>.tgz +
	// wrapper Chart.yaml + (for mixed components) post-install-hook
	// templates. Mixed components no longer split into primary + -post.
	// Off by default — non-vendored bundles preserve the upstream
	// CVE-yank fail-loud signal.
	VendorCharts bool

	// Puller fetches upstream chart bytes when VendorCharts is set. nil
	// is allowed and resolves to a default *CLIChartPuller; tests can
	// inject a stub here without touching package state. Ignored when
	// VendorCharts is false.
	Puller ChartPuller
}

// renderInputFor builds the per-component manifest.RenderInput. The Helm
// templates inside ComponentPreManifests/ComponentPostManifests reference
// ".Values[componentName]" and ".Release.Namespace" / ".Chart.{Name,Version}"
// — those all derive from the Component itself, so we construct it here
// rather than asking callers to pre-build N separate RenderInputs in
// lockstep with Components.
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

// injectionPhase selects whether injectAuxiliaryFolder reads pre- or
// post-phase manifests from Options. Pre folders apply before the
// primary chart (sync-wave N-1 in Argo CD / install step N-1 in Helm),
// post folders apply after (sync-wave N+1 / step N+1). Both phases
// share a single emission path so any future change to wrapped-chart
// shape lands in one place.
//
// Typed as string (not int-iota like the sibling manifestPhase in
// pkg/bundler/bundler.go) so the value embeds directly into the
// "<name>-<phase>" folder/release name and into %q error messages
// without a separate String() method.
type injectionPhase string

const (
	phasePre  injectionPhase = "pre"
	phasePost injectionPhase = "post"
)

// injectAuxiliaryFolder wraps the per-phase manifest list for component
// c into a local-helm folder named "<name>-<phase>" at the given index.
// Returns (nil, nil) when there are no manifests for the requested
// phase — the caller treats that as a no-op (do not increment idx).
// Both pre- and post-phase share this path so naming, render input,
// error handling, and writeLocalHelmFolder invocation stay in one
// place.
func (opts *Options) injectAuxiliaryFolder(idx int, c Component, phase injectionPhase) (*Folder, error) {
	var manifests map[string][]byte
	switch phase {
	case phasePre:
		manifests = opts.ComponentPreManifests[c.Name]
	case phasePost:
		manifests = opts.ComponentPostManifests[c.Name]
	default:
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("unknown injection phase %q", phase))
	}
	if len(manifests) == 0 {
		return nil, nil //nolint:nilnil // (nil, nil) is the documented no-op contract: callers treat it as "no folder for this phase, do not increment idx".
	}
	auxName := c.Name + "-" + string(phase)
	auxDir := fmt.Sprintf("%03d-%s", idx, auxName)
	// Both pre- and post-phase folders request createNamespace=true;
	// writeLocalHelmFolder downgrades to false automatically when the
	// rendered templates contain a Namespace resource (Talos
	// privileged-namespace pattern). Otherwise the target namespace may
	// not yet exist when the pre-phase runs — the primary chart hasn't
	// executed --create-namespace yet — so install.sh must create it.
	f, err := writeLocalHelmFolder(
		opts.OutputDir, auxDir, idx, c,
		manifests, renderInputFor(c),
		auxName, c.Name, true,
	)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// Write emits the numbered folder layout. Deterministic and idempotent.
//
// Removes any pre-existing NNN-* folders under OutputDir before writing, so
// reusing the same --output across recipe regenerations does not leave stale
// component folders that the deployer's loop would later install. Top-level
// orchestration files (deploy.sh, undeploy.sh, README.md, attestation/) are
// left intact; only files under [0-9][0-9][0-9]-* are removed.
//
// Returns the list of emitted folders plus, when opts.VendorCharts is set,
// one VendorRecord per pulled upstream chart for inclusion in the bundle's
// provenance.yaml. The records slice is empty when VendorCharts is false.
//
//nolint:funlen // single-pass component loop; further extraction reduces locality of the index/branch logic.
func Write(ctx context.Context, opts Options) (WriteResult, error) {
	// Honor cancellation before any filesystem mutation.
	if err := ctx.Err(); err != nil {
		return WriteResult{}, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
	}
	// Fail fast if the layout's three-digit prefix can't accommodate the
	// number of NNN-* folders this bundle will actually emit. Compute
	// the count using the same pre/primary/post emission rules the main
	// loop below applies — a worst-case 3x bound would reject valid
	// 500-component bundles whose components emit only their primary
	// folder. The deploy/undeploy templates glob [0-9][0-9][0-9]-*/, so
	// a 4-digit prefix would be silently skipped at install time.
	folderCount := 0
	for _, c := range opts.Components {
		if len(opts.ComponentPreManifests[c.Name]) > 0 {
			folderCount++ // <name>-pre
		}
		folderCount++ // primary
		// Post-injection conditions mirror the per-component branch
		// below: skipped under VendorCharts (mixed collapses into the
		// primary), and only meaningful for mixed components (helm
		// repository + raw manifests). Manifest-only and kustomize
		// primaries never inject a -post folder.
		if !opts.VendorCharts && c.Repository != "" && len(opts.ComponentPostManifests[c.Name]) > 0 {
			folderCount++ // <name>-post
		}
	}
	if folderCount > 999 {
		return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("too many emitted folders (%d) from %d components: NNN- folder prefix supports at most 999 entries",
				folderCount, len(opts.Components)))
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return WriteResult{}, errors.Wrap(errors.ErrCodeInternal, "create output dir", err)
	}
	if err := pruneStaleFolders(opts.OutputDir); err != nil {
		return WriteResult{}, err
	}

	puller := opts.Puller
	if opts.VendorCharts && puller == nil {
		puller = &CLIChartPuller{}
	}

	// Detect <name>-pre and <name>-post collisions up front: if a recipe
	// declares both a component "foo" with pre/post manifests and a
	// separate component "foo-pre" or "foo-post", the injection rule
	// would synthesize a second folder/release that collides with the
	// explicitly-declared one. The post check is skipped under
	// VendorCharts because vendored mode collapses mixed components
	// into a single folder and never injects -post; pre injection still
	// runs in vendored mode (pre folders are independent of the chart).
	declared := make(map[string]struct{}, len(opts.Components))
	for _, c := range opts.Components {
		declared[c.Name] = struct{}{}
	}
	for _, c := range opts.Components {
		// <name>-pre collision: any component with preManifestFiles would
		// inject a "<name>-pre" folder/release. Unlike the post check
		// below, no Repository-guard: pre injection runs regardless of
		// primary kind (upstream-helm, local-helm, or kustomize).
		if len(opts.ComponentPreManifests[c.Name]) > 0 {
			if _, clash := declared[c.Name+"-pre"]; clash {
				return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q has preManifestFiles and would inject %q-pre, but a component named %q-pre is already declared in the recipe — rename one to avoid collision",
						c.Name, c.Name, c.Name))
			}
			// Drift guard: any Namespace doc in a pre-manifest must
			// target ComponentRef.Namespace. The pre folder's
			// install.sh deliberately omits --create-namespace (the
			// chart's Namespace template is what creates it); if the
			// rendered Namespace metadata.name disagrees with the
			// release's --namespace, helm creates one namespace and
			// looks for the release in another, and install fails
			// with an opaque "namespace not found" downstream.
			// Catch the mismatch at bundle time with the offending
			// path so a recipe author can fix the YAML directly.
			if err := validatePreManifestNamespace(c.Name, c.Namespace, opts.ComponentPreManifests[c.Name]); err != nil {
				return WriteResult{}, err
			}
		}
		if opts.VendorCharts {
			continue // post-injection skipped under VendorCharts
		}
		if len(opts.ComponentPostManifests[c.Name]) == 0 {
			continue
		}
		if c.Repository == "" {
			continue // manifest-only doesn't inject; already a single local-helm folder
		}
		if _, clash := declared[c.Name+"-post"]; clash {
			return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q is mixed (helm + manifests) and would inject %q-post, but a component named %q-post is already declared in the recipe — rename one to avoid collision",
					c.Name, c.Name, c.Name))
		}
	}

	folders := make([]Folder, 0, len(opts.Components))
	var vendorRecords []VendorRecord
	idx := 1
	for _, c := range opts.Components {
		if err := ctx.Err(); err != nil {
			return WriteResult{}, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
		}
		if !deployer.IsSafePathComponent(c.Name) {
			return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q", c.Name))
		}

		// Reject kustomize + raw manifests: each recipe component must declare
		// EITHER kustomize (Tag/Path) OR raw manifests, not both. The bundle
		// shape can only wrap one primary source into the local chart.
		if (c.Tag != "" || c.Path != "") && len(opts.ComponentPostManifests[c.Name]) > 0 {
			return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q has both kustomize (Tag/Path) and raw manifests; use one", c.Name))
		}

		// Pre-injection: applies before the primary chart so resources
		// like a PSS-privileged Namespace exist by the time the chart's
		// pods schedule. Runs regardless of vendored/non-vendored mode
		// and regardless of primary kind (upstream-helm, local-helm, or
		// kustomize).
		if pf, err := opts.injectAuxiliaryFolder(idx, c, phasePre); err != nil {
			return WriteResult{}, err
		} else if pf != nil {
			folders = append(folders, *pf)
			slog.Info("wrote local chart folder",
				"index", idx, "dir", pf.Dir,
				"kind", KindLocalHelm.String(), "parent", c.Name)
			idx++
		}

		dir := fmt.Sprintf("%03d-%s", idx, c.Name)

		// Vendored Helm path: one wrapped folder per Helm-typed component
		// regardless of mixed/pure. Kustomize and manifest-only fall
		// through to the existing classify() path even with VendorCharts
		// on, because they are already local after #662.
		if opts.VendorCharts && shouldVendor(c) {
			f, rec, err := writeVendoredHelmFolder(
				ctx, opts.OutputDir, dir, idx, c,
				opts.ComponentPostManifests[c.Name], puller,
			)
			if err != nil {
				return WriteResult{}, err
			}
			folders = append(folders, f)
			vendorRecords = append(vendorRecords, rec)
			slog.Info("wrote vendored chart folder",
				"index", idx, "dir", dir, "parent", c.Name,
				"chart", rec.Chart, "version", rec.Version, "sha256", rec.SHA256)
			idx++
			continue
		}

		kind := classify(c, opts.ComponentPostManifests[c.Name])

		switch kind {
		case KindUpstreamHelm:
			f, err := writeUpstreamHelmFolder(opts.OutputDir, dir, idx, c)
			if err != nil {
				return WriteResult{}, err
			}
			folders = append(folders, f)
			slog.Info("wrote local chart folder", "index", idx, "dir", dir, "kind", kind.String(), "parent", c.Name)
			idx++

			// Post-injection: wrapped manifests apply after the primary chart.
			// Mixed component (upstream chart + raw manifests): emit an injected
			// "-post" wrapped chart immediately after the primary so raw manifests
			// apply post-install (after helm has registered the chart's CRDs).
			// The "mixed" concept lives only here at the bundle layer — no recipe
			// metadata involved.
			if pf, err := opts.injectAuxiliaryFolder(idx, c, phasePost); err != nil {
				return WriteResult{}, err
			} else if pf != nil {
				folders = append(folders, *pf)
				slog.Info("wrote local chart folder",
					"index", idx, "dir", pf.Dir,
					"kind", KindLocalHelm.String(), "parent", c.Name)
				idx++
			}
		case KindLocalHelm:
			manifests := opts.ComponentPostManifests[c.Name]
			if c.Tag != "" || c.Path != "" {
				// Kustomize-typed: materialize the overlay output to a single
				// templates/manifest.yaml inside the wrapped chart.
				//
				// Path is required (kustomize needs somewhere to build from);
				// Tag is only meaningful with a git Repository. Reject the
				// incomplete combinations explicitly so a recipe author sees
				// the misconfiguration rather than a silent empty build.
				if c.Path == "" {
					return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("kustomize component %q has Tag but no Path; Path is required", c.Name))
				}
				if c.Tag != "" && c.Repository == "" {
					return WriteResult{}, errors.New(errors.ErrCodeInvalidRequest,
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
					return WriteResult{}, kerr
				}
				manifests = map[string][]byte{"manifest.yaml": rendered}
			}
			// Primary local-helm folders never contain a Namespace
			// template (recipe convention: Namespace lives in the pre
			// folder). Pass createNamespace=true so install.sh can spin
			// up the namespace for manifest-only / kustomize components
			// that aren't preceded by a pre folder.
			f, err := writeLocalHelmFolder(opts.OutputDir, dir, idx, c,
				manifests, renderInputFor(c),
				c.Name, c.Name, true)
			if err != nil {
				return WriteResult{}, err
			}
			folders = append(folders, f)
			slog.Info("wrote local chart folder", "index", idx, "dir", dir, "kind", kind.String(), "parent", c.Name)
			idx++
		}
	}
	return WriteResult{Folders: folders, VendoredCharts: vendorRecords}, nil
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

// validatePreManifestNamespace scans each pre-manifest doc for kind:
// Namespace entries and requires metadata.name to equal expectedNS.
// Pre-folder install.sh deliberately omits --create-namespace so the
// chart's own Namespace template is the sole namespace-creator; if its
// metadata.name drifts from ComponentRef.Namespace, helm creates one
// namespace and looks for the release in another, and install fails
// downstream with an opaque error. Catch the mismatch at bundle time
// with the offending file path so a recipe author can fix the YAML
// directly.
//
// Non-Namespace documents and documents whose kind/metadata.name are
// templated (helm {{ }} placeholders that fail YAML parsing) are
// skipped: a literal name vs c.Namespace comparison is meaningful
// only for static manifests, which is the os-talos mixin's contract
// and the only shape we want to guard against silent drift for.
//
// Decode-error behavior: a YAML parse failure stops processing further
// docs in that file (the inner decode loop breaks). This is acceptable
// under the os-talos mixin contract — one static Namespace per
// pre-manifest file — and intentional for templated docs, where Helm's
// own renderer is the authoritative parser. A pre-manifest file that
// hides a static Namespace after a templated/invalid doc would
// silently bypass this guard; if that pattern becomes valid, swap the
// `break` for `continue` so subsequent docs are still validated.
func validatePreManifestNamespace(componentName, expectedNS string, manifests map[string][]byte) error {
	for path, body := range manifests {
		dec := yaml.NewDecoder(bytes.NewReader(body))
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if stderrors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				// Templated docs (e.g. metadata.name: {{ .Release.Namespace }})
				// fail YAML parsing. Don't fail the bundle here — the chart's
				// own renderer will reject genuinely malformed YAML; we're only
				// guarding the static-mixin shape.
				break
			}
			if len(doc) == 0 {
				continue
			}
			kind, _ := doc["kind"].(string)
			if kind != "Namespace" {
				continue
			}
			meta, _ := doc["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			if name == "" {
				continue
			}
			if name != expectedNS {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q: pre-manifest %s declares Namespace/%q but componentRef.namespace is %q — the pre folder's chart creates the Namespace, so the release namespace (%q) and the chart's metadata.name (%q) must match",
						componentName, path, name, expectedNS, expectedNS, name))
			}
		}
	}
	return nil
}
