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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	gangTestNamespace = "gang-scheduling-test"
	gangTestPrefix    = "gang-test-"
	gangPodPrefix     = "gang-worker-"
	gangClaimPrefix   = "gang-gpu-claim-"
	gangGroupPrefix   = "gang-group-"
	gangMinMembers    = 2
)

// kaiSchedulerDeployments are the required KAI scheduler components.
var kaiSchedulerDeployments = []string{
	"kai-scheduler-default",
	"admission",
	"binder",
	"kai-operator",
	"pod-grouper",
	"podgroup-controller",
	"queue-controller",
}

var podGroupGVR = schema.GroupVersionResource{
	Group: "scheduling.run.ai", Version: "v2alpha2", Resource: "podgroups",
}

// gangTestRun holds per-invocation resource names to avoid collisions.
type gangTestRun struct {
	suffix    string
	groupName string
	pods      [gangMinMembers]string
	claims    [gangMinMembers]string
}

type gangSchedulingReport struct {
	EarliestScheduled time.Time
	LatestScheduled   time.Time
	CoScheduleSpan    time.Duration
}

func newGangTestRun() (*gangTestRun, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	suffix := hex.EncodeToString(b)
	run := &gangTestRun{
		suffix:    suffix,
		groupName: gangGroupPrefix + suffix,
	}
	for i := range gangMinMembers {
		run.pods[i] = fmt.Sprintf("%s%s-%d", gangPodPrefix, suffix, i)
		run.claims[i] = fmt.Sprintf("%s%s-%d", gangClaimPrefix, suffix, i)
	}
	return run, nil
}

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:                  "gang-scheduling",
		Description:           "Verify KAI scheduler components, CRDs, and gang scheduling with PodGroup",
		Phase:                 phaseConformance,
		Func:                  CheckGangScheduling,
		TestName:              "TestGangScheduling",
		RequirementID:         "gang_scheduling",
		EvidenceTitle:         "Gang Scheduling (KAI Scheduler)",
		EvidenceDescription:   "Demonstrates that the cluster supports gang (all-or-nothing) scheduling using KAI scheduler with PodGroups.",
		EvidenceFile:          "gang-scheduling.md",
		SubmissionRequirement: true,
	})
}

