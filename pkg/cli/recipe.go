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
	"fmt"
	"log/slog"
	"strings"

	"github.com/urfave/cli/v3"

	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func recipeCmdFlags() []cli.Flag {
	// Help text + shell completions draw from AllCriteria*Types so values
	// introduced by --data overlays are discoverable. Before --data is
	// processed the registry is empty, so the listed values match the
	// embedded OSS set; once a data provider is initialized later in the
	// Action the registry is populated, but flag Usage strings have
	// already been formatted by then — discoverability in --help is on
	// a best-effort basis when --data is co-resident on disk for an
	// interactive `aicr recipe --data X --help`. The registry-aware
	// validation still works regardless.
	return []cli.Flag{
		withCompletions(&cli.StringFlag{
			Name:     "service",
			Usage:    fmt.Sprintf("Kubernetes service type (e.g. %s)", strings.Join(recipe.GetCriteriaServiceTypes(), ", ")),
			Category: catQueryParameters,
		}, recipe.GetCriteriaServiceTypes),
		withCompletions(&cli.StringFlag{
			Name:     "accelerator",
			Aliases:  []string{"gpu"},
			Usage:    fmt.Sprintf("Accelerator/GPU type (e.g. %s)", strings.Join(recipe.GetCriteriaAcceleratorTypes(), ", ")),
			Category: catQueryParameters,
		}, recipe.GetCriteriaAcceleratorTypes),
		withCompletions(&cli.StringFlag{
			Name:     "intent",
			Usage:    fmt.Sprintf("Workload intent (e.g. %s)", strings.Join(recipe.GetCriteriaIntentTypes(), ", ")),
			Category: catQueryParameters,
		}, recipe.GetCriteriaIntentTypes),
		withCompletions(&cli.StringFlag{
			Name:     "os",
			Usage:    fmt.Sprintf("Operating system type of the GPU node (e.g. %s)", strings.Join(recipe.GetCriteriaOSTypes(), ", ")),
			Category: catQueryParameters,
		}, recipe.GetCriteriaOSTypes),
		withCompletions(&cli.StringFlag{
			Name:     "platform",
			Usage:    fmt.Sprintf("Platform/framework type to include in the runtime (e.g. %s)", strings.Join(recipe.GetCriteriaPlatformTypes(), ", ")),
			Category: catQueryParameters,
		}, recipe.GetCriteriaPlatformTypes),
		&cli.IntFlag{
			Name:     "nodes",
			Usage:    "Number of worker/GPU nodes in the cluster",
			Category: catQueryParameters,
		},
		&cli.BoolFlag{
			Name: "criteria-strict",
			Usage: `Reject criteria values not in the embedded OSS catalog (ignore --data contributions).
	Use in CI gates so the upstream catalog cannot accidentally depend on internal-only values.
	Also honored via AICR_CRITERIA_STRICT=1.`,
			Category: catQueryParameters,
		},
		&cli.StringFlag{
			Name:    cmdNameSnapshot,
			Aliases: []string{"s"},
			Usage: `Path/URI to previously generated configuration snapshot.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	If provided, criteria are extracted from the snapshot.`,
			Category: catInput,
		},
		configFlag(),
		dataFlag(),
		outputFlag(),
		formatFlag(),
		kubeconfigFlag(),
	}
}

