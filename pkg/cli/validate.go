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
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// validateAgentConfig holds parsed agent configuration for validate command.
type validateAgentConfig struct {
	kubeconfig         string
	namespace          string
	image              string
	imagePullSecrets   []string
	jobName            string
	serviceAccountName string
	nodeSelector       map[string]string
	tolerations        []corev1.Toleration
	timeout            time.Duration
	cleanup            bool
	debug              bool
	requireGPU         bool
}

// parseValidateAgentConfig parses agent deployment flags from the command.
func parseValidateAgentConfig(cmd *cli.Command) (*validateAgentConfig, error) {
	nodeSelector, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid node-selector", err)
	}

	tolerations, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", err)
	}

	return &validateAgentConfig{
		kubeconfig:         cmd.String("kubeconfig"),
		namespace:          cmd.String("namespace"),
		image:              cmd.String("image"),
		imagePullSecrets:   cmd.StringSlice("image-pull-secret"),
		jobName:            cmd.String("job-name"),
		serviceAccountName: cmd.String("service-account-name"),
		nodeSelector:       nodeSelector,
		tolerations:        tolerations,
		timeout:            cmd.Duration("timeout"),
		cleanup:            cmd.Bool("cleanup"),
		debug:              cmd.Bool("debug"),
		requireGPU:         cmd.Bool("require-gpu"),
	}, nil
}

// parseValidationPhases parses phase strings into Phase values.
func parseValidationPhases(phaseStrs []string) ([]validator.Phase, error) {
	if len(phaseStrs) == 0 {
		return nil, nil // nil = all phases
	}

	for _, s := range phaseStrs {
		if s == "all" {
			return nil, nil
		}
	}

	validPhases := map[string]validator.Phase{
		"deployment":  validator.PhaseDeployment,
		"performance": validator.PhasePerformance,
		"conformance": validator.PhaseConformance,
	}

	seen := make(map[validator.Phase]bool)
	var phases []validator.Phase
	for _, s := range phaseStrs {
		p, ok := validPhases[s]
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid phase %q: must be one of: deployment, performance, conformance, all", s))
		}
		if !seen[p] {
			phases = append(phases, p)
			seen[p] = true
		}
	}

	return phases, nil
}

// validatePhasesAgainstRecipe warns when a requested phase has no checks
// defined in the recipe. The phase will still run but produce 0 tests
// in the CTRF report.
func validatePhasesAgainstRecipe(phases []validator.Phase, rec *recipe.RecipeResult) error {
	if rec.Validation == nil {
		if len(phases) > 0 {
			slog.Warn("recipe has no validation section; requested phases will have no checks",
				"phases", phases)
		}
		return nil
	}

	if len(phases) == 0 {
		return nil
	}

	defined := make(map[validator.Phase]bool)
	if rec.Validation.Deployment != nil && len(rec.Validation.Deployment.Checks) > 0 {
		defined[validator.PhaseDeployment] = true
	}
	if rec.Validation.Performance != nil && len(rec.Validation.Performance.Checks) > 0 {
		defined[validator.PhasePerformance] = true
	}
	if rec.Validation.Conformance != nil && len(rec.Validation.Conformance.Checks) > 0 {
		defined[validator.PhaseConformance] = true
	}

	for _, p := range phases {
		if !defined[p] {
			slog.Warn("phase requested but no checks defined in recipe; phase will be empty",
				"phase", p)
		}
	}

	return nil
}

// deployAgentForValidation deploys an agent to capture a snapshot and returns the Snapshot.
// Creates the namespace if it does not exist.
func deployAgentForValidation(ctx context.Context, cfg *validateAgentConfig) (*snapshotter.Snapshot, string, error) {
	// Ensure namespace exists before deploying the agent Job.
	clientset, _, err := k8sclient.GetKubeClient()
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes client", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.namespace}}
	if _, nsErr := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); nsErr != nil {
		if !apierrors.IsAlreadyExists(nsErr) {
			return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", nsErr)
		}
	}

	agentConfig := &snapshotter.AgentConfig{
		Kubeconfig:         cfg.kubeconfig,
		Namespace:          cfg.namespace,
		Image:              cfg.image,
		ImagePullSecrets:   cfg.imagePullSecrets,
		JobName:            cfg.jobName,
		ServiceAccountName: cfg.serviceAccountName,
		NodeSelector:       cfg.nodeSelector,
		Tolerations:        cfg.tolerations,
		Timeout:            cfg.timeout,
		Cleanup:            cfg.cleanup,
		Debug:              cfg.debug,
		Privileged:         true,
		RequireGPU:         cfg.requireGPU,
	}

	snap, err := snapshotter.DeployAndGetSnapshot(ctx, agentConfig)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to capture snapshot", err)
	}

	source := fmt.Sprintf("agent:%s/%s", cfg.namespace, cfg.jobName)
	return snap, source, nil
}

