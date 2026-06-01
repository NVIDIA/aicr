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
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/collector"
	"github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// resolveSnapshotNodeSelector resolves the snapshot node selector with
// CLI-overrides-config precedence. The CLI flag is a repeated string in
// key=value form; the config value is already a typed map. Either source
// can be empty; the result preserves the same nil-vs-empty semantics.
func resolveSnapshotNodeSelector(cmd *cli.Command, resolved *config.SnapshotResolved) (map[string]string, error) {
	if cmd.IsSet("node-selector") {
		ns, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid node-selector", err)
		}
		if resolved.NodeSelector != nil {
			slog.Info("CLI flag overriding config value", "flag", "node-selector",
				"config", resolved.NodeSelector, "override", ns)
		}
		return ns, nil
	}
	return resolved.NodeSelector, nil
}

// resolveSnapshotTolerations resolves the snapshot toleration list with
// CLI-overrides-config precedence.
//
// Behavior preserves the pre-config semantics of `aicr snapshot`:
//   - CLI flag set: parse the CLI value (empty input → DefaultTolerations).
//   - CLI unset, config set: use the config value (a non-nil empty slice
//     in config means "operator opted out of the tolerate-all default").
//   - CLI unset, config unset: fall through to DefaultTolerations()
//     (the legacy snapshot behavior — without it, an aicr snapshot
//     invocation that does not pass --toleration would suddenly stop
//     tolerating tainted nodes).
func resolveSnapshotTolerations(cmd *cli.Command, resolved *config.SnapshotResolved) ([]corev1.Toleration, error) {
	if cmd.IsSet("toleration") {
		tols, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", err)
		}
		if resolved.Tolerations != nil {
			slog.Info("CLI flag overriding config value", "flag", "toleration",
				"config", resolved.Tolerations, "override", tols)
		}
		return tols, nil
	}
	if resolved.Tolerations != nil {
		return resolved.Tolerations, nil
	}
	return snapshotter.DefaultTolerations(), nil
}

// snapshotCmdOptions holds every option resolved by parseSnapshotCmdOptions,
// in the form used by both the Action's deploy path and tests that want to
// assert on the merged CLI-overrides-config result. Resource lists and
// tolerations are typed so callers do not re-parse them.
type snapshotCmdOptions struct {
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
	runtimeClass       string
	os                 string
	maxNodesPerEntry   int
	requests           corev1.ResourceList
	limits             corev1.ResourceList
	tmplOpts           *snapshotTemplateOptions
}

// toAgentConfig converts the resolved options into the snapshotter.AgentConfig
// the snapshotter deploy path expects.
func (o *snapshotCmdOptions) toAgentConfig() *snapshotter.AgentConfig {
	return &snapshotter.AgentConfig{
		Kubeconfig:         o.kubeconfig,
		Namespace:          o.namespace,
		Image:              o.image,
		ImagePullSecrets:   o.imagePullSecrets,
		JobName:            o.jobName,
		ServiceAccountName: o.serviceAccountName,
		NodeSelector:       o.nodeSelector,
		Tolerations:        o.tolerations,
		Timeout:            o.timeout,
		Cleanup:            o.cleanup,
		Output:             o.tmplOpts.outputPath,
		Debug:              o.debug,
		Privileged:         o.privileged,
		RequireGPU:         o.requireGPU,
		RuntimeClassName:   o.runtimeClass,
		TemplatePath:       o.tmplOpts.templatePath,
		MaxNodesPerEntry:   o.maxNodesPerEntry,
		OS:                 o.os,
		Requests:           o.requests,
		Limits:             o.limits,
	}
}

