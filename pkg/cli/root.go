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
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/logging"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	name                   = "aicr"
	versionDefault         = "dev"
	functionalCategoryName = "Functional"
	agentImageBase         = "ghcr.io/nvidia/aicr"
	shellCompletionFlag    = "--generate-shell-completion"
)

// defaultAgentImage returns the agent container image reference matching the
// CLI version. Release builds (e.g. "0.8.10") produce "ghcr.io/…:v0.8.10".
// Dev builds ("dev") and snapshot builds ("v0.8.10-next") use ":latest".
func defaultAgentImage() string {
	if version == versionDefault || strings.Contains(version, "-next") {
		return agentImageBase + ":latest"
	}
	if strings.HasPrefix(version, "v") {
		return agentImageBase + ":" + version
	}
	return agentImageBase + ":v" + version
}

var (
	// overridden during build with ldflags
	version = versionDefault
	commit  = "unknown"
	date    = "unknown"

	outputFlag = &cli.StringFlag{
		Name:     "output",
		Aliases:  []string{"o"},
		Usage:    fmt.Sprintf("output destination: file path, ConfigMap URI (%snamespace/name), or stdout (default)", serializer.ConfigMapURIScheme),
		Category: "Output",
	}

	// formatFlag is a function to avoid sharing a single flag instance across
	// commands, which causes urfave/cli internal state conflicts in parallel tests.
	formatFlag = func() cli.Flag {
		return withCompletions(&cli.StringFlag{
			Name:     "format",
			Aliases:  []string{"t"},
			Value:    string(serializer.FormatYAML),
			Usage:    fmt.Sprintf("output format (%s)", strings.Join(serializer.SupportedFormats(), ", ")),
			Category: "Output",
		}, serializer.SupportedFormats)
	}

	kubeconfigFlag = &cli.StringFlag{
		Name:     "kubeconfig",
		Aliases:  []string{"k"},
		Usage:    "Path to kubeconfig file (overrides KUBECONFIG env and default ~/.kube/config)",
		Category: "Input",
	}

	dataFlag = &cli.StringFlag{
		Name: "data",
		Usage: `Path to external data directory to overlay on embedded recipe data.
	The directory must contain registry.yaml (required). Registry components are merged
	with embedded (external takes precedence by name). All other files (base.yaml,
	overlays, component values) fully replace embedded files or add new ones.`,
		Category: "Input",
	}
)

// newRootCmd builds the root CLI command tree.
func newRootCmd() *cli.Command {
	cmd := &cli.Command{
		Name:                  name,
		Usage:                 "AICR CLI",
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
				Sources: cli.EnvVars("AICR_DEBUG"),
			},
			&cli.BoolFlag{
				Name:    "log-json",
				Usage:   "enable structured logging",
				Sources: cli.EnvVars("AICR_LOG_JSON"),
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
			queryCmd(),
			bundleCmd(),
			bundleVerifyCmd(),
			validateCmd(),
			trustCmd(),
		},
		ShellComplete: completeWithAllFlags,
	}
	setShellComplete(cmd)
	return cmd
}

// setShellComplete recursively assigns completeWithAllFlags to all subcommands
// so that urfave/cli's setupDefaults does not replace it with
// DefaultCompleteWithFlags (which only shows the primary flag name, not aliases).
func setShellComplete(cmd *cli.Command) {
	for _, sub := range cmd.Commands {
		sub.ShellComplete = completeWithAllFlags
		setShellComplete(sub)
	}
}

