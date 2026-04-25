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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	gangUnknownValue  = "unknown"
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

var queueGVR = schema.GroupVersionResource{
	Group: "scheduling.run.ai", Version: "v2", Resource: "queues",
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

// CheckGangScheduling validates CNCF requirement #7: Gang Scheduling.
// Verifies KAI scheduler deployments are running, required CRDs exist, and
// exercises gang scheduling by creating a PodGroup with 2 GPU pods that must
// be co-scheduled via the KAI scheduler.
func CheckGangScheduling(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 0. Check if KAI scheduler is installed (skip gracefully if not).
	_, kaiCheckErr := ctx.Clientset.AppsV1().Deployments("kai-scheduler").Get(
		ctx.Ctx, "kai-scheduler-default", metav1.GetOptions{})
	if kaiCheckErr != nil {
		return validators.Skip("KAI scheduler not found — cluster may use a different scheduler")
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
	kaiPods, err := ctx.Clientset.CoreV1().Pods("kai-scheduler").List(ctx.Ctx, metav1.ListOptions{})
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
		if _, crdErr := dynClient.Resource(crdGVR).Get(ctx.Ctx, crd, metav1.GetOptions{}); crdErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("gang scheduling CRD %s not found", crd), crdErr)
		}
		fmt.Fprintf(&crdSummary, "  %s: present\n", crd)
	}
	recordRawTextArtifact(ctx, "Gang Scheduling CRDs",
		"kubectl get crd queues.scheduling.run.ai podgroups.scheduling.run.ai",
		crdSummary.String())

	// 3. Pre-flight: ensure enough free GPUs for the gang test.
	total, free, gpuErr := countAvailableGPUs(ctx.Ctx, dynClient)
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

	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		cleanupGangTestResources(cleanupCtx, ctx.Clientset, dynClient, run)
		recordRawTextArtifact(ctx, "Delete test namespace",
			"kubectl delete namespace gang-scheduling-test --ignore-not-found",
			"Deleted gang test pods, claims, and PodGroup; namespace retained intentionally to avoid DRA finalizer stalls.")
	}()

	recordRawTextArtifact(ctx, "Apply test manifest",
		"kubectl apply -f docs/conformance/cncf/manifests/gang-scheduling-test.yaml",
		fmt.Sprintf("Created PodGroup=%s ResourceClaims=%s,%s Pods=%s,%s in namespace=%s",
			run.groupName, run.claims[0], run.claims[1], run.pods[0], run.pods[1], gangTestNamespace))

	if err = deployGangTestResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations); err != nil {
		collectGangTestFailureArtifacts(ctx, dynClient, run, err)
		return err
	}

	pods, err := waitForGangTestPods(ctx.Ctx, ctx.Clientset, run)
	if err != nil {
		collectGangTestFailureArtifacts(ctx, dynClient, run, err)
		return err
	}

	gangReport, err := validateGangPatterns(pods, run)
	if err != nil {
		collectGangTestFailureArtifacts(ctx, dynClient, run, err)
		return err
	}

	collectGangTestArtifacts(ctx, dynClient, pods, gangReport, run)
	return nil
}

func collectGangTestArtifacts(ctx *validators.Context, dynClient dynamic.Interface,
	pods [gangMinMembers]*corev1.Pod, gangReport *gangSchedulingReport, run *gangTestRun) {

	// PodGroup status.
	pgList, listErr := dynClient.Resource(podGroupGVR).Namespace(gangTestNamespace).List(
		ctx.Ctx, metav1.ListOptions{})
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
			run.pods[i], &corev1.PodLogOptions{}).DoRaw(ctx.Ctx)
		label := fmt.Sprintf("gang-worker-%d logs", i)
		if logErr != nil {
			recordRawTextArtifact(ctx, label,
				fmt.Sprintf("kubectl logs gang-worker-%d -n gang-scheduling-test", i),
				fmt.Sprintf("failed to read logs: %v", logErr))
			continue
		}
		recordRawTextArtifact(ctx, label,
			fmt.Sprintf("kubectl logs gang-worker-%d -n gang-scheduling-test", i),
			string(logBytes))
	}
}

