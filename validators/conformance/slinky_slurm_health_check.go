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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	slinkySlurmComponent        = "slinky-slurm"
	slinkySlurmNamespace        = "slurm"
	kwokNodeAnnotation          = "kwok.x-k8s.io/node"
	defaultContainerAnnotation  = "kubectl.kubernetes.io/default-container"
	slinkyLoginPodContainerName = "login"
)

var (
	slinkyLoginSetGVR = schema.GroupVersionResource{
		Group:    "slinky.slurm.net",
		Version:  "v1beta1",
		Resource: "loginsets",
	}
	slinkyNodeSetGVR = schema.GroupVersionResource{
		Group:    "slinky.slurm.net",
		Version:  "v1beta1",
		Resource: "nodesets",
	}
)

type slinkySlurmHealthCommand struct {
	label         string
	command       []string
	requireStdout bool
}

// slinkySlurmSinfoIdleMixShell requires at least one idle or mixed Slurm node.
// grep -q exits 0 when sinfo prints data lines and 1 when inventory is empty.
const slinkySlurmSinfoIdleMixShell = "sinfo -h -Ne -t idle,mix | grep -q ."

var slinkySlurmHealthCommands = []slinkySlurmHealthCommand{
	{
		label:         "scontrol ping",
		command:       []string{"scontrol", "ping"},
		requireStdout: true,
	},
	{
		label:         "sinfo idle/mix",
		command:       []string{"/bin/sh", "-c", slinkySlurmSinfoIdleMixShell},
		requireStdout: false,
	},
	{
		label:         "srun hostname",
		command:       []string{"srun", "--immediate=5", "--time=0:03", "hostname"},
		requireStdout: true,
	},
}

var slinkyExecCommand podExecFunc = execPodCommand

var slinkyLoginPodExecOptions = podExecOptions{
	DefaultContainerAnnotation: defaultContainerAnnotation,
	PreferredContainerName:     slinkyLoginPodContainerName,
}

// CheckSlinkySlurmHealth validates that a Slinky-managed Slurm cluster is
// reachable from the login pod, has idle or mixed worker nodes, and can
// schedule a minimal job without queueing indefinitely.
func CheckSlinkySlurmHealth(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}
	if ctx.RESTConfig == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}
	if ctx.ValidationInput == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "validation is not available")
	}
	if !recipeHasComponent(ctx, slinkySlurmComponent) {
		return validators.Skip("slinky-slurm component not present in recipe")
	}
	namespace := resolveSlinkySlurmNamespace(ctx)

	if err := discoverSlinkySetAPIs(ctx); err != nil {
		return err
	}
	if err := skipIfAllNodeSetPodsAreKWOK(ctx, namespace); err != nil {
		return err
	}

	loginPod, err := findReadySlinkyLoginPod(ctx, namespace)
	if err != nil {
		return err
	}
	recordSlinkyInventories(ctx, namespace, loginPod)

	failures := runSlinkySlurmHealthCommands(ctx, namespace, loginPod.Name)
	if len(failures) > 0 {
		return errors.New(errors.ErrCodeInternal,
			"Slinky Slurm health commands failed:\n"+strings.Join(failures, "\n"))
	}

	return nil
}

func resolveSlinkySlurmNamespace(ctx *validators.Context) string {
	if ctx.ValidationInput == nil {
		return slinkySlurmNamespace
	}
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == slinkySlurmComponent && ref.IsEnabled() && strings.TrimSpace(ref.Namespace) != "" {
			return ref.Namespace
		}
	}
	return slinkySlurmNamespace
}

func runSlinkySlurmHealthCommands(ctx *validators.Context, namespace, loginPodName string) []string {
	var failures []string
	for _, check := range slinkySlurmHealthCommands {
		select {
		case <-ctx.Ctx.Done():
			failures = append(failures, fmt.Sprintf("context canceled: %v", ctx.Ctx.Err()))
			return failures
		default:
		}
		result, execErr := slinkyExecCommand(
			ctx.Ctx, ctx, namespace, loginPodName, check.command, slinkyLoginPodExecOptions)
		recordSlinkyExecResult(ctx, namespace, loginPodName, check, result, execErr)
		if execErr != nil {
			failures = append(failures, fmt.Sprintf("%s: exec failed: %v", check.label, execErr))
			continue
		}
		if result.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("%s: exit code %d", check.label, result.ExitCode))
			continue
		}
		if check.requireStdout && strings.TrimSpace(result.Stdout) == "" {
			failures = append(failures, fmt.Sprintf("%s: empty stdout", check.label))
		}
	}
	return failures
}

