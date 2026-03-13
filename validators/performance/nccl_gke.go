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
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	// gkeNCCLHost1PodName is the name of the first NCCL test pod.
	// Must stay in sync with testdata/h100/gke/nccl-test-tcpxo.yaml.
	gkeNCCLHost1PodName = "nccl-test-host-1"

	// gkeNCCLHost2PodName is the name of the second NCCL test pod.
	// Must stay in sync with testdata/h100/gke/nccl-test-tcpxo.yaml.
	gkeNCCLHost2PodName = "nccl-test-host-2"

	// gkeNCCLContainerName is the container that runs the NCCL benchmark.
	// Must stay in sync with testdata/h100/gke/nccl-test-tcpxo.yaml.
	gkeNCCLContainerName = "nccl-test"

	// gkeNCCLHost1ServiceName is the headless service for DNS resolution of host-1.
	// Must stay in sync with testdata/h100/gke/nccl-test-tcpxo.yaml.
	gkeNCCLHost1ServiceName = "nccl-host-1"

	// gkeNCCLHost2ServiceName is the headless service for DNS resolution of host-2.
	// Must stay in sync with testdata/h100/gke/nccl-test-tcpxo.yaml.
	gkeNCCLHost2ServiceName = "nccl-host-2"
)

// gkeResources tracks resources created during a GKE NCCL test for cleanup.
type gkeResources struct {
	PodNames     []string
	ServiceNames []string
	Namespace    string
}

// runGKENCCLTest runs the NCCL all-reduce benchmark on GKE using raw Pods with TCPXO sidecar.
// Unlike the EKS path (TrainJob + MPI launcher), GKE requires:
//   - Raw Pods with tcpxo-daemon sidecar for GPUDirect TCPXO
//   - hostNetwork: true for PCI sysfs visibility
//   - kubectl exec to trigger the benchmark (not MPI)
func runGKENCCLTest(ctx *validators.Context, gpuConfig *gpuConfiguration,
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) (string, error) {

	slog.Info("Running GKE NCCL test with TCPXO sidecar pods")

	// Best-effort pre-cleanup: remove stale resources from a prior failed run.
	// Pods are immutable (can't Update), so delete-before-create is the correct
	// idempotency strategy for re-runs after partial failure.
	cleanupGKEResources(ctx.Clientset, &gkeResources{
		PodNames:     []string{gkeNCCLHost1PodName, gkeNCCLHost2PodName},
		ServiceNames: []string{gkeNCCLHost1ServiceName, gkeNCCLHost2ServiceName},
		Namespace:    gpuConfig.Namespace,
	})

	// Apply Services + Pods from the multi-doc template.
	pods, resources, err := applyGKEResources(ctx.Ctx, ctx.Clientset, gpuConfig, accelerator, service)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply GKE NCCL resources", err)
	}
	defer cleanupGKEResources(ctx.Clientset, resources)

	podHelper := &helper.PodLifecycle{
		ClientSet:  ctx.Clientset,
		RESTConfig: ctx.RESTConfig,
		Namespace:  ctx.Namespace,
	}

	// Wait for both pods to be Ready (all containers initialized).
	// Running phase alone doesn't guarantee the TCPXO sidecar is initialized;
	// the Ready condition confirms all containers are ready for traffic.
	for _, pod := range pods {
		slog.Info("Waiting for pod to be Ready", "name", pod.Name)
		if waitErr := podHelper.WaitForPodReady(ctx.Ctx, pod, defaults.NCCLGKEPodReadyTimeout); waitErr != nil {
			return "", aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "GKE NCCL pod failed to reach Ready", waitErr)
		}
		slog.Info("Pod is Ready", "name", pod.Name)
	}

	// Exec the NCCL all-reduce benchmark from host-1.
	// The allreduce.sh script is created inline by the pod's entrypoint and calls:
	//   init_ssh.sh → gen_hostfiles.sh → demo-run-nccl-test-tcpxo-via-mpi.sh
	execCmd := []string{"/scripts/allreduce.sh", gkeNCCLHost1ServiceName, gkeNCCLHost2ServiceName}
	slog.Info("Executing NCCL benchmark", "pod", gkeNCCLHost1PodName, "container", gkeNCCLContainerName, "command", execCmd)

	// Find host-1 pod for exec.
	host1Pod := pods[0]
	for _, p := range pods {
		if p.Name == gkeNCCLHost1PodName {
			host1Pod = p
			break
		}
	}

	stdout, stderr, err := podHelper.ExecInContainer(ctx.Ctx, host1Pod, gkeNCCLContainerName, execCmd, defaults.NCCLGKEExecTimeout)
	if err != nil {
		slog.Error("NCCL exec failed", "stderr", stderr)
		return stdout, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "NCCL benchmark exec failed", err)
	}

	slog.Info("NCCL benchmark completed successfully")
	return stdout, nil
}

