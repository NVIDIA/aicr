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

package main

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Variant discovery is DECLARATION-level, per the revised #1611: it inspects
// the explicit Helm componentRef.version pins declared by the base, overlays,
// and mixins and compares each against the component's registry
// defaultVersion. Explicit NON-GOALS of this model (document them, do not
// approximate them):
//   - effective-recipe resolution (inheritance chains, mixin composition,
//     criteria matching) — that is the recipe resolver's job;
//   - source/chart coordinate divergence — variants render at REGISTRY
//     coordinates by definition (catalog parity);
//   - componentRef type analysis beyond "is this declared pin a Helm pin"
//     (explicitly non-Helm-typed refs are skipped).
//
// The registry and these sources are read from the same -repo-root, so
// variant data can never mix checkouts.

// recipeSource is one loaded base/overlay/mixin: the declaring name and its
// componentRefs, parsed into the CANONICAL pkg/recipe types so this tool's
// view of a pin cannot drift from the resolver's.
type recipeSource struct {
	Name string
	Refs []recipe.ComponentRef
}

// sourceDirs maps each recipe directory to its kind discipline.
//
// Scope note: this loader intentionally scans only overlays/ and mixins/,
// whereas the canonical metadata store (pkg/recipe/metadata_store.go) walks
// the entire recipes/ tree and treats any .yaml outside checks/, components/,
// and mixins/ (minus data-v1.yaml/registry.yaml) as RecipeMetadata. By
// convention every recipe overlay lives under overlays/, so the two scopes
// agree today. A RecipeMetadata file parked elsewhere (e.g. directly under
// recipes/) would be resolvable by the store but invisible to variant
// discovery — a silent gap in the "projection matches what deploys" claim. If
// the recipes/ layout ever grows metadata outside overlays/, this list must be
// widened to match the store's scan so no divergent pin is dropped.
var sourceDirs = []struct {
	dir  string
	kind string
}{
	{dir: "overlays", kind: recipe.RecipeMetadataKind},
	{dir: "mixins", kind: recipe.RecipeMixinKind},
}

// parseRecipeSource decodes one overlay/mixin file with the canonical
// metadata store's exact semantics; ok is false when the store would skip
// the file. Mixins decode strictly (KnownFields, hard kind check) — a typo'd
// field fails closed just as it does at recipe load. Overlays decode
// leniently (plain Unmarshal; empty kind is legacy RecipeMetadata; any other
// kind, e.g. a stray ValidatorCatalog, is skipped): a field the resolver
// would ignore is equally invisible here, so the projection matches what
// actually deploys.
func parseRecipeSource(rel, wantKind string, data []byte) (src recipeSource, ok bool, err error) {
	if wantKind == recipe.RecipeMixinKind {
		var mixin recipe.RecipeMixin
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&mixin); err != nil {
			return recipeSource{}, false, errors.Wrap(errors.ErrCodeInvalidRequest,
				"parse "+rel+" (unknown fields are not allowed)", err)
		}
		if mixin.Kind != recipe.RecipeMixinKind {
			return recipeSource{}, false, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"mixin file %s has wrong kind %q, expected %q", rel, mixin.Kind, recipe.RecipeMixinKind))
		}
		return recipeSource{Name: mixin.Metadata.Name, Refs: mixin.Spec.ComponentRefs}, true, nil
	}
	var metadata recipe.RecipeMetadata
	if err := yaml.Unmarshal(data, &metadata); err != nil {
		return recipeSource{}, false, errors.Wrap(errors.ErrCodeInvalidRequest, "parse "+rel, err)
	}
	if metadata.Kind != "" && metadata.Kind != recipe.RecipeMetadataKind {
		return recipeSource{}, false, nil
	}
	return recipeSource{Name: metadata.Metadata.Name, Refs: metadata.Spec.ComponentRefs}, true, nil
}