func discoverSlinkySetAPIs(ctx *validators.Context) error {
	resources, err := ctx.Clientset.Discovery().ServerResourcesForGroupVersion("slinky.slurm.net/v1beta1")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return validators.Skip("Slinky Slurm API not available")
		}
		return errors.Wrap(errors.ErrCodeInternal, "failed to discover Slinky Slurm API", err)
	}

	found := map[string]bool{}
	for _, resource := range resources.APIResources {
		isLoginSet := resource.Name == slinkyLoginSetGVR.Resource && resource.Kind == "LoginSet"
		isNodeSet := resource.Name == slinkyNodeSetGVR.Resource && resource.Kind == "NodeSet"
		if isLoginSet || isNodeSet {
			found[resource.Name] = true
		}
	}
	if !found[slinkyLoginSetGVR.Resource] || !found[slinkyNodeSetGVR.Resource] {
		return validators.Skip("Slinky Slurm LoginSet/NodeSet API not available")
	}
	return nil
}

func skipIfAllNodeSetPodsAreKWOK(ctx *validators.Context, namespace string) error {
	pods, err := listSlinkyNodeSetPods(ctx, namespace)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return errors.New(errors.ErrCodeNotFound, "slinky-slurm selected but no NodeSet pods were found")
	}

	var resolved, kwok int
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		node, getErr := ctx.Clientset.CoreV1().Nodes().Get(ctx.Ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if getErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to get node %s for NodeSet pod %s", pod.Spec.NodeName, pod.Name), getErr)
		}
		resolved++
		if _, ok := node.Annotations[kwokNodeAnnotation]; ok {
			kwok++
		}
	}
	if resolved == len(pods) && kwok == resolved {
		return validators.Skip("Slinky NodeSet pods are on KWOK nodes; skipping Slurm health validation")
	}
	return nil
}

func listSlinkyNodeSetPods(ctx *validators.Context, namespace string) ([]corev1.Pod, error) {
	return listPodsForSlinkySetSelectors(ctx, namespace, slinkyNodeSetGVR, "NodeSet")
}

func listPodsForSlinkySetSelectors(
	ctx *validators.Context,
	namespace string,
	gvr schema.GroupVersionResource,
	kind string,
) ([]corev1.Pod, error) {

	sets, err := listSlinkySetsForController(ctx, namespace, gvr, kind)
	if err != nil {
		return nil, err
	}

	pods := []corev1.Pod{}
	for _, set := range sets {
		if _, parseErr := labels.Parse(set.selector); parseErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("invalid %s selector for %s/%s: %q",
					kind, namespace, set.name, set.selector), parseErr)
		}
		podList, listErr := ctx.Clientset.CoreV1().Pods(namespace).List(ctx.Ctx, metav1.ListOptions{
			LabelSelector: set.selector,
		})
		if listErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to list Slinky Slurm pods for %s/%s", kind, set.name), listErr)
		}
		pods = append(pods, podList.Items...)
	}
	return pods, nil
}

func findReadySlinkyLoginPod(ctx *validators.Context, namespace string) (*corev1.Pod, error) {
	pods, err := listPodsForSlinkySetSelectors(ctx, namespace, slinkyLoginSetGVR, "LoginSet")
	if err != nil {
		return nil, err
	}

	var summary strings.Builder
	var selected *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		fmt.Fprintf(&summary, "%s phase=%s ready=%t node=%s\n",
			pod.Name, pod.Status.Phase, podIsReady(pod), valueOrUnknown(pod.Spec.NodeName))
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning && podIsReady(pod) &&
			(selected == nil || pod.CreationTimestamp.After(selected.CreationTimestamp.Time)) {

			selected = pod
		}
	}
	if selected != nil {
		return selected, nil
	}
	return nil, errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("no ready login pod found for Slinky LoginSet selectors in %s:\n%s",
			namespace, strings.TrimSpace(summary.String())))
}

type slinkySetSelection struct {
	kind     string
	name     string
	selector string
}