func collectGangTestFailureArtifacts(ctx *validators.Context, dynClient dynamic.Interface, run *gangTestRun, cause error) {
	diagCtx, cancel := context.WithTimeout(context.Background(), defaults.DiagnosticTimeout) //nolint:contextcheck // Fresh context: parent may be canceled on timeout.
	defer cancel()

	recordRawTextArtifact(ctx, "Gang scheduling failure",
		"", fmt.Sprintf("Failure: %v\nPodGroup: %s\nPods: %s,%s\nResourceClaims: %s,%s",
			cause, run.groupName, run.pods[0], run.pods[1], run.claims[0], run.claims[1]))
	recordGangPodDiagnostics(ctx, diagCtx)
	recordGangPodGroupDiagnostics(ctx, diagCtx, dynClient)
	recordGangQueueDiagnostics(ctx, diagCtx, dynClient)
	recordGangResourceSliceDiagnostics(ctx, diagCtx, dynClient)
	recordGangResourceClaimDiagnostics(ctx, diagCtx, dynClient)
	recordGangEventDiagnostics(ctx, diagCtx)
	recordGangKaiSchedulerDiagnostics(ctx, diagCtx)
	recordGangDraDriverDiagnostics(ctx, diagCtx)
}

func recordGangPodDiagnostics(ctx *validators.Context, diagCtx context.Context) {
	pods, err := ctx.Clientset.CoreV1().Pods(gangTestNamespace).List(diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "Gang test pods",
			"kubectl get pods -n gang-scheduling-test -o wide",
			fmt.Sprintf("failed to list gang test pods: %v", err))
		return
	}
	recordRawTextArtifact(ctx, "Gang test pods",
		"kubectl get pods -n gang-scheduling-test -o wide",
		summarizeGangPods(pods.Items))
}

func recordGangPodGroupDiagnostics(ctx *validators.Context, diagCtx context.Context, dynClient dynamic.Interface) {
	podGroups, err := dynClient.Resource(podGroupGVR).Namespace(gangTestNamespace).List(
		diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "Gang test PodGroups",
			"kubectl get podgroups -n gang-scheduling-test -o yaml",
			fmt.Sprintf("failed to list gang test PodGroups: %v", err))
		return
	}
	recordObjectYAMLArtifact(ctx, "Gang test PodGroups",
		"kubectl get podgroups -n gang-scheduling-test -o yaml", podGroups)
}

func recordGangQueueDiagnostics(ctx *validators.Context, diagCtx context.Context, dynClient dynamic.Interface) {
	queues, err := dynClient.Resource(queueGVR).List(diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "KAI queues",
			"kubectl get queues.scheduling.run.ai -o yaml",
			fmt.Sprintf("failed to list KAI queues: %v", err))
		return
	}
	recordObjectYAMLArtifact(ctx, "KAI queues",
		"kubectl get queues.scheduling.run.ai -o yaml", queues)
}

func recordGangResourceClaimDiagnostics(ctx *validators.Context, diagCtx context.Context, dynClient dynamic.Interface) {
	claims, err := dynClient.Resource(claimGVR).Namespace(gangTestNamespace).List(
		diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "Gang test ResourceClaims",
			"kubectl get resourceclaims -n gang-scheduling-test -o yaml",
			fmt.Sprintf("failed to list gang test ResourceClaims: %v", err))
		return
	}
	recordObjectYAMLArtifact(ctx, "Gang test ResourceClaims",
		"kubectl get resourceclaims -n gang-scheduling-test -o yaml", claims)
}

func recordGangResourceSliceDiagnostics(ctx *validators.Context, diagCtx context.Context, dynClient dynamic.Interface) {
	slices, err := dynClient.Resource(resourceSliceGVR).List(diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "ResourceSlices",
			"kubectl get resourceslices -o yaml",
			fmt.Sprintf("failed to list ResourceSlices: %v", err))
		return
	}
	recordObjectYAMLArtifact(ctx, "ResourceSlices",
		"kubectl get resourceslices -o yaml", slices)
}

func recordGangEventDiagnostics(ctx *validators.Context, diagCtx context.Context) {
	events, err := ctx.Clientset.CoreV1().Events(gangTestNamespace).List(
		diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "Gang test events",
			"kubectl get events -n gang-scheduling-test --sort-by=.lastTimestamp",
			fmt.Sprintf("failed to list gang test events: %v", err))
		return
	}
	recordRawTextArtifact(ctx, "Gang test events",
		"kubectl get events -n gang-scheduling-test --sort-by=.lastTimestamp",
		summarizeGangEvents(events.Items))
}

