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

package snapshotter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	k8scollector "github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/runid"
	"github.com/NVIDIA/aicr/pkg/serializer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// logWriter returns an io.Writer for streaming agent logs.
// Uses stderr to avoid interfering with stdout output.
func logWriter() io.Writer {
	return os.Stderr
}

// AgentConfig contains configuration for Kubernetes agent deployment.
type AgentConfig struct {
	// Kubeconfig path (optional override)
	Kubeconfig string

	// Namespace for agent deployment
	Namespace string

	// Image for agent container
	Image string

	// ImagePullSecrets for pulling the agent image from private registries
	ImagePullSecrets []string

	// JobName for the agent Job
	JobName string

	// ServiceAccountName selects the ServiceAccount the agent pod runs
	// as. It is EXACT-IF-EXISTS, so it carries two meanings resolved once
	// per deployment:
	//
	//   - A ServiceAccount of exactly this name already exists in
	//     Namespace: it is used verbatim, and the run creates NO
	//     ServiceAccount, Role, RoleBinding, ClusterRole or
	//     ClusterRoleBinding — and deletes none at cleanup. aicr adds and
	//     removes no permissions on an identity it did not create. This
	//     is how a ServiceAccount carrying IRSA
	//     (eks.amazonaws.com/role-arn) or GKE Workload Identity
	//     (iam.gke.io/gcp-service-account) annotations stays usable: both
	//     providers pin trust to the ServiceAccount NAME, which a
	//     run-scoped name can never satisfy. Generate its RBAC manifests
	//     with WriteAgentRoleManifests, then apply them out of band.
	//   - Otherwise: a name prefix. The run creates "<prefix>-<RunID>"
	//     and the full run-scoped RBAC set, and deletes them at cleanup.
	//
	// Empty falls back to NameBase and is never probed for existence, so
	// a stray ServiceAccount sitting at the default base cannot silently
	// capture the run.
	//
	// Using an existing ServiceAccount waives per-run permission
	// isolation: concurrent runs sharing it share its grants, and grants
	// provisioned for DiscoverNetwork persist beyond any one run.
	ServiceAccountName string

	// NodeSelector for targeting specific nodes
	NodeSelector map[string]string

	// Tolerations for scheduling on tainted nodes. Nil uses
	// DefaultTolerations; a non-nil empty slice explicitly disables that default.
	Tolerations []corev1.Toleration

	// Timeout for waiting for Job completion
	Timeout time.Duration

	// Cleanup determines whether to remove Job and RBAC on completion
	Cleanup bool

	// Output destination for snapshot
	Output string

	// Debug enables debug logging
	Debug bool

	// Privileged enables privileged mode (hostPID, hostNetwork, privileged container).
	// Required for GPU and SystemD collectors. When false, only K8s and OS collectors work.
	Privileged bool

	// RequireGPU requests nvidia.com/gpu resource for the agent pod.
	// Required in CDI environments (e.g., kind with nvkind) where GPU devices
	// are only injected when explicitly requested.
	RequireGPU bool

	// RuntimeClassName sets runtimeClassName on the agent pod and injects
	// NVIDIA_VISIBLE_DEVICES=all. Use instead of RequireGPU when all GPUs
	// are allocated — gives the agent nvidia-smi access without consuming
	// a GPU from the Device Plugin.
	RuntimeClassName string

	// TemplatePath is the path to a Go template file for custom output formatting.
	// When set, the snapshot output will be processed through this template.
	TemplatePath string

	// MaxNodesPerEntry limits node names per topology entry (0 = unlimited).
	MaxNodesPerEntry int

	// OS is the recipe OS criteria value (e.g., "ubuntu", "talos"). Drives
	// per-OS pod construction and in-pod collector backend selection. When
	// empty, defaults preserve the systemd-based behavior.
	OS string

	// ClusterConfigPath, when set, asks the in-pod network collector to
	// ingest a pre-existing l8k cluster-config.yaml at this path. In
	// Job-mode the path must resolve inside the agent pod (ConfigMap
	// mount, etc.) — this iteration plumbs the field through but does
	// not yet auto-mount the file; the typical use today is local mode
	// (AICR_AGENT_MODE=true) where the file lives on the caller's host.
	ClusterConfigPath string

	// AKSGPUPoolsPath, when set, points at an operator-supplied
	// `az aks nodepool list -o json` dump on the CALLER's filesystem.
	// The projection is pure file processing, so unlike ClusterConfigPath
	// it never enters the pod: the controller-side CLI projects it before
	// deploying (fail-loud on a bad file, before any cluster work) and
	// merges the aks-gpu-pools subtype into the snapshot the Job returns.
	AKSGPUPoolsPath string

	// OKEAddonsPath, when set, points at an operator-supplied
	// `oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json` dump on the
	// CALLER's filesystem. Same contract as AKSGPUPoolsPath: projected
	// controller-side before deploying, merged into the returned
	// snapshot as the oke-addons subtype.
	OKEAddonsPath string

	// DiscoverNetwork enables the in-pod network collector's live l8k
	// discovery path. Discovery is NOT read-only — it writes node labels
	// (nvidia.kubernetes-launch-kit.*) and patches NicClusterPolicy via
	// server-side-apply. RBAC must allow those writes.
	DiscoverNetwork bool

	// Requests overrides the agent container's per-resource requests.
	// When nil, the privileged/restricted defaults baked into
	// pkg/k8s/agent are used. Useful for right-sizing the agent on
	// resource-constrained dev clusters (e.g. talosctl Docker
	// provisioner workers).
	Requests corev1.ResourceList

	// Limits overrides the agent container's per-resource limits. When
	// nil, the privileged/restricted defaults are used. RequireGPU
	// defaults nvidia.com/gpu=1 only when the caller has not supplied
	// that key in Limits — e.g. --require-gpu --limits nvidia.com/gpu=4
	// keeps 4, not 1.
	Limits corev1.ResourceList

	// RunID scopes every resource this deployment creates (Job, RBAC, and
	// the internal staging ConfigMap when Output does not name one) to a
	// single run, so concurrent snapshot-agent runs never collide on a
	// shared resource name. DeployAndCollect generates one with
	// runid.Generate() when this is empty — callers normally leave it
	// unset; setting it explicitly is for correlating this run with an
	// external identifier (e.g. sharing one ID with a downstream
	// validator run).
	//
	// DeployAndCollect never writes the generated value back here: the
	// AgentConfig belongs to the caller, and a caller reusing one config
	// pointer across two runs would otherwise silently become a caller
	// pinning a duplicate RunID — the one state ADR-020 declares
	// unsupported, which fails the second run with ErrCodeInternal on the
	// first still-existing run-scoped object.
	RunID string

	// NameBase prefixes generated resource names (Job, ServiceAccount,
	// Role/RoleBinding). It applies per name: JobName falls back to it
	// when JobName is empty, and ServiceAccountName (which also names the
	// Role and RoleBinding) falls back to it when ServiceAccountName is
	// empty — so setting only one of the two leaves NameBase governing the
	// other. Forwarded verbatim to pkg/k8s/agent.Config.NameBase, which
	// defaults to "aicr" when also empty.
	NameBase string
}

