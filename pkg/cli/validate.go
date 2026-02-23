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
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
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
	privileged         bool
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
		privileged:         cmd.Bool("privileged"),
		requireGPU:         cmd.Bool("require-gpu"),
	}, nil
}

// parseValidationPhases parses phase strings into ValidationPhaseName values.
// It deduplicates phases and returns them in canonical order (readiness → deployment → performance → conformance).
func parseValidationPhases(phaseStrs []string) ([]validator.ValidationPhaseName, error) {
	if len(phaseStrs) == 0 {
		return []validator.ValidationPhaseName{validator.PhaseReadiness}, nil
	}

	// Parse and collect requested phases into a set for deduplication
	requested := make(map[validator.ValidationPhaseName]bool)
	for _, phaseStr := range phaseStrs {
		switch phaseStr {
		case "readiness":
			requested[validator.PhaseReadiness] = true
		case "deployment":
			requested[validator.PhaseDeployment] = true
		case "performance":
			requested[validator.PhasePerformance] = true
		case "conformance":
			requested[validator.PhaseConformance] = true
		case "all":
			// "all" means all phases - return PhaseAll which is handled specially by validator
			return []validator.ValidationPhaseName{validator.PhaseAll}, nil
		default:
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid phase %q: must be one of: readiness, deployment, performance, conformance, all", phaseStr))
		}
	}

	// Build result in canonical order using validator.PhaseOrder
	var phases []validator.ValidationPhaseName
	for _, phase := range validator.PhaseOrder {
		if requested[phase] {
			phases = append(phases, phase)
		}
	}

	return phases, nil
}

// deployAgentForValidation deploys an agent to capture a snapshot and returns the Snapshot.
func deployAgentForValidation(ctx context.Context, cfg *validateAgentConfig) (*snapshotter.Snapshot, string, error) {
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
		Privileged:         cfg.privileged,
		RequireGPU:         cfg.requireGPU,
	}

	snap, err := snapshotter.DeployAndGetSnapshot(ctx, agentConfig)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInternal, "failed to capture snapshot", err)
	}

	source := fmt.Sprintf("agent:%s/%s", cfg.namespace, cfg.jobName)
	return snap, source, nil
}

