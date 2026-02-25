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

package conformance

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// phaseConformance is the phase identifier for conformance checks.
const phaseConformance = "conformance"

// getDynamicClient returns the dynamic client from context, or creates one from RESTConfig.
func getDynamicClient(ctx *checks.ValidationContext) (dynamic.Interface, error) {
	if ctx.DynamicClient != nil {
		return ctx.DynamicClient, nil
	}
	if ctx.RESTConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}
	dc, err := dynamic.NewForConfig(ctx.RESTConfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}
	return dc, nil
}

// httpGet performs an HTTP GET to an in-cluster service URL with context timeout.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create request", err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704 -- URL constructed from in-cluster service config
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to reach %s", url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url))
	}
	return io.ReadAll(io.LimitReader(resp.Body, defaults.HTTPResponseBodyLimit))
}

type conditionObservation struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

func getConditionObservation(obj *unstructured.Unstructured, condType string) (*conditionObservation, error) {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return nil, errors.New(errors.ErrCodeInternal, "status.conditions not found")
	}

	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}

		kind, kindFound, kindErr := unstructured.NestedString(cond, "type")
		if kindErr != nil {
			slog.Debug("condition has non-string type field", "error", kindErr)
			continue
		}
		if !kindFound || kind != condType {
			continue
		}

		status, foundStatus, _ := unstructured.NestedString(cond, "status")
		if !foundStatus {
			if v, ok := cond["status"]; ok {
				status = fmt.Sprintf("%v", v)
			}
		}
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		return &conditionObservation{
			Type:    condType,
			Status:  valueOrUnknown(status),
			Reason:  valueOrUnknown(reason),
			Message: valueOrUnknown(message),
		}, nil
	}

	return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("condition %s not found", condType))
}

// verifyDeploymentAvailable checks that a Deployment has at least one available replica.
func verifyDeploymentAvailable(ctx *checks.ValidationContext, namespace, name string) error {
	_, err := getDeploymentIfAvailable(ctx, namespace, name)
	return err
}

// getDeploymentIfAvailable fetches a Deployment and verifies it has at least one available replica.
// Returns the Deployment object so callers can capture diagnostic artifacts from it.
func getDeploymentIfAvailable(ctx *checks.ValidationContext, namespace, name string) (*appsv1.Deployment, error) {
	deploy, err := ctx.Clientset.AppsV1().Deployments(namespace).Get(
		ctx.Context, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("deployment %s/%s not found", namespace, name), err)
	}
	if deploy.Status.AvailableReplicas < 1 {
		expected := int32(1)
		if deploy.Spec.Replicas != nil {
			expected = *deploy.Spec.Replicas
		}
		return deploy, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("deployment %s/%s not available: %d/%d replicas",
				namespace, name, deploy.Status.AvailableReplicas, expected))
	}
	return deploy, nil
}

// verifyDaemonSetReady checks that a DaemonSet has at least one ready pod.
func verifyDaemonSetReady(ctx *checks.ValidationContext, namespace, name string) error {
	_, err := getDaemonSetIfReady(ctx, namespace, name)
	return err
}

// getDaemonSetIfReady fetches a DaemonSet and verifies it has at least one ready pod.
// Returns the DaemonSet object so callers can capture diagnostic artifacts from it.
func getDaemonSetIfReady(ctx *checks.ValidationContext, namespace, name string) (*appsv1.DaemonSet, error) {
	ds, err := ctx.Clientset.AppsV1().DaemonSets(namespace).Get(
		ctx.Context, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("daemonset %s/%s not found", namespace, name), err)
	}
	if ds.Status.NumberReady < 1 {
		return ds, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("daemonset %s/%s not ready: %d/%d pods",
				namespace, name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}
	return ds, nil
}

// int32Ptr returns a pointer to the given int32 value.
func int32Ptr(i int32) *int32 { return &i }

// recordArtifact records diagnostic evidence if the artifact collector is active.
// Safe to call when ctx.Artifacts is nil (no-op).
func recordArtifact(ctx *checks.ValidationContext, label, data string) {
	if ctx.Artifacts == nil {
		return
	}
	if err := ctx.Artifacts.Record(label, data); err != nil {
		slog.Debug("artifact recording skipped", "label", label, "error", err)
	}
}

func formatArtifactBody(equivalent, data string) string {
	equivalent = strings.TrimSpace(equivalent)
	data = strings.TrimSpace(data)
	switch {
	case equivalent == "" && data == "":
		return ""
	case equivalent == "":
		return data
	case data == "":
		return fmt.Sprintf("Equivalent: %s", equivalent)
	default:
		return fmt.Sprintf("Equivalent: %s\n\n%s", equivalent, data)
	}
}

// recordRawTextArtifact records plain text evidence with an optional command equivalent.
func recordRawTextArtifact(ctx *checks.ValidationContext, label, equivalent, data string) {
	recordArtifact(ctx, label, formatArtifactBody(equivalent, data))
}