func recipeCmd() *cli.Command {
	return &cli.Command{
		Name:     cmdNameRecipe,
		Category: functionalCategoryName,
		Usage:    "Create optimized recipe for given intent and environment parameters.",
		Description: `Generate configuration recipe based on specified environment parameters including:
  - Kubernetes service type (e.g. eks, gke, aks, oke, kind, lke, bcm)
  - Accelerator type (e.g. h100, h200, gb200, b200, a100, l40, rtx-pro-6000)
  - Workload intent (e.g. training, inference)
  - GPU node operating system (e.g. ubuntu, rhel, cos, amazonlinux, talos)
  - Number of GPU nodes in the cluster

The recipe returns a list of components with deployment order based on dependencies.
Output can be in JSON or YAML format.

Examples:

Generate recipe from explicit criteria:
  aicr recipe --service eks --accelerator h100 --os ubuntu --intent training

Generate recipe from a config file:
  aicr recipe --config config.yaml

Generate recipe from a snapshot file:
  aicr recipe --snapshot snapshot.yaml

Generate recipe from a ConfigMap snapshot:
  aicr recipe --snapshot cm://gpu-operator/aicr-snapshot

Save recipe to a file:
  aicr recipe --snapshot cm://gpu-operator/aicr-snapshot -o recipe.yaml

Override config file values with flags:
  aicr recipe --config config.yaml --service gke

Override snapshot-detected criteria:
  aicr recipe --snapshot cm://gpu-operator/aicr-snapshot --service gke`,
		Flags: recipeCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, "service", "accelerator", "intent", "os", "platform", "snapshot", "config", "output", "format"); err != nil {
				return err
			}

			cfg, err := loadCmdConfig(ctx, cmd)
			if err != nil {
				return err
			}

			// Build a per-command Client bound to the resolved data source
			// (--data / spec.recipe.data, else embedded). The Client owns its
			// own DataProvider and per-provider criteria registry, replacing
			// the old process-global data provider.
			client, err := recipeClientFromCmd(cmd, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			// Eagerly load THIS Client's catalog so its criteria registry is
			// seeded before any criteria parse. The metadata store cache is
			// otherwise lazy and the first parse would run against an empty
			// registry. LoadCatalog preserves the inner error code so YAML
			// parse failures surface as ErrCodeInvalidRequest.
			if err = client.LoadCatalog(ctx); err != nil {
				return err
			}

			// Apply criteria-strict AFTER the catalog has loaded —
			// LoadCatalog seeded the Client's registry from every overlay's
			// spec.criteria; strict mode then hides external-origin entries
			// from subsequent registry lookups.
			//
			// Three sources can enable strict mode (logical OR):
			//   1. --criteria-strict CLI flag
			//   2. spec.recipe.criteriaStrict in --config
			//   3. AICR_CRITERIA_STRICT env var (honored at registry init)
			// AICR_CRITERIA_STRICT is read when the registry is first
			// constructed, so we only need to apply the flag + config here.
			if cmd.Bool("criteria-strict") || cfg.Recipe().IsCriteriaStrict() {
				client.CriteriaRegistry().SetStrict(true)
			}

			outFormat, err := parseRecipeOutputFormat(cmd, cfg)
			if err != nil {
				return err
			}

			kubeconfig := cmd.String("kubeconfig")

			result, err := buildRecipeFromCmdWithConfig(ctx, cmd, cfg, client)
			if err != nil {
				return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "error building recipe")
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

			output := recipeOutputPath(cmd, cfg)
			ser, err := serializer.NewFileWriterOrStdoutWithKubeconfig(outFormat, output, kubeconfig)
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

// recipeOutputPath returns the recipe output destination, with the CLI flag
// overriding spec.recipe.output.path.
func recipeOutputPath(cmd *cli.Command, cfg *appcfg.AICRConfig) string {
	return stringFlagOrConfig(cmd, "output", cfg.Recipe().OutputPath())
}

// parseRecipeOutputFormat reads --format with precedence
// CLI > spec.recipe.output.format > flag default ("yaml"), and validates
// the result. stringFlagOrConfig handles all three sources: it prefers a
// non-empty config value over the flag's Value: default, and falls
// through to cmd.String(flag) (which surfaces the Value: default) only
// when both CLI and config are empty.
func parseRecipeOutputFormat(cmd *cli.Command, cfg *appcfg.AICRConfig) (serializer.Format, error) {
	raw := stringFlagOrConfig(cmd, "format", cfg.Recipe().OutputFormat())
	out := serializer.Format(raw)
	if out.IsUnknown() {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unknown output format: %q, valid formats are: yaml, json, table", raw))
	}
	return out, nil
}

// applyCriteriaFromConfig merges spec.recipe.criteria values into an existing
// Criteria. Config values override snapshot-extracted values when both are
// present and differ; when criteria is empty (no snapshot), config simply
// populates it. CLI flags subsequently override both via applyCriteriaOverrides,
// yielding precedence: CLI > config > snapshot.
//
// Conversion (string→typed enum) happens in
// (*config.RecipeSpec).ResolveCriteriaWithRegistry against the per-provider
// registry threaded in by the caller — this function only handles the
// merge-and-log concern.
//
// Override events are logged at INFO so the resolved value is auditable.
func applyCriteriaFromConfig(criteria *recipe.Criteria, cfg *appcfg.AICRConfig, reg *recipe.CriteriaRegistry) error {
	resolved, err := cfg.Recipe().ResolveCriteriaWithRegistry(reg)
	if err != nil {
		return err
	}
	if resolved.Service != "" {
		logCriteriaOverride("service", string(criteria.Service), string(resolved.Service))
		criteria.Service = resolved.Service
	}
	if resolved.Accelerator != "" {
		logCriteriaOverride("accelerator", string(criteria.Accelerator), string(resolved.Accelerator))
		criteria.Accelerator = resolved.Accelerator
	}
	if resolved.Intent != "" {
		logCriteriaOverride("intent", string(criteria.Intent), string(resolved.Intent))
		criteria.Intent = resolved.Intent
	}
	if resolved.OS != "" {
		logCriteriaOverride("os", string(criteria.OS), string(resolved.OS))
		criteria.OS = resolved.OS
	}
	if resolved.Platform != "" {
		logCriteriaOverride("platform", string(criteria.Platform), string(resolved.Platform))
		criteria.Platform = resolved.Platform
	}
	if resolved.Nodes > 0 {
		if criteria.Nodes > 0 && criteria.Nodes != resolved.Nodes {
			slog.Info("config overriding snapshot-detected value",
				"field", "nodes", "snapshot", criteria.Nodes, "config", resolved.Nodes)
		}
		criteria.Nodes = resolved.Nodes
	}
	return nil
}

// logCriteriaOverride logs an INFO line when config replaces a non-default
// snapshot-extracted value with a different one. Empty/wildcard prior values
// are not interesting (the field was effectively unset).
func logCriteriaOverride(field, prior, override string) {
	if prior == "" || prior == "any" || prior == override {
		return
	}
	slog.Info("config overriding snapshot-detected value",
		"field", field, "snapshot", prior, "config", override)
}

// mergeCriteriaFromCmdAndConfig builds a Criteria starting from spec.recipe.criteria
// (when cfg is non-nil) and overlays CLI flag values on top. Every enum value
// — config-sourced and flag-sourced alike — is resolved against the supplied
// per-provider registry so a `--data` overlay's non-OSS values validate.
func mergeCriteriaFromCmdAndConfig(cmd *cli.Command, cfg *appcfg.AICRConfig, reg *recipe.CriteriaRegistry) (*recipe.Criteria, error) {
	criteria := recipe.NewCriteria()
	if err := applyCriteriaFromConfig(criteria, cfg, reg); err != nil {
		return nil, err
	}
	if err := applyCriteriaOverrides(cmd, criteria, reg); err != nil {
		return nil, err
	}
	return criteria, nil
}

// applyCriteriaOverrides applies CLI flag overrides to criteria, resolving each
// enum value against the supplied per-provider registry. Logs a warning when a
// flag overrides a value detected from the snapshot.
func applyCriteriaOverrides(cmd *cli.Command, criteria *recipe.Criteria, reg *recipe.CriteriaRegistry) error {
	if s := cmd.String("service"); s != "" {
		parsed, err := reg.ParseService(s)
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
		parsed, err := reg.ParseAccelerator(s)
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
		parsed, err := reg.ParseIntent(s)
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
		parsed, err := reg.ParseOS(s)
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
		parsed, err := reg.ParsePlatform(s)
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
