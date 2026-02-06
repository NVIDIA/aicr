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
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"github.com/NVIDIA/eidos/pkg/validator"
)

func recipeCmd() *cli.Command {
	return &cli.Command{
		Name:                  "recipe",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Create optimized recipe for given intent and environment parameters.",
		Description: `Generate configuration recipe based on specified environment parameters including:
  - Kubernetes service type (e.g. eks, gke, aks, oke, self-managed)
  - Accelerator type (e.g. h100, gb200, a100, l40)
  - Workload intent (e.g. training, inference)
  - GPU node operating system (e.g. ubuntu, rhel, cos, amazonlinux)
  - Number of GPU nodes in the cluster

The recipe returns a list of components with deployment order based on dependencies.
Output can be in JSON or YAML format.

Examples:

Generate recipe from explicit criteria:
  eidos recipe --service eks --accelerator h100 --os ubuntu --intent training

Generate recipe from a criteria file:
  eidos recipe --criteria criteria.yaml

Generate recipe from a snapshot file:
  eidos recipe --snapshot snapshot.yaml

Generate recipe from a ConfigMap snapshot:
  eidos recipe --snapshot cm://gpu-operator/eidos-snapshot

Save recipe to a file:
  eidos recipe --snapshot cm://gpu-operator/eidos-snapshot -o recipe.yaml

Override criteria file values with flags:
  eidos recipe --criteria criteria.yaml --service gke

Override snapshot-detected criteria:
  eidos recipe --snapshot cm://gpu-operator/eidos-snapshot --service gke`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "service",
				Usage: fmt.Sprintf("Kubernetes service type (e.g. %s)", strings.Join(recipe.GetCriteriaServiceTypes(), ", ")),
			},
			&cli.StringFlag{
				Name:    "accelerator",
				Aliases: []string{"gpu"},
				Usage:   fmt.Sprintf("Accelerator/GPU type (e.g. %s)", strings.Join(recipe.GetCriteriaAcceleratorTypes(), ", ")),
			},
			&cli.StringFlag{
				Name:  "intent",
				Usage: fmt.Sprintf("Workload intent (e.g. %s)", strings.Join(recipe.GetCriteriaIntentTypes(), ", ")),
			},
			&cli.StringFlag{
				Name:  "os",
				Usage: fmt.Sprintf("Operating system type of the GPU node (e.g. %s)", strings.Join(recipe.GetCriteriaOSTypes(), ", ")),
			},
			&cli.StringFlag{
				Name:  "platform",
				Usage: fmt.Sprintf("Platform/framework type to include in the runtime (e.g. %s)", strings.Join(recipe.GetCriteriaPlatformTypes(), ", ")),
			},
			&cli.IntFlag{
				Name:  "nodes",
				Usage: "Number of worker/GPU nodes in the cluster",
			},
			&cli.StringFlag{
				Name:    "snapshot",
				Aliases: []string{"s"},
				Usage: `Path/URI to previously generated configuration snapshot.
	Supports: file paths, HTTP/HTTPS URLs, or ConfigMap URIs (cm://namespace/name).
	If provided, criteria are extracted from the snapshot.`,
			},
			&cli.StringFlag{
				Name:    "criteria",
				Aliases: []string{"c"},
				Usage: `Path to criteria file (YAML/JSON), alternative to individual flags.
	Criteria file fields can be overridden by individual flags.`,
			},
			dataFlag,
			outputFlag,
			formatFlag,
			kubeconfigFlag,
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Validate single-value flags are not duplicated
			if err := validateSingleValueFlags(cmd, "service", "accelerator", "intent", "os", "platform", "snapshot", "criteria", "output", "format"); err != nil {
				return err
			}

			// Initialize external data provider if --data flag is set
			if err := initDataProvider(cmd); err != nil {
				return fmt.Errorf("failed to initialize data provider: %w", err)
			}

			// Parse output format
			outFormat, err := parseOutputFormat(cmd)
			if err != nil {
				return err
			}

			// Create builder
			builder := recipe.NewBuilder(
				recipe.WithVersion(version),
			)

			var result *recipe.RecipeResult

			// Check if using snapshot or criteria file
			// Precedence: snapshot > criteria file > CLI flags
			snapFilePath := cmd.String("snapshot")
			criteriaFilePath := cmd.String("criteria")

			//nolint:gocritic // if-else chain is appropriate for non-empty string conditions
			if snapFilePath != "" {
				slog.Info("loading snapshot from", "uri", snapFilePath)
				snap, loadErr := serializer.FromFileWithKubeconfig[snapshotter.Snapshot](snapFilePath, cmd.String("kubeconfig"))
				if loadErr != nil {
					return fmt.Errorf("failed to load snapshot from %q: %w", snapFilePath, loadErr)
				}

				// Extract criteria from snapshot
				criteria := extractCriteriaFromSnapshot(snap)

				// Apply CLI overrides
				if applyErr := applyCriteriaOverrides(cmd, criteria); applyErr != nil {
					return applyErr
				}

				// Create a constraint evaluator that uses the snapshot
				// This wraps validator.EvaluateConstraint with the snapshot data
				evaluator := func(constraint recipe.Constraint) recipe.ConstraintEvalResult {
					valResult := validator.EvaluateConstraint(constraint, snap)
					return recipe.ConstraintEvalResult{
						Passed: valResult.Passed,
						Actual: valResult.Actual,
						Error:  valResult.Error,
					}
				}

				slog.Info("building recipe from snapshot with constraint validation", "criteria", criteria.String())
				result, err = builder.BuildFromCriteriaWithEvaluator(ctx, criteria, evaluator)

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
			} else if criteriaFilePath != "" {
				// Load criteria from file
				slog.Info("loading criteria from file", "path", criteriaFilePath)
				// TODO: Add context support to LoadCriteriaFromFile for HTTP URL timeout/cancellation
				criteria, loadErr := recipe.LoadCriteriaFromFile(criteriaFilePath) //nolint:contextcheck // serializer.FromFile doesn't support context yet
				if loadErr != nil {
					return fmt.Errorf("failed to load criteria from %q: %w", criteriaFilePath, loadErr)
				}

				// Apply CLI overrides (individual flags take precedence over file)
				if applyErr := applyCriteriaOverrides(cmd, criteria); applyErr != nil {
					return applyErr
				}

				slog.Info("building recipe from criteria file", "criteria", criteria.String())
				result, err = builder.BuildFromCriteria(ctx, criteria)
			} else {
				// Build criteria from CLI flags
				criteria, buildErr := buildCriteriaFromCmd(cmd)
				if buildErr != nil {
					return fmt.Errorf("error parsing criteria: %w", buildErr)
				}

				// Validate that at least some criteria was provided
				if criteria.Specificity() == 0 {
					return fmt.Errorf("no criteria provided: specify at least one of --service, --accelerator, --intent, --os, --platform, --nodes, --criteria, or use --snapshot to load from a snapshot file")
				}

				slog.Info("building recipe from criteria", "criteria", criteria.String())
				result, err = builder.BuildFromCriteria(ctx, criteria)
			}

			if err != nil {
				return fmt.Errorf("error building recipe: %w", err)
			}

			// Serialize output
			output := cmd.String("output")
			ser, err := serializer.NewFileWriterOrStdout(outFormat, output)
			if err != nil {
				return fmt.Errorf("failed to create output writer: %w", err)
			}
			defer func() {
				if closer, ok := ser.(interface{ Close() error }); ok {
					if err := closer.Close(); err != nil {
						slog.Warn("failed to close serializer", "error", err)
					}
				}
			}()

			if err := ser.Serialize(ctx, result); err != nil {
				return fmt.Errorf("failed to serialize recipe: %w", err)
			}

			slog.Info("recipe generation completed",
				"output", output,
				"components", len(result.ComponentRefs),
				"overlays", len(result.Metadata.AppliedOverlays))

			return nil
		},
	}
}

