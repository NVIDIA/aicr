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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/logging"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

const (
	name                   = defaults.AgentName
	versionDefault         = defaults.DevVersion
	functionalCategoryName = "Functional"
	shellCompletionFlag    = "--generate-shell-completion"
)

// defaultAgentImage returns the agent container image reference matching the
// CLI version. Release builds (e.g. "0.8.10") produce "ghcr.io/…:v0.8.10".
// Dev builds ("dev") and snapshot builds ("v0.8.10-next") use ":latest".
//
// The rule lives in pkg/defaults because Client.CollectSnapshot applies the
// same one — the SDK deploys the same Job, and two copies would drift.
func defaultAgentImage() string {
	return defaults.AgentImageForVersion(version)
}

var (
	// overridden during build with ldflags
	version = versionDefault
	commit  = "unknown"
	date    = "unknown"

	// Shared flags are functions (not vars) so each Command gets its own
	// instance. urfave/cli mutates parsed-state on the Flag value, so a
	// shared instance leaks Count and parsed values across successive Run
	// invocations — particularly visible in tests that build multiple
	// command trees.

	outputFlag = func() cli.Flag {
		return &cli.StringFlag{
			Name:     flagOutput,
			Aliases:  []string{"o"},
			Usage:    fmt.Sprintf("output destination: file path, ConfigMap URI (%snamespace/name), or stdout (default)", serializer.ConfigMapURIScheme),
			Category: catOutput,
		}
	}

	formatFlag = func() cli.Flag {
		return withCompletions(&cli.StringFlag{
			Name:     flagFormat,
			Aliases:  []string{"t"},
			Value:    string(serializer.FormatYAML),
			Usage:    fmt.Sprintf("output format (%s)", strings.Join(serializer.SupportedFormats(), ", ")),
			Category: catOutput,
		}, serializer.SupportedFormats)
	}

	kubeconfigFlag = func() cli.Flag {
		return &cli.StringFlag{
			Name:     "kubeconfig",
			Aliases:  []string{"k"},
			Usage:    "Path to kubeconfig file (overrides KUBECONFIG env and default ~/.kube/config)",
			Category: catInput,
		}
	}

	dataFlag = func() cli.Flag {
		return &cli.StringFlag{
			Name: "data",
			Usage: `Path to external data directory to overlay on embedded recipe data.
	The directory must contain registry.yaml (required). Registry components and
	validator catalog entries are merged with embedded (external takes precedence
	by name). All other files (base.yaml, overlays, component values) fully
	replace embedded files or add new ones.`,
			Category: catInput,
		}
	}

	// configFlag is a function (not a var) to avoid sharing a single flag
	// instance across commands and successive test runs, which causes
	// urfave/cli internal state (Count, parsed value) to leak between Runs.
	// Mirrors the pattern used by formatFlag.
	configFlag = func() cli.Flag {
		return &cli.StringFlag{
			Name: "config",
			Usage: `Path or HTTP(S) URL to an AICRConfig file (YAML or JSON) populating
	defaults for this command. Individual CLI flags always override config file
	values. See docs/user/cli-reference.md for the file schema.`,
			Category: catInput,
		}
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

			// Configure logger based on flags. Precedence: log-json > debug >
			// default CLI logger. When both --log-json and --debug are set,
			// log-json wins (machine-readable output is the explicit ask) but
			// the debug log level is still applied via the shared logLevel.
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
			evidenceCmd(),
			diffCmd(),
			mirrorCmd(),
			trustCmd(),
			skillCmd(),
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

// RootCommand returns the fully-assembled root command tree, including every
// registered subcommand. It exists so out-of-tree tooling (e.g. tools/coverage)
// can enumerate the live CLI verb registry instead of duplicating the list.
func RootCommand() *cli.Command {
	return newRootCmd()
}

// Execute starts the CLI application.
// This is called by main.main().
func Execute() {
	cmd := newRootCmd()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := cmd.Run(ctx, sanitizeCompletionArgs(os.Args))
	cancel()
	if err != nil {
		exitCode := errors.ExitCodeFromError(err)
		slog.Error("command failed", "error", err, "exitCode", exitCode)
		os.Exit(exitCode) //nolint:gocritic // cancel() above; os.Exit skips defers intentionally
	}
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

// recipeClientFromCmd constructs an aicr.Client bound to the command's
// resolved recipe data source. The data directory is read from the --data
// flag, falling back to spec.recipe.data on the supplied Config when the
// flag is not set (cfg may be nil; only the flag is consulted then). A
// non-empty data dir yields a FilesystemSource (the external dir layered over
// the embedded data); an empty data dir yields the EmbeddedSource. The CLI
// version is threaded through so resolved recipes carry it in
// Metadata.Version.
//
// Instead of mutating a process-global DataProvider (the pre-Stage-4
// pattern), each command now owns a per-command Client whose own
// DataProvider backs recipe resolution and the per-provider criteria
// registry. Callers MUST Close the returned Client (defer client.Close()).
//
// The "initializing external data provider" INFO log matches validate /
// bundle / mirror so a `--data` invocation is auditable.
func recipeClientFromCmd(
	ctx context.Context,
	cmd *cli.Command,
	cfg *aicr.Config,
) (*aicr.Client, error) {

	source := aicr.EmbeddedSource()
	if dataDir := cmd.String("data"); dataDir != "" {
		slog.Info("initializing external data provider", "directory", dataDir)
		source = aicr.FilesystemSource(dataDir)
	} else if configured, ok := cfg.RecipeSource(); ok {
		// spec.recipe.data, derived through the facade so an SDK caller
		// building a Client from the same document gets the same source.
		// Log the concrete directory as the --data branch does: the audit line
		// exists so an operator can tell which catalog a run actually read, and
		// naming only the spec field leaves that unanswered. Unwrap() here is
		// the raw-document escape hatch documented on Config.Unwrap: the facade
		// has no RecipeDataDir() accessor, and adding one only for a log line
		// would be new public API for a cleanup commit. Read from the same
		// underlying field RecipeSource() uses, so the two cannot disagree.
		slog.Info("initializing external data provider",
			"directory", cfg.Unwrap().Recipe().DataDir(), "source", "spec.recipe.data")
		source = configured
	}
	client, err := aicr.NewClientContext(ctx,
		aicr.WithRecipeSource(source),
		aicr.WithVersion(version),
	)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to initialize data provider")
	}
	return client, nil
}

// embeddedClient constructs an aicr.Client bound to the embedded recipe data,
// for the supply-chain commands that operate on an artifact rather than on a
// recipe catalog (verify, evidence verify/digest/publish, recipe
// verify-catalog/sign-catalog).
//
// Deliberately NOT recipeClientFromCmd: none of these commands defines --data,
// and routing them through the config-aware constructor would make a
// spec.recipe.data entry in an unrelated AICRConfig change (or fail) an
// artifact verification that never reads the catalog. The catalog commands
// verify and sign the EMBEDDED catalog specifically, which is what ships
// signed as a release asset.
//
// Callers MUST Close the returned Client (defer client.Close()).
func embeddedClient(ctx context.Context) (*aicr.Client, error) {
	client, err := aicr.NewClientContext(ctx,
		aicr.WithRecipeSource(aicr.EmbeddedSource()),
		aicr.WithVersion(version),
	)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to initialize aicr client")
	}
	return client, nil
}

// stringFlagOrConfig returns the resolved value for a string CLI flag with
// CLI-overrides-config-overrides-default precedence:
//
//   - Explicit CLI flag (cmd.IsSet) → CLI value, with an INFO log if it
//     differs from a non-empty config fallback.
//   - No CLI flag, non-empty config fallback → fallback.
//   - No CLI flag, empty config fallback → cmd.String(flagName), which
//     surfaces the flag's compile-time Value: default when one is set.
//
// The third case matters when a flag declares Value: "..." in its
// definition (e.g., `--namespace` defaults to "aicr-validation"): an
// unset config field must not collapse that default to the empty string.
func stringFlagOrConfig(cmd *cli.Command, flagName, fallback string) string {
	if !cmd.IsSet(flagName) {
		if fallback != "" {
			return fallback
		}
		return cmd.String(flagName)
	}
	v := cmd.String(flagName)
	if fallback != "" && fallback != v {
		slog.Info("CLI flag overriding config value", "flag", flagName, "config", fallback, "override", v)
	}
	return v
}

// explicitStringFlagOrConfig is stringFlagOrConfig for a flag whose declared
// Value: default must keep appearing in `--help` — it is part of the v1 CLI
// surface pinned by testdata/cli-surface.golden — but must NOT be substituted
// for the caller's silence downstream:
//
//   - Explicit CLI flag (cmd.IsSet) → CLI value, with an INFO log if it
//     differs from a non-empty config fallback.
//   - No CLI flag, non-empty config fallback → fallback (a value in --config
//     is an operator choice too, so it counts as explicit input).
//   - Neither → "", NOT the flag's compile-time Value: default.
//
// The distinction is load-bearing for the agent's --job-name and
// --service-account-name, where "" is a value rather than a missing one: it
// tells pkg/k8s/agent that no prefix was named, so one is derived from
// Config.NameBase and this run's ID. Substituting the declared default there
// would hand agent.Deployer.resolveServiceAccount a name aicr picked rather
// than one the operator typed, and on any cluster still carrying a leftover
// "aicr" ServiceAccount from a pre-ADR-020 install EVERY run would silently
// resolve to exact-ServiceAccount mode and manage no RBAC at all. Exact mode
// must be reachable only from a name the operator actually supplied.
func explicitStringFlagOrConfig(cmd *cli.Command, flagName, fallback string) string {
	if !cmd.IsSet(flagName) {
		return fallback
	}
	v := cmd.String(flagName)
	if fallback != "" && fallback != v {
		slog.Info("CLI flag overriding config value", "flag", flagName, "config", fallback, "override", v)
	}
	return v
}

// intFlagOrConfig returns the CLI flag value when explicitly set; otherwise
// the fallback. Logs an INFO line whenever the resolved value differs from
// the fallback (matching stringFlagOrConfig's symmetric guard so a config
// value of 0 — or any value the user explicitly set — is not silently
// overridden).
func intFlagOrConfig(cmd *cli.Command, flagName string, fallback int) int {
	if !cmd.IsSet(flagName) {
		return fallback
	}
	v := cmd.Int(flagName)
	if fallback != v {
		slog.Info("CLI flag overriding config value", "flag", flagName, "config", fallback, "override", v)
	}
	return v
}

// durationFlagOrConfig returns the CLI flag value when explicitly set;
// otherwise the fallback. A nil fallback signals "config did not set the
// field" — in that case the CLI flag's default duration flows through,
// distinct from a fallback of *0 which preserves an explicit zero-timeout
// (e.g. "disable timeout") value from config.
func durationFlagOrConfig(cmd *cli.Command, flagName string, fallback *time.Duration) time.Duration {
	if !cmd.IsSet(flagName) {
		if fallback != nil {
			return *fallback
		}
		return cmd.Duration(flagName)
	}
	v := cmd.Duration(flagName)
	if fallback != nil && *fallback != v {
		slog.Info("CLI flag overriding config value", "flag", flagName, "config", *fallback, "override", v)
	}
	return v
}

// loadFacadeConfig reads --config and returns it as the facade *aicr.Config,
// so commands derive their options through pkg/client/v1 rather than reaching
// into pkg/config themselves. Returns a nil *aicr.Config when the flag is not
// set; every derivation on it is nil-safe, so callers need no branch.
//
// The flag-over-config overlay stays in this package deliberately: it depends
// on cmd.IsSet, which distinguishes "user passed the zero value" from "user
// said nothing" — a distinction the facade's plain-struct options cannot make.
//
// (nil, nil) is the deliberate "config flag not set" signal — a sentinel
// error would force every caller into a useless error-check branch when
// --config is simply absent.
//
//nolint:nilnil
func loadFacadeConfig(ctx context.Context, cmd *cli.Command) (*aicr.Config, error) {
	src := cmd.String("config")
	if src == "" {
		// (nil, nil) is the deliberate "flag not set" signal. Every derivation
		// on a nil *aicr.Config is nil-safe, so callers need no branch.
		return nil, nil
	}
	// aicr.LoadConfig, not pkg/config.Load: routing through the facade is the
	// point. A second loader path here would let validation or error handling
	// drift between what the CLI sees and what an SDK consumer sees from the
	// same document.
	return aicr.LoadConfig(ctx, src)
}
