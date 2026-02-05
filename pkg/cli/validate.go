// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"github.com/NVIDIA/eidos/pkg/validator"
)

func validateCmd() *cli.Command {
	return &cli.Command{
		Name:                  "validate",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Validate cluster using specific recipe.",
		Description: `Validate a system snapshot against the constraints defined in a recipe.

This command compares actual system measurements from a snapshot against the
expected constraints defined in a recipe file. It reports which constraints
pass, fail, or cannot be evaluated.

# Examples

Validate a snapshot against a recipe:
  eidos validate --recipe recipe.yaml --snapshot snapshot.yaml

Load snapshot from ConfigMap (results to stdout):
  eidos validate --recipe recipe.yaml --snapshot cm://gpu-operator/eidos-snapshot

Output validation result to a file:
  eidos validate -r recipe.yaml -s snapshot.yaml -o result.yaml

Run validation without failing on constraint errors (informational mode):
  eidos validate -r recipe.yaml -s snapshot.yaml --fail-on-error=false
`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "recipe",
				Aliases:  []string{"r"},
				Required: true,
				Usage: `Path/URI to recipe file containing constraints to validate.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).`,
			},
			&cli.StringFlag{
				Name:     "snapshot",
				Aliases:  []string{"s"},
				Required: true,
				Usage: `Path/URI to snapshot file containing actual system measurements.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).`,
			},
			&cli.StringFlag{
				Name:  "phase",
				Value: "readiness",
				Usage: `Validation phase to run.
	Options: "readiness", "deployment", "performance", "conformance", "all".
	Default: "readiness" (quick readiness check).`,
			},
			&cli.BoolFlag{
				Name:  "fail-on-error",
				Value: true,
				Usage: "Exit with non-zero status if any constraint fails validation",
			},
			outputFlag,
			formatFlag,
			kubeconfigFlag,
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Parse output format
			outFormat, err := parseOutputFormat(cmd)
			if err != nil {
				return err
			}

			recipeFilePath := cmd.String("recipe")
			snapshotFilePath := cmd.String("snapshot")
			kubeconfig := cmd.String("kubeconfig")
			phaseStr := cmd.String("phase")
			failOnError := cmd.Bool("fail-on-error")

			// Parse phase
			var phase validator.ValidationPhaseName
			switch phaseStr {
			case "readiness":
				phase = validator.PhaseReadiness
			case "deployment":
				phase = validator.PhaseDeployment
			case "performance":
				phase = validator.PhasePerformance
			case "conformance":
				phase = validator.PhaseConformance
			case "all":
				phase = validator.PhaseAll
			default:
				return fmt.Errorf("invalid phase %q: must be one of: readiness, deployment, performance, conformance, all", phaseStr)
			}

			slog.Info("loading recipe", "uri", recipeFilePath)

			// Load recipe
			rec, err := serializer.FromFileWithKubeconfig[recipe.RecipeResult](recipeFilePath, kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to load recipe from %q: %w", recipeFilePath, err)
			}

			slog.Info("loading snapshot", "uri", snapshotFilePath)

			// Load snapshot
			snap, err := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapshotFilePath, kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to load snapshot from %q: %w", snapshotFilePath, err)
			}

			slog.Info("running validation",
				"recipe", recipeFilePath,
				"snapshot", snapshotFilePath,
				"phase", phase,
				"constraints", len(rec.Constraints))

			// Create validator
			v := validator.New(
				validator.WithVersion(version),
			)

			// Validate with phase support
			result, err := v.ValidatePhase(ctx, phase, rec, snap)
			if err != nil {
				return fmt.Errorf("validation failed: %w", err)
			}

			// Set source information
			result.RecipeSource = recipeFilePath
			result.SnapshotSource = snapshotFilePath

			// Serialize output
			output := cmd.String("output")
			ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
			if err != nil {
				return fmt.Errorf("failed to create output writer: %w", err)
			}
			defer func() {
				if closer, ok := ser.(interface{ Close() error }); ok {
					if err := closer.Close(); err != nil {
						slog.Warn("failed to close serializer", "error", err)
					}
				}
			}()

			if err := ser.Serialize(ctx, result); err != nil {
				return fmt.Errorf("failed to serialize validation result: %w", err)
			}

			slog.Info("validation completed",
				"status", result.Summary.Status,
				"passed", result.Summary.Passed,
				"failed", result.Summary.Failed,
				"skipped", result.Summary.Skipped,
				"duration", result.Summary.Duration)

			// Check if we should fail on validation errors
			if failOnError && result.Summary.Status == validator.ValidationStatusFail {
				return fmt.Errorf("validation failed: %d constraint(s) did not pass", result.Summary.Failed)
			}

			return nil
		},
	}
}