// runValidation runs validation and handles result serialization.
func runValidation(
	ctx context.Context,
	rec *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
	phases []validator.ValidationPhaseName,
	recipeSource, snapshotSource, output string,
	outFormat serializer.Format,
	failOnError bool,
	validationNamespace string,
	resumeRunID string,
	validatorImage string,
	cleanup bool,
	imagePullSecrets []string,
	noCluster bool,
	evidenceDir string,
	evidenceResultPath string,
) error {

	slog.Info("running validation",
		"recipe", recipeSource,
		"snapshot", snapshotSource,
		"phases", phases,
		"constraints", len(rec.Constraints),
		"validation_namespace", validationNamespace,
		"validator_image", validatorImage,
		"resume", resumeRunID,
		"cleanup", cleanup)

	// Create validator with optional RunID for resume
	opts := []validator.Option{
		validator.WithVersion(version),
		validator.WithNamespace(validationNamespace),
		validator.WithImage(validatorImage),
		validator.WithCleanup(cleanup),
		validator.WithImagePullSecrets(imagePullSecrets),
		validator.WithNoCluster(noCluster),
	}
	if resumeRunID != "" {
		opts = append(opts, validator.WithRunID(resumeRunID))
	}
	v := validator.New(opts...)

	// Validate with phase support
	result, err := v.ValidatePhases(ctx, phases, rec, snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "validation failed", err)
	}

	// Set source information
	result.RecipeSource = recipeSource
	result.SnapshotSource = snapshotSource

	// Serialize output
	ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create output writer", err)
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				slog.Warn("failed to close serializer", "error", closeErr)
			}
		}
	}()

	if err := ser.Serialize(ctx, result); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize validation result", err)
	}

	slog.Info("validation completed",
		"status", result.Summary.Status,
		"passed", result.Summary.Passed,
		"failed", result.Summary.Failed,
		"skipped", result.Summary.Skipped,
		"duration", result.Summary.Duration)

	// Generate evidence if requested. Strict: failure is an error.
	if evidenceDir != "" {
		// Use a saved result file for evidence when --result is provided,
		// otherwise use the result from the validation run we just completed.
		evidenceSource := result
		if evidenceResultPath != "" {
			slog.Info("loading saved result for evidence rendering", "path", evidenceResultPath)
			saved, loadErr := serializer.FromFile[validator.ValidationResult](evidenceResultPath)
			if loadErr != nil {
				return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to load evidence result", loadErr)
			}
			evidenceSource = saved
		}

		evidenceCtx, evidenceCancel := context.WithTimeout(ctx, defaults.EvidenceRenderTimeout)
		defer evidenceCancel()

		renderer := evidence.New(evidence.WithOutputDir(evidenceDir))
		if err := renderer.Render(evidenceCtx, evidenceSource); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "evidence rendering failed", err)
		}
		slog.Info("conformance evidence written", "dir", evidenceDir)
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

	// Check if we should fail on validation errors
	if failOnError && result.Summary.Status == validator.ValidationStatusFail {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf("validation failed: %d constraint(s) did not pass", result.Summary.Failed))
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
		},
		&cli.StringFlag{
			Name:    "snapshot",
			Aliases: []string{"s"},
			Usage: `Path/URI to snapshot file containing actual system measurements.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	If not provided, an agent will be deployed to capture a fresh snapshot.`,
		},
		&cli.StringFlag{
			Name:  "resume",
			Usage: "Resume a previous validation run by RunID (format: YYYYMMDD-HHMMSS-XXXX). Skips phases that previously passed.",
		},
		&cli.StringSliceFlag{
			Name: "phase",
			Usage: `Validation phase(s) to run (can be repeated).
	Options: "readiness", "deployment", "performance", "conformance", "all".
	Default: "readiness" (quick readiness check).
	Example: --phase readiness --phase deployment`,
		},
		&cli.BoolFlag{
			Name:  "fail-on-error",
			Value: true,
			Usage: "Exit with non-zero status if any constraint fails validation",
		},
		&cli.BoolFlag{
			Name:  "no-cluster",
			Usage: "Run validation without cluster access (dry-run mode). Skips checks that require cluster operations.",
		},
		// Agent deployment flags (used when --snapshot is not provided)
		&cli.StringFlag{
			Name:    "namespace",
			Usage:   "Kubernetes namespace for snapshot agent deployment (enables agent mode when set without --snapshot)",
			Sources: cli.EnvVars("AICR_NAMESPACE"),
			Value:   "gpu-operator",
		},
		&cli.StringFlag{
			Name:    "validation-namespace",
			Usage:   "Kubernetes namespace where validation jobs will run. If not set via this flag or AICR_VALIDATION_NAMESPACE, defaults to the --namespace value.",
			Sources: cli.EnvVars("AICR_VALIDATION_NAMESPACE"),
			Value:   "aicr-validation",
		},
		&cli.StringFlag{
			Name:    "image",
			Usage:   "Container image for validation Jobs (must include Go toolchain)",
			Sources: cli.EnvVars("AICR_VALIDATOR_IMAGE"),
			Value:   "ghcr.io/nvidia/aicr-validator:latest",
		},
		&cli.StringSliceFlag{
			Name:  "image-pull-secret",
			Usage: "Secret name for pulling images from private registries (can be repeated)",
		},
		&cli.StringFlag{
			Name:  "job-name",
			Usage: "Override default Job name",
			Value: "aicr-validate",
		},
		&cli.StringFlag{
			Name:  "service-account-name",
			Usage: "Override default ServiceAccount name",
			Value: "aicr",
		},
		&cli.StringSliceFlag{
			Name:  "node-selector",
			Usage: "Node selector for Job scheduling (format: key=value, can be repeated)",
		},
		&cli.StringSliceFlag{
			Name:  "toleration",
			Usage: "Toleration for Job scheduling (format: key=value:effect). By default, all taints are tolerated.",
		},
		&cli.DurationFlag{
			Name:  "timeout",
			Usage: "Timeout for waiting for Job completion",
			Value: defaults.CLISnapshotTimeout,
		},
		&cli.BoolFlag{
			Name:  "cleanup",
			Value: true,
			Usage: "Remove Job and RBAC resources on completion",
		},
		&cli.BoolFlag{
			Name:  "privileged",
			Value: true,
			Usage: "Run agent in privileged mode (required for GPU/SystemD collectors)",
		},
		&cli.BoolFlag{
			Name:    "require-gpu",
			Sources: cli.EnvVars("AICR_REQUIRE_GPU"),
			Usage:   "Request nvidia.com/gpu resource for the agent pod. Required in CDI environments where GPU devices are only injected when explicitly requested.",
		},
		&cli.StringFlag{
			Name:  "evidence-dir",
			Usage: "Write CNCF conformance evidence markdown to this directory. Requires --phase conformance.",
		},
		&cli.StringFlag{
			Name:  "result",
			Usage: "Use a saved validation result file as the source for evidence rendering (live validation still runs). Requires --phase conformance and --evidence-dir.",
		},
		outputFlag,
		formatFlag,
		kubeconfigFlag,
	}
}