func recordGangKaiSchedulerDiagnostics(ctx *validators.Context, diagCtx context.Context) {
	pods, err := ctx.Clientset.CoreV1().Pods("kai-scheduler").List(diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "KAI scheduler pods",
			"kubectl get pods -n kai-scheduler -o wide",
			fmt.Sprintf("failed to list KAI scheduler pods: %v", err))
		return
	}
	recordRawTextArtifact(ctx, "KAI scheduler pods",
		"kubectl get pods -n kai-scheduler -o wide",
		summarizeGangPods(pods.Items))
	serviceAccounts := kaiSchedulerServiceAccountNames(pods.Items)
	recordGangKaiSchedulerRBACDiagnostics(ctx, diagCtx, serviceAccounts)
	recordGangKaiSchedulerAccessDiagnostics(ctx, diagCtx, serviceAccounts)
	recordGangPodLogs(ctx, diagCtx, "kai-scheduler", "KAI scheduler", pods.Items)
}

func recordGangDraDriverDiagnostics(ctx *validators.Context, diagCtx context.Context) {
	pods, err := ctx.Clientset.CoreV1().Pods("nvidia-dra-driver").List(diagCtx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "DRA driver pods",
			"kubectl get pods -n nvidia-dra-driver -o wide",
			fmt.Sprintf("failed to list DRA driver pods: %v", err))
		return
	}
	recordRawTextArtifact(ctx, "DRA driver pods",
		"kubectl get pods -n nvidia-dra-driver -o wide",
		summarizeGangPods(pods.Items))
	recordGangPodLogs(ctx, diagCtx, "nvidia-dra-driver", "DRA driver", pods.Items)
}

func recordGangKaiSchedulerRBACDiagnostics(ctx *validators.Context, diagCtx context.Context, serviceAccounts []string) {
	serviceAccountSet := stringSet(serviceAccounts)
	var out strings.Builder

	fmt.Fprintf(&out, "ServiceAccounts: %s\n\n", strings.Join(serviceAccounts, ","))

	roleBindings, err := ctx.Clientset.RbacV1().RoleBindings("kai-scheduler").List(diagCtx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(&out, "RoleBindings: failed to list: %v\n", err)
	} else {
		fmt.Fprintln(&out, "RoleBindings:")
		for _, binding := range roleBindings.Items {
			if shouldRecordKaiRBACBinding(binding.Name, binding.Subjects, serviceAccountSet) {
				fmt.Fprintf(&out, "  %s roleRef=%s/%s subjects=%s\n",
					binding.Name, binding.RoleRef.Kind, binding.RoleRef.Name, summarizeRBACSubjects(binding.Subjects))
			}
		}
	}

	clusterRoleBindings, err := ctx.Clientset.RbacV1().ClusterRoleBindings().List(diagCtx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(&out, "\nClusterRoleBindings: failed to list: %v\n", err)
	} else {
		fmt.Fprintln(&out, "\nClusterRoleBindings:")
		for _, binding := range clusterRoleBindings.Items {
			if shouldRecordKaiRBACBinding(binding.Name, binding.Subjects, serviceAccountSet) {
				fmt.Fprintf(&out, "  %s roleRef=%s/%s subjects=%s\n",
					binding.Name, binding.RoleRef.Kind, binding.RoleRef.Name, summarizeRBACSubjects(binding.Subjects))
			}
		}
	}

	recordRawTextArtifact(ctx, "KAI scheduler RBAC bindings",
		"kubectl get rolebindings -n kai-scheduler -o wide && kubectl get clusterrolebindings -o wide",
		out.String())
}

