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
	stderrors "errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"aicr.run/pkg/bom"
	"aicr.run/pkg/errors"
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
func readDeclaredImages(repoRoot string, c component, version string, rendered []string) ([]string, error) {
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

	out := make([]string, 0, len(images))
	for _, i := range images {
		out = append(out, i.Image)
	}
	return out, nil
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
