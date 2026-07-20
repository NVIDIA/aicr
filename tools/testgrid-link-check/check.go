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
	"fmt"
	"io"
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/testgrid"
)

// state is a link's classification against the live dashboard data.
type state string

const (
	// stateResolved: RQ1 linked this coordinate and the live dashboard serves
	// data for it — the link is good.
	stateResolved state = "resolved"

	// stateMissingButPresent: RQ1 linked this coordinate but the live dashboard
	// no longer serves data for it — a dead Evidence link. This is the only
	// warning the bot raises. It never blocks a merge; it prompts a maintainer
	// to drop or re-confirm the entry in pkg/testgrid/presence.yaml.
	stateMissingButPresent state = "missing-but-present"

	// stateNotYetLinked: the live dashboard serves a coordinate that RQ1 has not
	// linked yet (the committed manifest lags real coverage). Expected and
	// informational — the publish/ingest lag is normal — not a warning.
	stateNotYetLinked state = "not-yet-linked"
)

// result is one coordinate's classification.
type result struct {
	Path  string
	State state
}

// classify compares the committed presence paths (the links RQ1 emits) against
// the coordinate paths the live dashboard actually serves, and returns one
// result per coordinate in deterministic (state, then path) order:
//
//   - committed ∩ live      → resolved
//   - committed \ live      → missing-but-present (warn: dead link)
//   - live \ committed      → not-yet-linked (ok: coverage grew, manifest lags)
//
// Both inputs are plain sets, so the comparison is pure and fully unit-testable
// without any network.
func classify(committed []string, live map[string]struct{}) []result {
	committedSet := make(map[string]struct{}, len(committed))
	var results []result

	for _, path := range committed {
		committedSet[path] = struct{}{}
		st := stateMissingButPresent
		if _, ok := live[path]; ok {
			st = stateResolved
		}
		results = append(results, result{Path: path, State: st})
	}

	for path := range live {
		if _, ok := committedSet[path]; !ok {
			results = append(results, result{Path: path, State: stateNotYetLinked})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].State != results[j].State {
			return results[i].State < results[j].State
		}
		return results[i].Path < results[j].Path
	})
	return results
}

// warnCount returns the number of dead-link warnings in results.
func warnCount(results []result) int {
	n := 0
	for _, r := range results {
		if r.State == stateMissingButPresent {
			n++
		}
	}
	return n
}

// renderReport writes the Markdown report the workflow appends to
// $GITHUB_STEP_SUMMARY. It is deterministic for a given (committed, live) pair
// and carries no timestamp, so a dry run is byte-reproducible. The report
// always renders — even with zero warnings — so the weekly run leaves an
// audit trail either way.
func renderReport(w io.Writer, results []result) error {
	sw := &stickyWriter{w: w}

	warns := warnCount(results)
	fmt.Fprintf(sw, "## TestGrid link check\n\n")
	fmt.Fprintf(sw, "Verifies each Evidence deep-link in `docs/user/recipe-health.md` still "+
		"resolves against the live dashboard data at [%s](%s). Advisory only — never blocks merges, "+
		"never edits the doc.\n\n", testgrid.Origin, testgrid.DataURL)

	if warns == 0 {
		fmt.Fprintf(sw, "✅ **No dead links.** Every linked coordinate resolves.\n\n")
	} else {
		fmt.Fprintf(sw, "⚠️ **%d dead link(s).** A recipe links a coordinate the live dashboard no longer "+
			"serves — drop or re-confirm the entry in `pkg/testgrid/presence.yaml`.\n\n", warns)
	}

	fmt.Fprintln(sw, "| Coordinate | State |")
	fmt.Fprintln(sw, "|------------|-------|")
	for _, r := range results {
		fmt.Fprintf(sw, "| `%s` | %s |\n", r.Path, stateLabel(r.State))
	}
	fmt.Fprintln(sw)

	if sw.err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "write testgrid link-check report", sw.err)
	}
	return nil
}

// stateLabel renders a state with a leading glyph for the report.
func stateLabel(st state) string {
	switch st {
	case stateResolved:
		return "✅ resolved"
	case stateMissingButPresent:
		return "⚠️ missing-but-present (dead link)"
	case stateNotYetLinked:
		return "· not-yet-linked (expected)"
	default:
		return string(st)
	}
}

// stickyWriter remembers the first write error so callers check once at the
// end. Mirrors the helper in tools/health.
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