// buildAgentConfig projects snapshotter configuration onto the deployer's
// Job configuration. Keep scheduling defaults at this projection boundary so
// every snapshot-agent caller gets the same nil-versus-empty behavior.
//
// ownsOutput is agentConfigMapTarget's second return value, forwarded
// verbatim: it is true only when agentOutput is the internal staging
// ConfigMap this run owns (the caller did not name a cm:// destination), so
// Cleanup may delete it. A caller-supplied cm:// Output is never owned.
//
// runID is the resolved run ID for this invocation — config.RunID when the
// caller pinned one, otherwise the value DeployAndCollect generated. It is
// passed in rather than read from config because DeployAndCollect must not
// write the generated ID back into the caller's AgentConfig (see
// AgentConfig.RunID).
func buildAgentConfig(config *AgentConfig, runID, agentOutput string, ownsOutput bool) agent.Config {
	return agent.Config{
		Namespace:           config.Namespace,
		ServiceAccountName:  config.ServiceAccountName,
		JobName:             config.JobName,
		RunID:               runID,
		NameBase:            config.NameBase,
		Image:               config.Image,
		ImagePullSecrets:    config.ImagePullSecrets,
		NodeSelector:        config.NodeSelector,
		Tolerations:         effectiveAgentTolerations(config.Tolerations),
		Output:              agentOutput,
		Debug:               config.Debug,
		Privileged:          config.Privileged,
		RequireGPU:          config.RequireGPU,
		RuntimeClassName:    config.RuntimeClassName,
		MaxNodesPerEntry:    config.MaxNodesPerEntry,
		OS:                  config.OS,
		ClusterConfigPath:   config.ClusterConfigPath,
		DiscoverNetwork:     config.DiscoverNetwork,
		Requests:            config.Requests,
		Limits:              config.Limits,
		OwnsOutputConfigMap: ownsOutput,
	}
}

