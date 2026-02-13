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

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"github.com/NVIDIA/eidos/pkg/validator"
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
		return nil, fmt.Errorf("invalid node-selector: %w", err)
	}

	tolerations, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
	if err != nil {
		return nil, fmt.Errorf("invalid toleration: %w", err)
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
			return nil, fmt.Errorf("invalid phase %q: must be one of: readiness, deployment, performance, conformance, all", phaseStr)
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
		Enabled:            true,
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
		return nil, "", fmt.Errorf("failed to capture snapshot: %w", err)
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
) error {

	slog.Info("running validation",
		"recipe", recipeSource,
		"snapshot", snapshotSource,
		"phases", phases,
		"constraints", len(rec.Constraints),
		"validation_namespace", validationNamespace,
		"validator_image", validatorImage,
		"resume", resumeRunID)

	// Create validator with optional RunID for resume
	opts := []validator.Option{
		validator.WithVersion(version),
		validator.WithNamespace(validationNamespace),
		validator.WithImage(validatorImage),
	}
	if resumeRunID != "" {
		opts = append(opts, validator.WithRunID(resumeRunID))
	}
	v := validator.New(opts...)

	// Validate with phase support
	result, err := v.ValidatePhases(ctx, phases, rec, snap)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	// Set source information
	result.RecipeSource = recipeSource
	result.SnapshotSource = snapshotSource

	// Serialize output
	ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
	if err != nil {
		return fmt.Errorf("failed to create output writer: %w", err)
	}
	defer func() {
		if closer, ok := ser.(interface{ Close() error }); ok {
			if closeErr := closer.Close(); closeErr != nil {
				slog.Warn("failed to close serializer", "error", closeErr)
			}
		}
	}()

	if err := ser.Serialize(ctx, result); err != nil {
		return fmt.Errorf("failed to serialize validation result: %w", err)
	}

	slog.Info("validation completed",
		"status", result.Summary.Status,
		"passed", result.Summary.Passed,
		"failed", result.Summary.Failed,
		"skipped", result.Summary.Skipped,
		"duration", result.Summary.Duration)

	// Check if we should fail on validation errors
	if failOnError && result.Summary.Status == validator.ValidationStatusFail {
		return fmt.Errorf("validation failed: %d constraint(s) did not pass", result.Summary.Failed)
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
			// Agent deployment flags (used when --snapshot is not provided)
			&cli.StringFlag{
				Name:    "namespace",
				Usage:   "Kubernetes namespace for snapshot agent deployment (enables agent mode when set without --snapshot)",
				Sources: cli.EnvVars("EIDOS_NAMESPACE"),
				Value:   "gpu-operator",
			},
			&cli.StringFlag{
				Name:    "validation-namespace",
				Usage:   "Kubernetes namespace where validation jobs will run",
				Sources: cli.EnvVars("EIDOS_VALIDATION_NAMESPACE"),
				Value:   "eidos-validation",
			},
			&cli.StringFlag{
				Name:    "image",
				Usage:   "Container image for validation Jobs (must include Go toolchain)",
				Sources: cli.EnvVars("EIDOS_VALIDATOR_IMAGE"),
				Value:   "ghcr.io/nvidia/eidos-validator:latest",
			},
			&cli.StringSliceFlag{
				Name:  "image-pull-secret",
				Usage: "Secret name for pulling images from private registries (can be repeated)",
			},
			&cli.StringFlag{
				Name:  "job-name",
				Usage: "Override default Job name",
				Value: "eidos-validate",
			},
			&cli.StringFlag{
				Name:  "service-account-name",
				Usage: "Override default ServiceAccount name",
				Value: "eidos",
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
				Value: 5 * time.Minute,
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
				Sources: cli.EnvVars("EIDOS_REQUIRE_GPU"),
				Usage:   "Request nvidia.com/gpu resource for the agent pod. Required in CDI environments where GPU devices are only injected when explicitly requested.",
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
  eidos validate --recipe recipe.yaml --snapshot snapshot.yaml

Load snapshot from ConfigMap:
  eidos validate --recipe recipe.yaml --snapshot cm://gpu-operator/eidos-snapshot

Deploy agent to capture and validate in one step:
  eidos validate --recipe recipe.yaml --namespace gpu-operator

Target specific GPU nodes with node selector:
  eidos validate --recipe recipe.yaml \
    --namespace gpu-operator \
    --node-selector nodeGroup=customer-gpu

Run multiple validation phases:
  eidos validate -r recipe.yaml -s snapshot.yaml \
    --phase readiness --phase deployment --phase conformance

Run all validation phases:
  eidos validate -r recipe.yaml -s snapshot.yaml --phase all

Run validation jobs in custom namespace:
  eidos validate -r recipe.yaml -s snapshot.yaml \
    --validation-namespace my-validation-ns

Run validation without failing on constraint errors (informational mode):
  eidos validate -r recipe.yaml -s snapshot.yaml --fail-on-error=false

Resume a previous validation run from where it left off:
  eidos validate -r recipe.yaml -s snapshot.yaml --resume 20260206-140523-a3f9
`,
		Flags:  validateCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Validate single-value flags are not duplicated
			// Note: --phase allows multiple values so it's not included here
			if err := validateSingleValueFlags(cmd, "recipe", "snapshot", "output", "format", "namespace", "validation-namespace", "image", "job-name", "service-account-name", "timeout", "resume"); err != nil {
				return err
			}

			recipeFilePath := cmd.String("recipe")
			snapshotFilePath := cmd.String("snapshot")
			kubeconfig := cmd.String("kubeconfig")
			validationNamespace := cmd.String("validation-namespace")

			// Recipe is always required
			if recipeFilePath == "" {
				return fmt.Errorf("--recipe is required")
			}

			// Parse output format
			outFormat, err := parseOutputFormat(cmd)
			if err != nil {
				return err
			}

			failOnError := cmd.Bool("fail-on-error")

			// Parse phases (default to readiness if none specified)
			phases, err := parseValidationPhases(cmd.StringSlice("phase"))
			if err != nil {
				return err
			}

			slog.Info("loading recipe", "uri", recipeFilePath)

			// Load recipe
			rec, err := serializer.FromFileWithKubeconfig[recipe.RecipeResult](recipeFilePath, kubeconfig)
			if err != nil {
				return fmt.Errorf("failed to load recipe from %q: %w", recipeFilePath, err)
			}

			// Get snapshot - either from file or by deploying an agent
			var snap *snapshotter.Snapshot
			var snapshotSource string

			if snapshotFilePath != "" {
				// Load snapshot from file/URL/ConfigMap
				slog.Info("loading snapshot", "uri", snapshotFilePath)
				snap, err = serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapshotFilePath, kubeconfig)
				if err != nil {
					return fmt.Errorf("failed to load snapshot from %q: %w", snapshotFilePath, err)
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

			return runValidation(ctx, rec, snap, phases, recipeFilePath, snapshotSource, cmd.String("output"), outFormat, failOnError, validationNamespace, cmd.String("resume"), cmd.String("image"))
		},
	}
}