// parseSnapshotCmdOptions resolves snapshot command inputs by merging CLI
// flags with the optional --config (AICRConfig) source. CLI flags always win
// over config values. Returns a fully-typed snapshotCmdOptions that callers
// can pass to the snapshotter without further parsing.
func parseSnapshotCmdOptions(cmd *cli.Command, cfg *config.AICRConfig) (*snapshotCmdOptions, error) {
	if err := validateSingleValueFlags(cmd, "namespace", "image", "job-name", "service-account-name", "timeout", "template", "max-nodes-per-entry", "runtime-class", "output", "format", "config", "os", "requests", "limits"); err != nil {
		return nil, err
	}

	resolved, err := cfg.Snapshot().Resolve()
	if err != nil {
		return nil, err
	}

	// Normalize/validate the --os value via the recipe parser so that
	// only documented OS criteria values reach the agent and the
	// in-pod collector factory.
	osVal := stringFlagOrConfig(cmd, "os", resolved.OS)
	if osVal != "" {
		parsedOS, parseErr := recipe.NewCriteriaRegistry().ParseOS(osVal)
		if parseErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --os value", parseErr)
		}
		osVal = string(parsedOS)
	}

	requireGPU := boolFlagOrConfig(cmd, "require-gpu", resolved.RequireGPU)
	runtimeClass := stringFlagOrConfig(cmd, "runtime-class", resolved.RuntimeClassName)

	// Mutual exclusion: --require-gpu and --runtime-class cannot be used together
	if requireGPU && runtimeClass != "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"--require-gpu and --runtime-class are mutually exclusive; "+
				"prefer --runtime-class, which provides nvidia-smi access via the container runtime without consuming a GPU allocation")
	}

	// Parse output format. The config-provided format only kicks in
	// when the CLI flag is not explicitly set; otherwise the CLI
	// value wins. Validation of unknown formats happens here.
	if !cmd.IsSet("format") && resolved.OutputFormat != "" {
		if setErr := cmd.Set("format", resolved.OutputFormat); setErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid spec.snapshot.output.format", setErr)
		}
	}
	outFormat, err := parseOutputFormat(cmd)
	if err != nil {
		return nil, err
	}

	tmplOpts, err := parseSnapshotTemplateOptions(cmd, outFormat, resolved)
	if err != nil {
		return nil, err
	}

	nodeSelector, err := resolveSnapshotNodeSelector(cmd, resolved)
	if err != nil {
		return nil, err
	}
	tolerations, err := resolveSnapshotTolerations(cmd, resolved)
	if err != nil {
		return nil, err
	}

	resourceRequests, err := snapshotter.ParseResourceList(stringFlagOrConfig(cmd, "requests", resolved.Requests))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid --requests")
	}
	resourceLimits, err := snapshotter.ParseResourceList(stringFlagOrConfig(cmd, "limits", resolved.Limits))
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "invalid --limits")
	}

	return &snapshotCmdOptions{
		kubeconfig:         cmd.String("kubeconfig"),
		namespace:          stringFlagOrConfig(cmd, "namespace", resolved.Namespace),
		image:              stringFlagOrConfig(cmd, "image", resolved.Image),
		imagePullSecrets:   stringSliceFlagOrConfig(cmd, "image-pull-secret", resolved.ImagePullSecrets),
		jobName:            stringFlagOrConfig(cmd, "job-name", resolved.JobName),
		serviceAccountName: stringFlagOrConfig(cmd, "service-account-name", resolved.ServiceAccountName),
		nodeSelector:       nodeSelector,
		tolerations:        tolerations,
		timeout:            durationFlagOrConfig(cmd, "timeout", resolved.Timeout),
		cleanup:            !boolFlagOrConfig(cmd, "no-cleanup", resolved.NoCleanup),
		debug:              cmd.Bool("debug"),
		privileged:         boolFlagOrConfig(cmd, "privileged", derefBoolOr(resolved.Privileged, true)),
		requireGPU:         requireGPU,
		runtimeClass:       runtimeClass,
		os:                 osVal,
		maxNodesPerEntry:   intFlagOrConfig(cmd, "max-nodes-per-entry", resolved.MaxNodesPerEntry),
		requests:           resourceRequests,
		limits:             resourceLimits,
		tmplOpts:           tmplOpts,
	}, nil
}