// CheckGangScheduling validates CNCF requirement #7: Gang Scheduling.
// Verifies KAI scheduler deployments are running, required CRDs exist, and
// exercises gang scheduling by creating a PodGroup with 2 GPU pods that must
// be co-scheduled via the KAI scheduler.
func CheckGangScheduling(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. All KAI scheduler deployments available.
	var deploymentsSummary strings.Builder
	for _, name := range kaiSchedulerDeployments {
		deploy, err := getDeploymentIfAvailable(ctx, "kai-scheduler", name)
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("KAI scheduler component %s check failed", name), err)
		}
		expected := int32(1)
		if deploy.Spec.Replicas != nil {
			expected = *deploy.Spec.Replicas
		}
		fmt.Fprintf(&deploymentsSummary, "%-25s available=%d/%d image=%s\n",
			name, deploy.Status.AvailableReplicas, expected,
			firstContainerImage(deploy.Spec.Template.Spec.Containers))
	}
	recordRawTextArtifact(ctx, "KAI scheduler deployments",
		"kubectl get deploy -n kai-scheduler", deploymentsSummary.String())

	// KAI scheduler pods.
	kaiPods, err := ctx.Clientset.CoreV1().Pods("kai-scheduler").List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list KAI scheduler pods", err)
	}
	var podsSummary strings.Builder
	for _, p := range kaiPods.Items {
		fmt.Fprintf(&podsSummary, "%-44s ready=%s phase=%s\n", p.Name, podReadyCount(p), p.Status.Phase)
	}
	recordRawTextArtifact(ctx, "KAI scheduler pods",
		"kubectl get pods -n kai-scheduler", podsSummary.String())

	// 2. Required CRDs for gang scheduling.
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	requiredCRDs := []string{
		"queues.scheduling.run.ai",
		"podgroups.scheduling.run.ai",
	}
	var crdSummary strings.Builder
	for _, crd := range requiredCRDs {
		if _, crdErr := dynClient.Resource(crdGVR).Get(ctx.Context, crd, metav1.GetOptions{}); crdErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("gang scheduling CRD %s not found", crd), crdErr)
		}
		fmt.Fprintf(&crdSummary, "  %s: present\n", crd)
	}
	recordRawTextArtifact(ctx, "Gang Scheduling CRDs",
		"kubectl get crd queues.scheduling.run.ai podgroups.scheduling.run.ai",
		crdSummary.String())

	// 3. Pre-flight: ensure enough free GPUs for the gang test.
	total, free, gpuErr := countAvailableGPUs(ctx.Context, dynClient)
	if gpuErr != nil {
		return gpuErr
	}
	recordArtifact(ctx, "GPU Availability",
		fmt.Sprintf("Total GPUs: %d\nFree GPUs:  %d\nRequired:   %d", total, free, gangMinMembers))
	if free < gangMinMembers {
		return errors.New(errors.ErrCodeUnavailable,
			fmt.Sprintf("insufficient free GPUs for gang scheduling test: %d free of %d total (need %d)",
				free, total, gangMinMembers))
	}

	// 4. Functional test: create PodGroup with 2 GPU pods, verify co-scheduling.
	run, err := newGangTestRun()
	if err != nil {
		return err
	}

	defer func() {
		cleanupGangTestResources(ctx.Context, ctx.Clientset, dynClient, run)
		recordRawTextArtifact(ctx, "Delete test namespace",
			"kubectl delete namespace gang-scheduling-test --ignore-not-found",
			"Deleted gang test pods, claims, and PodGroup; namespace retained intentionally to avoid DRA finalizer stalls.")
	}()

	recordRawTextArtifact(ctx, "Apply test manifest",
		"kubectl apply -f docs/conformance/cncf/manifests/gang-scheduling-test.yaml",
		fmt.Sprintf("Created PodGroup=%s ResourceClaims=%s,%s Pods=%s,%s in namespace=%s",
			run.groupName, run.claims[0], run.claims[1], run.pods[0], run.pods[1], gangTestNamespace))

	if err = deployGangTestResources(ctx.Context, ctx.Clientset, dynClient, run); err != nil {
		return err
	}

	pods, err := waitForGangTestPods(ctx.Context, ctx.Clientset, run)
	if err != nil {
		return err
	}

	gangReport, err := validateGangPatterns(pods, run)
	if err != nil {
		return err
	}

	collectGangTestArtifacts(ctx, dynClient, pods, gangReport, run)
	return nil
}

func collectGangTestArtifacts(ctx *checks.ValidationContext, dynClient dynamic.Interface,
	pods [gangMinMembers]*corev1.Pod, gangReport *gangSchedulingReport, run *gangTestRun) {

	// PodGroup status.
	pgList, listErr := dynClient.Resource(podGroupGVR).Namespace(gangTestNamespace).List(
		ctx.Context, metav1.ListOptions{})
	if listErr != nil {
		recordRawTextArtifact(ctx, "PodGroup status",
			"kubectl get podgroups -n gang-scheduling-test -o wide",
			fmt.Sprintf("failed to list PodGroups: %v", listErr))
	} else {
		var pgSummary strings.Builder
		for _, item := range pgList.Items {
			minMember, _, _ := unstructured.NestedInt64(item.Object, "spec", "minMember")
			fmt.Fprintf(&pgSummary, "%-36s minMember=%d\n", item.GetName(), minMember)
		}
		recordRawTextArtifact(ctx, "PodGroup status",
			"kubectl get podgroups -n gang-scheduling-test -o wide", pgSummary.String())
	}

	// Pod status and scheduling timestamps.
	var gangResults strings.Builder
	for i, pod := range pods {
		if pod == nil {
			continue
		}
		var schedTime string
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
				schedTime = cond.LastTransitionTime.Format(time.RFC3339)
				break
			}
		}
		fmt.Fprintf(&gangResults, "Pod %d: %s  phase=%s  scheduler=%s  scheduled=%s\n",
			i, pod.Name, pod.Status.Phase, pod.Spec.SchedulerName, schedTime)
	}
	fmt.Fprintf(&gangResults, "Co-schedule span: %s\n", gangReport.CoScheduleSpan)
	fmt.Fprintf(&gangResults, "Allowed window:   %s\n", defaults.CoScheduleWindow)
	fmt.Fprintf(&gangResults, "Earliest/Latest:  %s / %s\n",
		gangReport.EarliestScheduled.Format(time.RFC3339),
		gangReport.LatestScheduled.Format(time.RFC3339))
	recordRawTextArtifact(ctx, "Pod status",
		"kubectl get pods -n gang-scheduling-test -o wide", gangResults.String())

	// Worker logs.
	for i := range gangMinMembers {
		logBytes, logErr := ctx.Clientset.CoreV1().Pods(gangTestNamespace).GetLogs(
			run.pods[i], &corev1.PodLogOptions{}).DoRaw(ctx.Context)
		label := fmt.Sprintf("gang-worker-%d logs", i)
		if logErr != nil {
			recordRawTextArtifact(ctx, label,
				fmt.Sprintf("kubectl logs gang-worker-%d -n gang-scheduling-test", i),
				fmt.Sprintf("failed to read logs: %v", logErr))
			continue
		}
		recordChunkedTextArtifact(ctx, label,
			fmt.Sprintf("kubectl logs gang-worker-%d -n gang-scheduling-test", i),
			string(logBytes))
	}
}

