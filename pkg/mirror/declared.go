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

package mirror

import (
	"context"
	stderrors "errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"

	"aicr.run/pkg/bom"
	"aicr.run/pkg/component"
	"aicr.run/pkg/defaults"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/recipe"
)

// declaredImages returns the images ref's images.yaml declares for the chart
// it deploys, or for a manifest-only ref, for the operator image in scanned.
// An image with an enabledBy path is included only when that path is true in
// values. A component with no images.yaml declares nothing.
//
// A file that cannot be read, or that has no entry for the deployed chart
// version or operator image, declares nothing and adds a warning to ci, the
// way a chart that fails to render does. It returns an error for a malformed
// file or a canceled ctx.
func declaredImages(
	ctx context.Context,
	dp recipe.DataProvider,
	ref recipe.ComponentRef,
	values map[string]any,
	scanned []string,
	ci *ComponentImages,
) ([]string, error) {

	p := path.Join("components", ref.Name, bom.DeclaredImagesFile)
	readCtx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	data, err := recipe.GetManifestContentWithContext(readCtx, dp, p)
	cancel()
	switch {
	case err == nil:
	case stderrors.Is(err, fs.ErrNotExist):
		return nil, nil
	case ctx.Err() != nil:
		return nil, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "read "+p)
	default:
		warnDeclared(ci, ref.Name, err)
		return nil, nil
	}
	d, err := bom.ParseDeclaredImages(ref.Name, data)
	if err != nil {
		return nil, err
	}

	var images []bom.DeclaredImage
	if ref.Type == recipe.ComponentTypeHelm && ref.HasExternalChart() {
		c, chartErr := d.ChartFor(ref.Source, ref.EffectiveChart(), ref.Version)
		if chartErr != nil {
			warnDeclared(ci, ref.Name, chartErr)
			return nil, nil
		}
		images = c.Images
	} else {
		m, manifestErr := d.ManifestFor(scanned)
		if manifestErr != nil {
			warnDeclared(ci, ref.Name, manifestErr)
			return nil, nil
		}
		images = m.Images
	}

	out := make([]string, 0, len(images))
	for _, i := range images {
		if i.EnabledBy != "" && !enabledIn(values, i.EnabledBy) {
			continue
		}
		out = append(out, i.Image)
	}
	return out, nil
}

// warnDeclared records that the declared images of a component were omitted.
func warnDeclared(ci *ComponentImages, name string, err error) {
	slog.Warn("declared images omitted", logKeyComponent, name, "error", err)
	ci.Warnings = append(ci.Warnings, fmt.Sprintf("declared images omitted: %v", err))
}

// enabledIn reports whether the value at the dot-separated path in values is
// the boolean true.
func enabledIn(values map[string]any, valuePath string) bool {
	v, ok := component.GetValueByPath(values, valuePath)
	b, isBool := v.(bool)
	return ok && isBool && b
}
