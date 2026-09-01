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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/collector"
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
func resolveSnapshotNodeSelector(cmd *cli.Command, agent *aicr.AgentConfig) (map[string]string, error) {
	if cmd.IsSet("node-selector") {
		ns, err := snapshotter.ParseNodeSelectors(cmd.StringSlice("node-selector"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid node-selector", err)
		}
		if agent.NodeSelector != nil {
			slog.Info("CLI flag overriding config value", "flag", "node-selector",
				"config", agent.NodeSelector, "override", ns)
		}
		return ns, nil
	}
	return agent.NodeSelector, nil
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
func resolveSnapshotTolerations(cmd *cli.Command, agent *aicr.AgentConfig) ([]corev1.Toleration, error) {
	if cmd.IsSet("toleration") {
		tols, err := snapshotter.ParseTolerations(cmd.StringSlice("toleration"))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid toleration", err)
		}
		if agent.Tolerations != nil {
			slog.Info("CLI flag overriding config value", "flag", "toleration",
				"config", agent.Tolerations, "override", tols)
		}
		return tols, nil
	}
	if agent.Tolerations != nil {
		return agent.Tolerations, nil
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
	// clusterConfigPath, when non-empty, asks the network collector to
	// ingest a pre-existing l8k cluster-config.yaml. Local-mode only in
	// this iteration — Job-mode would need ConfigMap mounting.
	clusterConfigPath string
	// aksGPUPoolsPath, when non-empty, projects an AKS GPU pool dump
	// into the aks-gpu-pools subtype at the snapshot orchestration
	// layer. Works in both agent Job mode (controller-side merge) and
	// local mode; the file never enters the pod.
	aksGPUPoolsPath string

	// okeAddonsPath, when non-empty, projects an OKE cluster add-ons
	// dump into the oke-addons subtype at the snapshot orchestration
	// layer — same contract as aksGPUPoolsPath.
	okeAddonsPath string
	// discoverNetwork enables the network collector's live-discovery
	// path. The collector calls l8k.Discover against the resolved
	// kubeconfig; discovery is NOT read-only.
	discoverNetwork bool
	requests        corev1.ResourceList
	limits          corev1.ResourceList
	tmplOpts        *snapshotTemplateOptions
}

// toAgentConfig converts the resolved options into the facade AgentConfig that
// Client.CollectSnapshot consumes. The facade type — not
// snapshotter.AgentConfig — is the single spelling the CLI builds, so the
// facade/internal mirror is exercised on every snapshot run instead of only
// by SDK consumers. Job mode is its only consumer: local (in-pod) collection
// deploys no Job and builds a collector.Factory and serializer.Serializer from
// opts directly, so it never needs an AgentConfig.
//
// NameBase carries the "aicr" prefix that --job-name and
// --service-account-name declare as their default. Routing it through this
// field rather than through JobName/ServiceAccountName is what lets an unset
// flag stay empty (see explicitStringFlagOrConfig) while the deployed objects
// keep the names the released defaults produced.
func (o *snapshotCmdOptions) toAgentConfig() *aicr.AgentConfig {
	return &aicr.AgentConfig{
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
		ClusterConfigPath:  o.clusterConfigPath,
		AKSGPUPoolsPath:    o.aksGPUPoolsPath,
		OKEAddonsPath:      o.okeAddonsPath,
		DiscoverNetwork:    o.discoverNetwork,
		Requests:           o.requests,
		Limits:             o.limits,
		NameBase:           name,
	}
}

// toSnapshotDelivery projects the resolved output options onto the delivery
// descriptor that Job mode writes through. It exists as a named projection —
// like toAgentConfig — because delivery is where the user's --format is
// applied: the agent Job always stages YAML in a ConfigMap, so a format
// dropped here is a format silently ignored (issue #2398).
func (o *snapshotCmdOptions) toSnapshotDelivery() snapshotter.SnapshotDelivery {
	return snapshotter.SnapshotDelivery{
		Output:       o.tmplOpts.outputPath,
		TemplatePath: o.tmplOpts.templatePath,
		Kubeconfig:   o.kubeconfig,
		Format:       o.tmplOpts.format,
	}
}

// parseSnapshotCmdOptions resolves snapshot command inputs by merging CLI
// flags with the optional --config (AICRConfig) source. CLI flags always win
// over config values. Returns a fully-typed snapshotCmdOptions that callers
// can pass to the snapshotter without further parsing.
func parseSnapshotCmdOptions(cmd *cli.Command, cfg *aicr.Config) (*snapshotCmdOptions, error) {
	if err := validateSingleValueFlags(cmd, "namespace", "image", "job-name", "service-account-name", flagAddRolesToSA, "timeout", "template", "max-nodes-per-entry", "runtime-class", "output", "format", "config", "os", "requests", "limits", "cluster-config", "aks-gpu-pools", "oke-addons"); err != nil {
		return nil, err
	}

	agent, present, err := cfg.SnapshotAgentConfig()
	if err != nil {
		return nil, err
	}
	out, err := cfg.SnapshotOutputOptions()
	if err != nil {
		return nil, err
	}

	// SnapshotAgentConfig returns a bare zero-value AgentConfig — Cleanup and
	// Privileged both false — when spec.snapshot is absent entirely (no
	// --config, or a document that never mentions the section), because a
	// document that made no snapshot decisions gets none invented for it (see
	// the method's own godoc, and the "present" bool it returns for exactly
	// this). That zero value is "not a working configuration": Cleanup=false
	// and Privileged=false are the opposite of the CLI's own flag defaults
	// (clean up; run privileged). When the section IS present, the derivation
	// already applies those defaults (NoCleanup inverted, Privileged defaulted
	// true), so agent.Cleanup and agent.Privileged are trustworthy fallbacks in
	// that case.
	cleanupFallback := agent.Cleanup
	privilegedFallback := agent.Privileged
	if !present {
		cleanupFallback = true
		privilegedFallback = true
	}

	// Normalize/validate the --os value via the recipe parser so that
	// only documented OS criteria values reach the agent and the
	// in-pod collector factory.
	osVal := stringFlagOrConfig(cmd, "os", agent.OS)
	if osVal != "" {
		parsedOS, parseErr := recipe.NewCriteriaRegistry().ParseOS(osVal)
		if parseErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid --os value", parseErr)
		}
		osVal = string(parsedOS)
	}

	requireGPU := boolFlagOrConfig(cmd, "require-gpu", agent.RequireGPU)
	runtimeClass := stringFlagOrConfig(cmd, "runtime-class", agent.RuntimeClassName)

	// Mutual exclusion: --require-gpu and --runtime-class cannot be used together
	if requireGPU && runtimeClass != "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"--require-gpu and --runtime-class are mutually exclusive; "+
				"prefer --runtime-class, which provides nvidia-smi access via the container runtime without consuming a GPU allocation")
	}

	// Parse output format. The config-provided format only kicks in
	// when the CLI flag is not explicitly set; otherwise the CLI
	// value wins. Validation of unknown formats happens here.
	if !cmd.IsSet("format") && out.Format != "" {
		if setErr := cmd.Set("format", out.Format); setErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid spec.snapshot.output.format", setErr)
		}
	}
	outFormat, err := parseOutputFormat(cmd)
	if err != nil {
		return nil, err
	}

	tmplOpts, err := parseSnapshotTemplateOptions(cmd, outFormat, out)
	if err != nil {
		return nil, err
	}

	nodeSelector, err := resolveSnapshotNodeSelector(cmd, agent)
	if err != nil {
		return nil, err
	}
	tolerations, err := resolveSnapshotTolerations(cmd, agent)
	if err != nil {
		return nil, err
	}

	// agent.Requests/Limits are already parsed corev1.ResourceList (the
	// facade derivation parses spec.snapshot.agent.requests/limits itself),
	// so a CLI-set flag is parsed here and the two are merged post-parse
	// rather than merging the raw strings and parsing once.
	resourceRequests := agent.Requests
	if cmd.IsSet("requests") {
		v, perr := snapshotter.ParseResourceList(cmd.String("requests"))
		if perr != nil {
			return nil, errors.PropagateOrWrap(perr, errors.ErrCodeInvalidRequest, "invalid --requests")
		}
		if len(agent.Requests) > 0 {
			slog.Info("CLI flag overriding config value", "flag", "requests", "config", agent.Requests, "override", v)
		}
		resourceRequests = v
	}
	resourceLimits := agent.Limits
	if cmd.IsSet("limits") {
		v, perr := snapshotter.ParseResourceList(cmd.String("limits"))
		if perr != nil {
			return nil, errors.PropagateOrWrap(perr, errors.ErrCodeInvalidRequest, "invalid --limits")
		}
		if len(agent.Limits) > 0 {
			slog.Info("CLI flag overriding config value", "flag", "limits", "config", agent.Limits, "override", v)
		}
		resourceLimits = v
	}

	// Timeout's config-set/unset distinction lives in *time.Duration
	// upstream, but AgentConfig.Timeout is a plain time.Duration (zero for
	// both "unset" and an explicit 0). Treat zero as "config did not set
	// it" so an absent config still yields the flag's own default.
	var timeoutFallback *time.Duration
	if agent.Timeout != 0 {
		timeoutFallback = &agent.Timeout
	}

	// jobName and serviceAccountName resolve through
	// explicitStringFlagOrConfig, not stringFlagOrConfig: the flags keep
	// their released "aicr" default for --help, but only a name the operator
	// actually supplied (flag or --config) may travel downstream. See
	// toAgentConfig's NameBase for what supplies the prefix instead.
	return &snapshotCmdOptions{
		kubeconfig:         cmd.String("kubeconfig"),
		namespace:          stringFlagOrConfig(cmd, "namespace", agent.Namespace),
		image:              stringFlagOrConfig(cmd, "image", agent.Image),
		imagePullSecrets:   stringSliceFlagOrConfig(cmd, "image-pull-secret", agent.ImagePullSecrets),
		jobName:            explicitStringFlagOrConfig(cmd, "job-name", agent.JobName),
		serviceAccountName: explicitStringFlagOrConfig(cmd, "service-account-name", agent.ServiceAccountName),
		nodeSelector:       nodeSelector,
		tolerations:        tolerations,
		timeout:            durationFlagOrConfig(cmd, "timeout", timeoutFallback),
		// cleanupFallback is already the inverted spec.snapshot.execution.noCleanup;
		// invert it back to merge with --no-cleanup, then invert once more.
		cleanup:           !boolFlagOrConfig(cmd, "no-cleanup", !cleanupFallback),
		debug:             cmd.Bool("debug"),
		privileged:        boolFlagOrConfig(cmd, "privileged", privilegedFallback),
		requireGPU:        requireGPU,
		runtimeClass:      runtimeClass,
		os:                osVal,
		maxNodesPerEntry:  intFlagOrConfig(cmd, "max-nodes-per-entry", agent.MaxNodesPerEntry),
		clusterConfigPath: cmd.String("cluster-config"),
		aksGPUPoolsPath:   cmd.String("aks-gpu-pools"),
		okeAddonsPath:     cmd.String("oke-addons"),
		discoverNetwork:   cmd.Bool("discover-network"),
		requests:          resourceRequests,
		limits:            resourceLimits,
		tmplOpts:          tmplOpts,
	}, nil
}