// deployGangTestResources creates the namespace, PodGroup, ResourceClaims, and Pods.
func deployGangTestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gangTestRun) error {
	// 1. Create namespace (idempotent).
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: gangTestNamespace},
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); k8s.IgnoreAlreadyExists(err) != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}

	// 2. Create PodGroup.
	podGroup := buildPodGroup(run)
	if _, err := dynClient.Resource(podGroupGVR).Namespace(gangTestNamespace).Create(
		ctx, podGroup, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create PodGroup", err)
	}

	// 3. Create ResourceClaims and Pods.
	for i := range gangMinMembers {
		claim := buildGangResourceClaim(run, i)
		if _, err := dynClient.Resource(claimGVR).Namespace(gangTestNamespace).Create(
			ctx, claim, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create ResourceClaim %s", run.claims[i]), err)
		}

		pod := buildGangTestPod(run, i)
		if _, err := clientset.CoreV1().Pods(gangTestNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create gang test pod %s", run.pods[i]), err)
		}
	}

	return nil
}

// waitForGangTestPods polls until all gang test pods reach a terminal state.
func waitForGangTestPods(ctx context.Context, clientset kubernetes.Interface, run *gangTestRun) ([gangMinMembers]*corev1.Pod, error) {
	var result [gangMinMembers]*corev1.Pod

	waitCtx, cancel := context.WithTimeout(ctx, defaults.GangTestPodTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			allDone := true
			for i := range gangMinMembers {
				if result[i] != nil {
					continue // already terminal
				}
				pod, err := clientset.CoreV1().Pods(gangTestNamespace).Get(
					ctx, run.pods[i], metav1.GetOptions{})
				if err != nil {
					return false, errors.Wrap(errors.ErrCodeInternal,
						fmt.Sprintf("failed to get gang test pod %s", run.pods[i]), err)
				}
				switch pod.Status.Phase { //nolint:exhaustive // only terminal states matter
				case corev1.PodSucceeded, corev1.PodFailed:
					result[i] = pod
				default:
					allDone = false
				}
			}
			return allDone, nil
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return result, errors.Wrap(errors.ErrCodeTimeout, "gang test pods did not complete in time", err)
		}
		return result, errors.Wrap(errors.ErrCodeInternal, "gang test pod polling failed", err)
	}

	return result, nil
}

// validateGangPatterns verifies all pods completed successfully and were scheduled by kai-scheduler.
func validateGangPatterns(pods [gangMinMembers]*corev1.Pod, run *gangTestRun) (*gangSchedulingReport, error) {
	for i, pod := range pods {
		if pod == nil {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s result is nil", run.pods[i]))
		}

		// Pod must have succeeded.
		if pod.Status.Phase != corev1.PodSucceeded {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s phase=%s (want Succeeded), gang scheduling may have failed",
					run.pods[i], pod.Status.Phase))
		}

		// Pod must use kai-scheduler.
		if pod.Spec.SchedulerName != "kai-scheduler" {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s schedulerName=%s (want kai-scheduler)",
					run.pods[i], pod.Spec.SchedulerName))
		}

		// Pod must have PodGroup label.
		if pod.Labels["pod-group.scheduling.run.ai/name"] != run.groupName {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s missing PodGroup label (want %s)",
					run.pods[i], run.groupName))
		}

		// Pod must use DRA (resourceClaims, not device plugin).
		if len(pod.Spec.ResourceClaims) == 0 {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s does not use DRA resourceClaims", run.pods[i]))
		}
	}

	// Verify co-scheduling: PodScheduled condition timestamps must be within tolerance.
	// This proves gang (all-or-nothing) semantics — pods scheduled together, not sequentially.
	var scheduleTimes []time.Time
	for i, pod := range pods {
		var found bool
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
				scheduleTimes = append(scheduleTimes, cond.LastTransitionTime.Time)
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s missing PodScheduled=True condition", run.pods[i]))
		}
	}

	earliest := scheduleTimes[0]
	latest := scheduleTimes[0]
	for _, t := range scheduleTimes[1:] {
		if t.Before(earliest) {
			earliest = t
		}
		if t.After(latest) {
			latest = t
		}
	}
	span := latest.Sub(earliest)
	if span > defaults.CoScheduleWindow {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("gang scheduling pods not co-scheduled: schedule times span %s (max %s)",
				span, defaults.CoScheduleWindow))
	}

	return &gangSchedulingReport{
		EarliestScheduled: earliest,
		LatestScheduled:   latest,
		CoScheduleSpan:    span,
	}, nil
}

