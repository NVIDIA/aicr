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

package cli

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/diff"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// diffCmd creates the "diff" CLI command.
func diffCmd() *cli.Command {
	return &cli.Command{
		Name:     "diff",
		Category: functionalCategoryName,
		Usage:    "Compare two snapshots to detect configuration drift",
		Description: `Compare two snapshots field-by-field to see what changed between
cluster states. Reports added, removed, and modified readings.

Examples:
  # Compare two snapshots
  aicr diff --baseline before.yaml --target after.yaml

  # Human-readable table output
  aicr diff --baseline before.yaml --target after.yaml --format table

  # JSON output for CI/CD pipelines with non-zero exit on drift
  aicr diff --baseline before.yaml --target after.yaml --format json --fail-on-drift

  # Compare snapshots from ConfigMaps
  aicr diff --baseline cm://default/baseline --target cm://default/current`,
		Flags:  diffCmdFlags(),
		Action: runDiffCmd,
	}
}

// diffCmdFlags returns the flags for the diff command.
func diffCmdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "baseline",
			Aliases: []string{"b"},
			Usage:   "baseline snapshot (file path or ConfigMap URI)",
		},
		&cli.StringFlag{
			Name:  "target",
			Usage: "target snapshot (file path or ConfigMap URI)",
		},
		&cli.BoolFlag{
			Name:  "fail-on-drift",
			Usage: "exit with non-zero status if drift is detected",
		},
		outputFlag(),
		formatFlag(),
		kubeconfigFlag(),
	}
}

// runDiffCmd executes the diff command.
func runDiffCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, "baseline", "target", "output", "format", "kubeconfig"); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.CLISnapshotTimeout)
	defer cancel()

	outFormat, err := parseOutputFormat(cmd)
	if err != nil {
		return err
	}

	baselinePath := cmd.String("baseline")
	targetPath := cmd.String("target")

	if baselinePath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--baseline is required")
	}
	if targetPath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--target is required")
	}

	kubeconfig := cmd.String("kubeconfig")

	slog.Debug("snapshot diff", slog.String("baseline", baselinePath), slog.String("target", targetPath))

	baseline, err := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](baselinePath, kubeconfig)
	if err != nil {
		return err
	}

	target, err := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](targetPath, kubeconfig)
	if err != nil {
		return err
	}

	result := diff.Snapshots(baseline, target)
	result.BaselineSource = baselinePath
	result.TargetSource = targetPath

	slog.Info("snapshot diff complete",
		slog.Int("added", result.Summary.Added),
		slog.Int("removed", result.Summary.Removed),
		slog.Int("modified", result.Summary.Modified))

	if err := writeDiffResult(ctx, cmd, outFormat, result); err != nil {
		return err
	}

	if cmd.Bool("fail-on-drift") && result.HasDrift() {
		return errors.New(errors.ErrCodeConflict,
			fmt.Sprintf("drift detected: %d change(s) found", result.Summary.Total))
	}

	return nil
}

// writeDiffResult serializes the diff result, using a custom table formatter
// when the output format is table. Uses a named return so Close() failures
// on writable handles are merged with any earlier error via errors.Join —
// data-loss on flush must surface even when the write itself also failed.
func writeDiffResult(ctx context.Context, cmd *cli.Command, outFormat serializer.Format, result *diff.Result) (err error) {
	output := cmd.String("output")

	// Use custom table writer for human-readable output
	if outFormat == serializer.FormatTable {
		output = strings.TrimSpace(output)
		if strings.HasPrefix(output, serializer.ConfigMapURIScheme) {
			return errors.New(errors.ErrCodeInvalidRequest, "table output does not support ConfigMap destinations")
		}
		w := cmd.Root().Writer
		if output != "" && output != "-" && output != serializer.StdoutURI {
			f, createErr := os.Create(output)
			if createErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output file", createErr)
			}
			defer func() {
				if closeErr := f.Close(); closeErr != nil {
					err = stderrors.Join(err, errors.Wrap(errors.ErrCodeInternal, "failed to close output file", closeErr))
				}
			}()
			w = f
		}
		return diff.WriteTable(w, result)
	}

	// JSON/YAML use standard serializer
	ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
	if err != nil {
		return err
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				err = stderrors.Join(err, errors.Wrap(errors.ErrCodeInternal, "failed to close serializer", closeErr))
			}
		}
	}()

	return ser.Serialize(ctx, result)
}