func validateCmd() *cli.Command {
	return &cli.Command{
		Name:                  "validate",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Validate cluster using specific recipe.",
		Description: `Validate a system snapshot against the constraints defined in a recipe.

This command compares actual system measurements from a snapshot against the
expected constraints defined in a recipe file. It reports which constraints
pass, fail, or cannot be evaluated.

You can either provide an existing snapshot file or deploy an agent to capture
a fresh snapshot from the cluster.

# Examples

Validate using an existing snapshot file:
  aicr validate --recipe recipe.yaml --snapshot snapshot.yaml

Load snapshot from ConfigMap:
  aicr validate --recipe recipe.yaml --snapshot cm://gpu-operator/aicr-snapshot

Deploy agent to capture and validate in one step:
  aicr validate --recipe recipe.yaml --namespace gpu-operator

Target specific GPU nodes with node selector:
  aicr validate --recipe recipe.yaml \
    --namespace gpu-operator \
    --node-selector nodeGroup=customer-gpu

Run multiple validation phases:
  aicr validate -r recipe.yaml -s snapshot.yaml \
    --phase readiness --phase deployment --phase conformance

Run all validation phases:
  aicr validate -r recipe.yaml -s snapshot.yaml --phase all

Run validation jobs in custom namespace:
  aicr validate -r recipe.yaml -s snapshot.yaml \
    --validation-namespace my-validation-ns

Run validation without failing on constraint errors (informational mode):
  aicr validate -r recipe.yaml -s snapshot.yaml --fail-on-error=false

Resume a previous validation run from where it left off:
  aicr validate -r recipe.yaml -s snapshot.yaml --resume 20260206-140523-a3f9

Generate conformance evidence alongside validation:
  aicr validate -r recipe.yaml -s snapshot.yaml \
    --phase conformance --evidence-dir ./evidence

Use a saved result file for evidence instead of the live run:
  aicr validate -r recipe.yaml -s snapshot.yaml \
    --phase conformance --evidence-dir ./evidence \
    --result validation-result.yaml
`,
		Flags: validateCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Validate single-value flags are not duplicated
			// Note: --phase allows multiple values so it's not included here
			if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output", "format", "namespace", "validation-namespace", "image", "job-name", "service-account-name", "timeout", "resume", "result"); err != nil {
				return err
			}

			evidenceDir := cmd.String("evidence-dir")
			resultPath := cmd.String("result")

			// Parse phases (default to readiness if none specified)
			phases, err := parseValidationPhases(cmd.StringSlice("phase"))
			if err != nil {
				return err
			}

			// Validate evidence flag constraints.
			hasConformance := false
			for _, p := range phases {
				if p == validator.PhaseConformance || p == validator.PhaseAll {
					hasConformance = true
					break
				}
			}
			if evidenceDir != "" && !hasConformance {
				return errors.New(errors.ErrCodeInvalidRequest, "--evidence-dir requires --phase conformance")
			}
			if resultPath != "" && evidenceDir == "" {
				return errors.New(errors.ErrCodeInvalidRequest, "--result requires --evidence-dir")
			}

			recipeFilePath := cmd.String("recipe")
			snapshotFilePath := cmd.String("snapshot")
			kubeconfig := cmd.String("kubeconfig")

			// If validation-namespace is not explicitly set, default to namespace value,
			// but only when still at its default (to avoid overriding env var values).
			validationNamespace := cmd.String("validation-namespace")
			if !cmd.IsSet("validation-namespace") && validationNamespace == "aicr-validation" {
				validationNamespace = cmd.String("namespace")
			}

			// Recipe is always required
			if recipeFilePath == "" {
				return errors.New(errors.ErrCodeInvalidRequest, "--recipe is required")
			}

			// Parse output format
			outFormat, err := parseOutputFormat(cmd)
			if err != nil {
				return err
			}

			failOnError := cmd.Bool("fail-on-error")

			slog.Info("loading recipe", "uri", recipeFilePath)

			// Load recipe
			rec, err := serializer.FromFileWithKubeconfig[recipe.RecipeResult](recipeFilePath, kubeconfig)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load recipe from %q", recipeFilePath), err)
			}

			// Get snapshot - either from file or by deploying an agent
			var snap *snapshotter.Snapshot
			var snapshotSource string

			if snapshotFilePath != "" {
				// Load snapshot from file/URL/ConfigMap
				slog.Info("loading snapshot", "uri", snapshotFilePath)
				snap, err = serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapshotFilePath, kubeconfig)
				if err != nil {
					return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %q", snapshotFilePath), err)
				}
				snapshotSource = snapshotFilePath
			} else {
				// Deploy agent to capture snapshot
				slog.Info("deploying agent to capture snapshot")

				agentCfg, cfgErr := parseValidateAgentConfig(cmd)
				if cfgErr != nil {
					return cfgErr
				}

				var deployErr error
				snap, snapshotSource, deployErr = deployAgentForValidation(ctx, agentCfg)
				if deployErr != nil {
					return deployErr
				}
			}

			return runValidation(ctx, rec, snap, phases, recipeFilePath, snapshotSource, cmd.String("output"), outFormat, failOnError, validationNamespace, cmd.String("resume"), cmd.String("image"), cmd.Bool("cleanup"), cmd.StringSlice("image-pull-secret"), cmd.Bool("no-cluster"), evidenceDir, resultPath)
		},
	}
}
