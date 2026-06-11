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

// Command coverage generates docs/user/coverage-matrix.md — a structural matrix
// of which critical user journeys (CUJs) and CLI verbs are exercised by an
// in-repo test or demo, on what hardware class, and at what cadence. CLI verbs
// are derived from the live pkg/cli command registry so a new verb surfaces as a
// row automatically. Live pass/fail is a link into the AICR TestGrid, never
// embedded here.
//
// Usage: coverage -repo-root <path> -out docs/user/coverage-matrix.md -deterministic
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func main() {
	var (
		repoRoot      string
		out           string
		deterministic bool
		noTitle       bool
	)
	flag.StringVar(&repoRoot, "repo-root", ".", "path to the AICR repository root")
	flag.StringVar(&out, "out", "docs/user/coverage-matrix.md", "output Markdown file")
	flag.BoolVar(&deterministic, "deterministic", false, "suppress per-run metadata for committable artifacts (output is deterministic regardless)")
	flag.BoolVar(&noTitle, "no-title", false, "omit the H1 title so the body can embed as a section")
	flag.Parse()

	if err := run(repoRoot, out, deterministic, noTitle); err != nil {
		fmt.Fprintln(os.Stderr, "coverage:", err)
		os.Exit(1)
	}
}

func run(repoRoot, out string, deterministic, noTitle bool) error {
	matrix := BuildMatrix(repoRoot)
	body := Render(matrix, deterministic, noTitle)

	if dir := filepath.Dir(out); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "mkdir out dir", err)
		}
	}
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil { //nolint:gosec // doc artifact, world-readable by design
		return errors.Wrap(errors.ErrCodeInternal, "write "+out, err)
	}

	covered, notYet, stubbed := tally(matrix)
	fmt.Printf("coverage: wrote %s (%d rows: %d covered, %d not-yet-covered, %d stubbed)\n",
		out, len(matrix.Rows), covered, notYet, stubbed)
	return nil
}

func tally(m Matrix) (covered, notYet, stubbed int) {
	for _, r := range m.Rows {
		switch r.Status {
		case StatusCovered:
			covered++
		case StatusNotYetCovered:
			notYet++
		case StatusStubbed:
			stubbed++
		}
	}
	return covered, notYet, stubbed
}
