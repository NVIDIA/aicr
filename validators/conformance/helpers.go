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

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// getDynamicClient returns the dynamic client from context, or creates one from RESTConfig.
func getDynamicClient(ctx *validators.Context) (dynamic.Interface, error) {
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
	client := defaults.NewHTTPClient(0)
	resp, err := client.Do(req) //nolint:gosec // G704 -- URL constructed from in-cluster service config
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to reach %s", url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url))
	}
	// Read up to the cap +1 so an oversized response is rejected with a clear
	// error rather than silently truncated — a truncated Prometheus/DCGM scrape
	// would make callers parse partial data or report metrics as "missing".
	body, err := io.ReadAll(io.LimitReader(resp.Body, defaults.HTTPResponseBodyLimit+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to read response from %s", url), err)
	}
	if int64(len(body)) > defaults.HTTPResponseBodyLimit {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("response from %s exceeds %d byte limit", url, defaults.HTTPResponseBodyLimit))
	}
	return body, nil
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
func verifyDeploymentAvailable(ctx *validators.Context, namespace, name string) error {
	_, err := getDeploymentIfAvailable(ctx, namespace, name)
	return err
}

// getDeploymentIfAvailable fetches a Deployment and verifies it has at least one available replica.
// Returns the Deployment object so callers can capture diagnostic artifacts from it.
func getDeploymentIfAvailable(ctx *validators.Context, namespace, name string) (*appsv1.Deployment, error) {
	deploy, err := ctx.Clientset.AppsV1().Deployments(namespace).Get(
		ctx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		// Shared classifier — a Forbidden/timeout/transport failure must not
		// masquerade as "not found" (ambiguous negative checks fail closed).
		return nil, classifyK8sReadError(err, fmt.Sprintf("deployment %s/%s", namespace, name))
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

// waitForDeploymentAvailable polls until the named Deployment reports at least
// one available replica, or the timeout elapses. Returns the last-observed
// Deployment so callers can capture diagnostics.
//
// Use this instead of getDeploymentIfAvailable for components that can be
// transiently unavailable — single-replica webhooks, rolling updates,
// restarts. An instantaneous check fails closed on a momentary 0/N blip; the
// bounded wait tolerates the blip but still fails (after the bound) when a
// deployment is genuinely down. A not-yet-created deployment is treated as
// "keep waiting" within the bound rather than an immediate NotFound.
//
//nolint:unparam // namespace is part of the general helper API (mirrors getDeploymentIfAvailable); the sole caller today happens to use one namespace.
func waitForDeploymentAvailable(ctx *validators.Context, namespace, name string, timeout time.Duration) (*appsv1.Deployment, error) {
	pollCtx, cancel := context.WithTimeout(ctx.Ctx, timeout)
	defer cancel()

	var last *appsv1.Deployment
	err := wait.PollUntilContextCancel(pollCtx, defaults.PodPollInterval, true,
		func(c context.Context) (bool, error) {
			deploy, getErr := ctx.Clientset.AppsV1().Deployments(namespace).Get(c, name, metav1.GetOptions{})
			if getErr != nil {
				if k8serrors.IsNotFound(getErr) {
					return false, nil // not created yet — keep waiting within the bound
				}
				return false, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to get deployment %s/%s", namespace, name), getErr)
			}
			last = deploy
			return deploy.Status.AvailableReplicas >= 1, nil
		},
	)
	if err == nil {
		return last, nil
	}

	// Caller cancellation is an external abort, not a readiness failure.
	// pollCtx derives from ctx.Ctx, so pollCtx.Err() is also set when the parent
	// is canceled — check the parent explicitly first and propagate it as a
	// transient timeout rather than reporting the deployment as not-available.
	if ctx.Ctx.Err() != nil {
		return last, errors.Wrap(errors.ErrCodeTimeout,
			fmt.Sprintf("waiting for deployment %s/%s canceled", namespace, name), ctx.Ctx.Err())
	}

	expected := int32(1)
	var avail int32
	if last != nil {
		avail = last.Status.AvailableReplicas
		if last.Spec.Replicas != nil {
			expected = *last.Spec.Replicas
		}
	}
	// Our own bound elapsed (parent still live): the deployment never became
	// available in time. Surface the NotFound-shaped not-available message the
	// caller wraps. A non-deadline error is a genuine API failure — propagate it.
	if pollCtx.Err() != nil {
		if last == nil {
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("deployment %s/%s not found after %s", namespace, name, timeout))
		}
		return last, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("deployment %s/%s not available after %s: %d/%d replicas",
				namespace, name, timeout, avail, expected))
	}
	return last, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
		fmt.Sprintf("failed waiting for deployment %s/%s", namespace, name))
}

