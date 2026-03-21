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
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func queryCmdFlags() []cli.Flag {
	flags := recipeCmdFlags()

	// Filter out --output flag: query always prints to stdout.
	filtered := make([]cli.Flag, 0, len(flags))
	for _, f := range flags {
		if sf, ok := f.(*cli.StringFlag); ok && sf.Name == "output" {
			continue
		}
		filtered = append(filtered, f)
	}

	return append(filtered, &cli.StringFlag{
		Name:     "selector",
		Usage:    "Dot-path to the configuration value to extract (e.g. components.gpu-operator.values.driver.version)",
		Category: "Query Parameters",
		Required: true,
	})
}

func queryCmd() *cli.Command {
	return &cli.Command{
		Name:     "query",
		Category: functionalCategoryName,
		Usage:    "Query a specific value from the hydrated recipe configuration.",
		Description: `Resolve a recipe from criteria and extract a specific configuration value
using a dot-path selector. Returns the fully hydrated value at the given path,
with all base, overlay, and inline overrides merged.

The selector uses dot-delimited paths consistent with Helm --set notation:

  components.<name>.values.<path>   Component Helm values
  components.<name>.chart           Component metadata field
  components.<name>                 Entire hydrated component
  criteria.<field>                  Recipe criteria
  deploymentOrder                   Component deployment order
  constraints                       Merged constraints

Scalar values are printed as plain text (shell-friendly).
Complex values are printed as YAML or JSON (with --format).

Examples:

Query a specific Helm value:
  aicr query --service eks --accelerator h100 --intent training \
    --selector components.gpu-operator.values.driver.version

Query a component subtree:
  aicr query --service eks --accelerator h100 --intent training \
    --selector components.gpu-operator.values.driver

Query deployment order:
  aicr query --service eks --accelerator h100 --intent training \
    --selector deploymentOrder

Query entire hydrated recipe:
  aicr query --service eks --accelerator h100 --intent training \
    --selector ''

Use in shell scripts:
  VERSION=$(aicr query --service eks --accelerator h100 --intent training \
    --selector components.gpu-operator.values.driver.version)`,
		Flags: queryCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, "service", "accelerator", "intent", "os", "platform", "snapshot", "criteria", "format", "selector"); err != nil {
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
				return err
			}

			hydrated, err := recipe.HydrateResult(result)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to hydrate recipe", err)
			}

			selector := cmd.String("selector")
			selected, err := recipe.Select(hydrated, selector)
			if err != nil {
				return err
			}

			return writeQueryResult(selected, outFormat)
		},
	}
}

// buildRecipeFromCmd resolves a recipe from CLI criteria flags.
// Shared logic extracted from the recipe command action.
func buildRecipeFromCmd(ctx context.Context, cmd *cli.Command) (*recipe.RecipeResult, error) {
	builder := recipe.NewBuilder(
		recipe.WithVersion(version),
	)

	snapFilePath := cmd.String("snapshot")
	criteriaFilePath := cmd.String("criteria")

	//nolint:gocritic // if-else chain is appropriate for non-empty string conditions
	if snapFilePath != "" {
		slog.Info("loading snapshot from", "uri", snapFilePath)
		snap, loadErr := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapFilePath, cmd.String("kubeconfig"))
		if loadErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %q", snapFilePath), loadErr)
		}

		criteria := recipe.ExtractCriteriaFromSnapshot(snap)
		if applyErr := applyCriteriaOverrides(cmd, criteria); applyErr != nil {
			return nil, applyErr
		}

		evaluator := func(constraint recipe.Constraint) recipe.ConstraintEvalResult {
			valResult := constraints.Evaluate(constraint, snap)
			return recipe.ConstraintEvalResult{
				Passed: valResult.Passed,
				Actual: valResult.Actual,
				Error:  valResult.Error,
			}
		}

		slog.Info("building recipe from snapshot", "criteria", criteria.String())
		return builder.BuildFromCriteriaWithEvaluator(ctx, criteria, evaluator)
	} else if criteriaFilePath != "" {
		slog.Info("loading criteria from file", "path", criteriaFilePath)
		criteria, loadErr := recipe.LoadCriteriaFromFileWithContext(ctx, criteriaFilePath)
		if loadErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load criteria from %q", criteriaFilePath), loadErr)
		}

		if applyErr := applyCriteriaOverrides(cmd, criteria); applyErr != nil {
			return nil, applyErr
		}

		slog.Info("building recipe from criteria file", "criteria", criteria.String())
		return builder.BuildFromCriteria(ctx, criteria)
	}

	criteria, buildErr := buildCriteriaFromCmd(cmd)
	if buildErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "error parsing criteria", buildErr)
	}

	if criteria.Specificity() == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no criteria provided: specify at least one of --service, --accelerator, --intent, --os, --platform, --nodes, --criteria, or use --snapshot to load from a snapshot file")
	}

	slog.Info("building recipe from criteria", "criteria", criteria.String())
	return builder.BuildFromCriteria(ctx, criteria)
}

// writeQueryResult formats and prints the selected value to stdout.
func writeQueryResult(val any, format serializer.Format) error {
	if format == serializer.FormatJSON {
		return writeComplexValue(val, format)
	}

	switch v := val.(type) {
	case string:
		fmt.Println(v)
		return nil
	case bool, int, int64, float64:
		fmt.Println(v)
		return nil
	default:
		return writeComplexValue(val, format)
	}
}

func writeComplexValue(val any, format serializer.Format) error {
	if format == serializer.FormatJSON {
		data, err := json.MarshalIndent(val, "", "  ")
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal JSON", err)
		}
		fmt.Println(string(data))
		return nil
	}

	data, err := yaml.Marshal(val)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal YAML", err)
	}
	fmt.Print(string(data))
	return nil
}