// deployAndWaitForResult handles the common deploy-wait-retrieve lifecycle for an agent Job.
// It creates the deployer, deploys RBAC and the Job, streams logs, waits for completion,
// and retrieves the snapshot data from the result ConfigMap.
//
// ownsOutput is agentConfigMapTarget's second return value: true when
// agentOutput is the internal staging ConfigMap this run owns, false when it
// is a caller-supplied cm:// destination. See buildAgentConfig and
// rewriteMergedSnapshotConfigMap for how each consumes it.
//
// runID is the resolved run ID for this invocation; see buildAgentConfig.
func deployAndWaitForResult(ctx context.Context, clientset k8sclient.Interface, config *AgentConfig, runID, agentOutput string, ownsOutput bool) ([]byte, error) {
	// The pool projection is pure file processing on the caller's host —
	// project it BEFORE deploying so a bad file fails in milliseconds,
	// not after a Job round-trip, and merge it into the returned snapshot
	// below. It deliberately does not ride the in-pod collector path.
	var projections []measurement.Subtype
	if config.AKSGPUPoolsPath != "" {
		subtype, err := k8scollector.ProjectAKSGPUPools(ctx, config.AKSGPUPoolsPath)
		if err != nil {
			return nil, err
		}
		projections = append(projections, subtype)
	}
	if config.OKEAddonsPath != "" {
		subtype, err := k8scollector.ProjectOKEAddons(ctx, config.OKEAddonsPath)
		if err != nil {
			return nil, err
		}
		projections = append(projections, subtype)
	}

	// Auto-inject GPU node selector when no placement constraints are set.
	// Returns true when injection occurred, so the wait-timeout error can
	// name the injected selector (TOCTOU: node may be cordoned after detection).
	autoInjectedGPUSelector := maybeInjectGPUNodeSelector(ctx, clientset, config)

	agentConfig := buildAgentConfig(config, runID, agentOutput, ownsOutput)

	deployer := agent.NewDeployer(clientset, agentConfig)

	//nolint:contextcheck // intentional: need fresh context for cleanup when parent is canceled
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cancel()

		cleanupOpts := agent.CleanupOptions{Enabled: config.Cleanup}
		if cleanupErr := deployer.Cleanup(cleanupCtx, cleanupOpts); cleanupErr != nil {
			slog.Warn("cleanup failed - resources may remain in cluster",
				slog.String("error", cleanupErr.Error()),
				slog.String("namespace", config.Namespace),
			)
		}
	}()

	slog.Info("deploying agent", slog.String("namespace", agentConfig.Namespace))

	if deployErr := deployer.Deploy(ctx); deployErr != nil {
		return nil, deployErr
	}

	slog.Info("agent deployed successfully")

	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaults.K8sJobCompletionTimeout
	}

	// Log the run-scoped Job name, not agentConfig.JobName: that field is
	// only the user's optional prefix and is empty by default, so logging it
	// prints job="".
	slog.Info("waiting for Job completion",
		slog.String("job", deployer.JobName()),
		slog.Duration("timeout", timeout))

	// Stream logs in background while waiting for Job completion.
	// If the pod completes before becoming "ready" (fast Jobs), log streaming
	// is skipped — WaitForCompletion will still capture the result.
	//
	// The WaitGroup ensures the goroutine has fully exited before this
	// function returns, so log writes cannot interleave with the caller's
	// output after the snapshot has been returned.
	logCtx, cancelLogs := context.WithCancel(ctx)
	var logWG sync.WaitGroup
	defer func() {
		cancelLogs()
		logWG.Wait()
	}()

	logWG.Go(func() {
		if podErr := deployer.WaitForPodReady(logCtx, defaults.K8sPodReadyTimeout); podErr != nil {
			// Only suppress logging when the parent context has been
			// canceled (expected during cleanup). Genuine failures
			// (pod stuck, image pull errors) must surface so operators
			// understand why agent logs are missing from their output.
			if logCtx.Err() == nil {
				slog.Warn("agent log streaming skipped: pod did not become ready",
					slog.String("namespace", agentConfig.Namespace),
					slog.String("job", deployer.JobName()),
					"error", podErr)
			}
			return
		}
		if streamErr := deployer.StreamLogs(logCtx, logWriter(), ""); streamErr != nil {
			if logCtx.Err() == nil {
				slog.Warn("agent log streaming ended early",
					"error", streamErr)
			}
		}
	})

	if waitErr := deployer.WaitForCompletion(ctx, timeout); waitErr != nil {
		if logs, logErr := deployer.GetPodLogs(ctx); logErr == nil && logs != "" {
			fmt.Fprintln(logWriter(), "--- agent logs ---")
			fmt.Fprintln(logWriter(), logs)
			fmt.Fprintln(logWriter(), "--- end logs ---")
		}
		isTransient := errors.IsTransient(waitErr)
		msg := "job failed"
		if autoInjectedGPUSelector {
			msg = "job failed (auto-injected node selector nvidia.com/gpu.present=true — " +
				"verify matching GPU nodes are Ready and schedulable; if tolerations were " +
				"explicitly cleared or replaced, pass a matching --toleration " +
				"key=value:effect. To override placement, pass --node-selector " +
				"kubernetes.io/hostname=<gpu-node>; --require-gpu selects a node " +
				"advertising the nvidia.com/gpu resource)"
		} else if isTransient {
			msg = "job failed (verify target nodes are Ready and schedulable; if " +
				"tolerations were explicitly cleared or replaced, pass a matching " +
				"--toleration key=value:effect)"
		}
		// A wait that exceeded the deadline (pending pod, image pull, no schedulable
		// node) is transient and retryable — classify it as ErrCodeTimeout rather
		// than masking it as a deterministic ErrCodeInternal failure.
		if isTransient {
			return nil, errors.Wrap(errors.ErrCodeTimeout, msg, waitErr)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, msg, waitErr)
	}

	slog.Info("job completed successfully")

	slog.Debug("retrieving snapshot from ConfigMap")
	snapshotData, err := deployer.GetSnapshot(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to retrieve snapshot", err)
	}

	if len(projections) > 0 {
		for _, projection := range projections {
			snapshotData, err = mergeProviderProjection(snapshotData, projection)
			if err != nil {
				return nil, err
			}
		}
		// The Job stored the PRE-merge snapshot in the result ConfigMap,
		// and Cleanup removes Job+RBAC but never that ConfigMap. Rewrite
		// it with the merged bytes so no projection-less artifact
		// persists — a later consumer of the ConfigMap must not see
		// "reading unavailable" on a cluster whose operator supplied the
		// pool file.
		//
		// The rewrite is load-bearing ONLY when the ConfigMap is the
		// delivery vehicle (the user asked for cm:// output; agentOutput
		// is that URI then) — there it fails the run. For file/stdout
		// output and SDK callers the returned bytes already carry the
		// merged reading, so a transient Apply failure on the internal
		// hygiene rewrite must not discard an already-captured snapshot;
		// warn loudly instead (the orphaned ConfigMap stays pre-merge).
		// ownsOutput is the run-owns-the-staging-ConfigMap flag, so its
		// logical inverse is "the ConfigMap is the user's delivery vehicle".
		if err := rewriteMergedSnapshotConfigMap(ctx, agentOutput, config.Kubeconfig,
			snapshotData, !ownsOutput); err != nil {
			return nil, err
		}
	}

	return snapshotData, nil
}

