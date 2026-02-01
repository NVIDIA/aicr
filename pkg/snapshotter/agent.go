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

	"github.com/NVIDIA/eidos/pkg/k8s/agent"
	k8sclient "github.com/NVIDIA/eidos/pkg/k8s/client"
	"github.com/NVIDIA/eidos/pkg/serializer"
	corev1 "k8s.io/api/core/v1"
)

// logWriter returns an io.Writer for streaming agent logs.
// Uses stderr to avoid interfering with stdout output.
func logWriter() io.Writer {
	return os.Stderr
}

// AgentConfig contains configuration for Kubernetes agent deployment.
type AgentConfig struct {
	// Enabled determines whether to deploy agent or run locally
	Enabled bool

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
}

// ParseNodeSelectors parses node selector strings in format "key=value".
func ParseNodeSelectors(selectors []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, s := range selectors {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format %q, expected key=value", s)
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
			return nil, fmt.Errorf("invalid format %q, expected key=value:effect or key:effect", t)
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
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Default output to ConfigMap if not specified
	output := n.AgentConfig.Output
	if output == "" {
		output = fmt.Sprintf("%s%s/eidos-snapshot", serializer.ConfigMapURIScheme, n.AgentConfig.Namespace)
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
		Output:             output,
		Debug:              n.AgentConfig.Debug,
		Privileged:         n.AgentConfig.Privileged,
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
						"kubectl delete clusterrole/eidos-node-reader clusterrolebinding/eidos-node-reader",
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
		return fmt.Errorf("failed to deploy agent: %w", deployErr)
	}

	slog.Info("agent deployed successfully")

	// Wait for Job completion
	timeout := n.AgentConfig.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	slog.Info("waiting for Job completion",
		slog.String("job", agentConfig.JobName),
		slog.Duration("timeout", timeout))

	// Wait for Pod to be ready before streaming logs
	podReadyTimeout := 60 * time.Second
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
		return fmt.Errorf("job failed: %w", waitErr)
	}

	slog.Info("job completed successfully")

	// Retrieve snapshot from ConfigMap
	slog.Debug("retrieving snapshot from ConfigMap")
	snapshotData, err := deployer.GetSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve snapshot: %w", err)
	}

	// Write snapshot to additional destinations if needed
	switch {
	case output == serializer.StdoutURI:
		// Write to stdout
		fmt.Println(string(snapshotData))
	case strings.HasPrefix(output, serializer.ConfigMapURIScheme):
		// Already in ConfigMap (written by Job)
		slog.Info("snapshot saved to ConfigMap", slog.String("uri", output))
	default:
		// Write to file (in addition to ConfigMap)
		if err := serializer.WriteToFile(output, snapshotData); err != nil {
			return fmt.Errorf("failed to write snapshot to file: %w", err)
		}
		slog.Info("snapshot saved to file", slog.String("path", output))
	}

	return nil
}
