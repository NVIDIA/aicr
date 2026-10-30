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

package resolver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// ReasonOverridePathUnknown marks an override whose path neither the base
// render nor the chart's defaults know: a likely typo.
const ReasonOverridePathUnknown = "OverridePathUnknown"

// Bounds on what the chart defaults read from one chart archive: each
// values file or packaged subchart, all of them together, and their number.
const (
	maxChartDefaultsBytes      = 4 << 20
	maxChartDefaultsTotalBytes = 16 << 20
	maxChartDefaultsEntries    = 1024
)

// ValueOverrides reports whether a spec overrides values, not only component
// toggles; only then does resolution need a base render.
func ValueOverrides(spec *v1alpha1.ClusterRuntimeSpec) bool {
	for i := range spec.Overrides {
		if spec.Overrides[i].Path != enabledPath {
			return true
		}
	}
	return false
}

// BaseSpec is the base render's input: the spec with its component toggles
// but without value overrides, so the same components render with the
// values AICR alone produces.
func BaseSpec(spec *v1alpha1.ClusterRuntimeSpec) *v1alpha1.ClusterRuntimeSpec {
	base := spec.DeepCopy()
	base.Overrides = nil
	for _, o := range spec.Overrides {
		if o.Path == enabledPath {
			base.Overrides = append(base.Overrides, o)
		}
	}
	return base
}

// BundleValues returns each component's merged values in a rendered bundle:
// those of its primary release.
func BundleValues(b *registry.Bundle, declaredVersion string) (map[string]map[string]any, error) {
	info, err := aicrjob.ParseBundleInfo(b.Files[aicrjob.BundleInfoFile], declaredVersion)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for i := range info.Layout.Releases {
		r := &info.Layout.Releases[i]
		if r.Phase != plan.PhasePrimary {
			continue
		}
		merged := map[string]any{}
		for _, name := range valuesFiles {
			raw, ok := b.Files[r.Path+"/"+name]
			if !ok {
				continue
			}
			doc, perr := parseValues(raw)
			if perr != nil {
				return nil, operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("%s/%s: %v", r.Path, name, perr))
			}
			merged = mergeValues(merged, doc)
		}
		out[r.Component] = merged
	}
	return out, nil
}

// CheckOverridePaths rejects a value override, unless it is additive, whose
// path exists neither in the base render's values for its component nor in
// its chart's defaults. AICR creates missing intermediate maps, so without
// this check a mistyped path renders silently.
func CheckOverridePaths(overrides []v1alpha1.Override, base, chartDefaults map[string]map[string]any) error {
	var unknown []string
	for _, o := range overrides {
		if o.Path == enabledPath || o.Additive {
			continue
		}
		if !hasPath(base[o.Component], o.Path) && !hasPath(chartDefaults[o.Component], o.Path) {
			unknown = append(unknown, o.Component+":"+o.Path)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(
		"override paths %s exist in neither the base render nor the chart defaults; fix the path or set additive: true",
		strings.Join(unknown, ", "))).WithReason(ReasonOverridePathUnknown)
}

// hasPath reports whether a dot-separated path names a key in nested maps;
// a key whose value is null exists.
func hasPath(values map[string]any, p string) bool {
	if values == nil {
		return false
	}
	var cur any = values
	for _, part := range strings.Split(p, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

// chartDefaults reads a chart archive's default values: the chart's
// values.yaml, with each bundled subchart's defaults under its name, as Helm
// scopes them. Subcharts may be directories or nested archives; archives
// nested deeper than the CRD scan follows contribute nothing.
func chartDefaults(archive []byte) (map[string]any, error) {
	return chartDefaultsAt(archive, 0)
}

func chartDefaultsAt(archive []byte, depth int) (map[string]any, error) {
	files, err := readArchive(archive)
	if err != nil {
		return nil, err
	}
	defaults := map[string]any{}
	for name, data := range files {
		parts := strings.Split(name, "/")
		switch {
		case len(parts) == 2 && parts[1] == chartValuesFile:
			doc, perr := parseValues(data)
			if perr != nil {
				return nil, operrors.New(operrors.ErrCodeInvalidRequest, "chart values.yaml: "+perr.Error())
			}
			defaults = mergeValues(defaults, doc)
		case len(parts) == 4 && parts[1] == subchartsDir && parts[3] == chartValuesFile:
			if doc, perr := parseValues(data); perr == nil {
				defaults = mergeValues(defaults, map[string]any{parts[2]: doc})
			}
		case len(parts) == 3 && parts[1] == subchartsDir && strings.HasSuffix(parts[2], ".tgz") && depth < maxNestedCharts:
			sub, serr := chartDefaultsAt(data, depth+1)
			if serr != nil {
				continue
			}
			meta, merr := registry.ReadChartMeta(data)
			if merr != nil {
				continue
			}
			defaults = mergeValues(defaults, map[string]any{meta.Name: sub})
		}
	}
	return defaults, nil
}

// readArchive reads the values files and nested chart archives of a chart
// archive, bounded per file, in total and in number.
func readArchive(archive []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, operrors.New(operrors.ErrCodeInvalidRequest, "chart archive is not gzip")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	entries, total := 0, 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest, "chart archive is not a tar archive")
		}
		name := path.Clean(h.Name)
		if h.Typeflag != tar.TypeReg || (path.Base(name) != chartValuesFile && !strings.HasSuffix(name, ".tgz")) {
			continue
		}
		if entries++; entries > maxChartDefaultsEntries {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest,
				fmt.Sprintf("chart archive has more than %d values files and subcharts", maxChartDefaultsEntries))
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxChartDefaultsBytes+1))
		if err != nil || len(data) > maxChartDefaultsBytes {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest, "chart archive entry "+name+" is unreadable or too large")
		}
		if total += len(data); total > maxChartDefaultsTotalBytes {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest,
				fmt.Sprintf("chart archive values files and subcharts exceed %d bytes", maxChartDefaultsTotalBytes))
		}
		files[name] = data
	}
}