// mergeProviderProjection attaches a controller-side provider projection
// (aks-gpu-pools, oke-addons) to the
// snapshot the agent Job returned. The merge is performed on generic maps,
// NOT through the controller's typed Snapshot struct: the agent image is
// user-pinnable, so a newer agent may emit fields this binary's struct does
// not declare, and a typed round-trip would silently drop them from both
// the returned snapshot and the rewritten ConfigMap. The subtype cannot
// affect the cluster fingerprint (fingerprint dimensions read no
// aks-gpu-pools key), so merging after the in-pod derivation is sound.
func mergeProviderProjection(snapshotData []byte, subtype measurement.Subtype) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(snapshotData, &doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to parse agent snapshot for provider projection merge", err)
	}
	if doc == nil {
		// An empty or `null` document unmarshals to a nil map without
		// error (malfunctioning agent image); the fallback insertion
		// below must not panic on it.
		doc = make(map[string]any)
	}

	// Render the subtype through its own YAML tags so the inserted node is
	// byte-shaped exactly like a collector-emitted one.
	subtypeYAML, err := yaml.Marshal(subtype)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to serialize provider projection subtype", err)
	}
	var subtypeNode map[string]any
	if reshapeErr := yaml.Unmarshal(subtypeYAML, &subtypeNode); reshapeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to reshape provider projection subtype", reshapeErr)
	}

	measurements, _ := doc["measurements"].([]any)
	attached := false
	for _, m := range measurements {
		mm, ok := m.(map[string]any)
		if !ok || mm["type"] != string(measurement.TypeK8s) {
			continue
		}
		subtypes, _ := mm["subtypes"].([]any)
		mm["subtypes"] = append(subtypes, subtypeNode)
		attached = true
		break
	}
	if !attached {
		doc["measurements"] = append(measurements, map[string]any{
			"type":     string(measurement.TypeK8s),
			"subtypes": []any{subtypeNode},
		})
	}

	// Deterministic serializer: plain yaml.Marshal walks Go map order,
	// so identical runs would produce byte-different snapshots.
	merged, err := serializer.MarshalYAMLDeterministic(doc)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to serialize snapshot after provider projection merge", err)
	}
	return merged, nil
}

// getKubeClient returns a Kubernetes client, using the kubeconfig override if provided.
func getKubeClient(kubeconfig string) (k8sclient.Interface, error) {
	var clientset k8sclient.Interface
	var err error

	if kubeconfig != "" {
		clientset, _, err = k8sclient.GetKubeClientWithConfig(kubeconfig)
	} else {
		clientset, _, err = k8sclient.GetKubeClient()
	}
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create Kubernetes client")
	}
	return clientset, nil
}

// DeployAndCollect deploys the agent Job, waits for it, and returns both the
// parsed Snapshot and the RAW bytes the agent emitted. It is the single
// deploy-and-retrieve implementation behind every caller — the aicr.Client
// facade's CollectSnapshot (and therefore `aicr snapshot` and `aicr validate`)
// and NodeSnapshotter.measureWithAgent.
//
// # Why both return values
//
// The parsed *Snapshot is what consumers reason about; the raw bytes are what
// gets written out. They are NOT interchangeable: re-serializing the typed
// struct drops any field a newer agent image emitted that this binary's
// Snapshot type does not know about. Callers that persist the snapshot must
// deliver the raw bytes (see DeliverSnapshot) so `aicr snapshot` output stays
// byte-identical to what the agent produced.
//
// # Where the agent writes
//
// The Job always stages its result in a ConfigMap. When config.Output is a
// cm:// URI that ConfigMap IS the user's destination, so the Job writes there
// directly; otherwise the Job writes to an internal ConfigMap in
// config.Namespace and the caller delivers the returned bytes.
//
// # Fail-before-mutate
//
// Every input that can be rejected without contacting the cluster is checked
// up front — before the Kubernetes client is even built, so a rejection is
// never masked by a kubeconfig error and never leaves RBAC or a Job behind
// (with Cleanup false, the zero value, they would persist). That covers a
// malformed cm:// Output, an empty Namespace, and a Job-mode
// ClusterConfigPath.
//
// The *Snapshot return has no consumer inside this package —
// measureWithAgent only delivers the bytes — but it is the value
// aicr.Client.CollectSnapshot hands to SDK callers and to `aicr validate`,
// hence the unparam exemption below. Keep the rationale in prose: gofmt moves
// //nolint (a directive-shaped comment) to the end of the doc block, so
// continuation lines written under it get hoisted above and orphaned.
//
//nolint:unparam // *Snapshot is consumed across the package boundary; see above.
func DeployAndCollect(ctx context.Context, config *AgentConfig) (*Snapshot, []byte, error) {
	if config == nil {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest, "agent config is required")
	}

	// Job mode forwards --cluster-config as an env var into the pod
	// (AICR_CLUSTER_CONFIG_PATH) without mounting the host file: the
	// caller's filesystem isn't reachable from inside the Job, and the
	// in-pod CLI would fail to open the path. Reject the combination
	// up front instead of producing a confusing failure deep in the
	// network collector. ConfigMap-backed file forwarding is tracked
	// as a follow-up; until then --cluster-config is local-mode-only
	// (AICR_AGENT_MODE=true) and --discover-network is the supported
	// Job-mode path.
	if config.ClusterConfigPath != "" {
		return nil, nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"--cluster-config is not supported in agent Job mode (the host path is not visible to the in-pod CLI); use --discover-network for live cluster discovery, or run with AICR_AGENT_MODE=true to use --cluster-config locally",
			map[string]any{"path": config.ClusterConfigPath})
	}

	// The Job, its RBAC, and the internal result ConfigMap all live in
	// config.Namespace. Empty (or whitespace) would build the invalid URI
	// "cm:///aicr-snapshot", which only fails when it is finally parsed —
	// after RBAC and the Job exist. The CLI always supplies a default, so
	// this is the SDK-caller path.
	if strings.TrimSpace(config.Namespace) == "" {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
			"Namespace is required: it is where the agent Job, its RBAC, and the result ConfigMap are created")
	}

	// Default RunID before any cluster access — and before
	// agentConfigMapTarget below, which folds it into the internal staging
	// ConfigMap's name — so every resource this run creates shares one
	// scope. pkg/k8s/agent's Deploy independently rejects an empty or
	// malformed RunID, but only as an ErrCodeInvalidRequest naming a Config
	// field a CLI user never set — so catch the whitespace-only value that
	// slips past this simple emptiness check here, where the message can
	// point at the knob the caller actually controls.
	//
	// The resolved value stays in a local: writing it back into the
	// caller-owned *AgentConfig would turn a caller who reuses one config
	// pointer for a second run into a caller pinning a duplicate RunID.
	runID := config.RunID
	if runID == "" {
		runID = runid.Generate()
	}
	if strings.TrimSpace(runID) == "" {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
			"RunID must not be all-whitespace; leave it unset to auto-generate one")
	}
	slog.Info("snapshot agent run", slog.String("runID", runID))

	// Resolve (and validate) the Job's ConfigMap target before any cluster
	// access: a malformed cm:// Output must not cost the caller a deployed
	// Job and a cluster-admin binding.
	agentOutput, ownsOutput, err := agentConfigMapTarget(config, runID)
	if err != nil {
		return nil, nil, err
	}

	slog.Debug("starting agent deployment for snapshot capture")

	clientset, err := getKubeClient(config.Kubeconfig)
	if err != nil {
		return nil, nil, err
	}

	snapshotData, err := deployAndWaitForResult(ctx, clientset, config, runID, agentOutput, ownsOutput)
	if err != nil {
		return nil, nil, err
	}

	var snap Snapshot
	if err := yaml.Unmarshal(snapshotData, &snap); err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse snapshot data", err)
	}

	warnOnGPUPlacementMismatch(&snap)

	return &snap, snapshotData, nil
}