// completeWithAllFlags replaces urfave/cli's DefaultCompleteWithFlags to include
// all flag names (primary + aliases) in shell completion output. This ensures
// aliases like --gpu (for --accelerator) appear in TAB completions.
//
// Unlike DefaultCompleteWithFlags which reads cmd.Args() (parsed positional
// args), this function reads os.Args directly to determine what the user was
// typing. This is necessary because partial flags like "--form" cause
// urfave/cli's flag parser to error, and the partial flag never appears in
// cmd.Args().
func completeWithAllFlags(_ context.Context, cmd *cli.Command) {
	lastArg := completionLastArg()
	writer := cmd.Root().Writer

	if strings.HasPrefix(lastArg, "-") {
		// Flag value completion: when lastArg exactly matches a completable
		// flag name (e.g. "--intent"), emit valid values. This handles the
		// zsh/fish case where "aicr recipe --intent <TAB>" sends
		// ["aicr", "recipe", "--intent", shellCompletionFlag]
		// without an empty string for the value being completed.
		if cf, ok := findCompletableFlag(cmd, lastArg); ok {
			for _, v := range cf.Completions() {
				fmt.Fprintln(writer, v)
			}
			return
		}

		cur := strings.TrimLeft(lastArg, "-")
		for _, f := range cmd.Flags {
			for _, flagName := range f.Names() {
				// Skip short flags when the user typed a -- prefix.
				if strings.HasPrefix(lastArg, "--") && len(flagName) == 1 {
					continue
				}
				if strings.HasPrefix(flagName, cur) && cur != flagName {
					prefix := "-"
					if len(flagName) > 1 {
						prefix = "--"
					}
					completion := prefix + flagName
					if usage := flagUsage(f); usage != "" {
						shell := os.Getenv("SHELL")
						if strings.HasSuffix(shell, "zsh") || strings.HasSuffix(shell, "fish") {
							completion = completion + ":" + usage
						}
					}
					fmt.Fprintln(writer, completion)
				}
			}
		}
		return
	}

	// Flag value completion: if the previous arg is a completable flag,
	// suggest its valid values instead of subcommands. This handles the
	// bash case where "aicr recipe --intent <TAB>" sends
	// ["aicr", "recipe", "--intent", "", shellCompletionFlag]
	// with an empty string for the value being completed.
	if prevArg := completionPrevArg(); prevArg != "" {
		if cf, ok := findCompletableFlag(cmd, prevArg); ok {
			for _, v := range cf.Completions() {
				fmt.Fprintln(writer, v)
			}
			return
		}
	}

	for _, sub := range cmd.Commands {
		if !sub.Hidden {
			fmt.Fprintln(writer, sub.Name)
		}
	}
}

// completionLastArg returns the last user-typed argument from os.Args,
// skipping the trailing --generate-shell-completion flag. This is the
// only reliable way to see what the user was typing, since partial flags
// (e.g. "--form") fail urfave/cli's flag parser and never appear in
// cmd.Args().
func completionLastArg() string {
	n := len(os.Args)
	if n >= 2 && os.Args[n-1] == shellCompletionFlag {
		return os.Args[n-2]
	}
	if n >= 1 {
		return os.Args[n-1]
	}
	return ""
}

// completionPrevArg returns the second-to-last user-typed argument from
// os.Args, which is the flag name when the user is completing a flag value.
// For "aicr recipe --intent <TAB>", os.Args is
// ["aicr", "recipe", "--intent", "", shellCompletionFlag]
// and this returns "--intent".
func completionPrevArg() string {
	n := len(os.Args)
	if n >= 3 && os.Args[n-1] == shellCompletionFlag {
		return os.Args[n-3]
	}
	return ""
}

// flagUsage returns the usage string for a flag if available.
func flagUsage(f cli.Flag) string {
	type usageProvider interface {
		GetUsage() string
	}
	if u, ok := f.(usageProvider); ok {
		return u.GetUsage()
	}
	return ""
}

// Execute starts the CLI application.
// This is called by main.main().
func Execute() {
	cmd := newRootCmd()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if err := cmd.Run(ctx, sanitizeCompletionArgs(os.Args)); err != nil {
		cancel()
		exitCode := errors.ExitCodeFromError(err)
		slog.Error("command failed", "error", err, "exitCode", exitCode)
		os.Exit(exitCode)
	}
	cancel()
}

// sanitizeCompletionArgs works around a urfave/cli v3 limitation where "--"
// immediately before shellCompletionFlag disables completion mode
// entirely (checkShellCompleteFlag treats "--" as a flag terminator). This
// causes the actual command to execute during TAB completion instead of
// returning suggestions.
//
// When the shell sends "aicr snapshot -- --generate-shell-completion" (user
// typed "--<TAB>"), we replace the bare "--" with "-" so urfave/cli keeps
// completion mode active and the "-" survives flag parsing as a positional
// arg that triggers flag suggestions.
func sanitizeCompletionArgs(args []string) []string {
	n := len(args)
	if n < 3 || args[n-1] != shellCompletionFlag {
		return args
	}
	if args[n-2] != "--" {
		return args
	}
	out := make([]string, n)
	copy(out, args)
	out[n-2] = "-"
	return out
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