// applyGKEResources reads the multi-doc YAML template, substitutes variables,
// and creates Services and Pods via the typed Kubernetes client.
func applyGKEResources(ctx context.Context, clientset kubernetes.Interface, gpuConfig *gpuConfiguration,
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType) ([]*v1.Pod, *gkeResources, error) {

	// Ensure namespace exists. The EKS path handles this implicitly through
	// Kubeflow Trainer, but the GKE raw-Pod path must create it explicitly.
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: gpuConfig.Namespace}}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to ensure namespace", err)
	}

	templateFile := templatePath(accelerator, service, "nccl-test-tcpxo.yaml")
	content, err := os.ReadFile(templateFile)
	if err != nil {
		return nil, nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read GKE template", err)
	}

	// Substitute template variables.
	yamlContent := string(content)
	templateData := map[string]string{
		"NAMESPACE":          gpuConfig.Namespace,
		"GPU_COUNT_PER_NODE": strconv.Itoa(gpuConfig.GPUCountPerNode),
		"WORKER_COUNT":       strconv.Itoa(gpuConfig.WorkerCount),
		"TEST_TYPE":          testType,
		"MIN_MESSAGE_SIZE":   minMessageSize,
		"MAX_MESSAGE_SIZE":   maxMessageSize,
	}
	for key, value := range templateData {
		yamlContent = strings.ReplaceAll(yamlContent, "${"+key+"}", value)
	}

	// Split multi-doc YAML and apply each document.
	docs := splitYAMLDocuments(yamlContent)
	resources := &gkeResources{Namespace: gpuConfig.Namespace}
	var pods []*v1.Pod

	applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	for _, doc := range docs {
		kind, kindErr := peekKind(doc)
		if kindErr != nil {
			return nil, resources, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine resource kind", kindErr)
		}

		switch kind {
		case "Service":
			svc := &v1.Service{}
			if unmarshalErr := yaml.Unmarshal([]byte(doc), svc); unmarshalErr != nil {
				return nil, resources, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse Service YAML", unmarshalErr)
			}
			_, createErr := clientset.CoreV1().Services(gpuConfig.Namespace).Create(applyCtx, svc, metav1.CreateOptions{})
			if createErr != nil {
				return nil, resources, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create Service", createErr)
			}
			resources.ServiceNames = append(resources.ServiceNames, svc.Name)
			slog.Info("Created Service", "name", svc.Name)

		case "Pod":
			pod := &v1.Pod{}
			if unmarshalErr := yaml.Unmarshal([]byte(doc), pod); unmarshalErr != nil {
				return nil, resources, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse Pod YAML", unmarshalErr)
			}
			createdPod, createErr := clientset.CoreV1().Pods(gpuConfig.Namespace).Create(applyCtx, pod, metav1.CreateOptions{})
			if createErr != nil {
				return nil, resources, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create Pod", createErr)
			}
			resources.PodNames = append(resources.PodNames, createdPod.Name)
			pods = append(pods, createdPod)
			slog.Info("Created Pod", "name", createdPod.Name)

		default:
			slog.Warn("Skipping unknown resource kind in GKE template", "kind", kind)
		}
	}

	if len(pods) < 2 {
		return nil, resources, aicrErrors.New(aicrErrors.ErrCodeInternal, "expected at least 2 pods from GKE template")
	}

	return pods, resources, nil
}

// cleanupGKEResources deletes pods and services created for the GKE NCCL test.
// Uses context.Background() because the parent context may already be canceled.
func cleanupGKEResources(clientset kubernetes.Interface, resources *gkeResources) {
	if resources == nil {
		return
	}

	slog.Info("Cleaning up GKE NCCL test resources...")

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.DiagnosticTimeout)
	defer cancel()

	for _, name := range resources.PodNames {
		err := clientset.CoreV1().Pods(resources.Namespace).Delete(cleanupCtx, name, metav1.DeleteOptions{})
		switch {
		case err == nil:
			slog.Info("Deleted Pod", "name", name)
		case apierrors.IsNotFound(err):
			// Already gone — expected during pre-cleanup on first run.
		default:
			slog.Error("Failed to delete Pod", "name", name, "error", err)
		}
	}

	for _, name := range resources.ServiceNames {
		err := clientset.CoreV1().Services(resources.Namespace).Delete(cleanupCtx, name, metav1.DeleteOptions{})
		switch {
		case err == nil:
			slog.Info("Deleted Service", "name", name)
		case apierrors.IsNotFound(err):
			// Already gone — expected during pre-cleanup on first run.
		default:
			slog.Error("Failed to delete Service", "name", name, "error", err)
		}
	}
}

// splitYAMLDocuments splits a multi-document YAML string on "---" boundaries.
// Empty or comment-only documents are skipped.
func splitYAMLDocuments(content string) []string {
	rawDocs := strings.Split(content, "\n---\n")
	var docs []string
	for _, doc := range rawDocs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" || trimmed == "---" {
			continue
		}
		// Skip comment-only documents.
		isCommentOnly := true
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				isCommentOnly = false
				break
			}
		}
		if isCommentOnly {
			continue
		}
		docs = append(docs, trimmed)
	}
	return docs
}

// peekKind extracts the "kind" field from a YAML document by scanning for the
// top-level "kind:" line, avoiding a full parse of the entire document.
func peekKind(doc string) (string, error) {
	for _, line := range strings.SplitN(doc, "\n", 20) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "kind:") {
			value := strings.TrimSpace(strings.TrimPrefix(trimmed, "kind:"))
			if value != "" {
				return value, nil
			}
		}
	}
	return "", aicrErrors.New(aicrErrors.ErrCodeInternal, "document has no kind field")
}
