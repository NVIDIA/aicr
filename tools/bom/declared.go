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
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"aicr.run/pkg/bom"
	pkgcomponent "aicr.run/pkg/component"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/helm"
)

// declaredRequiredComponents names the components whose operators launch
// images that no rendered template shows. Each must keep an images.yaml, so
// deleting one fails the survey instead of narrowing the BOM.
var declaredRequiredComponents = map[string]struct{}{
	"dynamo-platform":            {},
	"grove":                      {},
	"knative-operator":           {},
	"kube-prometheus-stack":      {},
	"kubeflow-training-operator": {},
	"network-operator":           {},
	"nodewright-operator":        {},
	"runai-cluster":              {},
}

// readDeclaredImages returns the images c's images.yaml declares for the
// surveyed version, or nil when c has no images.yaml and is not in
// declaredRequiredComponents. rendered holds the images the survey already
// found, which identify the operator image of a manifest-only component.
//
// The BOM renders a version variant from the registry repository, so a Helm
// component's entry is found with bom.DeclaredImages.ChartFor, which matches a
// chart from another repository by name and version. Every declared image is
// listed whatever its enabledBy path, since the BOM lists what a component can
// deploy. A file with no entry for the surveyed version fails the survey.
func readDeclaredImages(repoRoot string, c component, version string, rendered []string) ([]bom.DeclaredImage, error) {
	d, found, err := loadDeclaredImages(repoRoot, c.Name)
	if err != nil || !found {
		return nil, err
	}

	var images []bom.DeclaredImage
	if c.kind() == kindHelm {
		entry, chartErr := d.ChartFor(c.Helm.DefaultRepository, c.effectiveChart(), version)
		if chartErr != nil {
			return nil, chartErr
		}
		images = entry.Images
	} else {
		entries, manifestErr := d.ManifestsFor(rendered)
		if manifestErr != nil {
			return nil, manifestErr
		}
		for _, m := range entries {
			images = append(images, m.Images...)
		}
	}
	return images, nil
}

// verifyEnabledBy renders c once for each enabledBy path in declared, with
// that path set to true. Each image gated on the path must appear in that
// render and not in defaultRender, the render with c's values.yaml alone, so
// the path is the value the chart gates the image on. A failed render is a
// warning, which fails a strict run.
func verifyEnabledBy(ctx context.Context, repoRoot string, c component, r helm.Renderer,
	declared []bom.DeclaredImage, defaultRender []byte) ([]string, error) {

	gated := map[string][]string{}
	for _, d := range declared {
		if d.EnabledBy != "" {
			gated[d.EnabledBy] = append(gated[d.EnabledBy], d.Image)
		}
	}
	var warnings, problems []string
	for _, p := range slices.Sorted(maps.Keys(gated)) {
		values := map[string]any{}
		pkgcomponent.SetValueByPath(values, p, true)
		out, renderWarnings := renderHelmComponent(ctx, repoRoot, c, r, values)
		if len(renderWarnings) > 0 {
			for _, w := range renderWarnings {
				warnings = append(warnings, fmt.Sprintf("render with %s=true: %s", p, w))
			}
			continue
		}
		for _, img := range gated[p] {
			switch {
			case !containsRef(out, img):
				problems = append(problems, fmt.Sprintf("%s does not render with %s=true", img, p))
			case containsRef(defaultRender, img):
				problems = append(problems, fmt.Sprintf("%s renders without %s=true", img, p))
			}
		}
	}
	if len(problems) > 0 {
		return warnings, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q: %s enabledBy does not match chart version %s: %s",
				c.Name, bom.DeclaredImagesFile, c.Helm.DefaultVersion, strings.Join(problems, "; ")),
			map[string]any{componentContextKey: c.Name})
	}
	return warnings, nil
}

// containsRef reports whether ref occurs in data as a whole image reference,
// with no reference character on either side. A preceding "/" is allowed, so
// a short name such as nats:2.10 matches docker.io/library/nats:2.10.
func containsRef(data []byte, ref string) bool {
	for i := 0; i <= len(data); {
		j := bytes.Index(data[i:], []byte(ref))
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(ref)
		before := start == 0 || data[start-1] == '/' || !isRefByte(data[start-1])
		after := end == len(data) || !isRefByte(data[end])
		if before && after {
			return true
		}
		i = start + 1
	}
	return false
}

// isRefByte reports whether b can appear in an image reference.
func isRefByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("._-:/@", b) >= 0
}

// loadDeclaredImages reads and parses the named component's images.yaml.
// found is false for an absent file, which is an error only for a component
// in declaredRequiredComponents.
func loadDeclaredImages(repoRoot, name string) (d *bom.DeclaredImages, found bool, err error) {
	errCtx := map[string]any{componentContextKey: name}
	absent := func(cause error) (*bom.DeclaredImages, bool, error) {
		if _, required := declaredRequiredComponents[name]; required {
			return nil, false, errors.WrapWithContext(errors.ErrCodeNotFound,
				bom.DeclaredImagesFile+" is required for this component but absent", cause, errCtx)
		}
		return nil, false, nil
	}
	root, err := os.OpenRoot(filepath.Join(repoRoot, "recipes"))
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return absent(err)
		}
		return nil, false, errors.WrapWithContext(errors.ErrCodeInternal, "open recipes root", err, errCtx)
	}
	defer root.Close() // read-only handle

	data, err := readBoundedFile(root, path.Join("components", name, bom.DeclaredImagesFile))
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return absent(err)
		}
		return nil, false, err
	}
	d, err = bom.ParseDeclaredImages(name, data)
	if err != nil {
		return nil, false, err
	}
	return d, true, nil
}
