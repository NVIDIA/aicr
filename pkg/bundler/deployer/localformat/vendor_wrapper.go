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

// Vendored-chart wrapper helpers.
//
// When --vendor-charts is on we emit each Helm component as a single
// folder containing a wrapper Chart.yaml plus the upstream chart bytes
// under charts/. The wrapper declares the upstream chart as a
// dependencies: entry with an empty repository so Helm resolves it from
// the adjacent tarball at install time — no `helm dependency update`
// needed.
//
// Helm forwards the wrapper's values to the subchart only when nested
// under the subchart's name (or under "global"). Existing aicr values
// were authored against the upstream chart's value schema, so the
// wrapper emits them nested under the upstream chart's name.
//
// Mixed-component recipe-side manifests are NOT placed in the wrapper
// templates/ (#1835). Write emits a separate <name>-post local-helm
// folder after the vendored primary, matching the non-vendored path.

package localformat

import (
	"bytes"
	"embed"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
)

//go:embed templates/wrapper-chart.yaml.tmpl
var wrapperChartTemplates embed.FS

var wrapperChartTmpl = template.Must(
	template.New("wrapper-chart.yaml.tmpl").
		Funcs(deployer.TemplateFuncs).
		ParseFS(wrapperChartTemplates, "templates/wrapper-chart.yaml.tmpl"),
)

// WrapperChartInput carries the fields stamped into a vendored component's
// wrapper Chart.yaml. Grouped into a struct rather than passed positionally
// because six same-typed strings are easy to transpose at a call site.
type WrapperChartInput struct {
	// Name is the wrapper chart name (== folder name without the NNN- prefix).
	Name string
	// Parent is the originating component name (== Name today, but kept
	// distinct for symmetry with writeLocalHelmFolder).
	Parent string
	// ChartName and ChartVersion identify the vendored subchart and feed
	// the wrapper's dependencies: entry.
	ChartName    string
	ChartVersion string
	// AICRVersion is the raw AICR build version; it is normalized to a
	// Helm-valid SemVer here, so callers may pass "dev" unchanged.
	AICRVersion string
	// PayloadVersion is the free-form version of the content the wrapper
	// carries. Empty falls back to the normalized AICRVersion. See
	// chartStamp for why the two are separate.
	PayloadVersion string
}

// RenderWrapperChartYAML produces the wrapper Chart.yaml content for a
// vendored component. Exported for deployers that build their own
// vendored folder layout (e.g., flux).
func RenderWrapperChartYAML(in WrapperChartInput) ([]byte, error) {
	stamp := chartStamp{
		AICRVersion:    deployer.NormalizeChartVersion(in.AICRVersion),
		PayloadVersion: in.PayloadVersion,
	}
	if stamp.PayloadVersion == "" {
		stamp.PayloadVersion = stamp.AICRVersion
	}
	return renderWrapperChartYAML(wrapperSubchart{
		Name:         in.Name,
		Parent:       in.Parent,
		ChartName:    in.ChartName,
		ChartVersion: in.ChartVersion,
	}, stamp)
}

// wrapperSubchart is the identity half of WrapperChartInput — everything the
// wrapper template needs that is not a version stamp.
type wrapperSubchart struct {
	Name         string
	Parent       string
	ChartName    string
	ChartVersion string
}

// renderWrapperChartYAML executes the wrapper template against the subchart
// identity plus an already-normalized stamp. Split from the exported entry
// point so callers that derived the stamp from a Component (stampFor) do not
// re-normalize it.
func renderWrapperChartYAML(sub wrapperSubchart, stamp chartStamp) ([]byte, error) {
	data := struct {
		Name             string
		Parent           string
		ChartName        string
		ChartVersion     string
		AICRVersion      string
		ComponentVersion string
	}{
		Name:             sub.Name,
		Parent:           sub.Parent,
		ChartName:        sub.ChartName,
		ChartVersion:     sub.ChartVersion,
		AICRVersion:      stamp.AICRVersion,
		ComponentVersion: stamp.PayloadVersion,
	}
	var buf bytes.Buffer
	if err := wrapperChartTmpl.Execute(&buf, data); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "render wrapper Chart.yaml", err)
	}
	return buf.Bytes(), nil
}

// NestUnderSubchart wraps values under a single key so Helm forwards
// them to the named subchart at install time. Exported for deployers
// that build vendored folders with a wrapper chart (e.g., flux).
func NestUnderSubchart(values map[string]any, subchart string) map[string]any {
	return nestUnderSubchart(values, subchart)
}

// nestUnderSubchart wraps values under a single key so Helm forwards
// them to the named subchart at install time. Returns a fresh map with
// a deep-copied inner value; does not share state with the input. Mutating
// the result is safe and will not affect the caller's map. nil/empty
// input yields nil so callers don't emit an empty `<subchart>: {}` block
// in values.yaml.
//
// Helm's value-merging rule: a wrapper chart's values reach a subchart
// only when nested under the subchart's name (chart-resolved name, not
// alias) or under the magic key "global". We use the chart name; aliases
// are not supported in the recipe surface today.
func nestUnderSubchart(values map[string]any, subchart string) map[string]any {
	if len(values) == 0 || subchart == "" {
		return nil
	}
	// Deep-copy so the inner reference is not shared with the caller.
	// Without this, downstream writes (e.g., a later helper mutating the
	// returned map) would silently mutate the caller's values map and
	// produce non-deterministic bundle content. splitDynamicPaths already
	// deep-copies for the same reason.
	return map[string]any{subchart: component.DeepCopyMap(values)}
}