func listSlinkySetsForController(
	ctx *validators.Context,
	namespace string,
	gvr schema.GroupVersionResource,
	kind string,
) ([]slinkySetSelection, error) {

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
	}
	list, err := dynClient.Resource(gvr).Namespace(namespace).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		code := errors.ErrCodeInternal
		if apierrors.IsNotFound(err) {
			code = errors.ErrCodeNotFound
		}
		return nil, errors.Wrap(code, fmt.Sprintf("failed to list Slinky Slurm %ss in namespace %s", kind, namespace), err)
	}

	selected := make([]slinkySetSelection, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		if item.GetAPIVersion() != "slinky.slurm.net/v1beta1" || item.GetKind() != kind {
			continue
		}
		controllerName, _, controllerNameErr := unstructured.NestedString(item.Object, "spec", "controllerRef", "name")
		if controllerNameErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to read controllerRef.name from %s/%s", kind, item.GetName()), controllerNameErr)
		}
		controllerNamespace, _, controllerNamespaceErr := unstructured.NestedString(item.Object, "spec", "controllerRef", "namespace")
		if controllerNamespaceErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to read controllerRef.namespace from %s/%s", kind, item.GetName()), controllerNamespaceErr)
		}
		if controllerName != slinkySlurmComponent {
			continue
		}
		if controllerNamespace != "" && controllerNamespace != namespace {
			continue
		}
		selector, found, selectorErr := unstructured.NestedString(item.Object, "status", "selector")
		if selectorErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to read selector from %s/%s", kind, item.GetName()), selectorErr)
		}
		if !found || strings.TrimSpace(selector) == "" {
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("Slinky Slurm %s %s/%s has no status.selector",
					kind, item.GetNamespace(), item.GetName()))
		}
		selected = append(selected, slinkySetSelection{
			kind:     kind,
			name:     item.GetName(),
			selector: selector,
		})
	}
	if len(selected) == 0 {
		return nil, errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no Slinky Slurm %s found for controllerRef.name=%s", kind, slinkySlurmComponent))
	}
	return selected, nil
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func recordSlinkyInventories(ctx *validators.Context, namespace string, loginPod *corev1.Pod) {
	slurmPods, slurmPodsErr := ctx.Clientset.CoreV1().Pods(namespace).List(ctx.Ctx, metav1.ListOptions{})
	if slurmPodsErr != nil {
		recordRawTextArtifact(ctx, "Slinky Slurm pods", fmt.Sprintf("kubectl get pods -n %s -o wide", namespace),
			fmt.Sprintf("failed to list pods: %v", slurmPodsErr))
	} else {
		var podSummary strings.Builder
		for _, pod := range slurmPods.Items {
			fmt.Fprintf(&podSummary, "%-48s ready=%s phase=%s node=%s\n",
				pod.Name, podReadyCount(pod), pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName))
		}
		recordRawTextArtifact(ctx, "Slinky Slurm pods", fmt.Sprintf("kubectl get pods -n %s -o wide", namespace), podSummary.String())
	}

	nodeSetPods, nodeSetErr := listSlinkyNodeSetPods(ctx, namespace)
	if nodeSetErr != nil {
		recordRawTextArtifact(ctx, "Slinky Slurm NodeSet pods", fmt.Sprintf("kubectl get pods -n %s", namespace),
			fmt.Sprintf("failed to list NodeSet pods: %v", nodeSetErr))
	} else {
		var nodeSetSummary strings.Builder
		for _, pod := range nodeSetPods {
			fmt.Fprintf(&nodeSetSummary, "%-48s ready=%s phase=%s node=%s\n",
				pod.Name, podReadyCount(pod), pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName))
		}
		recordRawTextArtifact(ctx, "Slinky Slurm NodeSet pods",
			fmt.Sprintf("kubectl -n %s get nodesets -o json | jq -r '.items[] | select(.apiVersion == \"slinky.slurm.net/v1beta1\") | .status.selector'", namespace),
			nodeSetSummary.String())
	}

	recordRawTextArtifact(ctx, "Selected Slinky Slurm login pod", "",
		fmt.Sprintf("Name:      %s/%s\nReady:     %t\nNode:      %s",
			loginPod.Namespace, loginPod.Name, podIsReady(loginPod), valueOrUnknown(loginPod.Spec.NodeName)))
}

func recordSlinkyExecResult(ctx *validators.Context, namespace, podName string, check slinkySlurmHealthCommand, result podExecResult, execErr error) {
	var body strings.Builder
	fmt.Fprintf(&body, "Pod:      %s/%s\n", namespace, podName)
	fmt.Fprintf(&body, "Command:  %s\n", strings.Join(check.command, " "))
	fmt.Fprintf(&body, "ExitCode: %d\n", result.ExitCode)
	if execErr != nil {
		fmt.Fprintf(&body, "Error:    %v\n", execErr)
	}
	fmt.Fprintf(&body, "\nstdout:\n%s\n", result.Stdout)
	fmt.Fprintf(&body, "\nstderr:\n%s\n", result.Stderr)

	recordRawTextArtifact(ctx, fmt.Sprintf("Slinky Slurm %s result", check.label),
		fmt.Sprintf("kubectl exec -n %s %s -- %s", namespace, podName, strings.Join(check.command, " ")),
		body.String())
}
