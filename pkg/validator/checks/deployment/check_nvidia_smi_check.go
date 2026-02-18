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

package deployment

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
	"github.com/NVIDIA/eidos/pkg/validator/helper"
	v1 "k8s.io/api/core/v1"
)

func init() {
	// Register this check
	checks.RegisterCheck(&checks.Check{
		Name:        "check-nvidia-smi",
		Description: "Check nvidia smi works on GPU Nodes and that means GPU nodes are configured correctly",
		Phase:       "deployment",
		TestName:    "TestCheckCheckNvidiaSmi",
	})
}

const (
	podWaitTimeout     = 10 * time.Minute
	logContextLines    = 20
	podTemplateFile    = "testdata/nvidia-smi-verify-pod.yaml"
	gpuCheckSuccessMsg = "GPU_CHECK_SUCCESS"
)

// validateCheckNvidiaSmi verifies that nvidia-smi works correctly on all GPU nodes.
// Returns nil if validation passes, error if it fails.
func validateCheckNvidiaSmi(ctx *checks.ValidationContext, t *testing.T) error {
	// Find schedulable GPU nodes
	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx)
	if err != nil {
		return fmt.Errorf("failed to query for GPU nodes: %w", err)
	}

	if len(gpuNodes) == 0 {
		t.Skip("No GPU nodes found in the cluster, skipping test")
		return nil
	}

	t.Logf("Found %d GPU node(s):", len(gpuNodes))
	for _, node := range gpuNodes {
		t.Logf(" - %s", node.Name)
	}

	// Check if any nodes are busy before proceeding
	busyNodes := findBusyNodes(ctx, t, gpuNodes)
	if len(busyNodes) > 0 {
		t.Skipf("Skipping test: %d GPU node(s) busy with existing workloads: %v", len(busyNodes), busyNodes)
		return nil
	}

	t.Logf("All %d GPU node(s) available. Proceeding with verification.", len(gpuNodes))

	// Verify each GPU node
	results := make(map[string]error)
	for _, node := range gpuNodes {
		t.Logf("--- Verifying Node: %s ---", node.Name)
		if err := verifySingleNode(ctx, t, node.Name); err != nil {
			results[node.Name] = err
			t.Logf("Verification failed for node %s: %v", node.Name, err)
		} else {
			results[node.Name] = nil
			t.Logf("Successfully verified GPU on node %s", node.Name)
		}
	}

	// Aggregate and report results
	return reportResults(t, results, len(gpuNodes))
}

// findBusyNodes checks which GPU nodes are currently running GPU workloads.
// Nodes that error during the check are treated as busy to be safe.
func findBusyNodes(ctx *checks.ValidationContext, t *testing.T, nodes []v1.Node) []string {
	t.Log("Checking GPU node busy status...")
	var busyNodes []string

	for _, node := range nodes {
		busy, err := helper.IsNodeGpuBusy(ctx.Context, ctx.Clientset, node.Name)
		if err != nil {
			t.Logf("Warning: Error checking busy status for node %s: %v. Treating as busy.", node.Name, err)
			busyNodes = append(busyNodes, fmt.Sprintf("%s (error checking status)", node.Name))
			continue
		}
		if busy {
			busyNodes = append(busyNodes, node.Name)
		}
	}

	return busyNodes
}

// verifySingleNode runs a verification pod on a single node and checks the results.
func verifySingleNode(ctx *checks.ValidationContext, t *testing.T, nodeName string) error {
	templateData := map[string]string{
		"NODE_NAME": strings.ToLower(nodeName),
		"NAMESPACE": ctx.Namespace,
		"IMAGE":     getGpuVerifyImage(ctx),
	}

	podHelper := &helper.PodLifecycle{
		ClientSet:  ctx.Clientset,
		RESTConfig: ctx.RESTConfig,
		Namespace:  ctx.Namespace,
		T:          t,
	}

	// Create verification pod
	pod, err := podHelper.CreatePodFromTemplate(ctx.Context, podTemplateFile, templateData)
	if err != nil {
		return fmt.Errorf("failed to create pod: %w", err)
	}

	// Ensure cleanup happens regardless of success/failure
	defer func() {
		if cleanupErr := podHelper.CleanupPod(ctx.Context, pod); cleanupErr != nil {
			t.Logf("Warning: Failed to cleanup pod %s/%s: %v", pod.Namespace, pod.Name, cleanupErr)
		}
	}()

	// Wait for pod to complete
	waitErr := podHelper.WaitForPodSuccess(ctx.Context, pod, podWaitTimeout)

	// Retrieve logs for analysis
	podLogs, err := podHelper.GetPodLogs(ctx.Context, pod)
	if err != nil {
		t.Logf("Warning: Failed to get logs for pod on node %s: %v", nodeName, err)
		podLogs = fmt.Sprintf("Failed to retrieve pod logs: %v", err)
	}

	// Check if pod failed
	if waitErr != nil {
		logSnippet := getLogSnippet(podLogs, logContextLines)
		return fmt.Errorf("pod failed on node %s: %w\nFirst %d lines of logs:\n%s",
			nodeName, waitErr, logContextLines, logSnippet)
	}

	// Verify log content for expected markers
	return verifyLogContent(t, podLogs, pod)
}

// getLogSnippet returns the first N lines of logs safely.
func getLogSnippet(logs string, maxLines int) string {
	lines := strings.Split(logs, "\n")
	if len(lines) <= maxLines {
		return logs
	}
	return strings.Join(lines[:maxLines], "\n")
}

// verifyLogContent checks that the pod logs contain expected GPU verification markers.
func verifyLogContent(t *testing.T, podLogs string, pod *v1.Pod) error {
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
		// Log the failure for test output but don't use assert (which doesn't return bool reliably)
		t.Errorf("Log verification failed for pod %s/%s: missing required strings: %v",
			pod.Namespace, pod.Name, missing)
		return fmt.Errorf("log verification failed: missing %v", missing)
	}

	return nil
}

// reportResults aggregates verification results and returns an error if any node failed.
func reportResults(t *testing.T, results map[string]error, totalNodes int) error {
	t.Log("--- Overall Verification Results ---")

	var successfulNodes, failedNodes []string
	for nodeName, err := range results {
		if err == nil {
			successfulNodes = append(successfulNodes, nodeName)
		} else {
			failedNodes = append(failedNodes, fmt.Sprintf("%s (%v)", nodeName, err))
		}
	}

	t.Logf("Successfully verified nodes: %v", successfulNodes)

	if len(failedNodes) > 0 {
		t.Logf("Failed verification on nodes: %v", failedNodes)
		return fmt.Errorf("GPU verification failed on %d/%d nodes: %v",
			len(failedNodes), totalNodes, failedNodes)
	}

	if len(successfulNodes) != totalNodes {
		return fmt.Errorf("verification count mismatch: expected %d nodes, verified %d",
			totalNodes, len(successfulNodes))
	}

	t.Logf("Successfully verified GPU functionality on all %d nodes", len(successfulNodes))
	return nil
}

// getGpuVerifyImage returns the appropriate CUDA image based on accelerator type.
func getGpuVerifyImage(ctx *checks.ValidationContext) string {
	// Safe type assertion with default fallback
	if ctx.RecipeData != nil {
		if accelerator, ok := ctx.RecipeData["accelerator"].(recipe.CriteriaAcceleratorType); ok {
			if accelerator == recipe.CriteriaAcceleratorGB200 {
				return "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu24.04"
			}
		}
	}
	// Default image for most GPU types
	return "nvcr.io/nvidia/cuda:13.0.0-base-ubuntu22.04"
}
