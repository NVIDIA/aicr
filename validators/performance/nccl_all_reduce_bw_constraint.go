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
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	testType       = "all_reduce_perf"
	minMessageSize = "1K"
	maxMessageSize = "16G"

	// maxMessageSizeTCP is a reduced upper bound for clusters without
	// high-bandwidth interconnect (e.g. EFA). Multi-GB all_reduce over TCP
	// can hang or take unreasonably long with 16+ ranks.
	maxMessageSizeTCP = "4G"

	// ncclTrainJobName is the name used for both the TrainJob resource and the label
	// selector when waiting for the launcher pod. Must stay in sync with trainjob.yaml.
	ncclTrainJobName = "nccl-all-reduce-tj"

	// ncclTrainingRuntimeName is the name of the TrainingRuntime resource.
	// Must stay in sync with runtime.yaml.
	ncclTrainingRuntimeName = "nccl-all-reduce-runtime"
)

// Package-level GVR definitions for Kubeflow Trainer CRDs used by both
// applyNCCLResources and cleanupNCCLResources.
var (
	trainJobGVR = schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
		Resource: "trainjobs",
	}

	trainingRuntimeGVR = schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
		Resource: "trainingruntimes",
	}
)

// ncclBandwidthRe matches any data row in NCCL all-reduce output and captures the
// out-of-place busbw column. parseBandwidthFromLogs uses the last match (largest message size).
// EKS max is 16G (17179869184), GKE max is 8G (8589934592) — this regex handles both.
var ncclBandwidthRe = regexp.MustCompile(`\s+(\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+[\d.]+\s+([\d.]+)`)

// templatePath returns the path to a testdata template file for the given
// accelerator and service combination: testdata/{accelerator}/{service}/{filename}
func templatePath(accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, filename string) string {
	return filepath.Join("testdata", string(accelerator), string(service), filename)
}

// supportedNCCLCombinations maps each supported cloud service to the accelerator
// types for which the automated NCCL all-reduce test has been implemented.
// All platforms use Kubeflow TrainJob + MPI with per-platform TrainingRuntimes
// and a shared TrainJob.
var supportedNCCLCombinations = map[recipe.CriteriaServiceType][]recipe.CriteriaAcceleratorType{
	recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorH100},
	recipe.CriteriaServiceGKE: {recipe.CriteriaAcceleratorH100},
}