// runValidation runs validation using the container-per-validator engine.
func runValidation(
	ctx context.Context,
	rec *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
	phases []validator.Phase,
	output string,
	outFormat serializer.Format,
	failOnError bool,
	validationNamespace string,
	cleanup bool,
	imagePullSecrets []string,
	noCluster bool,
	tolerations []corev1.Toleration,
	evidenceDir string,
) error {

	slog.Info("running validation", "phases", phases)

	v := validator.New(
		validator.WithVersion(version),
		validator.WithNamespace(validationNamespace),
		validator.WithCleanup(cleanup),
		validator.WithImagePullSecrets(imagePullSecrets),
		validator.WithNoCluster(noCluster),
		validator.WithTolerations(tolerations),
	)

	results, err := v.ValidatePhases(ctx, phases, rec, snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "validation failed", err)
	}

	// Extract CTRF reports from phase results and merge into a single report.
	reports := make([]*ctrf.Report, 0, len(results))
	for _, pr := range results {
		reports = append(reports, pr.Report)
	}
	combined := ctrf.MergeReports("aicr", version, reports)

	// Serialize combined report
	ser, serErr := serializer.NewFileWriterOrStdout(outFormat, output)
	if serErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create output writer", serErr)
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				slog.Warn("failed to close serializer", "error", closeErr)
			}
		}
	}()

	if writeErr := ser.Serialize(ctx, combined); writeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize CTRF report", writeErr)
	}

	// Log per-phase summary
	anyFailed := false
	for _, pr := range results {
		slog.Info("phase result",
			"phase", pr.Phase,
			"status", pr.Status,
			"duration", pr.Duration)
		if pr.Status == "failed" {
			anyFailed = true
		}
	}

	// If cleanup is disabled, provide helpful debugging info
	if !cleanup {
		slog.Info("cleanup disabled - Jobs and RBAC kept for debugging",
			"namespace", validationNamespace,
			"runID", v.RunID)
		slog.Info("to inspect Job logs: kubectl logs -l aicr.nvidia.com/job -n " + validationNamespace)
		slog.Info("to list Jobs: kubectl get jobs -n " + validationNamespace)
		slog.Info("to cleanup manually: kubectl delete jobs -l app.kubernetes.io/name=aicr -n " + validationNamespace)
	}

	// Generate conformance evidence if requested.
	if evidenceDir != "" {
		evidenceCtx, evidenceCancel := context.WithTimeout(ctx, 30*time.Second) //nolint:mnd // Evidence rendering is fast, 30s is generous
		defer evidenceCancel()

		renderer := evidence.New(evidence.WithOutputDir(evidenceDir))
		if renderErr := renderer.Render(evidenceCtx, combined); renderErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "evidence rendering failed", renderErr)
		}
		slog.Info("conformance evidence written", "dir", evidenceDir)
	}

	if failOnError && anyFailed {
		return errors.New(errors.ErrCodeInternal, "validation failed: one or more phases did not pass")
	}

	return nil
}

func validateCmdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "recipe",
			Aliases: []string{"r"},
			Usage: `Path/URI to recipe file containing constraints to validate.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).`,
			Category: "Input",
		},
		&cli.StringFlag{
			Name:    "snapshot",
			Aliases: []string{"s"},
			Usage: `Path/URI to snapshot file containing actual system measurements.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	If not provided, an agent will be deployed to capture a fresh snapshot.`,
			Category: "Input",
		},
		&cli.StringSliceFlag{
			Name: "phase",
			Usage: `Validation phase(s) to run (can be repeated).
	Options: "deployment", "performance", "conformance", "all".
	Default: all phases.
	Example: --phase deployment --phase conformance`,
			Category: "Validation Control",
		},
		&cli.BoolFlag{
			Name:     "fail-on-error",
			Value:    true,
			Usage:    "Exit with non-zero status if any check fails validation",
			Category: "Validation Control",
		},
		&cli.BoolFlag{
			Name:     "no-cluster",
			Usage:    "Run validation without cluster access (dry-run mode). Reports all checks as skipped.",
			Category: "Validation Control",
		},
		// Agent deployment flags (used when --snapshot is not provided)
		&cli.StringFlag{
			Name:     "namespace",
			Aliases:  []string{"n"},
			Usage:    "Kubernetes namespace for snapshot agent and validation Jobs",
			Sources:  cli.EnvVars("AICR_NAMESPACE"),
			Value:    "aicr-validation",
			Category: "Deployment",
		},
		&cli.StringFlag{
			Name:     "image",
			Usage:    "Container image for snapshot agent",
			Sources:  cli.EnvVars("AICR_VALIDATOR_IMAGE"),
			Value:    defaultAgentImage(),
			Category: "Agent Deployment",
		},
		&cli.StringSliceFlag{
			Name:     "image-pull-secret",
			Usage:    "Secret name for pulling images from private registries (can be repeated)",
			Category: "Agent Deployment",
		},
		&cli.StringFlag{
			Name:     "job-name",
			Usage:    "Override default Job name",
			Value:    "aicr-validate",
			Category: "Agent Deployment",
		},
		&cli.StringFlag{
			Name:     "service-account-name",
			Usage:    "Override default ServiceAccount name",
			Value:    "aicr",
			Category: "Agent Deployment",
		},
		&cli.StringSliceFlag{
			Name:     "node-selector",
			Usage:    "Node selector for snapshot agent Job scheduling (format: key=value, can be repeated).",
			Category: "Agent Deployment",
		},
		&cli.StringSliceFlag{
			Name:     "toleration",
			Usage:    "Toleration for snapshot agent and validation Job scheduling (format: key=value:effect). By default, all taints are tolerated.",
			Category: "Agent Deployment",
		},
		&cli.DurationFlag{
			Name:     "timeout",
			Usage:    "Timeout for waiting for Job completion",
			Value:    defaults.CLISnapshotTimeout,
			Category: "Agent Deployment",
		},
		&cli.BoolFlag{
			Name:     "cleanup",
			Value:    true,
			Usage:    "Remove Job and RBAC resources on completion",
			Category: "Agent Deployment",
		},
		&cli.BoolFlag{
			Name:     "require-gpu",
			Sources:  cli.EnvVars("AICR_REQUIRE_GPU"),
			Usage:    "Request nvidia.com/gpu resource for the agent pod.",
			Category: "Agent Deployment",
		},
		&cli.StringFlag{
			Name:     "evidence-dir",
			Usage:    "Write CNCF conformance evidence markdown to this directory. Requires --phase conformance.",
			Category: "Evidence",
		},
		&cli.BoolFlag{
			Name:     "cncf-submission",
			Usage:    "Collect detailed behavioral evidence for CNCF AI Conformance submission. Deploys GPU workloads, captures nvidia-smi output, Prometheus queries, and HPA scaling tests. Requires --evidence-dir. Takes ~15 minutes.",
			Category: "Evidence",
		},
		&cli.StringSliceFlag{
			Name:    "feature",
			Aliases: []string{"f"},
			Usage: "Evidence feature to collect (repeatable, default: all). Only used with --cncf-submission.\n" +
				"Options: " + strings.Join(evidence.ValidFeatures, ", "),
			Category: "Evidence",
		},
		dataFlag,
		outputFlag,
		kubeconfigFlag,
	}
}

