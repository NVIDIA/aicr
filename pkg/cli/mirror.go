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
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	appcfg "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/mirror"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func mirrorCmd() *cli.Command {
	return &cli.Command{
		Name:     "mirror",
		Category: functionalCategoryName,
		Usage:    "Discover container images and Helm charts for air-gapped mirroring.",
		Commands: []*cli.Command{
			mirrorListCmd(),
		},
	}
}

func mirrorListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List all container images and Helm charts referenced by a recipe.",
		Description: `Discovers container images by rendering Helm charts with recipe-resolved
values and scanning manifests. The output feeds air-gap mirroring tools.

Accepts recipe input the same two ways as other commands:
  1. --recipe <path> to load a previously generated recipe
  2. Query parameters (--service, --accelerator, etc.) to resolve a recipe

Use --set to override values that affect which images appear (e.g.,
enabling/disabling sub-components).

Output formats:
  yaml    Machine-readable YAML (default)
  json    Machine-readable JSON
  hauler  Hauler manifest (content.hauler.cattle.io/v1)
  zarf    Zarf package config (ZarfPackageConfig)

Examples:

List images from a recipe file:
  aicr mirror list --recipe recipe.yaml

List images with query parameters:
  aicr mirror list --service eks --accelerator h100 --intent training --os ubuntu

Output Hauler manifest for air-gap mirroring:
  aicr mirror list --recipe recipe.yaml --format hauler > hauler-manifest.yaml

Output Zarf package config:
  aicr mirror list --recipe recipe.yaml --format zarf > zarf.yaml

Override a value that affects image discovery:
  aicr mirror list --recipe recipe.yaml --set gpuoperator:driver.enabled=false

Save to a file:
  aicr mirror list --recipe recipe.yaml --format hauler --output manifest.yaml`,
		Flags:  mirrorListFlags(),
		Action: runMirrorListCmd,
	}
}

func mirrorListFlags() []cli.Flag {
	// Start with the recipe criteria flags (service, accelerator, etc.).
	flags := recipeCmdFlags()

	// Filter out the default --format and --output flags — mirror list uses
	// its own format flag with mirror-specific valid values, and its own
	// output flag.
	filtered := make([]cli.Flag, 0, len(flags))
	for _, f := range flags {
		if flagMatchesName(f, flagFormat) || flagMatchesName(f, flagOutput) {
			continue
		}
		filtered = append(filtered, f)
	}

	return append(filtered,
		&cli.StringFlag{
			Name:    cmdNameRecipe,
			Aliases: []string{"r"},
			Usage: `Path/URI to previously generated recipe.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).`,
			Category: catInput,
		},
		&cli.StringSliceFlag{
			Name: "set",
			Usage: `Override values that affect image discovery
	(format: component:path.to.field=value, e.g., --set gpuoperator:driver.enabled=false)`,
			Category: catInput,
		},
		withCompletions(&cli.StringFlag{
			Name:     flagFormat,
			Aliases:  []string{"f"},
			Value:    string(mirror.FormatYAML),
			Usage:    fmt.Sprintf("output format (%s)", strings.Join(mirror.SupportedFormats(), ", ")),
			Category: catOutput,
		}, mirror.SupportedFormats),
		&cli.StringFlag{
			Name:     flagOutput,
			Aliases:  []string{"o"},
			Usage:    "output file path (default: stdout)",
			Category: catOutput,
		},
	)
}

// flagMatchesName returns true if a CLI flag has the given name among its names.
func flagMatchesName(f cli.Flag, name string) bool {
	for _, n := range f.Names() {
		if n == name {
			return true
		}
	}
	return false
}

