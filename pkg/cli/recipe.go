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
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func recipeCmdFlags() []cli.Flag {
	return []cli.Flag{
		withCompletions(&cli.StringFlag{
			Name:     "service",
			Usage:    fmt.Sprintf("Kubernetes service type (e.g. %s)", strings.Join(recipe.GetCriteriaServiceTypes(), ", ")),
			Category: "Query Parameters",
		}, recipe.GetCriteriaServiceTypes),
		withCompletions(&cli.StringFlag{
			Name:     "accelerator",
			Aliases:  []string{"gpu"},
			Usage:    fmt.Sprintf("Accelerator/GPU type (e.g. %s)", strings.Join(recipe.GetCriteriaAcceleratorTypes(), ", ")),
			Category: "Query Parameters",
		}, recipe.GetCriteriaAcceleratorTypes),
		withCompletions(&cli.StringFlag{
			Name:     "intent",
			Usage:    fmt.Sprintf("Workload intent (e.g. %s)", strings.Join(recipe.GetCriteriaIntentTypes(), ", ")),
			Category: "Query Parameters",
		}, recipe.GetCriteriaIntentTypes),
		withCompletions(&cli.StringFlag{
			Name:     "os",
			Usage:    fmt.Sprintf("Operating system type of the GPU node (e.g. %s)", strings.Join(recipe.GetCriteriaOSTypes(), ", ")),
			Category: "Query Parameters",
		}, recipe.GetCriteriaOSTypes),
		withCompletions(&cli.StringFlag{
			Name:     "platform",
			Usage:    fmt.Sprintf("Platform/framework type to include in the runtime (e.g. %s)", strings.Join(recipe.GetCriteriaPlatformTypes(), ", ")),
			Category: "Query Parameters",
		}, recipe.GetCriteriaPlatformTypes),
		&cli.IntFlag{
			Name:     "nodes",
			Usage:    "Number of worker/GPU nodes in the cluster",
			Category: "Query Parameters",
		},
		&cli.StringFlag{
			Name:    "snapshot",
			Aliases: []string{"s"},
			Usage: `Path/URI to previously generated configuration snapshot.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	If provided, criteria are extracted from the snapshot.`,
			Category: "Input",
		},
		&cli.StringFlag{
			Name:    "criteria",
			Aliases: []string{"c"},
			Usage: `Path to criteria file (YAML/JSON), alternative to individual flags.
	Criteria file fields can be overridden by individual flags.`,
			Category: "Input",
		},
		dataFlag,
		outputFlag,
		formatFlag(),
		kubeconfigFlag,
	}
}

func recipeCmd() *cli.Command {
	return &cli.Command{
		Name:     "recipe",
		Category: functionalCategoryName,
		Usage:    "Create optimized recipe for given intent and environment parameters.",
		Description: `Generate configuration recipe based on specified environment parameters including:
  - Kubernetes service type (e.g. eks, gke, aks, oke, self-managed)
  - Accelerator type (e.g. h100, gb200, a100, l40)
  - Workload intent (e.g. training, inference)
  - GPU node operating system (e.g. ubuntu, rhel, cos, amazonlinux)
  - Number of GPU nodes in the cluster

The recipe returns a list of components with deployment order based on dependencies.
Output can be in JSON or YAML format.

Examples:

Generate recipe from explicit criteria:
  aicr recipe --service eks --accelerator h100 --os ubuntu --intent training

Generate recipe from a criteria file:
  aicr recipe --criteria criteria.yaml

Generate recipe from a snapshot file:
  aicr recipe --snapshot snapshot.yaml

Generate recipe from a ConfigMap snapshot:
  aicr recipe --snapshot cm://gpu-operator/aicr-snapshot

Save recipe to a file:
  aicr recipe --snapshot cm://gpu-operator/aicr-snapshot -o recipe.yaml

Override criteria file values with flags:
  aicr recipe --criteria criteria.yaml --service gke

Override snapshot-detected criteria:
  aicr recipe --snapshot cm://gpu-operator/aicr-snapshot --service gke`,
		Flags: recipeCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, "service", "accelerator", "intent", "os", "platform", "snapshot", "criteria", "output", "format"); err != nil {
				return err
			}

			if err := initDataProvider(cmd); err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", err)
			}

			outFormat, err := parseOutputFormat(cmd)
			if err != nil {
				return err
			}

			result, err := buildRecipeFromCmd(ctx, cmd)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "error building recipe", err)
			}

			// Log constraint warnings for visibility
			if result != nil && len(result.Metadata.ConstraintWarnings) > 0 {
				for _, w := range result.Metadata.ConstraintWarnings {
					slog.Warn("overlay excluded due to constraint failure",
						"overlay", w.Overlay,
						"constraint", w.Constraint,
						"expected", w.Expected,
						"actual", w.Actual,
						"reason", w.Reason)
				}
			}

			output := cmd.String("output")
			ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output writer", err)
			}
			defer func() {
				if closer, ok := ser.(interface{ Close() error }); ok {
					if err := closer.Close(); err != nil {
						slog.Warn("failed to close serializer", "error", err)
					}
				}
			}()

			if err := ser.Serialize(ctx, result); err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to serialize recipe", err)
			}

			slog.Info("recipe generation completed",
				"output", output,
				"components", len(result.ComponentRefs),
				"overlays", len(result.Metadata.AppliedOverlays))

			return nil
		},
	}
}

