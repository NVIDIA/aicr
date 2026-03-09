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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	podutil "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	nvidiaSMIPodTemplateFile = "testdata/nvidia-smi-verify-pod.yaml"
	gpuCheckSuccessMsg       = "GPU_CHECK_SUCCESS"
	nvidiaSMILogContextLines = 20
)

// checkNvidiaSMI verifies that nvidia-smi works correctly on all GPU nodes.
func checkNvidiaSMI(ctx *validators.Context) error {
	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to query for GPU nodes", err)
	}

	if len(gpuNodes) == 0 {
		return validators.Skip("no GPU nodes found in the cluster")
	}

	fmt.Printf("Found %d GPU node(s):\n", len(gpuNodes))
	for _, node := range gpuNodes {
		fmt.Printf("  %s\n", node.Name)
	}

	// Check if any nodes are busy
	var busyNodes []string
	for _, node := range gpuNodes {
		busy, busyErr := helper.IsNodeGpuBusy(ctx.Ctx, ctx.Clientset, node.Name)
		if busyErr != nil {
			slog.Warn("error checking busy status, treating as busy", "node", node.Name, "error", busyErr)
			busyNodes = append(busyNodes, node.Name)
			continue
		}
		if busy {
			busyNodes = append(busyNodes, node.Name)
		}
	}

	if len(busyNodes) > 0 {
		return validators.Skip(fmt.Sprintf("GPU nodes busy with existing workloads: %v", busyNodes))
	}

	fmt.Printf("All %d GPU node(s) available. Verifying...\n", len(gpuNodes))

	// Verify each GPU node
	results := make(map[string]error)
	for _, node := range gpuNodes {
		slog.Info("verifying node", "node", node.Name)
		if verifyErr := verifySingleGPUNode(ctx, node.Name); verifyErr != nil {
			results[node.Name] = verifyErr
			fmt.Printf("  %s: FAILED (%v)\n", node.Name, verifyErr)
		} else {
			results[node.Name] = nil
			fmt.Printf("  %s: OK\n", node.Name)
		}
	}

	// Report results
	var failedNodes []string
	for nodeName, nodeErr := range results {
		if nodeErr != nil {
			failedNodes = append(failedNodes, fmt.Sprintf("%s (%v)", nodeName, nodeErr))
		}
	}

	if len(failedNodes) > 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GPU verification failed on %d/%d nodes: %v",
				len(failedNodes), len(gpuNodes), failedNodes))
	}

	fmt.Printf("Successfully verified GPU on all %d nodes\n", len(gpuNodes))
	return nil
}

func verifySingleGPUNode(ctx *validators.Context, nodeName string) error {
	podSuffix := sanitizeNodeName(nodeName)
	templateData := map[string]string{
		"POD_SUFFIX": podSuffix,
		"NODE_NAME":  nodeName,
		"NAMESPACE":  ctx.Namespace,
		"IMAGE":      getNvidiaSMIImage(ctx),
	}

	slog.Info("deploying nvidia-smi verify pod",
		"node", nodeName,
		"podName", "nvidia-smi-verify-"+podSuffix,
		"image", templateData["IMAGE"],
		"namespace", ctx.Namespace)

	// Load pod from template. The template uses tolerate-all (operator: Exists)
	// since the pod is pinned to a specific node via nodeName.
	pod, err := helper.LoadPodFromTemplate(nvidiaSMIPodTemplateFile, templateData)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to load pod template", err)
	}

	createdPod, err := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Create(ctx.Ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create verification pod", err)
	}

	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		if cleanupErr := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Delete(cleanupCtx, createdPod.Name, metav1.DeleteOptions{}); cleanupErr != nil {
			slog.Warn("failed to cleanup pod", "namespace", createdPod.Namespace, "pod", createdPod.Name, "error", cleanupErr)
		}
	}()

	// Use pkg/k8s/pod utilities directly.
	waitErr := podutil.WaitForPodSucceeded(ctx.Ctx, ctx.Clientset, ctx.Namespace, createdPod.Name, defaults.PodWaitTimeout)

	podLogs, logErr := podutil.GetPodLogs(ctx.Ctx, ctx.Clientset, ctx.Namespace, createdPod.Name, "")
	if logErr != nil {
		slog.Warn("failed to get logs for pod", "node", nodeName, "error", logErr)
		podLogs = fmt.Sprintf("failed to retrieve pod logs: %v", logErr)
	}

	if waitErr != nil {
		logSnippet := getLogSnippet(podLogs, nvidiaSMILogContextLines)

		// Capture pod status and events for debugging.
		debugInfo := collectPodDebugInfo(ctx, createdPod.Name)

		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("pod failed on node %s\n%s\nFirst %d lines:\n%s",
				nodeName, debugInfo, nvidiaSMILogContextLines, logSnippet), waitErr)
	}

	return verifyNvidiaSMILogs(podLogs, createdPod)
}

func getLogSnippet(logs string, maxLines int) string {
	lines := strings.Split(logs, "\n")
	if len(lines) <= maxLines {
		return logs
	}
	return strings.Join(lines[:maxLines], "\n")
}

func verifyNvidiaSMILogs(podLogs string, pod *v1.Pod) error {
	requiredStrings := []string{
		"NVIDIA-SMI",
		"Driver Version:",
		"CUDA Version:",
		gpuCheckSuccessMsg,
	}

	var missing []string
	for _, required := range requiredStrings {
		if !strings.Contains(podLogs, required) {
			missing = append(missing, required)
		}
	}

	if len(missing) > 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("log verification failed for pod %s/%s: missing %v",
				pod.Namespace, pod.Name, missing))
	}

	return nil
}

func getNvidiaSMIImage(_ *validators.Context) string {
	// Default image for most GPU types.
	// Future: read accelerator type from recipe to select GB200-specific image.
	return "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04"
}

// sanitizeNodeName converts a node name (e.g., ip-10-0-135-83.ec2.internal)
// into a valid DNS label for use in pod names (replaces dots with dashes).
func sanitizeNodeName(nodeName string) string {
	return strings.ReplaceAll(strings.ToLower(nodeName), ".", "-")
}

// collectPodDebugInfo captures container status and events for a failed pod.
func collectPodDebugInfo(ctx *validators.Context, podName string) string {
	var info strings.Builder

	pod, err := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Get(ctx.Ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf("could not retrieve pod status: %v", err)
	}

	// Container status
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			fmt.Fprintf(&info, "Container %s: waiting (%s: %s)\n", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
		if cs.State.Terminated != nil {
			fmt.Fprintf(&info, "Container %s: terminated (reason=%s exitCode=%d message=%s)\n",
				cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		}
	}

	// Pod events
	events, evtErr := ctx.Clientset.CoreV1().Events(ctx.Namespace).List(ctx.Ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + podName,
	})
	if evtErr == nil {
		for _, evt := range events.Items {
			if evt.Type == "Warning" {
				fmt.Fprintf(&info, "Event: %s — %s\n", evt.Reason, evt.Message)
			}
		}
	}

	if info.Len() == 0 {
		return "no additional debug info available"
	}
	return info.String()
}