//nolint:gocyclo // linear option resolution
func runMirrorListCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, "recipe", "service", "accelerator",
		"intent", "os", "platform", "snapshot", "config", "format", "output"); err != nil {
		return err
	}

	// Validate format early (fail-fast on pure input errors).
	format := mirror.Format(cmd.String("format"))
	if !isValidMirrorFormat(format) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unknown output format: %q, valid formats are: %s",
				format, strings.Join(mirror.SupportedFormats(), ", ")))
	}

	cfg, err := loadCmdConfig(ctx, cmd)
	if err != nil {
		return err
	}

	if err = initDataProvider(cmd, cfg); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", err)
	}

	// Resolve recipe: --recipe takes precedence over query parameters.
	rec, err := resolveRecipeForMirror(ctx, cmd, cfg)
	if err != nil {
		return err
	}

	// Parse --set overrides.
	var valueOverrides []config.ComponentPath
	if cmd.IsSet("set") {
		raw := cmd.StringSlice("set")
		valueOverrides, err = config.ParseValueOverrides(raw)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --set", err)
		}
	}

	// Discover images and charts.
	kubeVersion := mirror.KubeVersionFromConstraints(rec.Constraints)
	lister := mirror.NewLister(
		mirror.WithVersion(version),
		mirror.WithValueOverrides(valueOverrides),
		mirror.WithKubeVersion(kubeVersion),
	)

	slog.Info("discovering images and charts", "components", len(rec.ComponentRefs))

	result, err := lister.Discover(ctx, rec)
	if err != nil {
		return err
	}

	slog.Info("discovery complete",
		"images", len(result.Images),
		"charts", len(result.Charts),
		"components", len(result.Components))

	// Resolve output writer.
	w, cleanup, err := resolveOutputWriter(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	return mirror.Render(w, result, format)
}

// resolveRecipeForMirror loads a recipe from --recipe flag or builds one
// from query parameters (--service, --accelerator, etc.).
//
// The two branches consume two different recipe-resolution paths:
//
//   - --recipe path: routed through the aicr.Client facade. A Client built
//     from the command's data source (--data / spec.recipe.data) owns its
//     own DataProvider, so LoadRecipe hydrates overlays against that source
//     rather than the process-global. rec.Resolved() returns the raw
//     *recipe.RecipeResult the mirror Lister.Discover needs.
//   - criteria path: still uses buildRecipeFromCmdWithConfig, which depends
//     on the process-global criteria registry installed by initDataProvider
//     (the caller keeps initDataProvider for exactly this reason). Migrating
//     it requires de-globalizing the criteria registry, tracked by #987
//     (Stage 4). Leave this branch UNCHANGED until then.
func resolveRecipeForMirror(ctx context.Context, cmd *cli.Command, cfg *appcfg.AICRConfig) (*recipe.RecipeResult, error) {
	recipePath := cmd.String("recipe")
	if recipePath != "" {
		slog.Info("loading recipe from file", "path", recipePath)

		// Build the Client from the resolved data source (--data flag, else
		// spec.recipe.data), mirroring validate/bundle. A filesystem source
		// layers the external dir over the embedded data; an empty data dir
		// uses the embedded data only.
		dataDir := cmd.String("data")
		if dataDir == "" {
			dataDir = cfg.Recipe().DataDir()
		}
		source := aicr.EmbeddedSource()
		if dataDir != "" {
			source = aicr.FilesystemSource(dataDir)
		}
		client, err := aicr.NewClient(
			aicr.WithRecipeSource(source),
			aicr.WithVersion(version),
		)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to initialize data provider", err)
		}
		defer func() { _ = client.Close() }()

		loaded, err := client.LoadRecipe(ctx, recipePath, cmd.String("kubeconfig"))
		if err != nil {
			return nil, err
		}
		// Lister.Discover needs the raw *recipe.RecipeResult (constraints,
		// component refs); Resolved() returns the Client-owned internal recipe.
		return loaded.Resolved(), nil
	}

	// Fall through to criteria-based resolution. This branch still depends on
	// the process-global criteria registry (initDataProvider); de-globalizing
	// it is tracked by #987 (Stage 4), so it is intentionally left unchanged.
	return buildRecipeFromCmdWithConfig(ctx, cmd, cfg)
}

// resolveOutputWriter returns a writer for the mirror list output. When
// --output is set, it opens a file; otherwise it uses cmd.Root().Writer
// (which defaults to stdout).
func resolveOutputWriter(cmd *cli.Command) (io.Writer, func(), error) {
	output := cmd.String("output")
	if output == "" {
		return cmd.Root().Writer, func() {}, nil
	}

	f, err := os.Create(output)
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to create output file", err)
	}

	cleanup := func() {
		if closeErr := f.Close(); closeErr != nil {
			slog.Warn("failed to close output file", "error", closeErr)
		}
	}

	return f, cleanup, nil
}

// isValidMirrorFormat checks if the given format is in the supported list.
func isValidMirrorFormat(f mirror.Format) bool {
	for _, valid := range mirror.SupportedFormats() {
		if string(f) == valid {
			return true
		}
	}
	return false
}
