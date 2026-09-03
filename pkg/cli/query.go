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

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
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
			if err := validateSingleValueFlags(cmd, "service", "accelerator", "intent", "os", "platform",
				flagProfile, flagSlurmAccountingMode, flagRuntimeInventory, "snapshot", "config", "format", "selector"); err != nil {
				return err
			}

			cfg, err := loadFacadeConfig(ctx, cmd)
			if err != nil {
				return err
			}

			// Build a per-command Client bound to the resolved data source.
			// query historically relied on lazy global seeding of the criteria
			// registry; it now explicitly seeds its OWN provider via
			// LoadCatalog before parsing criteria, fixing a latent ordering
			// bug where the first parse could run against an empty registry.
			client, err := recipeClientFromCmd(ctx, cmd, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if err = client.LoadCatalog(ctx); err != nil {
				return err
			}
			applyClientCriteriaStrictMode(cmd, cfg, client)

			outFormat, err := parseRecipeOutputFormat(cmd, cfg)
			if err != nil {
				return err
			}

			result, err := buildRecipeFromCmdWithConfig(ctx, cmd, cfg, client)
			if err != nil {
				return err
			}

			// Hydrate + select through the facade so the CLI, the REST
			// query handler, and out-of-tree SDK callers all run the same
			// implementation; ctx bounds the values reads hydration performs.
			selected, err := aicr.SelectFromRecipeWithContext(ctx, result, cmd.String("selector"))
			if err != nil {
				return err
			}

			return writeQueryResult(cmd.Root().Writer, selected, outFormat)
		},
	}
}

// buildRecipeFromCmdWithConfig resolves a recipe from CLI flags layered on
// top of an optional AICRConfig, through the supplied aicr.Client. Resolution
// order for each input is:
//
//  1. CLI flag (if explicitly set)
//  2. spec.recipe.* field on cfg (if non-empty)
//  3. zero value
//
// A snapshot path provided by either source takes precedence over the
// criteria pathway, matching today's --snapshot behavior.
//
// All criteria enum values — fingerprint-derived, config-sourced, and
// flag-sourced — are parsed against the Client's OWN per-provider criteria
// registry (client.CriteriaRegistry), so a value contributed by a `--data`
// overlay validates against the same DataProvider the Client resolves with.
// The Client's catalog must already be loaded (LoadCatalog) so that registry
// is seeded.
func buildRecipeFromCmdWithConfig(ctx context.Context, cmd *cli.Command, cfg *aicr.Config, client *aicr.Client) (*aicr.RecipeResult, error) {
	reg := client.CriteriaRegistry()
	profile := stringFlagOrConfig(cmd, flagProfile, cfg.RecipeProfile())
	resolveOpts, err := buildSelectionResolveOptions(cmd, cfg)
	if err != nil {
		return nil, err
	}
	if profile != "" {
		resolveOpts = append(resolveOpts, aicr.WithProfile(profile))
	}

	snapFilePath := stringFlagOrConfig(cmd, "snapshot", cfg.SnapshotPath())

	if snapFilePath != "" {
		slog.Info("loading snapshot from", "uri", snapFilePath)
		snap, loadErr := client.LoadSnapshot(ctx, snapFilePath, cmd.String("kubeconfig"))
		if loadErr != nil {
			return nil, loadErr
		}

		// touched records which of the 5 coverage dimensions were explicitly
		// user-stated (config or CLI flag) rather than derived from the
		// snapshot fingerprint below. Everything unmarked is fair game for the
		// facade's relax-and-retry — see aicr.WithSnapshotCriteriaRelaxation.
		touched := map[aicr.CriteriaDimension]bool{}
		// Derived through the facade so this workflow needs only pkg/client/v1
		// (#2437). The Client parses against its own provider-scoped registry,
		// so an external --data catalog's registered values resolve here the
		// same way they will at resolve time.
		facadeCriteria, criteriaErr := client.CriteriaFromSnapshot(snap)
		if criteriaErr != nil {
			return nil, criteriaErr
		}
		criteria := aicr.ToInternalCriteria(facadeCriteria)
		if applyErr := applyCriteriaFromConfig(criteria, cfg, reg, touched); applyErr != nil {
			return nil, applyErr
		}
		if applyErr := applyCriteriaOverrides(cmd, criteria, reg, touched); applyErr != nil {
			return nil, applyErr
		}

		// Fail closed when the snapshot (plus any config/CLI criteria) yields no
		// recognizable dimension. A criteria(any) here means the measurements
		// identify nothing to specialize on — resolving it would silently emit
		// the generic fallback recipe with exit 0 (issue #1888). This mirrors
		// the Specificity guard on the non-snapshot path below and is the
		// semantic backstop for measurements that pass the loader's structural
		// gate but carry no usable signal (unknown/empty measurement type, or a
		// recognized type with no criteria-relevant content).
		if criteria.Specificity() == 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("snapshot %q yielded no recognizable criteria (criteria(any)); its measurements "+
					"identify no service, accelerator, intent, os, or platform — recapture with \"aicr snapshot\", "+
					"or state criteria explicitly via --service/--accelerator/--intent/--os/--platform/--nodes or --config",
					snapFilePath))
		}

		slog.Info("building recipe from snapshot", "criteria", criteria.String())
		// ResolveRecipeFromSnapshot builds the constraint evaluator
		// internally (constraints.Evaluate against snap), mirroring the
		// pre-facade BuildFromCriteriaWithEvaluator path.
		//
		// The relax-and-retry for fingerprint-derived criteria lives in the
		// facade (issue #2027). This layer's only remaining job is declaring
		// which dimensions the user stated — the one fact the facade cannot
		// infer, because only the CLI knows a flag was set.
		resolveOpts = append(resolveOpts,
			aicr.WithSnapshotCriteriaRelaxation(statedDimensions(touched)...))
		return client.ResolveRecipeFromSnapshotWithOptions(
			ctx, aicr.WrapCriteria(criteria), snap, resolveOpts...)
	}

	criteria, err := mergeCriteriaFromCmdAndConfig(cmd, cfg, reg)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "error parsing criteria", err)
	}

	if criteria.Specificity() == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"no criteria provided: specify at least one of --service, --accelerator, --intent, --os, --platform, --nodes, --config, or use --snapshot to load from a snapshot file")
	}

	slog.Info("building recipe from criteria", "criteria", criteria.String())
	return client.ResolveRecipeFromCriteriaWithOptions(ctx, aicr.WrapCriteria(criteria), resolveOpts...)
}