// agentConfigMapTarget resolves where the agent Job stages its result and
// whether this run owns that ConfigMap.
//
// The Job always writes to a ConfigMap. When config.Output is a cm:// URI the
// user asked for that exact ConfigMap, so the Job targets it directly,
// ownsOutput is false, and a failed provider-projection-merge rewrite is fatal rather
// than a warning — the bytes the user will read live there, and this run
// must never delete an artifact it does not own. Any other Output (file,
// stdout, template, or unset) stages to an internal, run-scoped ConfigMap in
// config.Namespace that the caller never names: ownsOutput is true, so
// Cleanup may delete it.
//
// runID is the resolved run ID for this invocation (config.RunID when the
// caller pinned one, otherwise the value DeployAndCollect generated); it is a
// parameter rather than a config field read because DeployAndCollect never
// writes the generated ID back into the caller's AgentConfig.
//
// In the owned case the returned uri is exactly
// cm://<config.Namespace>/<agent.StagingConfigMapName(runID)>. Cleanup
// in pkg/k8s/agent relies on both halves of that invariant rather than
// re-parsing the URI: deleteStagingConfigMap deletes in Config.Namespace, and
// deleteUnrecordedStagingConfigMap (the sweep for a run that failed before
// the ConfigMap's UID was observed) looks it up by that same generated name.
//
// A cm:// Output is fully parsed here, not merely prefix-matched. The
// namespace/name only has to be well-formed for the in-pod writer much later,
// so a typo like "cm://aicr-snapshot" (no namespace) would otherwise surface
// as a Job failure — after RBAC and the Job exist, and with Cleanup false
// (the zero value) they stay behind. Returns ErrCodeInvalidRequest instead.
func agentConfigMapTarget(config *AgentConfig, runID string) (uri string, ownsOutput bool, err error) {
	if strings.HasPrefix(config.Output, serializer.ConfigMapURIScheme) {
		if _, _, parseErr := pod.ParseConfigMapURI(config.Output); parseErr != nil {
			// Wrap with the same code rather than PropagateOrWrap: the inner
			// error says "invalid configmap URI", which does not tell the
			// caller WHICH input was wrong. Naming the field is the point.
			return "", false, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid ConfigMap output URI %q (expected cm://namespace/name)", config.Output),
				parseErr)
		}
		return config.Output, false, nil
	}
	return serializer.ConfigMapURIScheme + config.Namespace + "/" + agent.StagingConfigMapName(runID), true, nil
}

// SnapshotDelivery describes where DeliverSnapshot writes captured bytes.
// Mirrors the delivery-relevant subset of AgentConfig so callers that already
// hold one can forward it, and callers that hold only bytes (an SDK consumer
// with a Snapshot.Raw) can construct one directly.
type SnapshotDelivery struct {
	// Output is the destination: empty, "-", or the stdout URI for stdout;
	// a cm://namespace/name URI for a ConfigMap; any other value is a file
	// path.
	Output string

	// TemplatePath, when set, renders the snapshot through a Go template
	// instead of copying bytes. Takes precedence over the Output scheme —
	// Output then names the rendered report's destination.
	TemplatePath string

	// Kubeconfig is the path used to reach the cluster for a cm:// Output.
	// Empty means in-cluster or the standard discovery chain. Ignored for
	// every other destination.
	Kubeconfig string

	// Format is the rendering the caller asked for. The zero value and
	// serializer.FormatYAML both deliver the agent's bytes verbatim to
	// stdout and file destinations; JSON and table re-render the document
	// (see renderSnapshotFormat). A cm:// destination always re-serializes,
	// whatever the format — see the ConfigMap section on DeliverSnapshot.
	// Ignored when TemplatePath is set, since a template supplies its own
	// rendering.
	Format serializer.Format
}