func validateCmd() *cli.Command {
	return &cli.Command{
		Name:                  "validate",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Validate cluster against recipe constraints using containerized validators.",
		Description: `Run validation checks against a cluster snapshot using the constraints and
checks defined in a recipe. Each validator runs as an isolated Kubernetes Job.

Results are output in CTRF (Common Test Report Format) JSON — an industry-standard
schema for test reporting (https://ctrf.io/). Output goes to stdout or the file
specified by --output.

You can either provide an existing snapshot file or let the command deploy an
agent to capture a fresh snapshot from the cluster.

# Examples

Validate using an existing snapshot:
  aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

Deploy agent to capture and validate in one step:
  aicr validate --recipe recipe.yaml

Run specific phases:
  aicr validate -r recipe.yaml -s snapshot.yaml \
    --phase deployment --phase conformance

Save CTRF report to file:
  aicr validate -r recipe.yaml -s snapshot.yaml --output report.json

Run validation without failing on check errors (informational mode):
  aicr validate -r recipe.yaml -s snapshot.yaml --fail-on-error=false
`,
		Flags: validateCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output", "namespace", "image", "job-name", "service-account-name", "timeout", "data"); err != nil {
				return err
			}

			if err := initDataProvider(cmd); err != nil {
				return err
			}

			evidenceDir := cmd.String("evidence-dir")
			cncfSubmission := cmd.Bool("cncf-submission")
			features := cmd.StringSlice("feature")

			// Validate flag combinations.
			if cncfSubmission && evidenceDir == "" {
				return errors.New(errors.ErrCodeInvalidRequest, "--cncf-submission requires --evidence-dir")
			}
			if len(features) > 0 && !cncfSubmission {
				return errors.New(errors.ErrCodeInvalidRequest, "--feature requires --cncf-submission")
			}

			// Short-circuit: --cncf-submission bypasses normal validation and runs
			// the behavioral evidence collector directly.
			if cncfSubmission {
				return runCNCFSubmission(ctx, evidenceDir, features, cmd.String("kubeconfig"))
			}

			phases, err := parseValidationPhases(cmd.StringSlice("phase"))
			if err != nil {
				return err
			}

			recipeFilePath := cmd.String("recipe")
			snapshotFilePath := cmd.String("snapshot")
			kubeconfig := cmd.String("kubeconfig")

			validationNamespace := cmd.String("namespace")

			if recipeFilePath == "" {
				return errors.New(errors.ErrCodeInvalidRequest, "--recipe is required")
			}

			failOnError := cmd.Bool("fail-on-error")

			slog.Info("loading recipe", "uri", recipeFilePath)

			rec, err := serializer.FromFileWithKubeconfig[recipe.RecipeResult](recipeFilePath, kubeconfig)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load recipe from %q", recipeFilePath), err)
			}

			var snap *snapshotter.Snapshot

			if snapshotFilePath != "" {
				slog.Info("loading snapshot", "uri", snapshotFilePath)
				snap, err = serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapshotFilePath, kubeconfig)
				if err != nil {
					return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %q", snapshotFilePath), err)
				}
			} else {
				slog.Info("deploying agent to capture snapshot")

				agentCfg, cfgErr := parseValidateAgentConfig(cmd)
				if cfgErr != nil {
					return cfgErr
				}

				var deployErr error
				snap, _, deployErr = deployAgentForValidation(ctx, agentCfg)
				if deployErr != nil {
					return deployErr
				}
			}

			tolerations, tolErr := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
			if tolErr != nil {
				return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", tolErr)
			}

			// Validate that requested phases are defined in the recipe.
			if err := validatePhasesAgainstRecipe(phases, rec); err != nil {
				return err
			}

			return runValidation(ctx, rec, snap, phases, cmd.String("output"), serializer.FormatJSON, failOnError, validationNamespace, cmd.Bool("cleanup"), cmd.StringSlice("image-pull-secret"), cmd.Bool("no-cluster"), tolerations, evidenceDir)
		},
	}
}

// runCNCFSubmission handles --cncf-submission: validates feature names and
// runs the behavioral evidence collector against the live cluster.
func runCNCFSubmission(ctx context.Context, evidenceDir string, features []string, kubeconfig string) error {
	// Validate feature names.
	for _, f := range features {
		if !evidence.IsValidFeature(f) {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown feature %q; valid features: %s",
					f, strings.Join(evidence.ValidFeatures, ", ")))
		}
	}

	cncfTimeout := 20 * time.Minute //nolint:mnd // CNCF submission deploys GPU workloads and runs HPA tests
	ctx, cancel := context.WithTimeout(ctx, cncfTimeout)
	defer cancel()

	slog.Info("starting CNCF submission evidence collection",
		"evidenceDir", evidenceDir, "features", features)

	collector := evidence.NewCollector(evidenceDir,
		evidence.WithFeatures(features),
		evidence.WithKubeconfig(kubeconfig),
	)
	return collector.Run(ctx)
}
