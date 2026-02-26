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

package performance

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	"github.com/NVIDIA/aicr/pkg/validator/helper"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	trainJobTimeout    = 30 * time.Minute
	launcherPodTimeout = 5 * time.Minute
	testType           = "all_reduce_perf"
	minMessageSize     = "1K"
	maxMessageSize     = "16G"
)

// templatePath returns the path to a testdata template file for the given
// accelerator and service combination: testdata/{accelerator}/{service}/{filename}
func templatePath(accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, filename string) string {
	return fmt.Sprintf("testdata/%s/%s/%s", accelerator, service, filename)
}

func init() {
	// Register this constraint validator
	checks.RegisterConstraintValidator(&checks.ConstraintValidator{
		Pattern:     "nccl-all-reduce-bw",
		Description: "Verify NCCL All Reduce Bus Bandwidth within 10% of threshold",
		Func:        validateNcclAllReduceBw,
		TestName:    "TestNcclAllReduceBw",
		Phase:       "performance",
	})
}

// supportedNCCLCombinations maps each supported cloud service to the accelerator
// types for which the NCCL all-reduce test has been implemented. The test uses
// p5.48xlarge-specific templates with EFA networking and NVSwitch topology, so
// only EKS+H100 is currently supported.
var supportedNCCLCombinations = map[recipe.CriteriaServiceType][]recipe.CriteriaAcceleratorType{
	recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorH100},
}

// validateNcclAllReduceBw validates NCCL All Reduce bandwidth by running a TrainJob.
// It applies the runtime and trainjob YAML templates, waits for completion,
// and extracts bandwidth metrics from the launcher pod logs.
// Returns actual bandwidth value, whether it passed the threshold, and any error.
func validateNcclAllReduceBw(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
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

	// Create dynamic client for CRD operations (needed for trainer check and NCCL resources).
	dynamicClient, err := dynamic.NewForConfig(ctx.RESTConfig)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create dynamic client", err)
	}

	// Ensure Kubeflow Trainer is installed.  If it is already present we leave it
	// alone; if we install it we clean it up after the test completes.
	trainerInstalled, err := isTrainerInstalled(ctx.Context, dynamicClient)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to check Kubeflow Trainer installation", err)
	}
	if !trainerInstalled {
		slog.Info("Kubeflow Trainer not found, installing...")
		var installedResources []trainerResourceRef
		installedResources, err = installTrainer(ctx.Context, dynamicClient, ctx.Clientset.Discovery())
		if err != nil {
			return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to install Kubeflow Trainer", err)
		}
		defer deleteTrainer(dynamicClient, installedResources)
		slog.Info("Kubeflow Trainer installed", "resources", len(installedResources))
	} else {
		slog.Info("Kubeflow Trainer already installed, proceeding")
	}

	// Determine GPU configuration from snapshot
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

	// Apply runtime and trainjob resources
	if applyErr := applyNCCLResources(ctx, dynamicClient, gpuConfig, accelerator, service); applyErr != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL resources", applyErr)
	}

	// Ensure cleanup
	defer cleanupNCCLResources(dynamicClient, gpuConfig.Namespace)

	// Create pod helper for launcher pod operations
	podHelper := &helper.PodLifecycle{
		ClientSet:  ctx.Clientset,
		RESTConfig: ctx.RESTConfig,
		Namespace:  ctx.Namespace,
	}

	// Wait for launcher pod and get logs
	logs, err := waitForLauncherPodAndGetLogs(ctx, podHelper)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get launcher logs", err)
	}

	// Parse bandwidth from logs
	bandwidth, err := parseBandwidthFromLogs(logs)
	if err != nil {
		return logs, false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse bandwidth from logs", err)
	}

	slog.Info("Measured bandwidth", "bandwidth", bandwidth)

	// Check if bandwidth meets threshold (within 10% tolerance)
	passed := bandwidth >= (threshold * 0.9)
	actualValue := fmt.Sprintf("%.2f GB/s", bandwidth)

	if passed {
		slog.Info("Bandwidth validation passed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "90%")
	} else {
		slog.Info("Bandwidth validation failed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "90%")
	}

	return actualValue, passed, nil
}

// gpuConfiguration holds GPU node and count information
type gpuConfiguration struct {
	WorkerCount     int
	GPUCountPerNode int
	TotalGPUCount   int
	Namespace       string
}

// parseThreshold extracts the numeric threshold value from constraint
func parseThreshold(value string) (float64, error) {
	// Remove units and parse (e.g., "450 GB/s" -> 450)
	numStr := strings.TrimSpace(value)
	numStr = strings.Split(numStr, " ")[0]

	threshold, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid threshold format: %w", err)
	}

	return threshold, nil
}

// determineGPUConfig analyzes the snapshot to determine GPU node configuration
func determineGPUConfig(ctx *checks.ValidationContext) (*gpuConfiguration, error) {
	slog.Info("Analyzing GPU node configuration...")

	// Find schedulable GPU nodes
	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx)
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
	}, nil
}