// DeliverSnapshot writes captured snapshot bytes to the user's destination:
// a Go template render when TemplatePath is set, otherwise stdout (Output
// empty, "-", or the stdout URI), a ConfigMap (cm://namespace/name), or a
// file.
//
// data must be the RAW bytes from DeployAndCollect, not a re-serialization of
// the parsed Snapshot — see DeployAndCollect for why. Stdout and file
// destinations copy those bytes when Format is YAML (or unset). Three modes
// necessarily parse the document instead: a template, which exposes Snapshot
// fields to the template, a non-YAML Format, and any cm:// destination.
//
// # ConfigMap destinations
//
// A cm:// Output is WRITTEN here, not assumed. When the snapshot came from
// DeployAndCollect with the same URI as AgentConfig.Output the agent Job
// already staged those bytes, so this apply is redundant but idempotent —
// and it is what makes the function total: a caller that collected to the
// default internal ConfigMap and then delivers to cm://ns/name gets the
// artifact it asked for instead of a silent no-op. Failures surface; a
// destination the caller named is not something to log and skip past.
//
// A ConfigMap is a structured resource, not a byte sink: the writer derives
// the snapshot.<ext> data key, the format and timestamp entries, and the
// resource labels from the parsed document. So this destination re-serializes
// even for YAML — deterministically, via serializer.MarshalYAMLDeterministic,
// and through a generic map so no unmodeled field is lost. Only the exact
// bytes are not preserved. A caller that needs byte-identical YAML should
// deliver to a file or stdout.
func DeliverSnapshot(ctx context.Context, data []byte, dest SnapshotDelivery) error {
	if dest.TemplatePath != "" {
		return deliverWithTemplate(ctx, data, dest.TemplatePath, dest.Output)
	}

	// Resolve up front so every destination rejects the same set of
	// formats. It matters most for the ConfigMap branch: serializer's
	// ConfigMap writer coerces an unrecognized format to JSON, so without
	// this a cm:// destination would quietly accept what stdout and file
	// delivery reject.
	format, err := resolveDeliveryFormat(dest.Format)
	if err != nil {
		return err
	}

	// A ConfigMap carries its format in the resource itself — the data key
	// is snapshot.<ext> alongside a "format" entry, which is what the
	// reader in pkg/serializer keys off — so the ConfigMap writer does the
	// rendering rather than this function.
	if strings.HasPrefix(dest.Output, serializer.ConfigMapURIScheme) {
		if writeErr := writeSnapshotConfigMap(ctx, dest.Output, dest.Kubeconfig, data, format); writeErr != nil {
			return writeErr
		}
		slog.Info("snapshot saved to ConfigMap",
			slog.String("uri", dest.Output),
			slog.String("format", string(format)))
		return nil
	}

	rendered, err := renderSnapshotFormat(ctx, data, format)
	if err != nil {
		return err
	}

	if dest.Output == "" || dest.Output == "-" || dest.Output == serializer.StdoutURI {
		// Exactly the rendered bytes, no added terminator: stdout is a
		// delivery destination like any other, so `aicr snapshot >
		// snapshot.yaml` has to hash the same as `-o snapshot.yaml`.
		// Every rendering already ends in a newline — the agent's
		// document because it comes from MarshalYAMLDeterministic, JSON
		// and table because renderSnapshotFormat terminates them — so
		// this does not leave a terminal prompt dangling. A short write
		// or broken pipe must surface, not silently drop the snapshot.
		if _, err := os.Stdout.Write(rendered); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write snapshot to stdout", err)
		}
		return nil
	}

	if err := serializer.WriteToFile(dest.Output, rendered); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write snapshot to file", err)
	}
	slog.Info("snapshot saved to file",
		slog.String("path", dest.Output),
		slog.String("format", string(format)))

	return nil
}

// resolveDeliveryFormat resolves the delivery format and rejects anything
// unsupported. The zero value is YAML: delivery predates
// SnapshotDelivery.Format, so an SDK caller that never sets it must keep
// getting the agent's YAML bytes.
func resolveDeliveryFormat(format serializer.Format) (serializer.Format, error) {
	if format == "" {
		return serializer.FormatYAML, nil
	}
	if format.IsUnknown() {
		return "", errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"unsupported snapshot output format",
			map[string]any{"format": string(format), "supported": serializer.SupportedFormats()})
	}
	return format, nil
}

// renderSnapshotFormat converts the agent's raw YAML document into an already
// resolved format, for stdout and file destinations.
//
// YAML returns data untouched: those bytes are byte-identical to what the
// agent emitted, including fields this binary's Snapshot type does not model.
// JSON re-encodes through a generic map for the same reason — a typed round
// trip would drop those fields, and a JSON document with the same keys is
// what `aicr diff --target snapshot.json` and jq consumers expect. Table is a
// human rendering that goes through the typed struct, so it is the one format
// that is lossy by construction; it matches what in-pod (local) collection
// emits for the same flag.
func renderSnapshotFormat(ctx context.Context, data []byte, format serializer.Format) ([]byte, error) {
	switch format {
	case serializer.FormatYAML:
		return data, nil

	case serializer.FormatJSON:
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse snapshot for JSON output", err)
		}
		// json.Marshal sorts map keys, so the rendering is deterministic
		// for a given document.
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to render snapshot as JSON", err)
		}
		return append(out, '\n'), nil

	case serializer.FormatTable:
		snap, err := parseSnapshotDocument(data, "table")
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := serializer.NewWriter(serializer.FormatTable, &buf).Serialize(ctx, snap); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to render snapshot as a table", err)
		}
		return buf.Bytes(), nil

	default:
		// Unreachable via DeliverSnapshot, which resolves first; kept so a
		// future caller cannot skip the check and fall through to YAML.
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"unsupported snapshot output format",
			map[string]any{"format": string(format), "supported": serializer.SupportedFormats()})
	}
}

// parseSnapshotDocument unmarshals an agent document into this binary's
// Snapshot type for the delivery modes that cannot copy bytes.
func parseSnapshotDocument(data []byte, mode string) (*Snapshot, error) {
	var snap Snapshot
	if err := yaml.Unmarshal(data, &snap); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to parse snapshot for %s output", mode), err)
	}
	return &snap, nil
}

