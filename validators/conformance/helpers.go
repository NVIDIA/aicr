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
	stderrors "errors"
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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// readFailedBecauseContextEnded reports whether an I/O error came from the
// polling context itself ending. Such an error is the wait's terminal signal,
// not evidence that preceding object reads were persistently failing.
func readFailedBecauseContextEnded(ctx context.Context, err error) bool {
	return ctx.Err() != nil && stderrors.Is(err, ctx.Err())
}

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
		cond, ok := c.(map[string]any)
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
	var lastReadErr error
	err := wait.PollUntilContextCancel(pollCtx, defaults.PodPollInterval, true,
		func(c context.Context) (bool, error) {
			deploy, getErr := ctx.Clientset.AppsV1().Deployments(namespace).Get(c, name, metav1.GetOptions{})
			if getErr != nil {
				if k8serrors.IsNotFound(getErr) {
					// A NotFound is a successful read: the API answered. Clear
					// any earlier transient error so a recovered throttle does
					// not mislabel a genuinely-missing deployment at expiry.
					lastReadErr = nil
					return false, nil // not created yet — keep waiting within the bound
				}
				// A read that could not land is not a verdict. Client-go's own
				// rate limiter fails reads under load, and aborting here failed
				// callers on a healthy cluster (#2406). Retry within the bound;
				// genuine errors (RBAC, malformed) still abort immediately.
				if isK8sTimeoutErr(getErr) {
					// A Get interrupted by this wait's own cancellation is not a
					// transient read failure. Do not let the final in-flight read
					// overwrite the diagnostic from the preceding poll history.
					if readFailedBecauseContextEnded(c, getErr) {
						return false, nil
					}
					lastReadErr = getErr
					slog.Debug("transient read while waiting for deployment; retrying",
						"namespace", namespace, "deployment", name, "error", getErr)
					return false, nil
				}
				return false, classifyK8sReadError(getErr,
					fmt.Sprintf("deployment %s/%s", namespace, name))
			}
			lastReadErr = nil
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
		// A sustained throttle would otherwise be indistinguishable from "never
		// became ready" — keep the last read error so the operator sees why.
		if lastReadErr != nil {
			return last, errors.Wrap(errors.ErrCodeTimeout,
				fmt.Sprintf("deployment %s/%s unreadable for %s (reads kept failing)",
					namespace, name, timeout), lastReadErr)
		}
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

// podStuckReason inspects a Pod for non-recoverable stuck states and returns a
// human-readable reason. Returns empty string if the pod is not stuck.
// Follows the pattern from pkg/validator/agent/wait.go:getJobFailureReasonFromPod.
func podStuckReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", waitingReasonCrashLoopBackOff:
				return fmt.Sprintf("%s: %s (image: %s)", w.Reason, w.Message, cs.Image)
			}
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", waitingReasonCrashLoopBackOff:
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

// sandboxRuntimeMissingMarkers are the substrings of a FailedCreatePodSandBox
// event message that mean the pod's RuntimeClass handler is not served on the
// node it landed on, a state no retry repairs: containerd's "no runtime for
// \"<handler>\" is configured" (wrapped as "failed to get sandbox runtime"),
// CRI-O's "failed to find runtime handler" lookup failure, containerd's
// "runtime \"<type>\" binary not installed" for a shim the handler names but
// the node lacks (core/runtime/v2/shim_manager.go), and the exec failure a
// runc-type handler reports when its options.BinaryName runtime is missing.
// Network-setup failures of the same event reason are transient and
// deliberately absent, so a CNI that is still starting does not fail the
// probe.
var sandboxRuntimeMissingMarkers = []string{
	"no runtime for",
	"failed to get sandbox runtime",
	"runtime handler",
	"binary not installed",
	"executable file not found",
}

// sandboxCreateFailure returns the reason a pod is stuck when ev records a
// terminal sandbox-creation failure (eventReasonFailedCreatePodSandBox with
// a sandboxRuntimeMissingMarkers message), else "". The pod spends the whole
// pod timeout Pending otherwise, because the kubelet retries with backoff
// and never changes the pod's status (#3703).
func sandboxCreateFailure(ev *corev1.Event) string {
	if ev == nil || ev.Reason != eventReasonFailedCreatePodSandBox {
		return ""
	}
	message := strings.ToLower(ev.Message)
	for _, marker := range sandboxRuntimeMissingMarkers {
		if strings.Contains(message, marker) {
			return fmt.Sprintf("%s: %s", ev.Reason, strings.TrimSpace(ev.Message))
		}
	}
	return ""
}

// podSandboxEventSelector selects the FailedCreatePodSandBox events of one pod.
func podSandboxEventSelector(podName string) string {
	return fields.SelectorFromSet(fields.Set{
		"involvedObject.kind": involvedObjectKindPod,
		"involvedObject.name": podName,
		"reason":              eventReasonFailedCreatePodSandBox,
	}).String()
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

// wrapWithCode adds message to err while keeping the code err already
// carries, so the context grows at each layer without relabeling what the
// consumer reads; a plain error gets fallback.
func wrapWithCode(err error, fallback errors.ErrorCode, message string) error {
	code := fallback
	if se, ok := stderrors.AsType[*errors.StructuredError](err); ok {
		code = se.Code
	}
	return errors.Wrap(code, message, err)
}

// resourceQuantityPtr returns a pointer to a parsed resource.Quantity.
func resourceQuantityPtr(val string) *resource.Quantity {
	q := resource.MustParse(val)
	return &q
}

// waitForHPAScalingIntent polls the HPA until desiredReplicas > currentReplicas.
// This is the strict criterion: it proves the HPA read metrics and computed a
// scale-up. ScalingActive=True alone is not accepted, as it can be true
// without scale intent. Used by the cluster-autoscaling check's external
// metric HPA.
func waitForHPAScalingIntent(ctx context.Context, clientset kubernetes.Interface, namespace, hpaName string) (int32, int32, error) {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.HPAScaleTimeout)
	defer cancel()
	var observedDesired, observedCurrent int32

	err := wait.PollUntilContextCancel(waitCtx, defaults.HPAPollInterval, true,
		func(ctx context.Context) (bool, error) {
			hpa, getErr := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).Get(
				ctx, hpaName, metav1.GetOptions{})
			if getErr != nil {
				slog.Debug("HPA not ready yet", "error", getErr)
				return false, nil // retry
			}

			desired := hpa.Status.DesiredReplicas
			current := hpa.Status.CurrentReplicas
			observedDesired = desired
			observedCurrent = current
			slog.Debug("HPA status", "desired", desired, "current", current)

			if desired > current {
				slog.Info("HPA scaling intent detected",
					"desiredReplicas", desired, "currentReplicas", current)
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return 0, 0, errors.Wrap(errors.ErrCodeTimeout,
				"HPA did not report scaling intent within timeout — metrics pipeline may be broken", err)
		}
		return 0, 0, errors.Wrap(errors.ErrCodeInternal, "HPA scaling intent polling failed", err)
	}

	return observedDesired, observedCurrent, nil
}