// validateNcclAllReduceBw validates NCCL All Reduce bandwidth using Kubeflow TrainJob + MPI.
// Each platform has its own TrainingRuntime; the TrainJob is shared (just runtimeRef + numNodes).
// Returns actual bandwidth value, whether it passed the threshold, and any error.
func validateNcclAllReduceBw(ctx *validators.Context, constraint recipe.Constraint) (string, bool, error) {
	slog.Info("Starting NCCL All Reduce bandwidth validation")

	// Skip unless the recipe targets a supported service + accelerator combination.
	if ctx.Recipe == nil || ctx.Recipe.Criteria == nil {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: no recipe criteria")
		return "skipped - requires Service + Accelerator", true, nil
	}

	service := ctx.Recipe.Criteria.Service
	accelerator := ctx.Recipe.Criteria.Accelerator

	supported := false
	if supportedAccelerators, ok := supportedNCCLCombinations[service]; ok {
		for _, a := range supportedAccelerators {
			if accelerator == a {
				supported = true
				break
			}
		}
	}

	if !supported {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: unsupported service/accelerator combination",
			"service", service, "accelerator", accelerator)
		return "skipped - requires Service + Accelerator to be implemented", true, nil
	}

	// Extract threshold from constraint
	threshold, err := parseThreshold(constraint.Value)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid threshold", err)
	}
	slog.Info("Target bandwidth threshold", "threshold", threshold, "tolerance", "10%")

	// Determine GPU configuration from cluster.
	gpuConfig, err := determineGPUConfig(ctx)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine GPU configuration", err)
	}
	slog.Info("GPU Configuration", "nodes", gpuConfig.WorkerCount, ", GPUs/node", gpuConfig.GPUCountPerNode, ", total GPUs", gpuConfig.TotalGPUCount)

	// NCCL all-reduce tests EW (East-West) fabric between nodes and requires at least
	// two GPU nodes. Skip gracefully rather than fail when only one node is available.
	if gpuConfig.WorkerCount < 2 {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: requires at least 2 GPU nodes for EW fabric test",
			"nodes", gpuConfig.WorkerCount)
		return "skipped - requires at least 2 GPU nodes for EW fabric test", true, nil
	}

	// Run the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
	// Each platform has a per-platform TrainingRuntime with all platform-specific
	// configuration (image, mpirun args, resources, sidecars). The TrainJob is shared.
	logs, err := runNCCLTrainJob(ctx, gpuConfig, accelerator, service)
	if err != nil {
		return "", false, err
	}

	// Parse bandwidth from logs (shared across all service types).
	bandwidth, err := parseBandwidthFromLogs(logs)
	if err != nil {
		return logs, false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse bandwidth from logs", err)
	}

	slog.Info("Measured bandwidth", "bandwidth", bandwidth)

	// Check if bandwidth meets threshold (within 10% tolerance)
	passed := bandwidth >= (threshold * 0.9)
	actualValue := fmt.Sprintf("%.2f GB/s", bandwidth)

	if passed {
		slog.Info("Bandwidth validation passed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "10%")
	} else {
		slog.Info("Bandwidth validation failed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "10%")
	}

	return actualValue, passed, nil
}

// runNCCLTrainJob runs the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
// It applies the per-platform TrainingRuntime and shared TrainJob, waits for the launcher
// pod to complete, and returns the benchmark logs.
func runNCCLTrainJob(ctx *validators.Context, gpuConfig *gpuConfiguration,
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) (string, error) {

	dynamicClient := ctx.DynamicClient

	// Ensure Kubeflow Trainer is installed. If it is already present we leave it
	// alone; if we install it we clean it up after the test completes.
	trainerInstalled, err := isTrainerInstalled(ctx.Ctx, dynamicClient)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to check Kubeflow Trainer installation", err)
	}
	if !trainerInstalled {
		slog.Info("Kubeflow Trainer not found, installing...")
		var installedResources []trainerResourceRef
		installedResources, err = installTrainer(ctx.Ctx, dynamicClient, ctx.Clientset.Discovery())
		if err != nil {
			return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to install Kubeflow Trainer", err)
		}
		defer deleteTrainer(dynamicClient, installedResources)
		slog.Info("Kubeflow Trainer installed", "resources", len(installedResources))
	} else {
		slog.Info("Kubeflow Trainer already installed, proceeding")
	}

	// Apply runtime and trainjob resources.
	if applyErr := applyNCCLResources(ctx, dynamicClient, gpuConfig, accelerator, service); applyErr != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL resources", applyErr)
	}
	defer cleanupNCCLResources(dynamicClient, gpuConfig.Namespace)

	podHelper := &helper.PodLifecycle{
		ClientSet: ctx.Clientset,
		Namespace: ctx.Namespace,
	}

	// Wait for launcher pod and get logs.
	logs, err := waitForLauncherPodAndGetLogs(ctx, podHelper)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get launcher logs", err)
	}

	return logs, nil
}

// gpuConfiguration holds GPU node and count information
type gpuConfiguration struct {
	WorkerCount     int
	GPUCountPerNode int
	TotalGPUCount   int
	Namespace       string
	Nodes           []v1.Node
}

// parseThreshold extracts the numeric threshold value from a constraint value.
// Handles formats like "450", "450 GB/s", ">= 400", ">= 100 GB/s".
func parseThreshold(value string) (float64, error) {
	numStr := strings.TrimSpace(value)
	// Strip comparison operator prefix (>=, >, <=, <, ==, =)
	numStr = strings.TrimLeft(numStr, "><=! ")
	numStr = strings.TrimSpace(numStr)
	// Strip units suffix (e.g., "GB/s")
	numStr = strings.Split(numStr, " ")[0]

	if numStr == "" {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid threshold: no numeric value found in %q", value))
	}

	threshold, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid threshold format", err)
	}

	return threshold, nil
}

// determineGPUConfig analyzes the snapshot to determine GPU node configuration
func determineGPUConfig(ctx *validators.Context) (*gpuConfiguration, error) {
	slog.Info("Analyzing GPU node configuration...")

	// Find schedulable GPU nodes
	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to find GPU nodes", err)
	}

	if len(gpuNodes) == 0 {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "no schedulable GPU nodes found")
	}

	slog.Info("Found GPU nodes", "count", len(gpuNodes))

	// Get GPU count from first node (assuming homogeneous cluster)
	firstNode := gpuNodes[0]
	gpuResource := v1.ResourceName("nvidia.com/gpu")
	gpuQuantity := firstNode.Status.Allocatable[gpuResource]
	gpuCountPerNode := int(gpuQuantity.Value())

	if gpuCountPerNode == 0 {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "no GPUs found on nodes")
	}

	totalGPUs := len(gpuNodes) * gpuCountPerNode

	return &gpuConfiguration{
		WorkerCount:     len(gpuNodes),
		GPUCountPerNode: gpuCountPerNode,
		TotalGPUCount:   totalGPUs,
		Namespace:       ctx.Namespace,
		Nodes:           gpuNodes,
	}, nil
}