// buildCriteriaFromCmd constructs a recipe.Criteria from CLI command flags.
func buildCriteriaFromCmd(cmd *cli.Command) (*recipe.Criteria, error) {
	var opts []recipe.CriteriaOption

	if s := cmd.String("service"); s != "" {
		opts = append(opts, recipe.WithCriteriaService(s))
	}
	if s := cmd.String("accelerator"); s != "" {
		opts = append(opts, recipe.WithCriteriaAccelerator(s))
	}
	if s := cmd.String("intent"); s != "" {
		opts = append(opts, recipe.WithCriteriaIntent(s))
	}
	if s := cmd.String("os"); s != "" {
		opts = append(opts, recipe.WithCriteriaOS(s))
	}
	if s := cmd.String("platform"); s != "" {
		opts = append(opts, recipe.WithCriteriaPlatform(s))
	}
	if n := cmd.Int("nodes"); n > 0 {
		opts = append(opts, recipe.WithCriteriaNodes(n))
	}

	return recipe.BuildCriteria(opts...)
}

// applyCriteriaOverrides applies CLI flag overrides to criteria.
// Logs a warning when a flag overrides a value detected from the snapshot.
func applyCriteriaOverrides(cmd *cli.Command, criteria *recipe.Criteria) error {
	if s := cmd.String("service"); s != "" {
		parsed, err := recipe.ParseCriteriaServiceType(s)
		if err != nil {
			return err
		}
		if criteria.Service != "" && criteria.Service != parsed {
			slog.Info("CLI flag overriding snapshot-detected value",
				"field", "service",
				"detected", criteria.Service,
				"override", parsed)
		}
		criteria.Service = parsed
	}
	if s := cmd.String("accelerator"); s != "" {
		parsed, err := recipe.ParseCriteriaAcceleratorType(s)
		if err != nil {
			return err
		}
		if criteria.Accelerator != "" && criteria.Accelerator != parsed {
			slog.Info("CLI flag overriding snapshot-detected value",
				"field", "accelerator",
				"detected", criteria.Accelerator,
				"override", parsed)
		}
		criteria.Accelerator = parsed
	}
	if s := cmd.String("intent"); s != "" {
		parsed, err := recipe.ParseCriteriaIntentType(s)
		if err != nil {
			return err
		}
		if criteria.Intent != "" && criteria.Intent != parsed {
			slog.Info("CLI flag overriding snapshot-detected value",
				"field", "intent",
				"detected", criteria.Intent,
				"override", parsed)
		}
		criteria.Intent = parsed
	}
	if s := cmd.String("os"); s != "" {
		parsed, err := recipe.ParseCriteriaOSType(s)
		if err != nil {
			return err
		}
		if criteria.OS != "" && criteria.OS != parsed {
			slog.Info("CLI flag overriding snapshot-detected value",
				"field", "os",
				"detected", criteria.OS,
				"override", parsed)
		}
		criteria.OS = parsed
	}
	if s := cmd.String("platform"); s != "" {
		parsed, err := recipe.ParseCriteriaPlatformType(s)
		if err != nil {
			return err
		}
		if criteria.Platform != "" && criteria.Platform != parsed {
			slog.Info("CLI flag overriding snapshot-detected value",
				"field", "platform",
				"detected", criteria.Platform,
				"override", parsed)
		}
		criteria.Platform = parsed
	}
	if n := cmd.Int("nodes"); n > 0 {
		if criteria.Nodes > 0 && criteria.Nodes != n {
			slog.Info("CLI flag overriding snapshot-detected value",
				"field", "nodes",
				"detected", criteria.Nodes,
				"override", n)
		}
		criteria.Nodes = n
	}
	return nil
}
