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

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	appcfg "github.com/NVIDIA/aicr/pkg/config"
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
		if sf, ok := f.(*cli.StringFlag); ok && sf.Name == flagOutput {
			continue
		}
		filtered = append(filtered, f)
	}

	return append(filtered, &cli.StringFlag{
		Name:     "selector",
		Usage:    "Dot-path to the configuration value to extract (e.g. components.gpu-operator.values.driver.version)",
		Category: catQueryParameters,
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
			if err := validateSingleValueFlags(cmd, "service", "accelerator", "intent", "os", "platform", "snapshot", "config", "format", "selector"); err != nil {
				return err
			}

			cfg, err := loadCmdConfig(ctx, cmd)
			if err != nil {
				return err
			}

			if err = initDataProvider(cmd, cfg); err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", err)
			}

			outFormat, err := parseRecipeOutputFormat(cmd, cfg)
			if err != nil {
				return err
			}

			result, err := buildRecipeFromCmdWithConfig(ctx, cmd, cfg)
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

			return writeQueryResult(cmd.Root().Writer, selected, outFormat)
		},
	}
}

// buildRecipeFromCmdWithConfig resolves a recipe from CLI flags layered on
// top of an optional AICRConfig. Resolution order for each input is:
//
//  1. CLI flag (if explicitly set)
//  2. spec.recipe.* field on cfg (if non-empty)
//  3. zero value
//
// A snapshot path provided by either source takes precedence over the
// criteria pathway, matching today's --snapshot behavior.
func buildRecipeFromCmdWithConfig(ctx context.Context, cmd *cli.Command, cfg *appcfg.AICRConfig) (*recipe.RecipeResult, error) {
	builder := recipe.NewBuilder(
		recipe.WithVersion(version),
	)

	snapFilePath := stringFlagOrConfig(cmd, "snapshot", cfg.Recipe().SnapshotPath())

	if snapFilePath != "" {
		slog.Info("loading snapshot from", "uri", snapFilePath)
		snap, loadErr := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapFilePath, cmd.String("kubeconfig"))
		if loadErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %q", snapFilePath), loadErr)
		}

		criteria := recipe.ExtractCriteriaFromSnapshot(snap)
		if applyErr := applyCriteriaFromConfig(criteria, cfg); applyErr != nil {
			return nil, applyErr
		}
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
	}

	criteria, err := mergeCriteriaFromCmdAndConfig(cmd, cfg)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "error parsing criteria", err)
	}

	if criteria.Specificity() == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"no criteria provided: specify at least one of --service, --accelerator, --intent, --os, --platform, --nodes, --config, or use --snapshot to load from a snapshot file")
	}

	slog.Info("building recipe from criteria", "criteria", criteria.String())
	return builder.BuildFromCriteria(ctx, criteria)
}

// writeQueryResult formats and writes the selected value to w.
func writeQueryResult(w io.Writer, val any, format serializer.Format) error {
	if format == serializer.FormatJSON {
		return writeComplexValue(w, val, format)
	}

	switch v := val.(type) {
	case string:
		if _, err := fmt.Fprintln(w, v); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write query result", err)
		}
		return nil
	case bool, int, int64, float64:
		if _, err := fmt.Fprintln(w, v); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write query result", err)
		}
		return nil
	default:
		return writeComplexValue(w, val, format)
	}
}

func writeComplexValue(w io.Writer, val any, format serializer.Format) error {
	if format == serializer.FormatJSON {
		data, err := json.MarshalIndent(val, "", "  ")
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to marshal JSON", err)
		}
		if _, err := fmt.Fprintln(w, string(data)); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write JSON output", err)
		}
		return nil
	}

	data, err := yaml.Marshal(val)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal YAML", err)
	}
	if _, err := fmt.Fprint(w, string(data)); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write YAML output", err)
	}
	return nil
}