func recordGangKaiSchedulerAccessDiagnostics(ctx *validators.Context, diagCtx context.Context, serviceAccounts []string) {
	checks := []struct {
		verb      string
		group     string
		resource  string
		namespace string
	}{
		{verb: "list", group: "scheduling.run.ai", resource: "queues"},
		{verb: "watch", group: "scheduling.run.ai", resource: "queues"},
		{verb: "list", group: "scheduling.run.ai", resource: "podgroups", namespace: gangTestNamespace},
		{verb: "watch", group: "scheduling.run.ai", resource: "podgroups", namespace: gangTestNamespace},
		{verb: "list", group: "resource.k8s.io", resource: "resourceclaims", namespace: gangTestNamespace},
		{verb: "watch", group: "resource.k8s.io", resource: "resourceclaims", namespace: gangTestNamespace},
		{verb: "list", group: "resource.k8s.io", resource: "resourceslices"},
		{verb: "watch", group: "resource.k8s.io", resource: "resourceslices"},
	}

	var out strings.Builder
	for _, serviceAccount := range serviceAccounts {
		user := fmt.Sprintf("system:serviceaccount:kai-scheduler:%s", serviceAccount)
		fmt.Fprintf(&out, "ServiceAccount: %s\n", user)
		for _, check := range checks {
			sar := &authv1.SubjectAccessReview{
				Spec: authv1.SubjectAccessReviewSpec{
					User: user,
					ResourceAttributes: &authv1.ResourceAttributes{
						Namespace: check.namespace,
						Verb:      check.verb,
						Group:     check.group,
						Resource:  check.resource,
					},
				},
			}
			result, err := ctx.Clientset.AuthorizationV1().SubjectAccessReviews().Create(
				diagCtx, sar, metav1.CreateOptions{})
			if err != nil {
				fmt.Fprintf(&out, "  %s %s/%s namespace=%s error=%v\n",
					check.verb, check.group, check.resource, valueOrNone(check.namespace), err)
				continue
			}
			fmt.Fprintf(&out, "  %s %s/%s namespace=%s allowed=%t reason=%s evaluationError=%s\n",
				check.verb, check.group, check.resource, valueOrNone(check.namespace),
				result.Status.Allowed, valueOrNone(result.Status.Reason), valueOrNone(result.Status.EvaluationError))
		}
	}

	recordRawTextArtifact(ctx, "KAI scheduler access review",
		"kubectl auth can-i <verb> <resource> --as=system:serviceaccount:kai-scheduler:<serviceaccount>",
		out.String())
}

func recordGangPodLogs(ctx *validators.Context, diagCtx context.Context, namespace, labelPrefix string, pods []corev1.Pod) {
	tailLines := int64(200)
	for _, pod := range pods {
		for _, containerName := range gangPodContainerNames(pod) {
			logBytes, logErr := ctx.Clientset.CoreV1().Pods(namespace).GetLogs(
				pod.Name, &corev1.PodLogOptions{Container: containerName, TailLines: &tailLines}).DoRaw(diagCtx)
			label := fmt.Sprintf("%s logs: %s/%s", labelPrefix, pod.Name, containerName)
			equivalent := fmt.Sprintf("kubectl logs -n %s %s -c %s --tail=200",
				namespace, pod.Name, containerName)
			if logErr != nil {
				recordRawTextArtifact(ctx, label, equivalent,
					fmt.Sprintf("failed to read logs: %v", logErr))
				continue
			}
			recordRawTextArtifact(ctx, label, equivalent, string(logBytes))
		}
	}
}

func gangPodContainerNames(pod corev1.Pod) []string {
	containers := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, container := range pod.Spec.InitContainers {
		containers = append(containers, container.Name)
	}
	for _, container := range pod.Spec.Containers {
		containers = append(containers, container.Name)
	}
	return containers
}

func kaiSchedulerServiceAccountNames(pods []corev1.Pod) []string {
	seen := map[string]struct{}{}
	for _, pod := range pods {
		name := pod.Spec.ServiceAccountName
		if name == "" {
			name = "default"
		}
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func bindingReferencesServiceAccount(subjects []rbacv1.Subject, namespace string, names map[string]struct{}) bool {
	for _, subject := range subjects {
		if subject.Kind != rbacv1.ServiceAccountKind || subject.Namespace != namespace {
			continue
		}
		if _, ok := names[subject.Name]; ok {
			return true
		}
	}
	return false
}

func shouldRecordKaiRBACBinding(name string, subjects []rbacv1.Subject, serviceAccounts map[string]struct{}) bool {
	return bindingReferencesServiceAccount(subjects, "kai-scheduler", serviceAccounts) ||
		strings.Contains(strings.ToLower(name), "kai")
}

func summarizeRBACSubjects(subjects []rbacv1.Subject) string {
	if len(subjects) == 0 {
		return noneValue
	}
	values := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		values = append(values, fmt.Sprintf("%s/%s/%s",
			valueOrNone(subject.Kind), valueOrNone(subject.Namespace), valueOrNone(subject.Name)))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func valueOrNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return noneValue
	}
	return v
}

func summarizeGangPods(pods []corev1.Pod) string {
	if len(pods) == 0 {
		return "no pods found"
	}
	pods = append([]corev1.Pod(nil), pods...)
	sort.SliceStable(pods, func(i, j int) bool {
		return pods[i].Name < pods[j].Name
	})

	var out strings.Builder
	for _, pod := range pods {
		fmt.Fprintf(&out, "%s phase=%s node=%s scheduler=%s ready=%s waiting=%s claims=%s\n",
			pod.Name, pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName),
			valueOrUnknown(pod.Spec.SchedulerName), podReadyCount(pod),
			podWaitingStatus(&pod), gangPodClaimNames(pod))
		for _, cond := range pod.Status.Conditions {
			fmt.Fprintf(&out, "  condition %s=%s reason=%s message=%s\n",
				cond.Type, cond.Status, valueOrUnknown(cond.Reason), valueOrUnknown(cond.Message))
		}
		for _, cs := range pod.Status.ContainerStatuses {
			appendGangContainerStatus(&out, "container", cs)
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			appendGangContainerStatus(&out, "initContainer", cs)
		}
	}
	return out.String()
}