// applyNCCLResources applies the per-platform TrainingRuntime and shared TrainJob
// YAML files with template substitution using the dynamic client.
// Runtime: testdata/{accelerator}/{service}/runtime.yaml (per-platform, contains all config)
// TrainJob: testdata/trainjob.yaml (shared, just runtimeRef + numNodes)
func applyNCCLResources(ctx *validators.Context, dynamicClient dynamic.Interface, config *gpuConfiguration, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) error {
	slog.Info("Applying NCCL test resources...", "accelerator", accelerator, "service", service)

	templateData := map[string]string{
		"NAMESPACE":          config.Namespace,
		"WORKER_COUNT":       strconv.Itoa(config.WorkerCount),
		"GPU_COUNT_PER_NODE": strconv.Itoa(config.GPUCountPerNode),
		"GPU_COUNT":          strconv.Itoa(config.TotalGPUCount),
		"TEST_TYPE":          testType,
		"MIN_MESSAGE_SIZE":   minMessageSize,
		"MAX_MESSAGE_SIZE":   maxMessageSize,
	}

	// For GKE, discover GPU NIC network names (cluster-specific prefixes).
	if service == recipe.CriteriaServiceGKE {
		gpuNICs, err := discoverGKEGPUNICNetworks(ctx.Ctx, dynamicClient)
		if err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to discover GKE GPU NIC networks", err)
		}
		if len(gpuNICs) < 8 {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("expected 8 GPU NIC networks, found %d — cluster may not have multi-NIC networking configured", len(gpuNICs)))
		}
		templateData["GKE_NETWORK_INTERFACES"] = buildGKENetworkInterfacesAnnotation(gpuNICs)
		templateData["NRI_DEVICE_ANNOTATION"] = buildNRIDeviceAnnotation(config.GPUCountPerNode)
		slog.Info("Discovered GKE GPU NIC networks", "count", len(gpuNICs), "networks", gpuNICs)
	}

	// For EKS, discover instance type and EFA adapter count from GPU nodes.
	// EFA count of 0 is valid — NCCL falls back to TCP (slower but functional).
	if service == recipe.CriteriaServiceEKS {
		warnIfHeterogeneousNodes(config.Nodes)
		instanceType, efaCount, err := discoverEKSNodeConfig(config.Nodes[0])
		if err != nil {
			return err
		}
		templateData["INSTANCE_TYPE"] = instanceType
		// Indentation matches the resource block position in runtime.yaml.
		const efaIndent = "                      "
		templateData["EFA_RESOURCE_LIMITS"] = buildEFAResourceLine(efaCount, efaIndent)
		templateData["EFA_RESOURCE_REQUESTS"] = buildEFAResourceLine(efaCount, efaIndent)
		if efaCount == 0 {
			templateData["MAX_MESSAGE_SIZE"] = maxMessageSizeTCP
			slog.Warn("No EFA adapters found — NCCL will use TCP (reduced bandwidth)",
				"instanceType", instanceType, "maxMessageSize", maxMessageSizeTCP)
		} else {
			slog.Info("Discovered EKS node configuration", "instanceType", instanceType, "efaCount", efaCount)
		}
	}

	// Apply per-platform runtime: testdata/{accelerator}/{service}/runtime.yaml
	if err := applyYAMLWithDynamicClient(ctx.Ctx, dynamicClient, trainingRuntimeGVR, config.Namespace, templatePath(accelerator, service, "runtime.yaml"), templateData); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply training runtime", err)
	}
	slog.Info("Applied TrainingRuntime", "service", service)

	// Wait for the runtime to be visible to the Trainer admission webhook.
	// The webhook validates that the referenced runtime exists before allowing
	// TrainJob creation; without this wait we hit a race condition.
	if err := waitForTrainingRuntime(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "TrainingRuntime not ready", err)
	}

	// Apply shared trainjob: testdata/trainjob.yaml
	trainjobPath := filepath.Join("testdata", "trainjob.yaml")
	if err := applyYAMLWithDynamicClient(ctx.Ctx, dynamicClient, trainJobGVR, config.Namespace, trainjobPath, templateData); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply train job", err)
	}
	slog.Info("Applied TrainJob")

	return nil
}

// applyYAMLWithDynamicClient reads a YAML template, performs substitution, and applies it using dynamic client
func applyYAMLWithDynamicClient(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace, templatePath string, data map[string]string) error {
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read template", err)
	}

	// Perform template substitution
	yamlContent := string(content)
	for key, value := range data {
		yamlContent = strings.ReplaceAll(yamlContent, "${"+key+"}", value)
	}

	// Parse YAML to unstructured object
	obj := &unstructured.Unstructured{}
	if unmarshalErr := yaml.Unmarshal([]byte(yamlContent), obj); unmarshalErr != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse YAML", unmarshalErr)
	}

	// Apply with timeout
	applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	// Create the resource
	_, err = dynamicClient.Resource(gvr).Namespace(namespace).Create(applyCtx, obj, metav1.CreateOptions{})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create resource", err)
	}

	return nil
}

