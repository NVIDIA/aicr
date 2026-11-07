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

package argocdhelm

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// validateSizing rejects install-time sizing the chart could not render: a
// negative default count, rows for a component that does not read its values
// from static/, rows out of order, and a sizing file that would overwrite
// another component's static values.
func (g *Generator) validateSizing() error {
	if g.SizingNodeCount < 0 {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("sizing node count must be zero or positive, got %d", g.SizingNodeCount))
	}
	for name, rows := range g.SizingRows {
		var ref *recipe.ComponentRef
		for i := range g.RecipeResult.ComponentRefs {
			if g.RecipeResult.ComponentRefs[i].Name == name {
				ref = &g.RecipeResult.ComponentRefs[i]
			}
			if g.RecipeResult.ComponentRefs[i].Name == name+sizingFileSuffix {
				return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"component %q's static values would collide with component %q's sizing rows",
					name+sizingFileSuffix, name))
			}
		}
		if ref == nil || !ref.HasExternalChart() || g.VendorCharts {
			return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"component %q has install-time sizing rows but does not install an upstream chart from static values", name))
		}
		for i := range rows {
			if rows[i].MaxNodes <= 0 || (i > 0 && rows[i].MaxNodes <= rows[i-1].MaxNodes) {
				return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"component %q sizing rows must have positive, strictly ascending maxNodes", name))
			}
		}
	}
	return nil
}

// sizingRowsDocument is the content of static/<name>-sizing.yaml.
func sizingRowsDocument(rows []SizingRow) map[string]any {
	tiers := make([]any, 0, len(rows))
	for _, r := range rows {
		values := r.Values
		if values == nil {
			values = map[string]any{}
		}
		tiers = append(tiers, map[string]any{"maxNodes": r.MaxNodes, "values": values})
	}
	return map[string]any{"tiers": tiers}
}

// sizingSelector returns the template block that picks componentName's row
// from sizing.nodeCount, prepended to its child Application template. It
// sets $aicrSizingNodeCount, $aicrSizingRow and $aicrSizingTier (0 when the
// selected row changes nothing). Both operands of the comparison go through
// int64: --set delivers the count as int64 and a values file as float64, and
// Helm's comparison functions fail on that mix. A count of 0, or above every
// row, selects nothing.
func sizingSelector(componentName string) string {
	return fmt.Sprintf(`{{- $aicrSizingNodeCount := int64 ((.Values.%[1]s | default dict).nodeCount | default 0) -}}
{{- if lt $aicrSizingNodeCount 0 }}{{ fail (printf "%[1]s.nodeCount must be zero or positive, got %%d" $aicrSizingNodeCount) }}{{ end -}}
{{- $aicrSizingRow := dict -}}
{{- $aicrSizingTier := 0 -}}
{{- $aicrSizingFound := false -}}
{{- if gt $aicrSizingNodeCount 0 -}}
{{- range $row := ((.Files.Get "static/%[2]s%[3]s.yaml") | fromYaml | default dict).tiers -}}
{{- if and (not $aicrSizingFound) (le $aicrSizingNodeCount (int64 $row.maxNodes)) -}}
{{- $aicrSizingFound = true -}}
{{- if $row.values }}{{ $aicrSizingRow = $row.values }}{{ $aicrSizingTier = int64 $row.maxNodes }}{{ end -}}
{{- end -}}
{{- end -}}
{{- end -}}
`, rootValuesSizingKey, componentName, sizingFileSuffix)
}

// addSizingAnnotations stamps the selected count on a sized child
// Application, and a placeholder for the tier annotation that
// renderSizingTierAnnotation turns into a conditional.
func addSizingAnnotations(app map[string]any) error {
	metadata, ok := app["metadata"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'metadata'")
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations[annotationSizingNodeCount] = "{{ $aicrSizingNodeCount }}"
	annotations[annotationSizingTier] = sizingTierAnnotationSentinel
	return nil
}

// renderSizingTierAnnotation replaces the tier annotation placeholder line
// with one rendered only when the selected row changes a value; a YAML map
// cannot hold a conditional entry.
func renderSizingTierAnnotation(out []byte) ([]byte, error) {
	marker := []byte(annotationSizingTier + ": " + sizingTierAnnotationSentinel + "\n")
	if bytes.Count(out, marker) != 1 {
		return nil, errors.New(errors.ErrCodeInternal, "sizing tier annotation placeholder not found exactly once")
	}
	at := bytes.Index(out, marker)
	lineStart := bytes.LastIndexByte(out[:at], '\n') + 1
	indent := string(out[lineStart:at])
	replacement := fmt.Sprintf("{{- if $aicrSizingTier }}\n%s%s: {{ $aicrSizingTier | quote }}\n{{- end }}\n",
		indent, annotationSizingTier)
	var b bytes.Buffer
	b.Write(out[:lineStart])
	b.WriteString(replacement)
	b.Write(out[at+len(marker):])
	return b.Bytes(), nil
}

// writeSizingReadme documents sizing.nodeCount in the bundle README when any
// component is sized at install.
func (g *Generator) writeSizingReadme(buf *strings.Builder) {
	if len(g.SizingRows) == 0 {
		return
	}
	sized := make([]string, 0, len(g.SizingRows))
	for name := range g.SizingRows {
		sized = append(sized, "`"+name+"`")
	}
	sort.Strings(sized)
	buf.WriteString("\n## Memory Sizing\n\n")
	fmt.Fprintf(buf, "`--set sizing.nodeCount=<GPU nodes>` lowers the controller memory of %s\n", strings.Join(sized, ", "))
	buf.WriteString("to the measured tier for that count (ADR-030). It defaults to the\n" +
		"bundle's `--nodes` (here " + strconv.Itoa(g.SizingNodeCount) + "); 0, or a count above every tier, keeps\n" +
		"the bundle's values. Count the GPU nodes the cluster will reach before\n" +
		"the next upgrade: a count too low risks OOM kills. Each sized child\n" +
		"Application carries `" + annotationSizingNodeCount + "`, and\n" +
		"`" + annotationSizingTier + "` when a tier changed its values.\n\n" +
		"A component's own root-chart values still win. The chart never compares\n" +
		"quantities, so a memory request set at install above the selected limit\n" +
		"is rejected by the API server when Argo CD syncs, not by `helm install`.\n")
}