// ParseResourceList converts a comma-separated "name=quantity" list
// (e.g. "cpu=500m,memory=1Gi,ephemeral-storage=1Gi") into a
// corev1.ResourceList for use as a per-container request or limit
// override. An empty string returns a nil ResourceList so the caller
// can distinguish "no override supplied" (defaults apply) from
// "override supplied" (replace per-key); a sentinel error would force
// every call site to special-case the empty-flag path. Each quantity
// is parsed via resource.ParseQuantity, so the same suffixes accepted
// everywhere else in Kubernetes work here (m, Ki, Mi, Gi, Ti, ...).
//
//nolint:nilnil // (nil, nil) on empty input is the intended contract.
func ParseResourceList(spec string) (corev1.ResourceList, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	result := corev1.ResourceList{}
	for raw := range strings.SplitSeq(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("entry %q is not in name=quantity form", entry))
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("entry %q has empty name or quantity", entry))
		}
		q, err := resource.ParseQuantity(value)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("entry %q has invalid quantity", entry), err)
		}
		// Reject negative quantities at parse time so the user gets a
		// clear error instead of an obscure failure when the Job is
		// later created (Kubernetes resources cannot be negative).
		if q.Sign() < 0 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("entry %q has negative quantity", entry))
		}
		// Reject duplicate keys explicitly. Last-write-wins is too easy
		// to misuse silently from a shell-templated invocation.
		if _, dup := result[corev1.ResourceName(key)]; dup {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("duplicate key %q", key))
		}
		result[corev1.ResourceName(key)] = q
	}
	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// ParseNodeSelectors parses node selector strings in format "key=value".
func ParseNodeSelectors(selectors []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, s := range selectors {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format %q, expected key=value", s))
		}
		result[parts[0]] = parts[1]
	}
	return result, nil
}

// DefaultTolerations returns tolerations that accept all taints.
// This allows the agent Job to be scheduled on any node regardless of taints.
func DefaultTolerations() []corev1.Toleration {
	return []corev1.Toleration{
		{
			Operator: corev1.TolerationOpExists,
		},
	}
}

// effectiveAgentTolerations applies the snapshot agent's scheduling default
// without collapsing an explicit empty override into that default.
func effectiveAgentTolerations(tolerations []corev1.Toleration) []corev1.Toleration {
	if tolerations == nil {
		return DefaultTolerations()
	}
	return tolerations
}

func validateTaintEffect(effect corev1.TaintEffect) error {
	switch effect {
	case corev1.TaintEffectNoSchedule:
		return nil
	case corev1.TaintEffectPreferNoSchedule:
		return nil
	case corev1.TaintEffectNoExecute:
		return nil
	default:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid taint effect %q, expected %s, %s, or %s", effect, corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute))
	}
}

// ParseTolerations parses toleration strings in format "key=value:effect" or "key:effect".
// If no tolerations are provided, returns DefaultTolerations() which accepts all taints.
func ParseTolerations(tolerations []string) ([]corev1.Toleration, error) {
	// Return default "tolerate all" if no custom tolerations specified
	if len(tolerations) == 0 {
		return DefaultTolerations(), nil
	}

	result := make([]corev1.Toleration, 0, len(tolerations))
	for _, t := range tolerations {
		if t == "*" {
			result = append(result, corev1.Toleration{Operator: corev1.TolerationOpExists})
			continue
		}

		// Format: key=value:effect or key:effect (for exists operator)
		var key, value, effect string

		// Split by colon to get effect
		parts := strings.Split(t, ":")
		if len(parts) != 2 {
			return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format %q, expected key=value:effect or key:effect", t))
		}
		effect = parts[1]

		// Parse key and value
		if strings.Contains(parts[0], "=") {
			kvParts := strings.SplitN(parts[0], "=", 2)
			key = kvParts[0]
			value = kvParts[1]
		} else {
			key = parts[0]
			// No value means Exists operator
		}

		if err := validateTaintEffect(corev1.TaintEffect(effect)); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid taint effect", err)
		}

		toleration := corev1.Toleration{
			Key:    key,
			Effect: corev1.TaintEffect(effect),
		}

		if value != "" {
			toleration.Operator = corev1.TolerationOpEqual
			toleration.Value = value
		} else {
			toleration.Operator = corev1.TolerationOpExists
		}

		result = append(result, toleration)
	}
	return result, nil
}

// ParseTaint parses a single taint string in format "key=value:effect" or "key:effect".
// Returns a corev1.Taint struct.
func ParseTaint(taintStr string) (*corev1.Taint, error) {
	if taintStr == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "taint string cannot be empty")
	}

	// Format: key=value:effect or key:effect (for exists operator)
	var key, value, effect string

	// Split by colon to get effect
	parts := strings.Split(taintStr, ":")
	if len(parts) != 2 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format %q, expected key=value:effect or key:effect", taintStr))
	}
	effect = parts[1]

	// Parse key and value
	if strings.Contains(parts[0], "=") {
		kvParts := strings.SplitN(parts[0], "=", 2)
		key = kvParts[0]
		value = kvParts[1]
	} else {
		key = parts[0]
		// No value means empty value (taints don't have operators like tolerations)
	}

	// Validate key is not empty
	if key == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid format %q, key cannot be empty", taintStr))
	}

	if err := validateTaintEffect(corev1.TaintEffect(effect)); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid taint effect", err)
	}

	taint := &corev1.Taint{
		Key:    key,
		Effect: corev1.TaintEffect(effect),
	}

	if value != "" {
		taint.Value = value
	}

	return taint, nil
}

// measureWithAgent deploys a Kubernetes Job to capture snapshot on cluster
// nodes and writes the result to the user's destination. It is the
// deploy-then-deliver composition of DeployAndCollect and DeliverSnapshot,
// which is also what the aicr.Client facade path runs — so both spellings
// emit the same bytes by construction.
//
// One behavior converged when the two paths merged: a snapshot document this
// binary cannot unmarshal is now an error. Previously this path parsed
// best-effort (skipping only the GPU-placement warning) and delivered the
// bytes anyway, while the SDK path already failed. Strict is the correct
// side to converge on — a document `aicr snapshot` cannot parse is one
// `aicr validate` and `aicr recipe --snapshot` would reject too, so writing
// it out and exiting 0 reports a success the artifact cannot back.
func (n *NodeSnapshotter) measureWithAgent(ctx context.Context) error {
	_, snapshotData, err := DeployAndCollect(ctx, n.AgentConfig)
	if err != nil {
		return err
	}
	return DeliverSnapshot(ctx, snapshotData, SnapshotDelivery{
		Output:       n.AgentConfig.Output,
		TemplatePath: n.AgentConfig.TemplatePath,
		Kubeconfig:   n.AgentConfig.Kubeconfig,
	})
}