// applyNCCLResources applies the runtime and trainjob YAML files with template substitution using dynamic client
func applyNCCLResources(ctx *checks.ValidationContext, dynamicClient dynamic.Interface, config *gpuConfiguration, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) error {
	slog.Info("Applying NCCL test resources...")

	templateData := map[string]string{
		"NAMESPACE":          config.Namespace,
		"WORKER_COUNT":       strconv.Itoa(config.WorkerCount),
		"GPU_COUNT_PER_NODE": strconv.Itoa(config.GPUCountPerNode),
		"GPU_COUNT":          strconv.Itoa(config.TotalGPUCount),
		"TEST_TYPE":          testType,
		"MIN_MESSAGE_SIZE":   minMessageSize,
		"MAX_MESSAGE_SIZE":   maxMessageSize,
	}

	// Define GVRs for TrainingRuntime and TrainJob
	trainingRuntimeGVR := schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
		Resource: "trainingruntimes",
	}

	trainJobGVR := schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
		Resource: "trainjobs",
	}

	// Apply runtime first
	if err := applyYAMLWithDynamicClient(ctx.Context, dynamicClient, trainingRuntimeGVR, config.Namespace, templatePath(accelerator, service, "runtime.yaml"), templateData); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply training runtime", err)
	}
	slog.Info("Applied TrainingRuntime")

	// Apply trainjob
	if err := applyYAMLWithDynamicClient(ctx.Context, dynamicClient, trainJobGVR, config.Namespace, templatePath(accelerator, service, "trainjob.yaml"), templateData); err != nil {
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
func waitForLauncherPodAndGetLogs(ctx *checks.ValidationContext, podHelper *helper.PodLifecycle) (string, error) {
	slog.Info("Waiting for launcher pod to be created...")

	// Wait for launcher pod to be created (pattern: nccl-all-reduce-tj-launcher-*)
	launcherPod, err := waitForPodByLabelSelector(
		ctx.Context,
		ctx.Clientset,
		ctx.Namespace,
		"jobset.sigs.k8s.io/jobset-name=nccl-all-reduce-tj,jobset.sigs.k8s.io/replicatedjob-name=launcher",
		launcherPodTimeout,
	)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "failed to find launcher pod", err)
	}

	slog.Info("Found launcher pod", "name", launcherPod.Name)

	// Wait for pod to complete using helper method
	err = podHelper.WaitForPodSuccess(ctx.Context, launcherPod, trainJobTimeout)
	if err != nil {
		// Get logs even if pod failed for debugging
		slog.Info("Pod did not succeed, retrieving logs for debugging...")
		logs, _ := podHelper.GetPodLogs(ctx.Context, launcherPod)
		return logs, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "pod failed to complete successfully", err)
	}

	// Get logs from completed pod using helper method
	slog.Info("Retrieving logs from successful pod...")
	logs, err := podHelper.GetPodLogs(ctx.Context, launcherPod)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get pod logs", err)
	}

	return logs, nil
}

// waitForPodByLabelSelector waits for a pod matching the label selector to be created
func waitForPodByLabelSelector(ctx context.Context, clientset kubernetes.Interface, namespace, labelSelector string, timeout time.Duration) (*v1.Pod, error) {
	slog.Info("Polling for pod with selector", "selector", labelSelector)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(defaults.PodPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timeout waiting for pod", ctx.Err())
		case <-ticker.C:
			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				slog.Error("Error listing pods, retrying...", "error", err)
				continue
			}
			if len(pods.Items) > 0 {
				return &pods.Items[0], nil
			}
			slog.Info("Pod not found yet, continuing to poll...")
		}
	}
}

// parseBandwidthFromLogs extracts the bus bandwidth value from NCCL test logs
func parseBandwidthFromLogs(logs string) (float64, error) {
	// NCCL test output format example:
	// #       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
	// #        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
	//  17179869184    4294967296     float     sum      -1   123456   139.2   450.3      0   123456   139.2   450.3      0

	// Look for the line with the max message size (16G = 17179869184 bytes)
	re := regexp.MustCompile(`\s+17179869184\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+[\d.]+\s+([\d.]+)`)
	matches := re.FindStringSubmatch(logs)

	if len(matches) < 2 {
		return 0, fmt.Errorf("could not find bandwidth value in logs")
	}

	bandwidth, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse bandwidth value: %w", err)
	}

	return bandwidth, nil
}

// cleanupNCCLResources removes the trainjob and runtime resources using dynamic client
func cleanupNCCLResources(dynamicClient dynamic.Interface, namespace string) {
	slog.Info("Cleaning up NCCL test resources...")

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.DiagnosticTimeout)
	defer cancel()

	// Define GVRs
	trainJobGVR := schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
		Resource: "trainjobs",
	}

	trainingRuntimeGVR := schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
		Resource: "trainingruntimes",
	}

	// Delete trainjob
	err := dynamicClient.Resource(trainJobGVR).Namespace(namespace).Delete(cleanupCtx, "nccl-all-reduce-tj", metav1.DeleteOptions{})
	if err != nil {
		slog.Error("Warning: Failed to delete TrainJob", "error", err)
	} else {
		slog.Info("Deleted TrainJob")
	}

	// Delete runtime
	err = dynamicClient.Resource(trainingRuntimeGVR).Namespace(namespace).Delete(cleanupCtx, "nccl-all-reduce-runtime", metav1.DeleteOptions{})
	if err != nil {
		slog.Error("Warning: Failed to delete TrainingRuntime", "error", err)
	} else {
		slog.Info("Deleted TrainingRuntime")
	}
}