func accountingResolveOptions(cmd *cli.Command, cfg *aicr.Config) ([]aicr.RecipeResolveOption, error) {
	value := cmd.String(flagSlurmAccountingMode)
	if !cmd.IsSet(flagSlurmAccountingMode) {
		if cfg == nil {
			return nil, nil
		}
		mode, present, err := cfg.RecipeAccountingMode()
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, nil
		}
		value = mode
	}
	if _, err := recipe.ParseAccountingMode(value); err != nil {
		return nil, err
	}
	return []aicr.RecipeResolveOption{aicr.WithAccountingMode(value)}, nil
}

// runtimeInventoryResolveOptions turns the --runtime-inventory flag, or the
// equivalent AICRConfig field, into a resolve option. Mirrors
// accountingResolveOptions: the flag wins, the config file is the fallback,
// and an absent selection leaves the recipe's own declaration alone.
func runtimeInventoryResolveOptions(cmd *cli.Command, cfg *aicr.Config) ([]aicr.RecipeResolveOption, error) {
	value := cmd.String(flagRuntimeInventory)
	if !cmd.IsSet(flagRuntimeInventory) {
		if cfg == nil {
			return nil, nil
		}
		mode, present, err := cfg.RecipeRuntimeInventoryMode()
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, nil
		}
		value = mode
	}
	if _, err := recipe.ParseRuntimeInventoryMode(value); err != nil {
		return nil, err
	}
	return []aicr.RecipeResolveOption{aicr.WithRuntimeInventoryMode(value)}, nil
}

// buildSelectionResolveOptions gathers every generation-time selection into one
// option slice, so callers cannot wire one and forget the other.
func buildSelectionResolveOptions(cmd *cli.Command, cfg *aicr.Config) ([]aicr.RecipeResolveOption, error) {
	opts, err := accountingResolveOptions(cmd, cfg)
	if err != nil {
		return nil, err
	}
	riOpts, err := runtimeInventoryResolveOptions(cmd, cfg)
	if err != nil {
		return nil, err
	}
	return append(opts, riOpts...), nil
}

// statedDimensions converts the touched set into the argument
// aicr.WithSnapshotCriteriaRelaxation expects: the dimensions the user stated
// explicitly, which the facade must never relax. Order is canonical so the
// option argument is stable across runs (Go map iteration is randomized).
func statedDimensions(touched map[aicr.CriteriaDimension]bool) []aicr.CriteriaDimension {
	stated := make([]aicr.CriteriaDimension, 0, len(touched))
	for _, dim := range aicr.AllCriteriaDimensions() {
		if touched[dim] {
			stated = append(stated, dim)
		}
	}
	return stated
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

	// Use deterministic marshal so query output is byte-stable across runs;
	// selector results commonly include map[string]any fragments from
	// values.yaml whose Go map iteration would otherwise be randomized.
	data, err := serializer.MarshalYAMLDeterministic(val)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to marshal YAML", err)
	}
	if _, err := fmt.Fprint(w, string(data)); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write YAML output", err)
	}
	return nil
}
