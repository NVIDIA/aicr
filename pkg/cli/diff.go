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
	"fmt"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/diff"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// diffCmd creates the "diff" CLI command.
func diffCmd() *cli.Command {
	return &cli.Command{
		Name:     "diff",
		Category: functionalCategoryName,
		Usage:    "Detect configuration drift by comparing snapshots or evaluating recipe constraints",
		Description: `Detect configuration drift in two modes:

  Recipe mode (--recipe + --snapshot):
    Evaluate recipe constraints against a snapshot to determine if the
    cluster still meets the recipe's requirements. Checks top-level
    constraints, component versions, and validation phase configuration.
    Reports pass/fail per constraint with severity and remediation.

  Snapshot mode (--baseline + --target):
    Compare two snapshots field-by-field to see what changed.

Examples:
  # Check if cluster matches recipe requirements (primary use case)
  aicr diff --recipe recipe.yaml --snapshot current.yaml

  # Human-readable table output
  aicr diff --recipe recipe.yaml --snapshot current.yaml --format table

  # JSON output for CI/CD pipelines with non-zero exit on drift
  aicr diff --recipe recipe.yaml --snapshot current.yaml --format json --fail-on-drift

  # Compare two snapshots to see what changed
  aicr diff --baseline before.yaml --target after.yaml

  # Compare snapshots from ConfigMaps
  aicr diff --baseline cm://default/baseline --target cm://default/current`,
		Flags: diffCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runDiffCmd(ctx, cmd)
		},
	}
}

// diffCmdFlags returns the flags for the diff command.
func diffCmdFlags() []cli.Flag {
	return []cli.Flag{
		// Recipe mode flags
		&cli.StringFlag{
			Name:     "recipe",
			Aliases:  []string{"r"},
			Usage:    "recipe file: evaluate constraints against snapshot (recipe mode)",
			Category: "Recipe Mode",
		},
		&cli.StringFlag{
			Name:     "snapshot",
			Aliases:  []string{"s"},
			Usage:    "snapshot to evaluate against recipe constraints (recipe mode)",
			Category: "Recipe Mode",
		},

		// Snapshot mode flags
		&cli.StringFlag{
			Name:     "baseline",
			Aliases:  []string{"b"},
			Usage:    "baseline snapshot for field-level comparison (snapshot mode)",
			Category: "Snapshot Mode",
		},
		&cli.StringFlag{
			Name:     "target",
			Usage:    "target snapshot for field-level comparison (snapshot mode)",
			Category: "Snapshot Mode",
		},

		// Common flags
		&cli.BoolFlag{
			Name:     "fail-on-drift",
			Usage:    "exit with non-zero status if drift is detected",
			Category: "Output",
		},
		outputFlag,
		formatFlag,
		kubeconfigFlag,
		dataFlag,
	}
}

// runDiffCmd executes the diff command.
func runDiffCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "baseline", "target", "output", "format"); err != nil {
		return err
	}

	outFormat, err := parseOutputFormat(cmd)
	if err != nil {
		return err
	}

	recipePath := cmd.String("recipe")
	snapshotPath := cmd.String("snapshot")
	baselinePath := cmd.String("baseline")
	targetPath := cmd.String("target")

	if err := initDataProvider(cmd); err != nil {
		return err
	}

	hasRecipeMode := recipePath != "" || snapshotPath != ""
	hasSnapshotMode := baselinePath != "" || targetPath != ""

	if hasRecipeMode && hasSnapshotMode {
		return errors.New(errors.ErrCodeInvalidRequest,
			"cannot mix recipe mode (--recipe/--snapshot) with snapshot mode (--baseline/--target)")
	}

	if hasRecipeMode {
		return runRecipeDiff(ctx, cmd, recipePath, snapshotPath, outFormat)
	}

	if hasSnapshotMode {
		return runSnapshotDiff(ctx, cmd, baselinePath, targetPath, outFormat)
	}

	return errors.New(errors.ErrCodeInvalidRequest,
		"specify either --recipe and --snapshot (recipe mode) or --baseline and --target (snapshot mode)")
}

// runRecipeDiff evaluates recipe constraints against a snapshot.
func runRecipeDiff(ctx context.Context, cmd *cli.Command, recipePath, snapshotPath string, outFormat serializer.Format) error {
	if recipePath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--recipe is required in recipe mode")
	}
	if snapshotPath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--snapshot is required in recipe mode")
	}

	kubeconfig := cmd.String("kubeconfig")

	slog.Debug("recipe mode", slog.String("recipe", recipePath), slog.String("snapshot", snapshotPath))

	rec, err := serializer.FromFileWithKubeconfig[recipe.RecipeResult](recipePath, kubeconfig)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load recipe from %q", recipePath), err)
	}

	snap, err := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapshotPath, kubeconfig)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %q", snapshotPath), err)
	}

	result := diff.RecipeVsSnapshot(rec, snap)
	result.BaselineSource = recipePath
	result.TargetSource = snapshotPath

	slog.Info("recipe diff complete",
		slog.Int("passed", result.Summary.ConstraintsPassed),
		slog.Int("failed", result.Summary.ConstraintsFailed),
		slog.Int("errors", result.Summary.ConstraintsError),
		slog.Int("componentsOk", result.Summary.ComponentsOK),
		slog.Int("componentsDrifted", result.Summary.ComponentsDrifted))

	if err := writeDiffResult(ctx, cmd, outFormat, result); err != nil {
		return err
	}

	if cmd.Bool("fail-on-drift") && result.HasDrift() {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("drift detected: %d constraint(s) failed, %d component(s) drifted",
				result.Summary.ConstraintsFailed, result.Summary.ComponentsDrifted))
	}

	return nil
}

// runSnapshotDiff compares two snapshots field-by-field.
func runSnapshotDiff(ctx context.Context, cmd *cli.Command, baselinePath, targetPath string, outFormat serializer.Format) error {
	if baselinePath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--baseline is required in snapshot mode")
	}
	if targetPath == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--target is required in snapshot mode")
	}

	kubeconfig := cmd.String("kubeconfig")

	slog.Debug("snapshot mode", slog.String("baseline", baselinePath), slog.String("target", targetPath))

	baseline, err := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](baselinePath, kubeconfig)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load baseline snapshot from %q", baselinePath), err)
	}

	target, err := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](targetPath, kubeconfig)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load target snapshot from %q", targetPath), err)
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
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("drift detected: %d change(s) found", result.Summary.Total))
	}

	return nil
}

// writeDiffResult serializes the diff result, using a custom table formatter
// when the output format is table.
func writeDiffResult(ctx context.Context, cmd *cli.Command, outFormat serializer.Format, result *diff.Result) error {
	output := cmd.String("output")

	// Use custom table writer for human-readable output
	if outFormat == serializer.FormatTable {
		w := os.Stdout
		if output != "" {
			f, err := os.Create(output)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output file", err)
			}
			defer f.Close()
			w = f
		}
		return diff.WriteTable(w, result)
	}

	// JSON/YAML use standard serializer
	ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create output writer", err)
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}()

	return ser.Serialize(ctx, result)
}
