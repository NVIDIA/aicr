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

package snapshotter

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/agent"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/serializer"
	corev1 "k8s.io/api/core/v1"
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

	// TemplatePath is the path to a Go template file for custom output formatting.
	// When set, the snapshot output will be processed through this template.
	TemplatePath string
}

// DeployAndGetSnapshot deploys an agent to capture a snapshot and returns the Snapshot struct.
// This is used by commands that need to capture a snapshot but also process the data
// (e.g., validate command that needs to run validation on the captured snapshot).
func DeployAndGetSnapshot(ctx context.Context, config *AgentConfig) (*Snapshot, error) {
	if config == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "agent config is required")
	}

	slog.Debug("starting agent deployment for snapshot capture")

	// Get Kubernetes client
	var clientset k8sclient.Interface
	var err error

	if config.Kubeconfig != "" {
		clientset, _, err = k8sclient.GetKubeClientWithConfig(config.Kubeconfig)
	} else {
		clientset, _, err = k8sclient.GetKubeClient()
	}
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create Kubernetes client", err)
	}

	// Agent Job always writes to a ConfigMap internally.
	agentOutput := fmt.Sprintf("%s%s/aicr-snapshot", serializer.ConfigMapURIScheme, config.Namespace)

	// Build agent configuration
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
	}

	// Create deployer
	deployer := agent.NewDeployer(clientset, agentConfig)

	// Ensure cleanup on error or success
	//nolint:contextcheck // intentional: need fresh context for cleanup when parent is canceled
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	// Deploy RBAC and Job
	if deployErr := deployer.Deploy(ctx); deployErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to deploy agent", deployErr)
	}

	slog.Info("agent deployed successfully")

	// Wait for Job completion
	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaults.K8sJobCompletionTimeout
	}

	slog.Info("waiting for Job completion",
		slog.String("job", agentConfig.JobName),
		slog.Duration("timeout", timeout))

	// Wait for Pod to be ready before streaming logs
	podReadyTimeout := defaults.K8sPodReadyTimeout
	logCtx, cancelLogs := context.WithCancel(ctx)
	defer cancelLogs()

	if podErr := deployer.WaitForPodReady(ctx, podReadyTimeout); podErr != nil {
		slog.Warn("could not wait for pod ready, skipping log streaming", slog.String("error", podErr.Error()))
	} else {
		// Start streaming logs in background
		go func() {
			if streamErr := deployer.StreamLogs(logCtx, logWriter(), ""); streamErr != nil {
				// Only log if not canceled (expected when job completes)
				if logCtx.Err() == nil {
					slog.Debug("log streaming ended", slog.String("reason", streamErr.Error()))
				}
			}
		}()
	}

	if waitErr := deployer.WaitForCompletion(ctx, timeout); waitErr != nil {
		// On failure, try to get pod logs to show what went wrong
		if logs, logErr := deployer.GetPodLogs(ctx); logErr == nil && logs != "" {
			fmt.Fprintln(logWriter(), "--- agent logs ---")
			fmt.Fprintln(logWriter(), logs)
			fmt.Fprintln(logWriter(), "--- end logs ---")
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "job failed", waitErr)
	}

	slog.Info("job completed successfully")

	// Retrieve snapshot from ConfigMap
	slog.Debug("retrieving snapshot from ConfigMap")
	snapshotData, err := deployer.GetSnapshot(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to retrieve snapshot", err)
	}

	// Parse snapshot YAML into Snapshot struct
	var snap Snapshot
	if err := yaml.Unmarshal(snapshotData, &snap); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse snapshot data", err)
	}

	return &snap, nil
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

	// Get Kubernetes client
	var clientset k8sclient.Interface
	var err error

	if n.AgentConfig.Kubeconfig != "" {
		clientset, _, err = k8sclient.GetKubeClientWithConfig(n.AgentConfig.Kubeconfig)
	} else {
		clientset, _, err = k8sclient.GetKubeClient()
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create Kubernetes client", err)
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

	// Build agent configuration
	agentConfig := agent.Config{
		Namespace:          n.AgentConfig.Namespace,
		ServiceAccountName: n.AgentConfig.ServiceAccountName,
		JobName:            n.AgentConfig.JobName,
		Image:              n.AgentConfig.Image,
		ImagePullSecrets:   n.AgentConfig.ImagePullSecrets,
		NodeSelector:       n.AgentConfig.NodeSelector,
		Tolerations:        n.AgentConfig.Tolerations,
		Output:             agentOutput,
		Debug:              n.AgentConfig.Debug,
		Privileged:         n.AgentConfig.Privileged,
		RequireGPU:         n.AgentConfig.RequireGPU,
	}

	// Create deployer
	deployer := agent.NewDeployer(clientset, agentConfig)

	// Ensure cleanup on error or success
	//nolint:contextcheck // intentional: need fresh context for cleanup when parent is canceled
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		cleanupOpts := agent.CleanupOptions{Enabled: n.AgentConfig.Cleanup}
		if cleanupErr := deployer.Cleanup(cleanupCtx, cleanupOpts); cleanupErr != nil {
			slog.Warn("cleanup failed - resources may remain in cluster",
				slog.String("error", cleanupErr.Error()),
				slog.String("namespace", n.AgentConfig.Namespace),
			)
			slog.Warn("to manually clean up, run:",
				slog.String("command", fmt.Sprintf(
					"kubectl delete job/%s sa/%s role/%s rolebinding/%s -n %s && "+
						"kubectl delete clusterrole/aicr-node-reader clusterrolebinding/aicr-node-reader",
					n.AgentConfig.JobName,
					n.AgentConfig.ServiceAccountName,
					n.AgentConfig.ServiceAccountName,
					n.AgentConfig.ServiceAccountName,
					n.AgentConfig.Namespace,
				)),
			)
		}
	}()

	slog.Info("deploying agent", slog.String("namespace", agentConfig.Namespace))

	// Deploy RBAC and Job
	if deployErr := deployer.Deploy(ctx); deployErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to deploy agent", deployErr)
	}

	slog.Info("agent deployed successfully")

	// Wait for Job completion
	timeout := n.AgentConfig.Timeout
	if timeout == 0 {
		timeout = defaults.K8sJobCompletionTimeout
	}

	slog.Info("waiting for Job completion",
		slog.String("job", agentConfig.JobName),
		slog.Duration("timeout", timeout))

	// Wait for Pod to be ready before streaming logs
	podReadyTimeout := defaults.K8sPodReadyTimeout
	logCtx, cancelLogs := context.WithCancel(ctx)
	defer cancelLogs()

	if podErr := deployer.WaitForPodReady(ctx, podReadyTimeout); podErr != nil {
		slog.Warn("could not wait for pod ready, skipping log streaming", slog.String("error", podErr.Error()))
	} else {
		// Start streaming logs in background
		go func() {
			if streamErr := deployer.StreamLogs(logCtx, logWriter(), ""); streamErr != nil {
				// Only log if not canceled (expected when job completes)
				if logCtx.Err() == nil {
					slog.Debug("log streaming ended", slog.String("reason", streamErr.Error()))
				}
			}
		}()
	}

	if waitErr := deployer.WaitForCompletion(ctx, timeout); waitErr != nil {
		// On failure, try to get pod logs to show what went wrong
		if logs, logErr := deployer.GetPodLogs(ctx); logErr == nil && logs != "" {
			fmt.Fprintln(logWriter(), "--- agent logs ---")
			fmt.Fprintln(logWriter(), logs)
			fmt.Fprintln(logWriter(), "--- end logs ---")
		}
		return errors.Wrap(errors.ErrCodeInternal, "job failed", waitErr)
	}

	slog.Info("job completed successfully")

	// Retrieve snapshot from ConfigMap
	slog.Debug("retrieving snapshot from ConfigMap")
	snapshotData, err := deployer.GetSnapshot(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to retrieve snapshot", err)
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
func (n *NodeSnapshotter) processWithTemplate(ctx context.Context, snapshotData []byte, output string) error {
	// Unmarshal YAML to Snapshot struct
	var snap Snapshot
	if err := yaml.Unmarshal(snapshotData, &snap); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to unmarshal snapshot for template processing", err)
	}

	// Create template writer
	tw, err := serializer.NewTemplateFileWriter(n.AgentConfig.TemplatePath, output)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create template writer", err)
	}
	defer tw.Close()

	// Execute template
	if err := tw.Serialize(ctx, &snap); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to execute template", err)
	}

	if output != "" && output != "-" && output != serializer.StdoutURI {
		slog.Info("snapshot saved to file with template", slog.String("path", output))
	}

	return nil
}