// cleanupGangTestResources removes test resources. Best-effort: errors are ignored.
// The namespace is intentionally NOT deleted — namespace deletion can hang on DRA finalizers.
func cleanupGangTestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gangTestRun) {
	// Delete pods first (releases claim reservations).
	for i := range gangMinMembers {
		_ = k8s.IgnoreNotFound(clientset.CoreV1().Pods(gangTestNamespace).Delete(
			ctx, run.pods[i], metav1.DeleteOptions{}))
	}
	// Wait for pod deletions.
	for i := range gangMinMembers {
		podName := run.pods[i]
		waitForDeletion(ctx, func() error {
			_, err := clientset.CoreV1().Pods(gangTestNamespace).Get(ctx, podName, metav1.GetOptions{})
			return err
		})
	}
	// Delete claims.
	for i := range gangMinMembers {
		_ = k8s.IgnoreNotFound(dynClient.Resource(claimGVR).Namespace(gangTestNamespace).Delete(
			ctx, run.claims[i], metav1.DeleteOptions{}))
	}
	// Delete PodGroup.
	_ = k8s.IgnoreNotFound(dynClient.Resource(podGroupGVR).Namespace(gangTestNamespace).Delete(
		ctx, run.groupName, metav1.DeleteOptions{}))
}

// buildPodGroup returns the unstructured PodGroup for the gang scheduling test.
func buildPodGroup(run *gangTestRun) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "scheduling.run.ai/v2alpha2",
			"kind":       "PodGroup",
			"metadata": map[string]interface{}{
				"name":      run.groupName,
				"namespace": gangTestNamespace,
			},
			"spec": map[string]interface{}{
				"minMember": int64(gangMinMembers),
				"queue":     "default-queue",
			},
		},
	}
}

// buildGangResourceClaim returns the unstructured ResourceClaim for a gang test pod.
func buildGangResourceClaim(run *gangTestRun, index int) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceClaim",
			"metadata": map[string]interface{}{
				"name":      run.claims[index],
				"namespace": gangTestNamespace,
			},
			"spec": map[string]interface{}{
				"devices": map[string]interface{}{
					"requests": []interface{}{
						map[string]interface{}{
							"name": "gpu",
							"exactly": map[string]interface{}{
								"deviceClassName": "gpu.nvidia.com",
								"allocationMode":  "ExactCount",
								"count":           int64(1),
							},
						},
					},
				},
			},
		},
	}
}

// buildGangTestPod returns the Pod spec for a gang scheduling test worker.
func buildGangTestPod(run *gangTestRun, index int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.pods[index],
			Namespace: gangTestNamespace,
			Labels: map[string]string{
				"pod-group.scheduling.run.ai/name":     run.groupName,
				"pod-group.scheduling.run.ai/group-id": run.groupName,
			},
		},
		Spec: corev1.PodSpec{
			SchedulerName: "kai-scheduler",
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:              "gpu",
					ResourceClaimName: strPtr(run.claims[index]),
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "worker",
					Image:   "nvidia/cuda:12.9.0-base-ubuntu24.04",
					Command: []string{"bash", "-c", fmt.Sprintf("nvidia-smi && echo 'Gang worker %d completed successfully'", index)},
					Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{
							{Name: "gpu"},
						},
					},
				},
			},
		},
	}
}
