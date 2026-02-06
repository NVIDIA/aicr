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
	"time"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/eidos/pkg/collector"
	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
)

// snapshotTemplateOptions holds parsed template options for the snapshot command.
type snapshotTemplateOptions struct {
	templatePath string
	outputPath   string
	format       serializer.Format
}

// parseSnapshotTemplateOptions parses and validates template-related flags.
func parseSnapshotTemplateOptions(cmd *cli.Command, outFormat serializer.Format) (*snapshotTemplateOptions, error) {
	templatePath := cmd.String("template")
	outputPath := cmd.String("output")

	if templatePath != "" {
		// Validate format is YAML when using template
		if cmd.IsSet("format") && outFormat != serializer.FormatYAML {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--template requires YAML format; --format must be \"yaml\" or omitted")
		}

		// Validate template file exists
		if validateErr := serializer.ValidateTemplateFile(templatePath); validateErr != nil {
			return nil, validateErr
		}

		// Force YAML format for template processing
		outFormat = serializer.FormatYAML
	}

	return &snapshotTemplateOptions{
		templatePath: templatePath,
		outputPath:   outputPath,
		format:       outFormat,
	}, nil
}

func snapshotCmd() *cli.Command {
	return &cli.Command{
		Name:                  "snapshot",
		Category:              functionalCategoryName,
		EnableShellCompletion: true,
		Usage:                 "Capture cluster configuration snapshot.",
		Description: `Generate a comprehensive snapshot of cluster measurements including:
  - CPU and GPU settings
  - GRUB boot parameters
  - Kubernetes cluster configuration
  - Loaded kernel modules
  - Sysctl kernel parameters
  - SystemD service configurations

Note: All collection is done locally and no data is egressed out of the cluster.

Output can be in JSON or YAML format. 
For a more complete snapshot use --deploy-agent to deploy a Kubernetes Job that captures the snapshot on a GPU node:

  eidos snapshot --deploy-agent --namespace gpu-operator --output cm://gpu-operator/eidos-snapshot

The agent mode will:
  1. Deploy RBAC resources (ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding)
  2. Deploy a Job on GPU nodes to capture the snapshot
  3. Wait for the Job to complete
  4. Retrieve the snapshot from the ConfigMap
  5. Save it to the target output location
  6. Clean up the Job (optionally keep RBAC for reuse)

Examples:

Basic agent deployment:
  eidos snapshot --deploy-agent

Target specific GPU nodes with node selector:
  eidos snapshot --deploy-agent --node-selector nodeGroup=customer-gpu

Override default tolerations (by default, all taints are tolerated):
  eidos snapshot --deploy-agent \
    --toleration dedicated=user-workload:NoSchedule

Combined node selector and custom tolerations:
  eidos snapshot --deploy-agent \
    --node-selector nodeGroup=customer-gpu \
    --toleration dedicated=user-workload:NoSchedule \
    --output cm://gpu-operator/eidos-snapshot

Custom output formatting with Go templates:
  eidos snapshot --template my-template.tmpl --output report.md

  eidos snapshot --deploy-agent \
    --node-selector nodeGroup=customer-gpu \
    --template my-template.tmpl \
    --output report.md

The template receives the full Snapshot struct with Header (Kind, APIVersion, Metadata)
and Measurements array. Sprig template functions are available for rich formatting.
See examples/templates/snapshot-template.md.tmpl for a sample template.
`,
		Flags: []cli.Flag{
			// Agent deployment flags
			&cli.BoolFlag{
				Name:  "deploy-agent",
				Usage: "Deploy Kubernetes Job to capture snapshot on GPU nodes",
			},
			&cli.StringFlag{
				Name:    "namespace",
				Usage:   "Kubernetes namespace for agent deployment",
				Sources: cli.EnvVars("EIDOS_NAMESPACE"),
				Value:   "gpu-operator",
			},
			&cli.StringFlag{
				Name:    "image",
				Usage:   "Container image for agent Job",
				Sources: cli.EnvVars("EIDOS_IMAGE"),
				Value:   "ghcr.io/nvidia/eidos:latest",
			},
			&cli.StringSliceFlag{
				Name:  "image-pull-secret",
				Usage: "Secret name for pulling images from private registries (can be repeated)",
			},
			&cli.StringFlag{
				Name:  "job-name",
				Usage: "Override default Job name",
				Value: "eidos",
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
				Usage: "Toleration for Job scheduling (format: key=value:effect). By default, all taints are tolerated. Specifying this flag overrides the defaults.",
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
				Usage: "Run agent in privileged mode (required for GPU/SystemD collectors). Set to false for PSS-restricted namespaces.",
			},
			&cli.StringFlag{
				Name:  "template",
				Usage: "Path to Go template file for custom output formatting (requires YAML format)",
			},
			outputFlag,
			formatFlag,
			kubeconfigFlag,
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Validate single-value flags are not duplicated
			if err := validateSingleValueFlags(cmd, "namespace", "image", "job-name", "service-account-name", "timeout", "template", "output", "format"); err != nil {
				return err
			}

			// Parse output format
			outFormat, err := parseOutputFormat(cmd)
			if err != nil {
				return err
			}

			// Parse and validate template options
			tmplOpts, err := parseSnapshotTemplateOptions(cmd, outFormat)
			if err != nil {
				return err
			}

			// Create factory
			factory := collector.NewDefaultFactory(
				collector.WithVersion(version),
			)

			// Create output serializer
			var ser serializer.Serializer
			if tmplOpts.templatePath != "" {
				// Use template writer
				ser, err = serializer.NewTemplateFileWriter(tmplOpts.templatePath, tmplOpts.outputPath)
				if err != nil {
					return fmt.Errorf("failed to create template writer: %w", err)
				}
			} else {
				// Use standard format writer
				ser, err = serializer.NewFileWriterOrStdout(tmplOpts.format, tmplOpts.outputPath)
				if err != nil {
					return fmt.Errorf("failed to create output writer: %w", err)
				}
			}

			// Build snapshotter configuration
			ns := snapshotter.NodeSnapshotter{
				Version:    version,
				Factory:    factory,
				Serializer: ser,
			}

			// Check if agent deployment mode is enabled
			if cmd.Bool("deploy-agent") {
				// Parse node selectors
				nodeSelector, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
				if err != nil {
					return fmt.Errorf("invalid node-selector: %w", err)
				}

				// Parse tolerations
				tolerations, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
				if err != nil {
					return fmt.Errorf("invalid toleration: %w", err)
				}

				// Configure agent deployment
				ns.AgentConfig = &snapshotter.AgentConfig{
					Enabled:            true,
					Kubeconfig:         cmd.String("kubeconfig"),
					Namespace:          cmd.String("namespace"),
					Image:              cmd.String("image"),
					ImagePullSecrets:   cmd.StringSlice("image-pull-secret"),
					JobName:            cmd.String("job-name"),
					ServiceAccountName: cmd.String("service-account-name"),
					NodeSelector:       nodeSelector,
					Tolerations:        tolerations,
					Timeout:            cmd.Duration("timeout"),
					Cleanup:            cmd.Bool("cleanup"),
					Output:             tmplOpts.outputPath,
					Debug:              cmd.Bool("debug"),
					Privileged:         cmd.Bool("privileged"),
					TemplatePath:       tmplOpts.templatePath,
				}
			}

			// Execute snapshot (routes to local or agent based on config)
			return ns.Measure(ctx)
		},
	}
}
