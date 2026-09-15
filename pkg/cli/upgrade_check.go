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
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// upgradeCheckCmd creates the "upgrade-check" CLI command.
func upgradeCheckCmd() *cli.Command {
	return &cli.Command{
		Name:     "upgrade-check",
		Category: functionalCategoryName,
		Usage:    "Report whether moving between two recipes or bundles is safe to apply",
		Description: `Compare two artifacts component by component and report a verdict for
each version that changed, from the transition records this aicr release
ships. No cluster is contacted.

Either side may be a recipe file or a bundle directory; a bundle is read
through the recipe.yaml at its root, which today only helm bundles carry
(NVIDIA/aicr#2753). Omitting --to re-resolves the --from artifact's own
criteria against this binary's registry, which answers "am I behind, and does
catching up hurt?" rather than "is this move safe?".

Operator steps are deployer-scoped, and no bundle records which deployer
built it, so --deployer is required whenever any component needs steps.

Exits non-zero when any component needs attention: a manual or blocked
verdict, versions that are not comparable, or no record across a breaking
boundary (a major bump, or a minor bump while the major version is 0). The
report prints in full either way; pass --fail-on-error=false to report
without failing.

Examples:
  # Two recipes, for a GitOps pipeline that already knows its deployer
  aicr upgrade-check --from old-recipe.yaml --to new-recipe.yaml --deployer argocd

  # Two helm bundles
  aicr upgrade-check --from ./bundles-v0.16.0 --to ./bundles-v0.17.0 --deployer helm

  # Am I behind, and does catching up hurt?
  aicr upgrade-check --from ./bundles-v0.16.0 --deployer helm

  # JSON for a pipeline, reporting only
  aicr upgrade-check --from old.yaml --to new.yaml --format json --fail-on-error=false`,
		Flags:  upgradeCheckCmdFlags(),
		Action: runUpgradeCheckCmd,
	}
}

// upgradeCheckCmdFlags returns the flags for the upgrade-check command.
func upgradeCheckCmdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:     "from",
			Aliases:  []string{"f"},
			Usage:    "source artifact: recipe file, bundle directory, or ConfigMap URI",
			Category: catInput,
		},
		&cli.StringFlag{
			Name:     "to",
			Usage:    "target artifact (default: re-resolve --from's criteria against this binary's registry)",
			Category: catInput,
		},
		withCompletions(&cli.StringFlag{
			Name:     "deployer",
			Aliases:  []string{"d"},
			Usage:    fmt.Sprintf("deployer the reported steps are scoped to (%s)", strings.Join(config.GetDeployerTypes(), ", ")),
			Category: catInput,
		}, config.GetDeployerTypes),
		&cli.BoolFlag{
			Name:  "fail-on-error",
			Value: true,
			Usage: "Exit with non-zero status if any component needs attention (manual, blocked, " +
				"unversioned, or unknown across a breaking boundary)",
		},
		outputFlag(),
		// Table, unlike every other command's yaml: the report's payload is
		// the operator steps, and folded YAML scalars bury a migration the
		// reader is meant to act on. Machine consumers pass --format json,
		// and a pipeline reads the exit code rather than the document.
		formatFlagDefault(serializer.FormatTable),
		kubeconfigFlag(),
	}
}

// runUpgradeCheckCmd executes the upgrade-check command.
func runUpgradeCheckCmd(ctx context.Context, cmd *cli.Command) error {
	if err := validateSingleValueFlags(cmd, "from", "to", "deployer", "output", "format", "kubeconfig"); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.CLIUpgradeCheckTimeout)
	defer cancel()

	outFormat, err := parseOutputFormat(cmd)
	if err != nil {
		return err
	}

	from := cmd.String("from")
	if from == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "--from is required")
	}
	deployer := cmd.String("deployer")
	if deployer != "" {
		// Reject a typo here rather than letting it silently select no step
		// group, which would render a manual verdict with no steps under it.
		if _, parseErr := config.ParseDeployerType(deployer); parseErr != nil {
			return parseErr
		}
	}

	to := cmd.String("to")
	kubeconfig := cmd.String("kubeconfig")
	slog.Debug("upgrade check", slog.String("from", from), slog.String("to", to), slog.String("deployer", deployer))

	client, err := embeddedClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	report, err := client.UpgradeCheck(ctx, aicr.UpgradeCheckRequest{
		From:       from,
		To:         to,
		Deployer:   deployer,
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		return err
	}

	slog.Info("upgrade check complete",
		slog.Int("components", report.Summary.Components),
		slog.Int("failing", report.Summary.Failing))

	if err := writeUpgradeReport(ctx, cmd, outFormat, kubeconfig, report); err != nil {
		return err
	}

	// The report is written first: informing and erroring are not
	// alternatives, and the exit code is orthogonal to the report.
	if cmd.Bool("fail-on-error") && report.FailsRun() {
		return errors.New(errors.ErrCodeConflict, fmt.Sprintf(
			"upgrade check failed: %d component change(s) need attention", report.Summary.Failing))
	}
	return nil
}

// writeUpgradeReport serializes the report, using the package's own table
// formatter when the output format is table. Uses a named return so Close()
// failures on writable handles are merged with any earlier error via
// errors.Join, so data loss on flush surfaces even when the write also failed.
//
// kubeconfig is propagated to ConfigMap writers so a cm:// destination lands in
// the same cluster the cm:// artifacts were read from.
func writeUpgradeReport(
	ctx context.Context,
	cmd *cli.Command,
	outFormat serializer.Format,
	kubeconfig string,
	report *aicr.UpgradeReport,
) (err error) {

	output := cmd.String(flagOutput)

	if outFormat == serializer.FormatTable {
		output = strings.TrimSpace(output)
		if strings.HasPrefix(output, serializer.ConfigMapURIScheme) {
			return errors.New(errors.ErrCodeInvalidRequest, "table output does not support ConfigMap destinations")
		}
		w := cmd.Root().Writer
		if output != "" && output != "-" && output != serializer.StdoutURI {
			f, createErr := os.Create(output)
			if createErr != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output file", createErr)
			}
			defer func() {
				if closeErr := f.Close(); closeErr != nil {
					err = stderrors.Join(err, errors.Wrap(errors.ErrCodeInternal, "failed to close output file", closeErr))
				}
			}()
			w = f
		}
		return aicr.WriteUpgradeReportTable(w, report)
	}

	ser, err := serializer.NewFileWriterOrStdoutWithKubeconfig(outFormat, output, kubeconfig)
	if err != nil {
		return err
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				err = stderrors.Join(err, errors.Wrap(errors.ErrCodeInternal, "failed to close serializer", closeErr))
			}
		}
	}()

	return ser.Serialize(ctx, report)
}