// loadRecipeSources reads every overlay and mixin YAML under repoRoot's
// recipes/ tree with the canonical loader's semantics: the walk is RECURSIVE
// (subdirectories under overlays/ are documented and supported, so a nested
// divergent pin must not be silently omitted). A file that fails to read or
// parse is an error — silently skipping a source could hide a divergent pin
// — and duplicate source identities fail closed, since declaration-level
// scanning cannot know which duplicate "wins" during resolution. Reads are
// size-bounded, honor ctx between steps, and are confined to the recipes
// root via os.Root, so a checked-in symlink cannot smuggle another
// checkout's overlay in.
func loadRecipeSources(ctx context.Context, repoRoot string) ([]recipeSource, error) {
	root, err := os.OpenRoot(filepath.Join(repoRoot, "recipes"))
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, "recipes root not found", err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "open recipes root", err)
	}
	defer root.Close()

	var sources []recipeSource
	seenNames := map[string]string{}
	for _, sd := range sourceDirs {
		files, err := walkYAML(ctx, root, sd.dir)
		if err != nil {
			return nil, err
		}
		for _, rel := range files {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, errors.Wrap(errors.ErrCodeTimeout, "loading recipe sources canceled", ctxErr)
			}
			data, err := readBoundedFile(root, rel)
			if err != nil {
				return nil, err
			}
			src, ok, err := parseRecipeSource(rel, sd.kind, data)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // foreign kind under overlays/: skipped, never mined
			}
			if src.Name == "" {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("recipe source %s has no metadata.name", rel))
			}
			if prev, dup := seenNames[src.Name]; dup {
				return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"duplicate recipe source name %q (%s and %s); declaration-level variant "+
						"discovery cannot attribute pins to an ambiguous identity",
					src.Name, prev, rel))
			}
			seenNames[src.Name] = rel
			sources = append(sources, src)
		}
	}
	return sources, nil
}

// walkYAML recursively collects every regular .yaml file under dir inside
// root, sorted. The walk is fail-closed — any I/O error aborts rather than
// yielding a silently incomplete source set — and checks ctx per entry so a
// timed-out caller stops the traversal.
func walkYAML(ctx context.Context, root *os.Root, dir string) ([]string, error) {
	var files []string
	err := fs.WalkDir(root.FS(), dir, func(path string, d fs.DirEntry, werr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if werr != nil {
			return werr
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		// Symlinks are collected, not skipped: root.Open resolves them under
		// os.Root confinement, so an in-tree link is read like the canonical
		// loader would read it and an escaping link fails closed instead of
		// silently dropping a source.
		if !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		switch {
		case stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded):
			return nil, errors.Wrap(errors.ErrCodeTimeout, "listing recipes/"+dir+" timed out", err)
		case stderrors.Is(err, fs.ErrNotExist):
			return nil, errors.Wrap(errors.ErrCodeNotFound, "recipes/"+dir+" not found", err)
		default:
			return nil, errors.Wrap(errors.ErrCodeInternal, "walk recipes/"+dir, err)
		}
	}
	sort.Strings(files)
	return files, nil
}

// readBoundedFile reads rel inside root with a size bound (the standard
// defense against reading an unexpectedly huge file whole). root.OpenFile
// confines symlink resolution to the recipes tree: an escaping symlink is
// invalid operator input, not an internal fault. The open is O_NONBLOCK so a
// FIFO target cannot block it (opening a FIFO for read otherwise waits for a
// writer; O_NONBLOCK is a no-op for regular-file reads), and the regular-file
// requirement is checked by fstat on the OPENED handle — validating the
// descriptor we actually read leaves no stat-to-open window in which the
// target could be swapped for a blocking file.
func readBoundedFile(root *os.Root, rel string) ([]byte, error) {
	mapErr := func(op string, err error) error {
		switch {
		case stderrors.Is(err, fs.ErrNotExist):
			return errors.Wrap(errors.ErrCodeNotFound, "recipe source "+rel+" not found", err)
		case strings.Contains(err.Error(), "escapes from parent"):
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				"recipe source "+rel+" escapes the recipes root (symlinks must stay inside the checkout)", err)
		default:
			return errors.Wrap(errors.ErrCodeInternal, op+" "+rel, err)
		}
	}
	fh, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, mapErr("open", err)
	}
	defer fh.Close() // read-only handle
	info, err := fh.Stat()
	if err != nil {
		return nil, mapErr("stat", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"recipe source %s resolves to a non-regular file (%s); sources must be regular files",
			rel, info.Mode().Type()))
	}
	data, err := io.ReadAll(io.LimitReader(fh, defaults.MaxExternalDataFileBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+rel, err)
	}
	if int64(len(data)) > defaults.MaxExternalDataFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"recipe source %s exceeds %d bytes", rel, defaults.MaxExternalDataFileBytes))
	}
	return data, nil
}

