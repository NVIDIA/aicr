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
)

// WriteMarkdown emits a human-readable summary of a component-level BOM
// suitable for embedding in docs.
func WriteMarkdown(w io.Writer, meta Metadata, results []ComponentResult) error {
	// Copy before sorting so callers don't observe their input reordered.
	sorted := append([]ComponentResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})
	results = sorted

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

	if _, err := fmt.Fprintf(w, "# %s — Container Image Inventory\n\n", titleFor(meta)); err != nil {
		return err
	}
	if !meta.Deterministic {
		if _, err := fmt.Fprintf(w, "_Generated %s for %s %s._\n\n",
			time.Now().UTC().Format(time.RFC3339), meta.Name, meta.Version); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w, "## Summary\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Components: **%d**\n", len(results)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Unique images: **%d**\n", totalImages); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Distinct registries: **%d**\n\n", len(totalRegistries)); err != nil {
		return err
	}

	regs := make([]string, 0, len(totalRegistries))
	for r := range totalRegistries {
		regs = append(regs, r)
	}
	sort.Strings(regs)
	if _, err := fmt.Fprintf(w, "Registries: %s\n\n", strings.Join(quoteAll(regs), ", ")); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "## Components\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "| Component | Type | Chart | Pinned Version | Images |"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "|-----------|------|-------|----------------|--------|"); err != nil {
		return err
	}
	for _, r := range results {
		chart := r.Chart
		if chart == "" {
			chart = "—"
		}
		ver := r.Version
		if ver == "" {
			ver = "—"
		}
		if _, err := fmt.Fprintf(w, "| %s | %s | %s | %s | %d |\n",
			r.Name, r.Type, chart, ver, len(r.Images)); err != nil {
			return err
		}
	}

	if _, err := fmt.Fprintf(w, "\n## Images by component\n\n"); err != nil {
		return err
	}
	for _, r := range results {
		if _, err := fmt.Fprintf(w, "### %s\n\n", r.Name); err != nil {
			return err
		}
		for _, warn := range r.Warnings {
			if _, err := fmt.Fprintf(w, "> Warning: %s\n\n", warn); err != nil {
				return err
			}
		}
		if len(r.Images) == 0 {
			if _, err := fmt.Fprintln(w, "_No images extracted._"); err != nil {
				return err
			}
		} else {
			for _, img := range r.Images {
				if _, err := fmt.Fprintf(w, "- `%s`\n", img); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	return nil
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
