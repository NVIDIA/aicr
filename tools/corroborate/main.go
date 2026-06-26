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

// Command corroborate reads the source-keyed corroboration evidence tree (the
// Contract 3 GCS layout synced to a local directory) and emits the deterministic
// interim-evidence dashboard: index.json + per-recipe series/<recipe>.json plus
// a self-contained static HTML/CSS/JS renderer that fetches them.
//
// The emit is byte-identical across runs from the same inputs (no clock, no
// random), so the output is safe to commit and publish from CI.
//
// Usage:
//
//	corroborate -in <evidence-dir> -out <output-dir> [-allowlist <allowlist.yaml>]
//
// Outputs:
//
//	<out>/index.html
//	<out>/data/index.json
//	<out>/data/series/<recipe>.json
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/NVIDIA/aicr/pkg/corroborate"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func main() {
	os.Exit(realMain())
}

// realMain runs the CLI and returns the process exit code. It is split from main
// so the signal-context teardown (defer stop()) runs before os.Exit, which would
// otherwise skip deferred functions.
func realMain() int {
	// Cancel an in-flight generation on Ctrl-C / SIGTERM rather than imposing an
	// arbitrary deadline: the run time scales with the (operator-supplied)
	// evidence-tree size, so signal cancellation is the right bound for a batch
	// CLI over local input.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return parseAndRun(ctx, os.Args[1:], os.Stderr)
}

// parseAndRun parses args and runs the generator, returning a process exit
// code: 0 on success, 1 on a generation error, 2 on a flag-parse error.
func parseAndRun(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("corroborate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		inDir     string
		outDir    string
		allowlist string
	)
	fs.StringVar(&inDir, "in", "", "root of the source-keyed evidence tree (Contract 3 GCS layout synced to disk)")
	fs.StringVar(&outDir, "out", "dist/corroborate", "output directory for index.html + data/")
	fs.StringVar(&allowlist, "allowlist", "", "optional signer allowlist; re-derives each source class from the verified signer instead of trusting meta.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := run(ctx, inDir, outDir, allowlist); err != nil {
		fmt.Fprintln(stderr, "corroborate:", err)
		// Preserve the coded pkg/errors exit-code contract so callers can
		// distinguish INVALID_REQUEST (2) from TIMEOUT (5), etc., rather than
		// collapsing every failure into a generic 1.
		return errors.ExitCodeFromError(err)
	}
	return 0
}

func run(ctx context.Context, inDir, outDir, allowlist string) error {
	if inDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "missing required -in <evidence-dir>")
	}
	if outDir == "" {
		// Guard against writing index.html + data/ into the current working
		// directory; the CLI flag defaults to a path, so an empty value here is
		// a direct-caller error.
		return errors.New(errors.ErrCodeInvalidRequest, "missing required -out <output-dir>")
	}
	res, err := corroborate.Generate(ctx, corroborate.Options{
		InputDir:      inDir,
		OutputDir:     outDir,
		AllowlistPath: allowlist,
	})
	if err != nil {
		return err
	}
	fmt.Printf("corroborate: wrote %s (%d recipes, %d sources, %d runs)\n",
		outDir, res.Recipes, res.Sources, res.Runs)
	return nil
}