// rewriteMergedSnapshotConfigMap applies the merged snapshot bytes back to
// the result ConfigMap and encodes the delivery contract: when the ConfigMap
// is the user's delivery vehicle (cm:// output) a rewrite failure fails the
// run, otherwise it degrades to a loud warning — the returned bytes already
// carry the merged reading, and a transient Apply failure on the internal
// hygiene rewrite must not discard an already-captured snapshot.
func rewriteMergedSnapshotConfigMap(ctx context.Context, uri, kubeconfig string, snapshotData []byte, deliverViaConfigMap bool) error {
	// YAML regardless of the caller's --format: this ConfigMap is the
	// agent-to-controller staging vehicle, and pkg/k8s/agent's GetSnapshot
	// reads its snapshot.yaml key. When the user asked for a cm://
	// destination in another format, DeliverSnapshot re-applies it below.
	err := writeSnapshotConfigMap(ctx, uri, kubeconfig, snapshotData, serializer.FormatYAML)
	if err == nil {
		return nil
	}
	if deliverViaConfigMap {
		return err
	}
	slog.Warn("failed to rewrite the internal snapshot ConfigMap with the merged pool projection; "+
		"the returned snapshot carries the reading, but the orphaned ConfigMap remains pre-merge",
		"uri", uri, "error", err)
	return nil
}

// writeSnapshotConfigMap applies snapshot bytes to a cm://namespace/name
// destination in the requested format, replacing whatever is there. Shared by
// DeliverSnapshot (the caller's chosen destination) and the provider-projection merge
// rewrite (replacing the pre-merge content the agent Job stored), which pins
// YAML: pkg/k8s/agent reads the staging ConfigMap's snapshot.yaml key.
//
// The format is resolved here as well as in DeliverSnapshot, because the
// ConfigMap writer coerces an unrecognized format to JSON — an unsupported
// format must fail, not change the artifact's encoding.
func writeSnapshotConfigMap(ctx context.Context, uri, kubeconfig string, snapshotData []byte, format serializer.Format) error {
	namespace, name, err := pod.ParseConfigMapURI(uri)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse snapshot ConfigMap URI", err)
	}
	format, err = resolveDeliveryFormat(format)
	if err != nil {
		return err
	}
	payload, err := configMapPayload(snapshotData, format)
	if err != nil {
		return err
	}
	writer := serializer.NewConfigMapWriterWithKubeconfig(namespace, name, kubeconfig, format)
	if err := writer.Serialize(ctx, payload); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write snapshot ConfigMap", err)
	}
	return nil
}

// configMapPayload picks what the ConfigMap writer serializes.
//
// YAML and JSON go through the generic-map wrapper, same reason as
// mergeAKSGPUPools: the typed Snapshot struct would drop fields a newer agent
// image emitted, and rawSnapshotDoc still exposes the kind/version the writer
// puts on the ConfigMap's labels. A table is a flattened FIELD/VALUE view the
// writer derives by reflecting over exported fields, which a generic map
// collapses into a single row — so that mode hands over the typed struct and
// accepts the lossy round trip.
func configMapPayload(snapshotData []byte, format serializer.Format) (any, error) {
	if format == serializer.FormatTable {
		return parseSnapshotDocument(snapshotData, "table")
	}
	var doc map[string]any
	if err := yaml.Unmarshal(snapshotData, &doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse snapshot for ConfigMap write", err)
	}
	return rawSnapshotDoc{doc: doc}, nil
}

// rawSnapshotDoc lets a generic snapshot document flow through serializers
// that extract header metadata: it marshals as the raw document (no typed
// round trip, no field loss) while exposing kind and string metadata from
// the document itself.
type rawSnapshotDoc struct {
	doc map[string]any
}

//nolint:unparam // the (any, error) shape is yaml.Marshaler's fixed contract
func (r rawSnapshotDoc) MarshalYAML() (any, error) { return r.doc, nil }

// MarshalJSON keeps the no-field-loss guarantee on the JSON path: without it
// the wrapper has no exported fields and would serialize as "{}".
func (r rawSnapshotDoc) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(r.doc)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot document as JSON", err)
	}
	return out, nil
}

func (r rawSnapshotDoc) GetKind() header.Kind {
	kind, _ := r.doc["kind"].(string)
	return header.Kind(kind)
}

func (r rawSnapshotDoc) GetMetadata() map[string]string {
	out := make(map[string]string)
	meta, _ := r.doc["metadata"].(map[string]any)
	for k, v := range meta {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// deliverWithTemplate renders snapshot data through a Go template. Unlike the
// other delivery modes this one must parse the document, because the template
// addresses the Snapshot struct's fields.
func deliverWithTemplate(ctx context.Context, snapshotData []byte, templatePath, output string) (err error) {
	// Unmarshal YAML to Snapshot struct
	var snap Snapshot
	if err = yaml.Unmarshal(snapshotData, &snap); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal snapshot for template processing", err)
	}

	// Create template writer
	tw, err := serializer.NewTemplateFileWriter(templatePath, output)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create template writer", err)
	}
	defer func() {
		if closeErr := tw.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close template writer", closeErr)
		}
	}()

	// Execute template
	if err := tw.Serialize(ctx, &snap); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to execute template", err)
	}

	if output != "" && output != "-" && output != serializer.StdoutURI {
		slog.Info("snapshot saved to file with template", slog.String("path", output))
	}

	return nil
}