// runWriteRoleManifests handles the generate-and-exit invocation
// `aicr snapshot --add-roles-to-service-account <name>`: it writes the RBAC
// manifests that would grant the agent's permissions to that ServiceAccount
// and returns, applying nothing, contacting no cluster, and capturing
// nothing.
//
// It takes no context because there is no I/O to bound beyond writing four
// local files.
//
// It is a thin adapter — name derivation, the rule sets, the explanatory
// headers, and the directory layout all live in pkg/snapshotter and
// pkg/k8s/agent. What belongs here is only presenting the outcome, including
// the two things an operator must not have to discover on their own: nothing
// is live yet, and the exact commands that make it live and take it away
// again.
func runWriteRoleManifests(cmd *cli.Command, opts *snapshotCmdOptions, saName string) error {
	res, err := snapshotter.WriteAgentRoleManifests(&snapshotter.AgentRolesConfig{
		Namespace:          opts.namespace,
		ServiceAccountName: saName,
		DiscoverNetwork:    opts.discoverNetwork,
	})
	if err != nil {
		return err
	}

	writeManifestReport(cmd.Root().Writer, res)
	return nil
}

// writeManifestReport renders the outcome of a manifest-generating run. It is
// split out from runWriteRoleManifests so the properties an operator must not
// have to discover on their own — nothing was applied, how to apply it, how to
// remove it again, and that a shared ServiceAccount waives per-run permission
// isolation — are assertable without a cluster or a filesystem.
func writeManifestReport(w io.Writer, res *snapshotter.AgentRolesResult) {
	fmt.Fprintf(w, `Wrote the snapshot agent's RBAC manifests for ServiceAccount %[1]q in namespace
%[2]q to:

  %[3]s/

NOTHING WAS APPLIED. No cluster was contacted, and %[1]s has no new permissions
yet.

`, res.ServiceAccountName, res.Namespace, res.Dir)

	for _, obj := range res.Objects {
		fmt.Fprintf(w, "  %-28s %s/%s\n", filepath.Base(obj.Path), strings.ToLower(obj.Kind), obj.Name)
	}

	fmt.Fprintf(w, `
Read them — each file explains what it grants and why the agent needs it — then
apply them yourself:
  kubectl apply -f %[1]s/

Capture a snapshot as this ServiceAccount with:
  aicr snapshot --namespace %[2]s --service-account-name %[3]s

Remove the grant when the ServiceAccount no longer needs it:
  kubectl delete -f %[1]s/

That delete is the only teardown: no aicr run creates, refreshes, or deletes
these objects. Keep the directory for as long as you want the easy teardown.

The ServiceAccount is not verified to exist — aicr contacted no cluster. Check
it before you rely on the grant:
  kubectl get serviceaccount %[3]s -n %[2]s

Trade-off: runs that share this ServiceAccount share its permissions, so per-run
permission isolation is waived for them.
`, res.Dir, res.Namespace, res.ServiceAccountName)

	if !res.DiscoverNetwork {
		return
	}
	fmt.Fprintf(w, `
WARNING: --discover-network means the ClusterRole in this directory also carries
cluster-scoped MUTATING rules (nodes: patch, pods/exec: create, CRD/namespace/
DaemonSet create-delete). Applying it grants them permanently, not for one run.
Each rule and the discovery step it exists for is explained in %s.
`, clusterRoleManifestName(res))
}

