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
// For mixed components (Helm + raw manifests), the recipe-side raw
// manifests are placed in the wrapper's templates/ directory with
// helm.sh/hook: post-install + a stable hook-weight so they apply
// after the subchart's resources.

package localformat

import (
	"bytes"
	"embed"
	stderrors "errors"
	"io"
	"strconv"
	"text/template"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"

	"gopkg.in/yaml.v3"
)

//go:embed templates/wrapper-chart.yaml.tmpl
var wrapperChartTemplates embed.FS

var wrapperChartTmpl = template.Must(
	template.ParseFS(wrapperChartTemplates, "templates/wrapper-chart.yaml.tmpl"),
)

// RenderWrapperChartYAML produces the wrapper Chart.yaml content for a
// vendored component. Exported for deployers that build their own
// vendored folder layout (e.g., flux).
func RenderWrapperChartYAML(name, parent, chartName, chartVersion string) ([]byte, error) {
	return renderWrapperChartYAML(name, parent, chartName, chartVersion)
}

// renderWrapperChartYAML produces the wrapper Chart.yaml content for a
// vendored component. Name is the wrapper chart name (== folder name
// without the NNN- prefix); ChartName/ChartVersion identify the vendored
// subchart; Parent is the originating component name (== Name today, but
// kept distinct for symmetry with writeLocalHelmFolder).
func renderWrapperChartYAML(name, parent, chartName, chartVersion string) ([]byte, error) {
	data := struct {
		Name         string
		Parent       string
		ChartName    string
		ChartVersion string
	}{
		Name:         name,
		Parent:       parent,
		ChartName:    chartName,
		ChartVersion: chartVersion,
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
	// produce non-deterministic bundle content. splitDynamicPaths and
	// renderInputForVendored already deep-copy for the same reason.
	return map[string]any{subchart: component.DeepCopyMap(values)}
}

// postInstallHookWeightBase is the starting hook-weight applied to
// recipe-side raw manifests in mixed-component wrappers. Hook weights
// are int strings; lower runs first. We set a positive base so any
// future need to inject a "very early" or "very late" wrapper-level
// hook (negative weight, or weight > base+N) has room.
const postInstallHookWeightBase = 100

// InjectPostInstallHooks rewrites a multi-document YAML stream to add
// post-install hook annotations. Exported for deployers that build
// vendored mixed-component folders outside of localformat.Write()
// (e.g., flux).
func InjectPostInstallHooks(in []byte) ([]byte, error) {
	return injectPostInstallHooks(in)
}

// injectPostInstallHooks rewrites a multi-document YAML stream to add
// `helm.sh/hook: post-install` and a stable per-document hook-weight on
// every top-level resource that doesn't already declare a hook. Used in
// mixed-component vendored wrappers so the recipe-side raw manifests
// install AFTER the vendored subchart's resources.
//
// Stable ordering: documents are visited in input order; weights start
// at postInstallHookWeightBase and increment by 1. This preserves the
// sort order that the recipe author saw in their manifest list. Callers
// MUST feed manifests in deterministic order (writeMixedManifests
// sorts by basename before calling).
//
// Idempotence: if a document already has helm.sh/hook annotation set,
// it is left untouched (and does not consume a weight). This lets
// recipes that intentionally tag a manifest as a different hook (e.g.
// pre-install) opt out of post-install coercion.
//
// Empty docs and comment-only docs are skipped in the output rather
// than preserved — yaml.v3's Decoder collapses them naturally, and the
// downstream Helm renderer doesn't care about the missing whitespace.
//
// Built on yaml.v3's streaming Decoder so document boundaries are
// recognized inside YAML's quoting and literal-block rules — a leading
// `---`, a literal-block string containing `---` on its own line, and
// interleaved comments all parse correctly.
func injectPostInstallHooks(in []byte) ([]byte, error) {
	if len(in) == 0 {
		return in, nil
	}

	dec := yaml.NewDecoder(bytes.NewReader(in))
	out := &bytes.Buffer{}
	weight := postInstallHookWeightBase
	first := true

	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"parse manifest doc for hook injection", err)
		}
		if len(doc) == 0 {
			// Comment-only / empty doc; skip.
			continue
		}

		if injectHookOnDoc(doc, weight) {
			weight++
		}

		body, mErr := yaml.Marshal(doc)
		if mErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"re-marshal manifest doc after hook injection", mErr)
		}
		if !first {
			out.WriteString("---\n")
		}
		out.Write(body)
		first = false
	}
	return out.Bytes(), nil
}

// injectHookOnDoc adds the post-install hook annotations to a parsed
// document if helm.sh/hook is not already set. Returns true when the
// caller should consume a hook-weight.
func injectHookOnDoc(doc map[string]any, weight int) bool {
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	// Recover annotations whether they were emitted as map[string]any
	// (yaml.v3 default) or as map[interface{}]interface{} (yaml.v2 /
	// upstream tooling that round-trips through that shape). Without the
	// second case we'd silently allocate a fresh map and overwrite the
	// author's existing annotations.
	annos := annotationsAsStringMap(meta["annotations"])
	if annos == nil {
		annos = map[string]any{}
	}
	meta["annotations"] = annos
	if _, alreadyHooked := annos["helm.sh/hook"]; alreadyHooked {
		// Honor an author-declared hook (could be pre-install,
		// post-upgrade, etc.). Don't overwrite, don't consume a weight.
		return false
	}
	annos["helm.sh/hook"] = "post-install"
	annos["helm.sh/hook-weight"] = strconv.Itoa(weight)
	// Honor an author-declared delete-policy for the same reason we
	// honor an author-declared hook: a recipe author who tagged a
	// resource explicitly (e.g., "keep" to preserve install-time state
	// across upgrades) means it.
	if _, alreadySetPolicy := annos["helm.sh/hook-delete-policy"]; !alreadySetPolicy {
		annos["helm.sh/hook-delete-policy"] = "before-hook-creation"
	}
	return true
}

// annotationsAsStringMap normalizes the value at metadata.annotations to
// a map[string]any regardless of which YAML library produced it. Returns
// nil when the value is absent or not a map. Preserves existing entries
// so the hook-injection caller doesn't accidentally clobber them.
func annotationsAsStringMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		// Convert keys to strings; non-string keys are skipped (Kubernetes
		// annotations are always string-keyed by the schema).
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[ks] = val
		}
		return out
	default:
		return nil
	}
}