// snapshotTemplateOptions holds parsed template options for the snapshot command.
type snapshotTemplateOptions struct {
	templatePath string
	outputPath   string
	format       serializer.Format
}

func parseSnapshotTemplateOptions(cmd *cli.Command, outFormat serializer.Format, resolved *config.SnapshotResolved) (*snapshotTemplateOptions, error) {
	templatePath := stringFlagOrConfig(cmd, "template", resolved.OutputTemplate)
	outputPath := stringFlagOrConfig(cmd, "output", resolved.OutputPath)

	if templatePath != "" {
		// Validate format is YAML when using template
		if cmd.IsSet("format") && outFormat != serializer.FormatYAML {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--template requires YAML format; --format must be \"yaml\" or omitted")
		}

		// Templates only emit local files; a ConfigMap URI here would be
		// taken literally as a filename and silently create a file named
		// "cm:..." instead of writing to Kubernetes.
		if strings.HasPrefix(strings.TrimSpace(outputPath), serializer.ConfigMapURIScheme) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--template does not support ConfigMap output (cm://...); render to a file or stdout instead")
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
// kubeconfig is threaded through so ConfigMap destinations (cm://...) write to
// the same cluster the rest of the snapshot pipeline is configured against.
func createSnapshotSerializer(tmplOpts *snapshotTemplateOptions, kubeconfig string) (serializer.Serializer, error) {
	if tmplOpts.templatePath != "" {
		return serializer.NewTemplateFileWriter(tmplOpts.templatePath, tmplOpts.outputPath)
	}
	return serializer.NewFileWriterOrStdoutWithKubeconfig(tmplOpts.format, tmplOpts.outputPath, kubeconfig)
}

func snapshotCmdFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:     "namespace",
			Aliases:  []string{"n"},
			Usage:    "Kubernetes namespace for agent deployment",
			Sources:  cli.EnvVars("AICR_NAMESPACE"),
			Value:    "default",
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "image",
			Usage:    "Container image for agent Job",
			Sources:  cli.EnvVars("AICR_IMAGE"),
			Value:    defaultAgentImage(),
			Category: catAgentDeployment,
		},
		&cli.StringSliceFlag{
			Name:     "image-pull-secret",
			Usage:    "Secret name for pulling images from private registries (can be repeated)",
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "job-name",
			Usage:    "Override default Job name",
			Value:    name,
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "service-account-name",
			Usage:    "Override default ServiceAccount name",
			Value:    name,
			Category: catAgentDeployment,
		},
		&cli.StringSliceFlag{
			Name:     "node-selector",
			Usage:    "Node selector for Job scheduling (format: key=value, can be repeated). Recommended in heterogeneous clusters to target GPU nodes",
			Category: catAgentDeployment,
		},
		&cli.StringSliceFlag{
			Name:     "toleration",
			Usage:    "Toleration for Job scheduling (format: key=value:effect). By default, all taints are tolerated. Specifying this flag overrides the defaults.",
			Category: catAgentDeployment,
		},
		&cli.DurationFlag{
			Name:     "timeout",
			Usage:    "Timeout for waiting for Job completion",
			Value:    defaults.CLISnapshotTimeout,
			Category: catAgentDeployment,
		},
		&cli.BoolFlag{
			Name:     "no-cleanup",
			Usage:    "Skip removal of Job and RBAC resources on completion (leaves cluster-admin binding active)",
			Category: catAgentDeployment,
		},
		&cli.BoolFlag{
			Name:     "privileged",
			Value:    true,
			Usage:    "Run agent in privileged mode (required for GPU/SystemD collectors). Set to false for PSS-restricted namespaces.",
			Category: catAgentDeployment,
		},
		&cli.BoolFlag{
			Name:     "require-gpu",
			Sources:  cli.EnvVars("AICR_REQUIRE_GPU"),
			Usage:    "Require GPU detection. Fails the snapshot if no GPU is found. In agent mode, also requests nvidia.com/gpu resource for the pod (required in CDI environments).",
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "runtime-class",
			Sources:  cli.EnvVars("AICR_RUNTIME_CLASS"),
			Usage:    "Set runtimeClassName on the agent pod for nvidia-smi access without consuming a GPU. Use with --node-selector to target GPU nodes.",
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "template",
			Usage:    "Path to Go template file for custom output formatting (requires YAML format)",
			Category: catOutput,
		},
		&cli.IntFlag{
			Name:     "max-nodes-per-entry",
			Usage:    "Maximum node names per taint/label entry in topology collection (0 = unlimited)",
			Value:    0,
			Category: catOutput,
		},
		&cli.StringFlag{
			Name:     "os",
			Usage:    "Node OS family (ubuntu, rhel, cos, amazonlinux, talos). Selects the per-OS pod configuration and service collector backend. Talos skips systemd hostPath mounts and uses the Kubernetes-API service backend.",
			Sources:  cli.EnvVars("AICR_OS"),
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "requests",
			Usage:    "Override agent container resource requests as a comma-separated list of name=quantity pairs (e.g. 'cpu=500m,memory=1Gi,ephemeral-storage=1Gi'). Unspecified resources keep their built-in defaults.",
			Sources:  cli.EnvVars("AICR_REQUESTS"),
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "limits",
			Usage:    "Override agent container resource limits as a comma-separated list of name=quantity pairs (e.g. 'cpu=1,memory=2Gi,ephemeral-storage=2Gi'). Unspecified resources keep their built-in defaults. With --require-gpu, the default nvidia.com/gpu=1 is applied only when --limits does not already contain that key; an explicit --limits nvidia.com/gpu=N wins.",
			Sources:  cli.EnvVars("AICR_LIMITS"),
			Category: catAgentDeployment,
		},
		outputFlag(),
		formatFlag(),
		configFlag(),
		kubeconfigFlag(),
	}
}