// waitForLauncherPodAndGetLogs waits for the launcher pod to be created and retrieves logs
func waitForLauncherPodAndGetLogs(ctx *validators.Context, podHelper *helper.PodLifecycle) (string, error) {
	slog.Info("Waiting for launcher pod to be created...")

	// Wait for launcher pod to be created (pattern: nccl-all-reduce-tj-launcher-*)
	launcherPod, err := waitForPodByLabelSelector(
		ctx.Ctx,
		ctx.Clientset,
		ctx.Namespace,
		fmt.Sprintf("jobset.sigs.k8s.io/jobset-name=%s,jobset.sigs.k8s.io/replicatedjob-name=launcher", ncclTrainJobName),
		defaults.NCCLLauncherPodTimeout,
	)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "failed to find launcher pod", err)
	}

	slog.Info("Found launcher pod", "name", launcherPod.Name)

	// Wait for pod to complete using helper method
	err = podHelper.WaitForPodSuccess(ctx.Ctx, launcherPod, defaults.NCCLTrainJobTimeout)
	if err != nil {
		// Get logs even if pod failed for debugging
		slog.Info("Pod did not succeed, retrieving logs for debugging...")
		logs, _ := podHelper.GetPodLogs(ctx.Ctx, launcherPod)
		return logs, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "pod failed to complete successfully", err)
	}

	// Get logs from completed pod using helper method
	slog.Info("Retrieving logs from successful pod...")
	logs, err := podHelper.GetPodLogs(ctx.Ctx, launcherPod)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get pod logs", err)
	}

	return logs, nil
}

// waitForTrainingRuntime polls until the TrainingRuntime is visible via GET.
// The Trainer admission webhook validates that the referenced runtime exists
// before allowing TrainJob creation; a brief propagation delay can cause a race.
func waitForTrainingRuntime(ctx context.Context, dynamicClient dynamic.Interface, namespace string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	for {
		_, err := dynamicClient.Resource(trainingRuntimeGVR).Namespace(namespace).Get(waitCtx, ncclTrainingRuntimeName, metav1.GetOptions{})
		if err == nil {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for TrainingRuntime to be visible", waitCtx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForPodByLabelSelector waits for a pod matching the label selector to be created.
// Uses the Watch API for efficiency instead of polling.
func waitForPodByLabelSelector(ctx context.Context, clientset kubernetes.Interface, namespace, labelSelector string, timeout time.Duration) (*v1.Pod, error) {
	slog.Info("Watching for pod with selector", "selector", labelSelector)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch pods", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timeout waiting for pod", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "pod watch channel closed unexpectedly")
			}
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				continue
			}
			slog.Info("Found launcher pod", "name", pod.Name)
			return pod, nil
		}
	}
}

// parseBandwidthFromLogs extracts the bus bandwidth value from NCCL test logs.
// It finds all data rows and returns the out-of-place busbw from the last row
// (largest message size). This works regardless of max message size:
// EKS uses 16G (17179869184), GKE uses 8G (8589934592).
func parseBandwidthFromLogs(logs string) (float64, error) {
	// NCCL test output format example:
	// #       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
	// #        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
	//  8589934592    2147483648     float     sum      -1   48298   177.85  333.47      0   48292   177.87  333.51      0

	allMatches := ncclBandwidthRe.FindAllStringSubmatch(logs, -1)
	if len(allMatches) == 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal, "could not find bandwidth value in logs")
	}

	// Last match = largest message size row.
	lastMatch := allMatches[len(allMatches)-1]
	slog.Info("Parsing bandwidth from largest message size row", "bytes", lastMatch[1])

	bandwidth, err := strconv.ParseFloat(lastMatch[2], 64)
	if err != nil {
		return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse bandwidth value", err)
	}

	return bandwidth, nil
}

// cleanupNCCLResources removes the trainjob and runtime resources using dynamic client
func cleanupNCCLResources(dynamicClient dynamic.Interface, namespace string) {
	slog.Info("Cleaning up NCCL test resources...")

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.DiagnosticTimeout)
	defer cancel()

	// Delete trainjob
	err := dynamicClient.Resource(trainJobGVR).Namespace(namespace).Delete(cleanupCtx, ncclTrainJobName, metav1.DeleteOptions{})
	if err != nil {
		slog.Error("Warning: Failed to delete TrainJob", "error", err)
	} else {
		slog.Info("Deleted TrainJob")
	}

	// Delete runtime
	err = dynamicClient.Resource(trainingRuntimeGVR).Namespace(namespace).Delete(cleanupCtx, ncclTrainingRuntimeName, metav1.DeleteOptions{})
	if err != nil {
		slog.Error("Warning: Failed to delete TrainingRuntime", "error", err)
	} else {
		slog.Info("Deleted TrainingRuntime")
	}
}
