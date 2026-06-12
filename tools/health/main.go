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

// Command health computes catalog-wide recipe structural health via
// pkg/health.Compute and renders a Markdown matrix. Unlike tools/bom — which
// does a live `helm template` render to catch upstream image drift — this
// generator is hermetic and offline: every signal it scores is a pure read of
// the resolved recipe, so it needs no network, no GPU, and no cluster.
//
// Usage: health -out-dir <path> [-summary-out <path>] [-aicr-version <v>] [-deterministic] [-no-title]
//
// Outputs:
//
//	<out-dir>/recipe-health.md       always — the committable matrix
//	<summary-out>                    when -summary-out is set — the per-dimension
//	                                 structural detail (appended), the content the
//	                                 weekly health-refresh workflow points at
//	                                 $GITHUB_STEP_SUMMARY
//
// `make recipe-health-docs` runs this with -deterministic -no-title and
// splices the matrix body into the marked region of docs/user/recipe-health.md.
// `make recipe-health-summary` additionally sets -summary-out for the workflow.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/health"
)

// matrixFile is the basename of the rendered Markdown matrix written under
// -out-dir. The Makefile splice target reads this exact name.
const matrixFile = "recipe-health.md"

func main() {
	var (
		outDir        string
		summaryOut    string
		aicrVersion   string
		deterministic bool
		noTitle       bool
	)
	flag.StringVar(&outDir, "out-dir", "dist/health", "directory to write recipe-health.md")
	flag.StringVar(&summaryOut, "summary-out", "", "when set, append the per-dimension structural detail to this path (e.g. $GITHUB_STEP_SUMMARY); empty skips it")
	flag.StringVar(&aicrVersion, "aicr-version", "dev", "AICR version label embedded in the non-deterministic generated-stamp line")
	flag.BoolVar(&deterministic, "deterministic", false, "suppress per-run metadata (the generated timestamp) so the Markdown output is byte-stable and committable")
	flag.BoolVar(&noTitle, "no-title", false, "omit the H1 title in the Markdown output so the body can be embedded as a section of a larger document")
	flag.Parse()

	if err := run(context.Background(), outDir, summaryOut, aicrVersion, deterministic, noTitle); err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, outDir, summaryOut, aicrVersion string, deterministic, noTitle bool) error {
	// Provider nil resolves against the package-global embedded catalog, so the
	// run is hermetic — no repo-root or filesystem inputs beyond the binary.
	report, err := health.Compute(ctx, health.Options{Version: aicrVersion})
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "compute recipe health")
	}

	if mkErr := os.MkdirAll(outDir, 0o755); mkErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir out-dir", mkErr)
	}

	mdPath := filepath.Join(outDir, matrixFile)
	if err := writeRendered(mdPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, func(w io.Writer) error {
		return renderMatrix(w, report, markdownOptions{
			AICRVersion:   aicrVersion,
			Deterministic: deterministic,
			NoTitle:       noTitle,
		})
	}); err != nil {
		return err
	}

	// The per-dimension detail is appended (not truncated) so the workflow can
	// point -summary-out at $GITHUB_STEP_SUMMARY, which earlier steps may have
	// already written to, matching the file's documented append (`>>`) contract.
	if summaryOut != "" {
		if err := writeRendered(summaryOut, os.O_CREATE|os.O_APPEND|os.O_WRONLY, func(w io.Writer) error {
			return renderDetail(w, report)
		}); err != nil {
			return err
		}
	}

	fmt.Printf("health: wrote %s (%d recipes)\n", mdPath, len(report.Combos))
	return nil
}

// writeRendered opens path with the given flags and invokes render against it,
// checking the writable Close() error per the repo's file-handle rule. A
// render error is returned as-is (renderMatrix/renderDetail already return a
// structured ErrCodeInternal error, so re-wrapping would double-code it).
func writeRendered(path string, flag int, render func(io.Writer) error) error {
	f, err := os.OpenFile(path, flag, 0o644) //nolint:gosec // path is a trusted operator flag or runner-supplied ($GITHUB_STEP_SUMMARY), never attacker-controlled
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "open "+path, err)
	}
	renderErr := render(f)
	closeErr := f.Close()
	if renderErr != nil {
		return renderErr
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+path, closeErr)
	}
	return nil
}