// clusterRoleManifestName returns the file name of the rendered ClusterRole,
// looked up by kind rather than by position so the discovery warning keeps
// pointing at the right file if the manifest order ever changes.
func clusterRoleManifestName(res *snapshotter.AgentRolesResult) string {
	for _, obj := range res.Objects {
		if obj.Kind == "ClusterRole" {
			return filepath.Base(obj.Path)
		}
	}
	return "the ClusterRole manifest"
}

// snapshotTemplateOptions holds parsed template options for the snapshot command.
type snapshotTemplateOptions struct {
	templatePath string
	outputPath   string
	format       serializer.Format
}

func parseSnapshotTemplateOptions(cmd *cli.Command, outFormat serializer.Format, out aicr.SnapshotOutputOptions) (*snapshotTemplateOptions, error) {
	templatePath := stringFlagOrConfig(cmd, "template", out.Template)
	outputPath := stringFlagOrConfig(cmd, "output", out.Path)

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
		// Value stays "aicr" on both name flags: it is the released v1
		// default `--help` prints and testdata/cli-surface.golden pins.
		// parseSnapshotCmdOptions reads them with
		// explicitStringFlagOrConfig, so an unset flag reaches the deployer
		// as "" rather than as this default — see that helper for why the
		// two must not be conflated.
		&cli.StringFlag{
			Name:     "job-name",
			Usage:    "Job name prefix; the run ID is always appended",
			Value:    name,
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "service-account-name",
			Usage:    "ServiceAccount to run the agent as. Exact-if-exists: when a ServiceAccount of exactly this name already exists in --namespace it is used verbatim and aicr creates and deletes no RBAC for the run (generate its RBAC manifests with --add-roles-to-service-account, then apply them yourself). Otherwise it is a name prefix and the run ID is appended. Leaving the flag unset is not the same as passing the default shown: an unset value is never probed for existence, so a stray ServiceAccount cannot capture the run.",
			Value:    name,
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     flagAddRolesToSA,
			Usage:    "WRITES MANIFESTS AND APPLIES NOTHING. Renders the Role, RoleBinding, ClusterRole and ClusterRoleBinding that grant the agent's permissions to the named ServiceAccount into ./snapshot-rbac-<run-id>/, then exits without taking a snapshot. No cluster is contacted, so no kubeconfig or privileges are needed. Review the files, then grant with 'kubectl apply -f <dir>/' and revoke with 'kubectl delete -f <dir>/' yourself. Add --discover-network to include the mutating live-discovery rules.",
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
			Usage:    "Node OS family (ubuntu, rhel, cos, amazonlinux, ol, talos). Selects the per-OS pod configuration and service collector backend. Talos skips systemd hostPath mounts and uses the Kubernetes-API service backend.",
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
		&cli.StringFlag{
			Name:     "cluster-config",
			Usage:    "Path to a pre-existing k8s-launch-kit (l8k) cluster-config.yaml. Ingests the file's network topology into the snapshot as a NetworkTopology Measurement. Local agent mode only (AICR_AGENT_MODE=true) — Job mode rejects this flag with INVALID_REQUEST until ConfigMap mounting is implemented; use --discover-network for live cluster discovery in Job mode. Mutually exclusive with --discover-network at the collector level — file path wins when both are set.",
			Sources:  cli.EnvVars("AICR_CLUSTER_CONFIG_PATH"),
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "oke-addons",
			Usage:    "Path to an `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump on the local filesystem. Projects the NvidiaGpuPlugin add-on's control-plane state into the K8s oke-addons subtype (installed/absent; any other lifecycle state projects a value no profile constraint accepts). The --all flag on the oci command is required: without it removed and non-ACTIVE add-ons are omitted from the dump and would read as absent instead of failing closed. The projection runs controller-side in both agent Job mode (merged into the returned snapshot) and local mode, and a bad file fails the snapshot before any cluster work.",
			Sources:  cli.EnvVars("AICR_OKE_ADDONS_PATH"),
			Category: catAgentDeployment,
		},
		&cli.StringFlag{
			Name:     "aks-gpu-pools",
			Usage:    "Path to an `az aks nodepool list -o json` dump on the local filesystem. Projects each GPU agent pool's gpuProfile.driver into the K8s aks-gpu-pools subtype (Install/None; mixed or AKS-managed pools project a value no profile constraint accepts, ADR-015 DD3). The projection runs controller-side in both agent Job mode (merged into the returned snapshot) and local mode, and a bad file fails the snapshot before any cluster work.",
			Sources:  cli.EnvVars("AICR_AKS_GPU_POOLS_PATH"),
			Category: catAgentDeployment,
		},
		&cli.BoolFlag{
			Name:     "discover-network",
			Usage:    "Enable live l8k discovery to populate the NetworkTopology Measurement. NOT read-only — discovery writes nvidia.kubernetes-launch-kit.* node labels and may patch NicClusterPolicy via server-side-apply.",
			Sources:  cli.EnvVars("AICR_DISCOVER_NETWORK"),
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
			cfg, err := loadFacadeConfig(ctx, cmd)
			if err != nil {
				return err
			}
			opts, err := parseSnapshotCmdOptions(cmd, cfg)
			if err != nil {
				return err
			}

			// Generate-and-exit: --add-roles-to-service-account writes the
			// RBAC manifests for an existing ServiceAccount, applies nothing,
			// and takes no snapshot. Checked ahead of every collection path
			// (including the in-pod one below) so the flag can never be
			// combined with a capture.
			if saName := cmd.String(flagAddRolesToSA); saName != "" {
				return runWriteRoleManifests(cmd, opts, saName)
			}

			agentCfg := opts.toAgentConfig()

			// When running inside an agent Job, collect locally instead of
			// deploying another agent (prevents infinite nesting). This path
			// deploys nothing, so it stays on pkg/snapshotter directly — it
			// needs a collector.Factory and a serializer.Serializer, which the
			// semver-stable facade deliberately does not expose (see the
			// Client.CollectSnapshot godoc).
			//
			// The factory carries every CLI-resolved option —
			// opts.clusterConfigPath, opts.discoverNetwork, opts.kubeconfig,
			// opts.os, opts.maxNodesPerEntry. The flags' cli.EnvVars Sources
			// have already populated opts from the Job-set env vars before we
			// reach this point, which keeps the dev-bypass case
			// (`AICR_AGENT_MODE=true aicr snapshot --cluster-config <path>`)
			// consistent with the in-pod path.
			//
			// Both the factory and the serializer are built HERE rather than
			// above the branch: createSnapshotSerializer opens the destination
			// with os.Create, which truncates it. Building it on the Job path —
			// which never uses it, delivering raw agent bytes instead — would
			// erase an existing snapshot file before the Job had produced a
			// replacement, so a failed collection would destroy the previous
			// capture. Job-mode destinations are instead validated without
			// side effects: parseSnapshotCmdOptions checks the template, and
			// DeployAndCollect parses a cm:// URI before touching the cluster.
			if os.Getenv("AICR_AGENT_MODE") == "true" {
				factory := collector.NewDefaultFactory(
					collector.WithMaxNodesPerEntry(opts.maxNodesPerEntry),
					collector.WithOS(opts.os),
					collector.WithClusterConfigPath(opts.clusterConfigPath),
					collector.WithDiscoverNetwork(opts.discoverNetwork),
					collector.WithKubeconfigPath(opts.kubeconfig),
				)

				ser, serErr := createSnapshotSerializer(opts.tmplOpts, opts.kubeconfig)
				if serErr != nil {
					return errors.Wrap(errors.ErrCodeInternal, "failed to create output serializer", serErr)
				}
				if c, ok := ser.(serializer.Closer); ok {
					defer func() {
						if closeErr := c.Close(); closeErr != nil {
							slog.Warn("failed to close snapshot serializer", "error", closeErr)
						}
					}()
				}

				ns := snapshotter.NodeSnapshotter{
					Version:         version,
					Factory:         factory,
					Serializer:      ser,
					RequireGPU:      opts.requireGPU,
					AKSGPUPoolsPath: opts.aksGPUPoolsPath,
					OKEAddonsPath:   opts.okeAddonsPath,
				}
				return ns.Measure(ctx)
			}

			// Job mode goes through the facade so `aicr snapshot`, `aicr
			// validate`, and SDK consumers all deploy the agent the same way.
			// The recipe source is irrelevant here — CollectSnapshot does not
			// consult the Client's DataProvider — so the embedded source keeps
			// construction free of cluster- or flag-dependent setup.
			client, err := aicr.NewClientContext(
				ctx, aicr.WithRecipeSource(aicr.EmbeddedSource()))
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			snap, err := client.CollectSnapshot(ctx, agentCfg)
			if err != nil {
				return err
			}

			// Deliver the RAW agent bytes, never a re-serialization of the
			// parsed snapshot: a newer agent image can emit fields this
			// binary's Snapshot type does not model, and a typed round trip
			// would silently drop them. YAML delivery is a byte copy for
			// that reason; json and table necessarily re-render.
			return snapshotter.DeliverSnapshot(ctx, snap.Raw, opts.toSnapshotDelivery())
		},
	}
}