// variantKey aggregates divergent pins by (component, version): multiple
// sources pinning the same divergent version become one variant.
type variantKey struct {
	name    string
	version string
}

// deriveVariants compares every explicit Helm componentRef.version pin
// declared by the base, overlays, and mixins against the component's
// registry defaultVersion. A differing pin yields a variant aggregated by
// (component, version) with the sorted declaring source names; default-equal
// pins yield nothing. The recipe pins are the source facts — the derivation
// deliberately has no dependency on the version-pin guard's exemption policy
// (which decides whether a divergence is ALLOWED, not what is deployed). See
// issue #1611 and the non-goals at the top of this file.
func deriveVariants(reg *registry, sources []recipeSource) ([]bom.VariantResult, error) {
	byName := make(map[string]component, len(reg.Components))
	for _, c := range reg.Components {
		byName[c.Name] = c
	}

	agg := map[variantKey]map[string]struct{}{}
	for _, src := range sources {
		for _, ref := range src.Refs {
			pin := ref.Version
			if pin == "" {
				continue
			}
			// The pin is a source fact — never normalize it. A padded value
			// is rejected explicitly: the doc could not faithfully represent
			// a distinct explicit value it silently trimmed.
			if pin != strings.TrimSpace(pin) {
				return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"source %s pins component %s to version %q with surrounding whitespace; "+
						"fix the pin", src.Name, ref.Name, pin))
			}
			// An explicitly non-Helm-typed ref is not a Helm chart pin;
			// type analysis beyond this is a documented non-goal. A
			// type-less ref inherits the registry component's type, checked
			// below via c.kind().
			if ref.Type != "" && !strings.EqualFold(string(ref.Type), "helm") {
				continue
			}
			c, ok := byName[ref.Name]
			if !ok {
				// Not a registry component; the BOM does not render it from
				// a registry default, so there is no default to diverge from.
				continue
			}
			if c.kind() != kindHelm {
				continue
			}
			// An explicit pin with NO registry default is still the version
			// the source deploys: render it as a variant (the default row
			// shows the unpinned sentinel) rather than dropping it from the
			// inventory. Unreachable for the embedded registry
			// (bom-pinning-check), but -repo-root data is arbitrary.
			if pin == c.Helm.DefaultVersion {
				continue
			}
			k := variantKey{name: ref.Name, version: pin}
			if agg[k] == nil {
				agg[k] = map[string]struct{}{}
			}
			agg[k][src.Name] = struct{}{}
		}
	}

	variants := make([]bom.VariantResult, 0, len(agg))
	for k, srcSet := range agg {
		srcs := make([]string, 0, len(srcSet))
		for s := range srcSet {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		c := byName[k.name]
		variants = append(variants, bom.VariantResult{
			Name:       k.name,
			Version:    k.version,
			Sources:    srcs,
			Repository: c.Helm.DefaultRepository,
			Chart:      c.Helm.DefaultChart,
			Namespace:  c.Helm.DefaultNamespace,
		})
	}
	sort.Slice(variants, func(i, j int) bool {
		if variants[i].Name != variants[j].Name {
			return variants[i].Name < variants[j].Name
		}
		return variants[i].Version < variants[j].Version
	})
	return variants, nil
}

// surveyVariant surveys the variant through the SAME path as default entries
// (surveyComponent: chart render plus the embedded manifests walk), with the
// component's default version swapped for the variant version — catalog
// parity, not deployment fidelity. Variant image sets are rendered, never
// copied from the default entry: the two versions may ship different images,
// and a mixed Helm+manifest component contributes its manifest images too.
func surveyVariant(ctx context.Context, repoRoot string, v bom.VariantResult, base component, r helm.Renderer, skipHelm bool) bom.VariantResult {
	variantComponent := base
	variantComponent.Helm.DefaultVersion = v.Version
	res := surveyComponent(ctx, repoRoot, variantComponent, r, skipHelm)
	v.Images = res.Images
	v.Warnings = append(v.Warnings, res.Warnings...)
	return v
}
