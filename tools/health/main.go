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
// Usage: health -out-dir <path> [-aicr-version <v>] [-deterministic] [-no-title]
//
// Outputs:
//
//	<out-dir>/recipe-health.md
//
// `make recipe-health-docs` runs this with -deterministic -no-title and
// splices the body into the marked region of docs/user/recipe-health.md.
package main

import (
	"context"
	"flag"
	"fmt"
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
		aicrVersion   string
		deterministic bool
		noTitle       bool
	)
	flag.StringVar(&outDir, "out-dir", "dist/health", "directory to write recipe-health.md")
	flag.StringVar(&aicrVersion, "aicr-version", "dev", "AICR version label embedded in the non-deterministic generated-stamp line")
	flag.BoolVar(&deterministic, "deterministic", false, "suppress per-run metadata (the generated timestamp) so the Markdown output is byte-stable and committable")
	flag.BoolVar(&noTitle, "no-title", false, "omit the H1 title in the Markdown output so the body can be embedded as a section of a larger document")
	flag.Parse()

	if err := run(context.Background(), outDir, aicrVersion, deterministic, noTitle); err != nil {
		fmt.Fprintln(os.Stderr, "health:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, outDir, aicrVersion string, deterministic, noTitle bool) error {
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
	mf, err := os.Create(mdPath) //nolint:gosec // outDir is operator-supplied
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create "+mdPath, err)
	}
	mdErr := renderMatrix(mf, report, markdownOptions{
		AICRVersion:   aicrVersion,
		Deterministic: deterministic,
		NoTitle:       noTitle,
	})
	closeErr := mf.Close()
	if mdErr != nil {
		// renderMatrix already returns a structured ErrCodeInternal error;
		// return it as-is rather than double-wrapping with the same code.
		return mdErr
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+mdPath, closeErr)
	}

	fmt.Printf("health: wrote %s (%d recipes)\n", mdPath, len(report.Combos))
	return nil
}