// buildCriteriaFromCmd constructs a recipe.Criteria from CLI command flags.
func buildCriteriaFromCmd(cmd *cli.Command) (*recipe.Criteria, error) {
	var opts []recipe.CriteriaOption

	if s := cmd.String("service"); s != "" {
		opts = append(opts, recipe.WithCriteriaService(s))
	}
	if s := cmd.String("accelerator"); s != "" {
		opts = append(opts, recipe.WithCriteriaAccelerator(s))
	}
	if s := cmd.String("intent"); s != "" {
		opts = append(opts, recipe.WithCriteriaIntent(s))
	}
	if s := cmd.String("os"); s != "" {
		opts = append(opts, recipe.WithCriteriaOS(s))
	}
	if s := cmd.String("platform"); s != "" {
		opts = append(opts, recipe.WithCriteriaPlatform(s))
	}
	if n := cmd.Int("nodes"); n > 0 {
		opts = append(opts, recipe.WithCriteriaNodes(n))
	}

	return recipe.BuildCriteria(opts...)
}

// extractCriteriaFromSnapshot extracts criteria from a snapshot.
// This maps snapshot measurements to criteria fields.
func extractCriteriaFromSnapshot(snap *snapshotter.Snapshot) *recipe.Criteria {
	criteria := recipe.NewCriteria()

	if snap == nil {
		return criteria
	}

	// Extract from K8s measurements
	for _, m := range snap.Measurements {
		if m == nil {
			continue
		}

		switch m.Type {
		case measurement.TypeK8s:
			// Look for service type in server subtype
			for _, st := range m.Subtypes {
				if st.Name == "server" {
					// Try direct "service" field first
					if svcType, ok := st.Data["service"]; ok {
						if parsed, err := recipe.ParseCriteriaServiceType(svcType.String()); err == nil {
							criteria.Service = parsed
						}
					}

					// Extract service from K8s version string (e.g., "v1.33.5-eks-3025e55")
					if version, ok := st.Data["version"]; ok {
						versionStr := version.String()
						switch {
						case strings.Contains(versionStr, "-eks-"):
							criteria.Service = recipe.CriteriaServiceEKS
						case strings.Contains(versionStr, "-gke"):
							criteria.Service = recipe.CriteriaServiceGKE
						case strings.Contains(versionStr, "-aks"):
							criteria.Service = recipe.CriteriaServiceAKS
						}
					}
				}
			}

		case measurement.TypeGPU:
			// Look for GPU/accelerator type in smi or device subtype
			for _, st := range m.Subtypes {
				if st.Name == "smi" || st.Name == "device" {
					// Try "gpu.model" field (from nvidia-smi)
					if model, ok := st.Data["gpu.model"]; ok {
						modelStr := model.String()
						// Map model names to accelerator types
						switch {
						case containsIgnoreCase(modelStr, "gb200"):
							criteria.Accelerator = recipe.CriteriaAcceleratorGB200
						case containsIgnoreCase(modelStr, "h100"):
							criteria.Accelerator = recipe.CriteriaAcceleratorH100
						case containsIgnoreCase(modelStr, "a100"):
							criteria.Accelerator = recipe.CriteriaAcceleratorA100
						case containsIgnoreCase(modelStr, "l40"):
							criteria.Accelerator = recipe.CriteriaAcceleratorL40
						}
					}

					// Also try plain "model" field
					if model, ok := st.Data["model"]; ok {
						modelStr := model.String()
						switch {
						case containsIgnoreCase(modelStr, "gb200"):
							criteria.Accelerator = recipe.CriteriaAcceleratorGB200
						case containsIgnoreCase(modelStr, "h100"):
							criteria.Accelerator = recipe.CriteriaAcceleratorH100
						case containsIgnoreCase(modelStr, "a100"):
							criteria.Accelerator = recipe.CriteriaAcceleratorA100
						case containsIgnoreCase(modelStr, "l40"):
							criteria.Accelerator = recipe.CriteriaAcceleratorL40
						}
					}
				}
			}

		case measurement.TypeOS:
			// Look for OS type in release subtype
			for _, st := range m.Subtypes {
				if st.Name == "release" {
					if osID, ok := st.Data["ID"]; ok {
						if parsed, err := recipe.ParseCriteriaOSType(osID.String()); err == nil {
							criteria.OS = parsed
						}
					}
				}
			}

		case measurement.TypeSystemD:
			// SystemD measurements not used for criteria extraction
			continue
		}
	}

	return criteria
}

// applyCriteriaOverrides applies CLI flag overrides to criteria.
// Logs a warning when a flag overrides a value detected from the snapshot.
func applyCriteriaOverrides(cmd *cli.Command, criteria *recipe.Criteria) error {
	if s := cmd.String("service"); s != "" {
		parsed, err := recipe.ParseCriteriaServiceType(s)
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
		parsed, err := recipe.ParseCriteriaAcceleratorType(s)
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
		parsed, err := recipe.ParseCriteriaIntentType(s)
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
		parsed, err := recipe.ParseCriteriaOSType(s)
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
		parsed, err := recipe.ParseCriteriaPlatformType(s)
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

// containsIgnoreCase checks if s contains substr (case-insensitive).
func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(s) > 0 && len(substr) > 0 &&
			(s[0]|0x20 == substr[0]|0x20) && containsIgnoreCase(s[1:], substr[1:]) ||
		len(s) > 0 && containsIgnoreCase(s[1:], substr))
}