// verifyDaemonSetReady checks that a DaemonSet has at least one ready pod.
func verifyDaemonSetReady(ctx *validators.Context, namespace, name string) error {
	_, err := getDaemonSetIfReady(ctx, namespace, name)
	return err
}

// getDaemonSetIfReady fetches a DaemonSet and verifies it has at least one ready pod.
// Returns the DaemonSet object so callers can capture diagnostic artifacts from it.
func getDaemonSetIfReady(ctx *validators.Context, namespace, name string) (*appsv1.DaemonSet, error) {
	ds, err := ctx.Clientset.AppsV1().DaemonSets(namespace).Get(
		ctx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		// Shared classifier — see getDeploymentIfAvailable.
		return nil, classifyK8sReadError(err, fmt.Sprintf("daemonset %s/%s", namespace, name))
	}
	if ds.Status.NumberReady < 1 {
		return ds, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("daemonset %s/%s not ready: %d/%d pods",
				namespace, name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}
	return ds, nil
}

// cleanupInspectEquivalent returns the READ-ONLY "equivalent" command for the
// per-run test-namespace cleanup artifact. Deliberately not a `kubectl
// delete`: when cleanup does delete, it does so with a metadata.uid
// precondition plus an ownership-label check (see cleanupGPUTestNamespace),
// and no kubectl one-liner reproduces those guards — publishing an executable
// delete would invite an UNGUARDED deletion that could remove a same-name
// replacement namespace. The note stays STATE-NEUTRAL about what cleanup did
// (some paths deliberately delete nothing: foreign namespace, definitively
// rejected create, never-surfacing ambiguous create) and about what the
// inspection shows (a same-name namespace created later legitimately produces
// output) — the artifact body describes the actual outcome.
func cleanupInspectEquivalent(namespace string) string {
	return fmt.Sprintf(
		"kubectl get namespace %s --ignore-not-found -o jsonpath='{.metadata.uid} {.status.phase}'  # read-only inspection of the current state; see the artifact body for what cleanup actually did",
		namespace)
}

// recordArtifact writes diagnostic evidence to stdout.
// In v2, stdout is captured as the CTRF stdout field. No chunking needed.
func recordArtifact(_ *validators.Context, label, data string) {
	fmt.Printf("--- %s ---\n%s\n", label, data)
}

// recordRawTextArtifact writes text evidence with an optional command equivalent.
func recordRawTextArtifact(_ *validators.Context, label, equivalent, data string) {
	if equivalent != "" {
		fmt.Printf("--- %s ---\nEquivalent: %s\n\n%s\n", label, equivalent, data)
	} else {
		fmt.Printf("--- %s ---\n%s\n", label, data)
	}
}

// recordObjectYAMLArtifact writes a structured object as YAML evidence.
func recordObjectYAMLArtifact(ctx *validators.Context, label, equivalent string, obj any) {
	payload, err := yaml.Marshal(obj)
	if err != nil {
		recordRawTextArtifact(ctx, label, equivalent, fmt.Sprintf("failed to marshal YAML: %v", err))
		return
	}
	recordRawTextArtifact(ctx, label, equivalent, string(payload))
}

// firstContainerImage returns the image of the first container, or "unknown" if empty.
func firstContainerImage(containers []corev1.Container) string {
	if len(containers) > 0 {
		return containers[0].Image
	}
	return statusUnknown
}

func valueOrUnknown(v string) string {
	if strings.TrimSpace(v) == "" {
		return statusUnknown
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

// waitForDeletion polls until a resource is gone (NotFound) or the context expires.
func waitForDeletion(ctx context.Context, getFunc func() error) {
	pollCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()
	_ = wait.PollUntilContextCancel(pollCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			err := getFunc()
			if k8serrors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		},
	)
}