// recordChunkedTextArtifact records text evidence across multiple artifacts when needed.
// This avoids collector-side truncation for large YAML/JSON/metrics payloads.
// Splits on line boundaries to avoid producing malformed YAML/JSON chunks.
func recordChunkedTextArtifact(ctx *checks.ValidationContext, label, equivalent, data string) {
	if strings.TrimSpace(data) == "" {
		recordRawTextArtifact(ctx, label, equivalent, data)
		return
	}

	prefix := ""
	if strings.TrimSpace(equivalent) != "" {
		prefix = fmt.Sprintf("Equivalent: %s\n\n", strings.TrimSpace(equivalent))
	}

	chunkSize := defaults.ArtifactMaxDataSize - len(prefix) - 256
	if chunkSize < 512 {
		chunkSize = 512
	}
	if len(data) <= chunkSize {
		recordArtifact(ctx, label, prefix+strings.TrimSpace(data))
		return
	}

	// Split on line boundaries to avoid cutting mid-line or mid-multibyte-character.
	lines := strings.Split(data, "\n")
	var chunks []string
	var current strings.Builder
	for _, line := range lines {
		// If adding this line would exceed the budget, flush current chunk.
		if current.Len() > 0 && current.Len()+len(line)+1 > chunkSize {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	total := len(chunks)
	for i, chunk := range chunks {
		partLabel := fmt.Sprintf("%s (part %d/%d)", label, i+1, total)
		partBody := fmt.Sprintf("%sPart: %d/%d\n\n%s", prefix, i+1, total, strings.TrimSpace(chunk))
		recordArtifact(ctx, partLabel, partBody)
	}
}

// recordObjectYAMLArtifact records a structured object as YAML.
func recordObjectYAMLArtifact(ctx *checks.ValidationContext, label, equivalent string, obj any) {
	payload, err := yaml.Marshal(obj)
	if err != nil {
		recordRawTextArtifact(ctx, label, equivalent, fmt.Sprintf("failed to marshal YAML: %v", err))
		return
	}
	recordChunkedTextArtifact(ctx, label, equivalent, string(payload))
}

// firstContainerImage returns the image of the first container, or "unknown" if empty.
func firstContainerImage(containers []corev1.Container) string {
	if len(containers) > 0 {
		return containers[0].Image
	}
	return "unknown"
}

func valueOrUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unknown"
	}
	return v
}

func podReadyCount(pod corev1.Pod) string {
	var ready, total int
	for _, cs := range pod.Status.ContainerStatuses {
		total++
		if cs.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, total)
}

// truncateLines limits text to at most n lines, appending a truncation marker if needed.
func truncateLines(text string, n int) string {
	lines := strings.SplitN(text, "\n", n+1)
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n") + "\n... [truncated]"
}

// containsAllMetrics checks that all required metric names appear in the given text.
// Returns the list of missing metrics.
func containsAllMetrics(text string, required []string) []string {
	var missing []string
	for _, metric := range required {
		if !strings.Contains(text, metric) {
			missing = append(missing, metric)
		}
	}
	return missing
}

// podStuckReason inspects a Pod for non-recoverable stuck states and returns a
// human-readable reason. Returns empty string if the pod is not stuck.
// Follows the pattern from pkg/validator/agent/wait.go:getJobFailureReasonFromPod.
func podStuckReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CrashLoopBackOff":
				return fmt.Sprintf("%s: %s (image: %s)", w.Reason, w.Message, cs.Image)
			}
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CrashLoopBackOff":
				return fmt.Sprintf("%s: %s (init container, image: %s)", w.Reason, w.Message, cs.Image)
			}
		}
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse &&
			cond.Reason == string(corev1.PodReasonUnschedulable) {

			return fmt.Sprintf("Unschedulable: %s", cond.Message)
		}
	}
	return ""
}

// podWaitingStatus returns the first container's waiting reason and message, or "none"
// if no container is in a waiting state. Used for diagnostic output on timeout.
func podWaitingStatus(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			return fmt.Sprintf("%s: %s", w.Reason, w.Message)
		}
	}
	return "none"
}

// gpuDriverName is the DRA driver name for NVIDIA GPUs.
const gpuDriverName = "gpu.nvidia.com"

// countAvailableGPUs counts total GPU devices from ResourceSlices and subtracts
// allocated devices from ResourceClaims to determine how many are free.
func countAvailableGPUs(ctx context.Context, dynClient dynamic.Interface) (total, free int, err error) {
	sliceGVR := schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices",
	}

	// Count total GPU devices from ResourceSlices.
	slices, err := dynClient.Resource(sliceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceSlices", err)
	}
	for _, slice := range slices.Items {
		driver, _, _ := unstructured.NestedString(slice.Object, "spec", "driver")
		if driver != gpuDriverName {
			continue
		}
		devices, found, _ := unstructured.NestedSlice(slice.Object, "spec", "devices")
		if found {
			total += len(devices)
		}
	}

	// Count allocated GPU devices from ResourceClaims.
	var allocated int
	claims, err := dynClient.Resource(claimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceClaims", err)
	}
	for _, claim := range claims.Items {
		results, found, _ := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
		if !found {
			continue
		}
		for _, r := range results {
			result, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			if result["driver"] == gpuDriverName {
				allocated++
			}
		}
	}

	return total, total - allocated, nil
}
