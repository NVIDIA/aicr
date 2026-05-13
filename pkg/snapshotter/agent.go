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
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
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

	// ServiceAccountName for the agent
	ServiceAccountName string

	// NodeSelector for targeting specific nodes
	NodeSelector map[string]string

	// Tolerations for scheduling on tainted nodes
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
}

// deployAndWaitForResult handles the common deploy-wait-retrieve lifecycle for an agent Job.
// It creates the deployer, deploys RBAC and the Job, streams logs, waits for completion,
// and retrieves the snapshot data from the result ConfigMap.
func deployAndWaitForResult(ctx context.Context, clientset k8sclient.Interface, config *AgentConfig, agentOutput string) ([]byte, error) {
	agentConfig := agent.Config{
		Namespace:          config.Namespace,
		ServiceAccountName: config.ServiceAccountName,
		JobName:            config.JobName,
		Image:              config.Image,
		ImagePullSecrets:   config.ImagePullSecrets,
		NodeSelector:       config.NodeSelector,
		Tolerations:        config.Tolerations,
		Output:             agentOutput,
		Debug:              config.Debug,
		Privileged:         config.Privileged,
		RequireGPU:         config.RequireGPU,
		RuntimeClassName:   config.RuntimeClassName,
		MaxNodesPerEntry:   config.MaxNodesPerEntry,
		OS:                 config.OS,
		Requests:           config.Requests,
		Limits:             config.Limits,
	}

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

	slog.Info("waiting for Job completion",
		slog.String("job", agentConfig.JobName),
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

	logWG.Add(1)
	go func() {
		defer logWG.Done()
		if podErr := deployer.WaitForPodReady(logCtx, defaults.K8sPodReadyTimeout); podErr != nil {
			// Only suppress logging when the parent context has been
			// canceled (expected during cleanup). Genuine failures
			// (pod stuck, image pull errors) must surface so operators
			// understand why agent logs are missing from their output.
			if logCtx.Err() == nil {
				slog.Warn("agent log streaming skipped: pod did not become ready",
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
	}()

	if waitErr := deployer.WaitForCompletion(ctx, timeout); waitErr != nil {
		if logs, logErr := deployer.GetPodLogs(ctx); logErr == nil && logs != "" {
			fmt.Fprintln(logWriter(), "--- agent logs ---")
			fmt.Fprintln(logWriter(), logs)
			fmt.Fprintln(logWriter(), "--- end logs ---")
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "job failed", waitErr)
	}

	slog.Info("job completed successfully")

	slog.Debug("retrieving snapshot from ConfigMap")
	snapshotData, err := deployer.GetSnapshot(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to retrieve snapshot", err)
	}

	return snapshotData, nil
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
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create Kubernetes client", err)
	}
	return clientset, nil
}

// DeployAndGetSnapshot deploys an agent to capture a snapshot and returns the Snapshot struct.
// This is used by commands that need to capture a snapshot but also process the data
// (e.g., validate command that needs to run validation on the captured snapshot).
func DeployAndGetSnapshot(ctx context.Context, config *AgentConfig) (*Snapshot, error) {
	if config == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "agent config is required")
	}

	slog.Debug("starting agent deployment for snapshot capture")

	clientset, err := getKubeClient(config.Kubeconfig)
	if err != nil {
		return nil, err
	}

	agentOutput := fmt.Sprintf("%s%s/aicr-snapshot", serializer.ConfigMapURIScheme, config.Namespace)

	snapshotData, err := deployAndWaitForResult(ctx, clientset, config, agentOutput)
	if err != nil {
		return nil, err
	}

	var snap Snapshot
	if err := yaml.Unmarshal(snapshotData, &snap); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse snapshot data", err)
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
	for _, raw := range strings.Split(spec, ",") {
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

// measureWithAgent deploys a Kubernetes Job to capture snapshot on cluster nodes.
func (n *NodeSnapshotter) measureWithAgent(ctx context.Context) error {
	slog.Debug("starting agent deployment")

	clientset, err := getKubeClient(n.AgentConfig.Kubeconfig)
	if err != nil {
		return err
	}

	// The user's final output destination (file, stdout, or ConfigMap)
	finalOutput := n.AgentConfig.Output

	// Agent Job always writes to a ConfigMap internally.
	// If user specified a ConfigMap URI, use that; otherwise use a default ConfigMap.
	agentOutput := fmt.Sprintf("%s%s/aicr-snapshot", serializer.ConfigMapURIScheme, n.AgentConfig.Namespace)
	if strings.HasPrefix(finalOutput, serializer.ConfigMapURIScheme) {
		// User explicitly wants ConfigMap output, use their URI
		agentOutput = finalOutput
	}

	snapshotData, err := deployAndWaitForResult(ctx, clientset, n.AgentConfig, agentOutput)
	if err != nil {
		return err
	}

	// If template is specified, process the snapshot through the template
	if n.AgentConfig.TemplatePath != "" {
		return n.processWithTemplate(ctx, snapshotData, finalOutput)
	}

	// Write snapshot to final destination
	switch {
	case finalOutput == "" || finalOutput == "-" || finalOutput == serializer.StdoutURI:
		// Output snapshot data to stdout for consumption by caller
		os.Stdout.Write(snapshotData)
		os.Stdout.Write([]byte("\n"))
	case strings.HasPrefix(finalOutput, serializer.ConfigMapURIScheme):
		// Already in ConfigMap (written by Job)
		slog.Info("snapshot saved to ConfigMap", slog.String("uri", finalOutput))
	default:
		// Write to file
		if err := serializer.WriteToFile(finalOutput, snapshotData); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to write snapshot to file", err)
		}
		slog.Info("snapshot saved to file", slog.String("path", finalOutput))
	}

	return nil
}

// processWithTemplate processes snapshot data through a Go template.
func (n *NodeSnapshotter) processWithTemplate(ctx context.Context, snapshotData []byte, output string) (err error) {
	// Unmarshal YAML to Snapshot struct
	var snap Snapshot
	if err = yaml.Unmarshal(snapshotData, &snap); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal snapshot for template processing", err)
	}

	// Create template writer
	tw, err := serializer.NewTemplateFileWriter(n.AgentConfig.TemplatePath, output)
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