func gangPodClaimNames(pod corev1.Pod) string {
	if len(pod.Spec.ResourceClaims) == 0 {
		return noneValue
	}
	claims := make([]string, 0, len(pod.Spec.ResourceClaims))
	for _, claim := range pod.Spec.ResourceClaims {
		target := gangUnknownValue
		if claim.ResourceClaimName != nil {
			target = *claim.ResourceClaimName
		} else if claim.ResourceClaimTemplateName != nil {
			target = "template:" + *claim.ResourceClaimTemplateName
		}
		claims = append(claims, claim.Name+"="+target)
	}
	return strings.Join(claims, ",")
}

func appendGangContainerStatus(out *strings.Builder, kind string, cs corev1.ContainerStatus) {
	fmt.Fprintf(out, "  %s %s ready=%t restartCount=%d state=%s image=%s\n",
		kind, cs.Name, cs.Ready, cs.RestartCount, gangContainerState(cs.State), cs.Image)
}

func gangContainerState(state corev1.ContainerState) string {
	switch {
	case state.Running != nil:
		return "Running"
	case state.Waiting != nil:
		return fmt.Sprintf("Waiting(%s: %s)",
			valueOrUnknown(state.Waiting.Reason), valueOrUnknown(state.Waiting.Message))
	case state.Terminated != nil:
		return fmt.Sprintf("Terminated(exitCode=%d reason=%s message=%s)",
			state.Terminated.ExitCode, valueOrUnknown(state.Terminated.Reason),
			valueOrUnknown(state.Terminated.Message))
	default:
		return noneValue
	}
}

func summarizeGangEvents(events []corev1.Event) string {
	if len(events) == 0 {
		return "no events found"
	}
	events = append([]corev1.Event(nil), events...)
	sort.SliceStable(events, func(i, j int) bool {
		return gangEventTime(events[i]).Before(gangEventTime(events[j]))
	})
	if len(events) > 50 {
		events = events[len(events)-50:]
	}

	var out strings.Builder
	for _, event := range events {
		fmt.Fprintf(&out, "%s %s %s/%s reason=%s count=%d message=%s\n",
			gangEventTime(event).Format(time.RFC3339), event.Type,
			event.InvolvedObject.Kind, event.InvolvedObject.Name,
			event.Reason, event.Count, event.Message)
	}
	return out.String()
}

func gangEventTime(event corev1.Event) time.Time {
	switch {
	case !event.EventTime.Time.IsZero():
		return event.EventTime.Time
	case !event.LastTimestamp.Time.IsZero():
		return event.LastTimestamp.Time
	case !event.FirstTimestamp.Time.IsZero():
		return event.FirstTimestamp.Time
	default:
		return event.CreationTimestamp.Time
	}
}

// deployGangTestResources creates the namespace, PodGroup, ResourceClaims, and Pods.
// tolerations, when non-nil, replace the default tolerate-all policy on test pods.
func deployGangTestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gangTestRun, tolerations []corev1.Toleration) error {
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

		pod := buildGangTestPod(run, i, tolerations)
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
// The kai.scheduler/queue label is required by KAI v0.13.0+ for DRA claims.
func buildGangResourceClaim(run *gangTestRun, index int) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceClaim",
			"metadata": map[string]interface{}{
				"name":      run.claims[index],
				"namespace": gangTestNamespace,
				"labels": map[string]interface{}{
					"kai.scheduler/queue": "default-queue",
				},
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
// tolerations, when non-nil, replace the default tolerate-all policy.
func buildGangTestPod(run *gangTestRun, index int, tolerations []corev1.Toleration) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
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
			Tolerations:   tolerations,
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:              "gpu",
					ResourceClaimName: helper.StrPtr(run.claims[index]),
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
