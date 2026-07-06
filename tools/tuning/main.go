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

// Command tuning computes the nodewright tuning-status matrix via
// pkg/tuning.Compute and renders a Markdown table. It is hermetic and offline —
// every input is a pure read of the embedded recipe catalog and component
// manifests. `make tuning-docs` runs this with -deterministic -no-title and
// splices the body into the marked region of
// docs/integrator/components/nodewright.md.
//
// Usage: tuning -out-dir <path> [-aicr-version <v>] [-deterministic] [-no-title]
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/tuning"
	"github.com/NVIDIA/aicr/tools/internal/docgen"
)

// matrixFile is the basename written under -out-dir; the Makefile splice reads it.
const matrixFile = "tuning-status.md"

func main() {
	var (
		outDir        string
		aicrVersion   string
		deterministic bool
		noTitle       bool
	)
	flag.StringVar(&outDir, "out-dir", "dist/tuning", "directory to write tuning-status.md")
	flag.StringVar(&aicrVersion, "aicr-version", "dev", "AICR version label for the non-deterministic generated stamp")
	flag.BoolVar(&deterministic, "deterministic", false, "suppress per-run metadata (timestamp) for a byte-stable committable artifact")
	flag.BoolVar(&noTitle, "no-title", false, "omit the H1 title so the body can be spliced into a larger document")
	flag.Parse()

	if err := run(context.Background(), outDir, aicrVersion, deterministic, noTitle); err != nil {
		fmt.Fprintln(os.Stderr, "tuning:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, outDir, aicrVersion string, deterministic, noTitle bool) error {
	report, err := tuning.Compute(ctx, tuning.Options{Version: aicrVersion})
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "compute tuning status")
	}
	if mkErr := os.MkdirAll(outDir, 0o755); mkErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir out-dir", mkErr)
	}
	mdPath := filepath.Join(outDir, matrixFile)
	if werr := docgen.WriteRendered(mdPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, func(w io.Writer) error {
		return renderTable(w, report, markdownOptions{
			AICRVersion:   aicrVersion,
			Deterministic: deterministic,
			NoTitle:       noTitle,
		})
	}); werr != nil {
		return werr
	}
	fmt.Printf("tuning: wrote %s (%d rows)\n", mdPath, len(report.Rows))
	return nil
}
