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
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/logging"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
)

const (
	name                   = "eidos"
	versionDefault         = "dev"
	functionalCategoryName = "Functional"
)

var (
	// overridden during build with ldflags
	version = versionDefault
	commit  = "unknown"
	date    = "unknown"

	outputFlag = &cli.StringFlag{
		Name:    "output",
		Aliases: []string{"o"},
		Usage:   fmt.Sprintf("output destination: file path, ConfigMap URI (%snamespace/name), or stdout (default)", serializer.ConfigMapURIScheme),
	}

	formatFlag = &cli.StringFlag{
		Name:    "format",
		Aliases: []string{"t"},
		Value:   string(serializer.FormatYAML),
		Usage:   fmt.Sprintf("output format (%s)", strings.Join(serializer.SupportedFormats(), ", ")),
	}

	kubeconfigFlag = &cli.StringFlag{
		Name:    "kubeconfig",
		Aliases: []string{"k"},
		Usage:   "Path to kubeconfig file (overrides KUBECONFIG env and default ~/.kube/config)",
	}

	dataFlag = &cli.StringFlag{
		Name: "data",
		Usage: `Path to external data directory to overlay on embedded recipe data.
	The directory must contain registry.yaml (required). Registry components are merged
	with embedded (external takes precedence by name). All other files (base.yaml,
	overlays, component values) fully replace embedded files or add new ones.`,
	}
)

// Execute starts the CLI application.
// This is called by main.main().
func Execute() {
	cmd := &cli.Command{
		Name:                  name,
		Usage:                 "Eidos CLI",
		Version:               fmt.Sprintf("%s (commit: %s, date: %s)", version, commit, date),
		EnableShellCompletion: true,
		HideHelpCommand:       true,
		ConfigureShellCompletionCommand: func(cmd *cli.Command) {
			cmd.Hidden = false
			cmd.Category = "Utilities"
			cmd.Usage = "Output shell completion script for a given shell."
		},
		Metadata: map[string]any{
			"git-commit": commit,
			"build-date": date,
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "debug",
				Usage:   "enable debug logging",
				Sources: cli.EnvVars("EIDOS_DEBUG"),
			},
			&cli.BoolFlag{
				Name:    "log-json",
				Usage:   "enable structured logging",
				Sources: cli.EnvVars("EIDOS_LOG_JSON"),
			},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			isDebug := c.Bool("debug")
			logLevel := "info"
			if isDebug {
				logLevel = "debug"
			}

			// Configure logger based on flags
			switch {
			case c.Bool("log-json"):
				logging.SetDefaultStructuredLoggerWithLevel(name, version, logLevel)
			case isDebug:
				// In debug mode, use text logger with full metadata
				logging.SetDefaultLoggerWithLevel(name, version, logLevel)
			default:
				// Default mode: use CLI logger for clean, user-friendly output
				logging.SetDefaultCLILogger(logLevel)
			}

			slog.Debug("starting",
				"name", name,
				"version", version,
				"commit", commit,
				"date", date,
				"logLevel", logLevel)
			return ctx, nil
		},
		Commands: []*cli.Command{
			snapshotCmd(),
			recipeCmd(),
			bundleCmd(),
			validateCmd(),
		},
		ShellComplete: commandLister,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		exitCode := errors.ExitCodeFromError(err)
		slog.Error("command failed", "error", err, "exitCode", exitCode)
		os.Exit(exitCode)
	}
}

func commandLister(_ context.Context, cmd *cli.Command) {
	if cmd == nil || cmd.Root() == nil {
		return
	}
	for _, c := range cmd.Root().Commands {
		if c.Hidden {
			continue
		}
		fmt.Println(c.Name)
	}
}

// initDataProvider initializes the data provider from the --data flag.
// If the flag is not set, returns nil (uses embedded data).
// If the flag is set, creates a layered provider that overlays the external
// directory on top of embedded data.
func initDataProvider(cmd *cli.Command) error {
	dataDir := cmd.String("data")
	if dataDir == "" {
		return nil
	}

	slog.Info("initializing external data provider", "directory", dataDir)

	// Create embedded provider
	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")

	// Create layered provider
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
		ExternalDir:   dataDir,
		AllowSymlinks: false,
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to initialize external data", err)
	}

	// Set as global data provider
	recipe.SetDataProvider(layered)

	slog.Info("external data provider initialized successfully", "directory", dataDir)
	return nil
}
