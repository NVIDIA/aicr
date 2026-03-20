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
	"os"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/collector"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
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

// createSnapshotSerializer creates the output serializer based on template options.
func createSnapshotSerializer(tmplOpts *snapshotTemplateOptions) (serializer.Serializer, error) {
	if tmplOpts.templatePath != "" {
		return serializer.NewTemplateFileWriter(tmplOpts.templatePath, tmplOpts.outputPath)
	}
	return serializer.NewFileWriterOrStdout(tmplOpts.format, tmplOpts.outputPath)
}

func snapshotCmdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:     "namespace",
			Aliases:  []string{"n"},
			Usage:    "Kubernetes namespace for agent deployment",
			Sources:  cli.EnvVars("AICR_NAMESPACE"),
			Value:    "default",
			Category: "Agent Deployment",
		},
		&cli.StringFlag{
			Name:     "image",
			Usage:    "Container image for agent Job",
			Sources:  cli.EnvVars("AICR_IMAGE"),
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
			Value:    "aicr",
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
			Usage:    "Node selector for Job scheduling (format: key=value, can be repeated). Recommended in heterogeneous clusters to target GPU nodes",
			Category: "Agent Deployment",
		},
		&cli.StringSliceFlag{
			Name:     "toleration",
			Usage:    "Toleration for Job scheduling (format: key=value:effect). By default, all taints are tolerated. Specifying this flag overrides the defaults.",
			Category: "Agent Deployment",
		},
		&cli.DurationFlag{
			Name:     "timeout",
			Usage:    "Timeout for waiting for Job completion",
			Value:    defaults.CLISnapshotTimeout,
			Category: "Agent Deployment",
		},
		&cli.BoolFlag{
			Name:     "no-cleanup",
			Usage:    "Skip removal of Job and RBAC resources on completion (leaves cluster-admin binding active)",
			Category: "Agent Deployment",
		},
		&cli.BoolFlag{
			Name:     "privileged",
			Value:    true,
			Usage:    "Run agent in privileged mode (required for GPU/SystemD collectors). Set to false for PSS-restricted namespaces.",
			Category: "Agent Deployment",
		},
		&cli.BoolFlag{
			Name:     "require-gpu",
			Sources:  cli.EnvVars("AICR_REQUIRE_GPU"),
			Usage:    "Require GPU detection. Fails the snapshot if no GPU is found. In agent mode, also requests nvidia.com/gpu resource for the pod (required in CDI environments).",
			Category: "Agent Deployment",
		},
		&cli.StringFlag{
			Name:     "template",
			Usage:    "Path to Go template file for custom output formatting (requires YAML format)",
			Category: "Output",
		},
		&cli.IntFlag{
			Name:     "max-nodes-per-entry",
			Usage:    "Maximum node names per taint/label entry in topology collection (0 = unlimited)",
			Value:    0,
			Category: "Output",
		},
		outputFlag,
		formatFlag,
		kubeconfigFlag,
	}
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
  - Kubernetes cluster configuration (server, nodes, images, policies)
  - Loaded kernel modules
  - Sysctl kernel parameters
  - SystemD service configurations

Deploys a Kubernetes Job on a GPU node to capture the snapshot. All collection
is done inside the cluster and no data is egressed out.

The snapshot process:
  1. Deploy RBAC resources (ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding)
  2. Deploy a Job on GPU nodes to capture the snapshot
  3. Wait for the Job to complete
  4. Retrieve the snapshot from the ConfigMap
  5. Save it to the target output location
  6. Clean up the Job (optionally keep RBAC for reuse)

The snapshot Job must run on a GPU node to collect GPU hardware information
(nvidia-smi, device properties, driver version). In heterogeneous clusters
with both CPU and GPU nodes, use --node-selector to ensure the Job lands
on a GPU node. Before GPU Operator is installed, use the node name or a
user-defined label; after installation, nvidia.com/gpu.present=true is
available.

Examples:

Basic snapshot (homogeneous GPU cluster):
  aicr snapshot --output cm://default/aicr-snapshot

Target a GPU node before GPU Operator installation:
  aicr snapshot --node-selector kubernetes.io/hostname=gpu-node-1

Target GPU nodes after GPU Operator installation:
  aicr snapshot --node-selector nvidia.com/gpu.present=true

Override default tolerations (by default, all taints are tolerated):
  aicr snapshot --toleration dedicated=user-workload:NoSchedule

Combined node selector and custom tolerations:
  aicr snapshot \
    --node-selector nodeGroup=customer-gpu \
    --toleration dedicated=user-workload:NoSchedule \
    --output cm://default/aicr-snapshot

Custom output formatting with Go templates:
  aicr snapshot --template my-template.tmpl --output report.md

  aicr snapshot \
    --node-selector nodeGroup=customer-gpu \
    --template my-template.tmpl \
    --output report.md

The template receives the full Snapshot struct with Header (Kind, APIVersion, Metadata)
and Measurements array. Sprig template functions are available for rich formatting.
See examples/templates/snapshot-template.md.tmpl for a sample template.
`,
		Flags: snapshotCmdFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Validate single-value flags are not duplicated
			if err := validateSingleValueFlags(cmd, "namespace", "image", "job-name", "service-account-name", "timeout", "template", "max-nodes-per-entry", "output", "format"); err != nil {
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
				collector.WithMaxNodesPerEntry(cmd.Int("max-nodes-per-entry")),
			)

			// Create output serializer
			ser, err := createSnapshotSerializer(tmplOpts)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output serializer", err)
			}

			// Build snapshotter configuration
			ns := snapshotter.NodeSnapshotter{
				Version:    version,
				Factory:    factory,
				Serializer: ser,
				RequireGPU: cmd.Bool("require-gpu"),
			}

			// Parse node selectors
			nodeSelector, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
			if err != nil {
				return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid node-selector", err)
			}

			// Parse tolerations
			tolerations, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
			if err != nil {
				return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", err)
			}

			// When running inside an agent Job, collect locally instead of
			// deploying another agent (prevents infinite nesting).
			// Clear pre-created factory so measure() rebuilds it from env vars
			// (AICR_MAX_NODES_PER_ENTRY).
			if os.Getenv("AICR_AGENT_MODE") == "true" {
				ns.Factory = nil
				return ns.Measure(ctx)
			}

			// Configure agent deployment
			ns.AgentConfig = &snapshotter.AgentConfig{
				Kubeconfig:         cmd.String("kubeconfig"),
				Namespace:          cmd.String("namespace"),
				Image:              cmd.String("image"),
				ImagePullSecrets:   cmd.StringSlice("image-pull-secret"),
				JobName:            cmd.String("job-name"),
				ServiceAccountName: cmd.String("service-account-name"),
				NodeSelector:       nodeSelector,
				Tolerations:        tolerations,
				Timeout:            cmd.Duration("timeout"),
				Cleanup:            !cmd.Bool("no-cleanup"),
				Output:             tmplOpts.outputPath,
				Debug:              cmd.Bool("debug"),
				Privileged:         cmd.Bool("privileged"),
				RequireGPU:         cmd.Bool("require-gpu"),
				TemplatePath:       tmplOpts.templatePath,
				MaxNodesPerEntry:   cmd.Int("max-nodes-per-entry"),
			}

			return ns.Measure(ctx)
		},
	}
}