func snapshotCmd() *cli.Command {
	return &cli.Command{
		Name:     cmdNameSnapshot,
		Category: functionalCategoryName,
		Usage:    "Capture cluster configuration snapshot.",
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

CDI environment where all GPUs are allocated (use runtime class instead of requesting a GPU):
  aicr snapshot \
    --runtime-class nvidia \
    --node-selector nvidia.com/gpu.present=true \
    --output snapshot.yaml

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
			cfg, err := loadCmdConfig(ctx, cmd)
			if err != nil {
				return err
			}
			opts, err := parseSnapshotCmdOptions(cmd, cfg)
			if err != nil {
				return err
			}

			// Create factory
			factory := collector.NewDefaultFactory(
				collector.WithMaxNodesPerEntry(opts.maxNodesPerEntry),
				collector.WithOS(opts.os),
			)

			// Create output serializer
			ser, err := createSnapshotSerializer(opts.tmplOpts, opts.kubeconfig)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "failed to create output serializer", err)
			}
			if c, ok := ser.(serializer.Closer); ok {
				defer func() {
					if closeErr := c.Close(); closeErr != nil {
						slog.Warn("failed to close snapshot serializer", "error", closeErr)
					}
				}()
			}

			// Build snapshotter configuration
			ns := snapshotter.NodeSnapshotter{
				Version:    version,
				Factory:    factory,
				Serializer: ser,
				RequireGPU: opts.requireGPU,
			}

			// When running inside an agent Job, collect locally instead of
			// deploying another agent (prevents infinite nesting).
			// Clear pre-created factory so measure() rebuilds it from env vars
			// (AICR_MAX_NODES_PER_ENTRY).
			if os.Getenv("AICR_AGENT_MODE") == "true" {
				ns.Factory = nil
				return ns.Measure(ctx)
			}

			ns.AgentConfig = opts.toAgentConfig()
			return ns.Measure(ctx)
		},
	}
}
