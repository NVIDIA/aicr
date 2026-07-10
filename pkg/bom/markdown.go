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

package bom

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// stickyWriter wraps an io.Writer and remembers the first write error so the
// caller can check once at the end instead of after every Fprintf. Subsequent
// writes after a failure are no-ops.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// WriteMarkdown emits a human-readable summary of a component-level BOM
// suitable for embedding in docs.
func WriteMarkdown(w io.Writer, meta Metadata, results []ComponentResult) error {
	return writeMarkdownDoc(w, meta, results, nil)
}

// writeMarkdownDoc is the shared writer behind WriteMarkdown (nil variants:
// legacy output, byte-identical) and WriteMarkdownWithVariants.
func writeMarkdownDoc(w io.Writer, meta Metadata, results []ComponentResult, variants []VariantResult) error {
	// Copy before sorting so callers don't observe their input reordered.
	sorted := append([]ComponentResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})
	results = sorted

	sortedVariants := append([]VariantResult(nil), variants...)
	sort.Slice(sortedVariants, func(i, j int) bool {
		if sortedVariants[i].Name != sortedVariants[j].Name {
			return sortedVariants[i].Name < sortedVariants[j].Name
		}
		return sortedVariants[i].Version < sortedVariants[j].Version
	})
	variants = sortedVariants

	var (
		totalImages     int
		totalRegistries = map[string]struct{}{}
		uniqueImages    = map[string]struct{}{}
	)
	for _, r := range results {
		for _, img := range r.Images {
			if _, dup := uniqueImages[img]; !dup {
				uniqueImages[img] = struct{}{}
				totalImages++
				totalRegistries[ParseImageRef(img).Registry] = struct{}{}
			}
		}
	}
	for _, v := range variants {
		for _, img := range v.Images {
			if _, dup := uniqueImages[img]; !dup {
				uniqueImages[img] = struct{}{}
				totalImages++
				totalRegistries[ParseImageRef(img).Registry] = struct{}{}
			}
		}
	}

	sw := &stickyWriter{w: w}

	if !meta.NoTitle {
		fmt.Fprintf(sw, "# %s — Container Image Inventory\n\n", titleFor(meta))
	}
	if !meta.Deterministic {
		// Honor an injected Timestamp (e.g., commit-derived) so the markdown
		// output matches the CycloneDX BOM, which already respects
		// meta.Timestamp in BuildBOM. Only fall back to wall-clock when both
		// the caller hasn't injected and Deterministic mode is off.
		ts := meta.Timestamp
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(sw, "_Generated %s for %s %s._\n\n", ts, meta.Name, meta.Version)
	}

	fmt.Fprintf(sw, "## Summary\n\n")
	fmt.Fprintf(sw, "- Components: **%d**\n", len(results))
	fmt.Fprintf(sw, "- Unique images: **%d**\n", totalImages)
	fmt.Fprintf(sw, "- Distinct registries: **%d**\n\n", len(totalRegistries))

	regs := make([]string, 0, len(totalRegistries))
	for r := range totalRegistries {
		regs = append(regs, r)
	}
	sort.Strings(regs)
	fmt.Fprintf(sw, "Registries: %s\n\n", strings.Join(quoteAll(regs), ", "))

	// State the rendering fidelity when the producer supplies one (the
	// CycloneDX metadata carries the same note for JSON consumers). The
	// value goes in a code span so a literal placeholder like <name> renders
	// as text instead of being swallowed as raw HTML. Legacy callers never
	// set the field, so their output is unchanged.
	if meta.RenderFidelity != "" {
		fmt.Fprintf(sw, "_Rendering fidelity:_ `%s`\n\n", meta.RenderFidelity)
	}

	fmt.Fprintf(sw, "## Components\n\n")
	fmt.Fprintln(sw, "| Component | Type | Chart | Pinned Version | Images |")
	fmt.Fprintln(sw, "|-----------|------|-------|----------------|--------|")
	for _, r := range results {
		chart := r.Chart
		if chart == "" {
			chart = "—"
		}
		ver := r.Version
		if ver == "" {
			ver = "—"
		}
		fmt.Fprintf(sw, "| %s | %s | %s | %s | %d |\n",
			r.Name, r.Type, chart, ver, len(r.Images))
	}

	// Version variants: a separate table (with distinct column headers) so
	// the one-row-per-registry-component Components table — and its
	// freshness gate — remains intact. Omitted entirely when no divergent
	// pins exist.
	if len(variants) > 0 {
		fmt.Fprintf(sw, "\n## Version variants\n\n")
		fmt.Fprintln(sw, "These versions are explicitly pinned by the listed sources and differ")
		fmt.Fprintln(sw, "from the component's registry default above.")
		fmt.Fprintln(sw)
		fmt.Fprintln(sw, "| Component | Variant Version | Declared By | Images |")
		fmt.Fprintln(sw, "|-----------|-----------------|-------------|--------|")
		for _, v := range variants {
			srcs := append([]string(nil), v.Sources...)
			sort.Strings(srcs)
			fmt.Fprintf(sw, "| %s | %s | %s | %d |\n",
				v.Name, v.Version, strings.Join(srcs, ", "), len(v.Images))
		}
	}

	fmt.Fprintf(sw, "\n## Images by component\n\n")
	for _, r := range results {
		writeImageSection(sw, r.Name, r.Warnings, r.Images)
	}
	for _, v := range variants {
		writeImageSection(sw, v.Name+"@"+v.Version+" (variant)", v.Warnings, v.Images)
	}

	if sw.err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write markdown BOM", sw.err)
	}
	return nil
}

// writeImageSection renders one "### <heading>" block with its warnings and
// image list; shared by default components and version variants.
func writeImageSection(sw *stickyWriter, heading string, warnings, images []string) {
	fmt.Fprintf(sw, "### %s\n\n", heading)
	for _, warn := range warnings {
		fmt.Fprintf(sw, "> Warning: %s\n\n", warn)
	}
	if len(images) == 0 {
		fmt.Fprintln(sw, "_No images extracted._")
	} else {
		for _, img := range images {
			fmt.Fprintf(sw, "- `%s`\n", img)
		}
	}
	fmt.Fprintln(sw)
}

func titleFor(m Metadata) string {
	if m.Description != "" {
		return m.Description
	}
	if m.Name != "" {
		return m.Name
	}
	return "AICR"
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = "`" + s + "`"
	}
	return out
}
