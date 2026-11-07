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
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	gangTestNSPrefix        = defaults.GangSchedulingTestNamespacePrefix
	gangPodPrefix           = "gang-worker-"
	gangGroupPrefix         = "gang-group-"
	gangNegativePodPrefix   = "gang-neg-worker-"
	gangNegativeGroupPrefix = "gang-neg-group-"
	gangMinMembers          = 2

	// gangNegativeMaxMembers caps the non-fitting PodGroup. The member CPU
	// request is sized so that the group cannot fit whatever the count, so the
	// cap only bounds how many Pod creates the phase issues on a large cluster.
	gangNegativeMaxMembers = 16

	// Per-member requests of the positive case, matching the upstream
	// TestGangScheduling so the scheduler has something to co-schedule against.
	// The negative case keeps the memory request and sizes CPU per cluster.
	gangWorkerCPURequest    = "100m"
	gangWorkerMemoryRequest = "128Mi"

	// gangPhaseLabel marks which phase a test pod belongs to; its values are
	// gangPhasePositive and gangPhaseNegative.
	gangPhaseLabel    = "gang-scheduling.aicr.nvidia.com/phase"
	gangPhasePositive = "positive"
	gangPhaseNegative = "negative"

	// podGroupNameAnnotation is the LOAD-BEARING association between a pod and a
	// pre-created KAI PodGroup: the pod-grouper skips a bare pod carrying it and
	// the scheduler joins the pod to that group. The pod-group.scheduling.run.ai
	// labels alone do NOT associate — the pod-grouper ignores them and
	// auto-creates per-pod groups, silently degrading the test to individual
	// scheduling (proven live on the GB200 conformance cluster, 2026-08-08; KAI
	// v0.14.1 PodGroupAnnotationForPod).
	podGroupNameAnnotation = "pod-group-name"
	podGroupNameLabel      = "pod-group.scheduling.run.ai/name"
	podGroupIDLabel        = "pod-group.scheduling.run.ai/group-id"

	// kaiSchedulerName is the KAI scheduler's canonical identifier — it is
	// simultaneously the install namespace, the schedulerName the test pods
	// request, and the recipe componentRef name that declares the capability.
	kaiSchedulerName = "kai-scheduler"

	// kaiGangRefusalFormat is the affirmative all-or-nothing refusal KAI writes
	// to PodGroup status.schedulingConditions when fewer members fit than
	// minMember (pkg/scheduler/actions/common/allocate.go,
	// handleFailedTaskAllocation: "Resources were found for %d pods while %d
	// are required for gang scheduling"). The negative phase is credited only
	// when this phrase names the group's minMember, so an unbound pod from a
	// stalled or absent scheduler, or a generic quota refusal, never passes.
	kaiGangRefusalFormat = "while %d are required for gang scheduling"
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

// Gang scheduling scope: this check validates KAI PodGroup all-or-nothing
// semantics with CPU-only members, matching the upstream TestGangScheduling.
// GPU access and DRA allocation are covered by the DRA support and secure
// accelerator access checks so full conformance can run on one H100.

// gangTestRun holds per-invocation resource names to avoid collisions.
type gangTestRun struct {
	suffix    string
	namespace string

	// Positive case: a fitting PodGroup of gangMinMembers members.
	groupName string
	pods      [gangMinMembers]string

	// Negative case: a PodGroup whose members cannot all fit. negPods is
	// filled once the cluster has been sized (see sizeGangNegativeShape).
	negGroupName string
	negPods      []string
	negShape     gangNegativeShape

	// imagePullSecrets and validatorNamespace are the --image-pull-secret
	// names the orchestrator forwarded and where they live, copied into the
	// per-run namespace at creation and attached to every member pod, as
	// gpuTestRun does for the GPU probes.
	imagePullSecrets   []string
	validatorNamespace string
}

// imagePullSecretRefs returns the pod-spec references to the run's copied
// image pull Secrets; nil when the orchestrator forwarded none.
func (run *gangTestRun) imagePullSecretRefs() []corev1.LocalObjectReference {
	if run == nil || len(run.imagePullSecrets) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, 0, len(run.imagePullSecrets))
	for _, name := range run.imagePullSecrets {
		refs = append(refs, corev1.LocalObjectReference{Name: name})
	}
	return refs
}

type gangSchedulingReport struct {
	EarliestScheduled time.Time
	LatestScheduled   time.Time
	CoScheduleSpan    time.Duration
}

// gangNegativeShape is the per-cluster sizing of the non-fitting PodGroup.
type gangNegativeShape struct {
	Members          int
	CPUPerMember     resource.Quantity
	MemoryPerMember  resource.Quantity
	SchedulableNodes int
	AllocatableCPU   resource.Quantity
}

// gangNegativeObservation is what the verification window saw.
type gangNegativeObservation struct {
	Events int
	Pods   []string
}

// gangPhaseOutcome is one line of the verdict artifact.
type gangPhaseOutcome struct {
	Status string
	Detail string
}

// newGangTestRun names the run's namespace, PodGroups and pods from one
// 128-bit random token, as newGPUTestRun does: a 32-bit suffix made same-name
// collisions across runs plausible, and the unconditional cleanup would then
// have deleted a namespace another run owned. Every prefix plus the 32 hex
// characters plus a member index stays within the 63-character DNS-1123
// limit.
func newGangTestRun() (*gangTestRun, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	suffix := hex.EncodeToString(b)
	run := &gangTestRun{
		suffix:             suffix,
		namespace:          gangTestNSPrefix + suffix,
		groupName:          gangGroupPrefix + suffix,
		negGroupName:       gangNegativeGroupPrefix + suffix,
		imagePullSecrets:   validators.ImagePullSecretNames(),
		validatorNamespace: validators.ValidatorNamespace(),
	}
	for i := range gangMinMembers {
		run.pods[i] = fmt.Sprintf("%s%s-%d", gangPodPrefix, suffix, i)
	}
	return run, nil
}

// setNegativeShape records the sizing and derives the negative member names.
func (run *gangTestRun) setNegativeShape(shape gangNegativeShape) {
	run.negShape = shape
	run.negPods = make([]string, shape.Members)
	for i := range shape.Members {
		run.negPods[i] = fmt.Sprintf("%s%s-%d", gangNegativePodPrefix, run.suffix, i)
	}
}

// CheckGangScheduling validates CNCF requirement #7: Gang Scheduling.
// Verifies KAI scheduler deployments are running and required CRDs exist,
// then exercises all-or-nothing scheduling in two phases, both CPU-only:
//
//   - Positive: a PodGroup with minMember=2 and two members requesting 100m
//     CPU and 128Mi must have both members Succeeded, each bound to a node,
//     within defaults.GangPositiveCompletionTimeout, and co-scheduled within
//     defaults.CoScheduleWindow.
//   - Negative: a PodGroup whose members' combined CPU requests exceed the
//     cluster's allocatable CPU must keep zero members bound (spec.nodeName)
//     or Running for defaults.GangNegativeWindow, and its status must carry
//     KAI's affirmative gang refusal naming the group's minMember.
//
// GPU access and DRA isolation are validated separately by the DRA and secure
// accelerator access checks; keeping both phases CPU-only lets one-GPU CI
// clusters run the full conformance phase.
func CheckGangScheduling(ctx *validators.Context) (err error) {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 0. Applicability gate (#2122). KAI scheduler supplies gang scheduling;
	// base recipes declare kai-scheduler. When the recipe declares it, a missing
	// Deployment (or an RBAC/timeout/transport error reading it) is a real
	// failure — not an inapplicable Skip. Only a clean NotFound on a recipe that
	// does NOT declare kai-scheduler skips (the cluster uses another scheduler).
	_, kaiCheckErr := ctx.Clientset.AppsV1().Deployments(kaiSchedulerName).Get(
		ctx.Ctx, "kai-scheduler-default", metav1.GetOptions{})
	if capErr := (validators.Capability{
		Component: kaiSchedulerName,
		Subject:   "KAI scheduler Deployment kai-scheduler/kai-scheduler-default",
		AbsentMsg: "recipe declares kai-scheduler but its Deployment kai-scheduler/kai-scheduler-default is absent — apply the bundle or check RBAC",
		InapplicableMsg: "KAI scheduler not found and kai-scheduler not declared in recipe — " +
			"cluster may use a different scheduler",
	}).Require(ctx, kaiCheckErr, kaiCheckErr == nil); capErr != nil {
		return capErr
	}

	// 1. All KAI scheduler deployments available. Wait (bounded) for each to
	// become Available rather than sampling instantaneously: the single-replica
	// admission webhook can be transiently unavailable (rollout, restart, node
	// pressure), and a point-in-time 0/1 read would flake the whole conformance
	// phase. A genuinely-down deployment still fails after the bound.
	var deploymentsSummary strings.Builder
	for _, name := range kaiSchedulerDeployments {
		deploy, waitErr := waitForDeploymentAvailable(ctx, kaiSchedulerName, name, defaults.K8sPodReadyTimeout)
		if waitErr != nil {
			// Preserve the helper's code (NotFound for missing, Internal for
			// not-available/API failure, Timeout for cancellation) instead of
			// flattening every failure to NotFound.
			return errors.PropagateOrWrap(waitErr, errors.ErrCodeNotFound,
				fmt.Sprintf("KAI scheduler component %s check failed", name))
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
	kaiPods, err := ctx.Clientset.CoreV1().Pods(kaiSchedulerName).List(ctx.Ctx, metav1.ListOptions{})
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
		Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs,
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

	// 3. Functional test, two phases in one per-run namespace.
	run, err := newGangTestRun()
	if err != nil {
		return err
	}

	// The verdict artifact names which phase decided the check, so the
	// evidence page and e2e.log show both phases whichever one fails.
	outcomes := map[string]*gangPhaseOutcome{
		gangPhasePositive: {Status: "NOT RUN"},
		gangPhaseNegative: {Status: "NOT RUN"},
	}
	defer recordGangVerdict(ctx, run, outcomes)

	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		nsErr := cleanupGangTestResources(cleanupCtx, ctx.Clientset, dynClient, run)
		result := fmt.Sprintf("Deleted gang test pods, PodGroups, and the %s namespace.", run.namespace)
		if nsErr != nil {
			// Report the real outcome: a failed namespace delete leaves residue,
			// so don't record a false success (tools/cleanup is the backstop).
			result = fmt.Sprintf(
				"Deleted gang test pods and PodGroups; namespace deletion FAILED (residue may remain, rerun tools/cleanup): %v",
				nsErr)
		}
		recordRawTextArtifact(ctx, "Delete test namespace",
			fmt.Sprintf("kubectl delete namespace %s --ignore-not-found", run.namespace), result)
		err = failOnGangCleanup(err, nsErr)
	}()

	if err = createGangTestNamespace(ctx.Ctx, ctx.Clientset, run); err != nil {
		return err
	}

	positive, err := runGangPositivePhase(ctx, dynClient, run)
	if err != nil {
		outcomes[gangPhasePositive].Status = "FAIL"
		outcomes[gangPhasePositive].Detail = err.Error()
		return wrapGangPhase(err, "positive phase failed")
	}
	outcomes[gangPhasePositive].Status = "PASS"
	outcomes[gangPhasePositive].Detail = fmt.Sprintf(
		"%d/%d members Succeeded and bound within %s; co-schedule span %s (max %s)",
		gangMinMembers, gangMinMembers, defaults.GangPositiveCompletionTimeout,
		positive.CoScheduleSpan, defaults.CoScheduleWindow)

	refusal, err := runGangNegativePhase(ctx, dynClient, run, defaults.GangNegativeWindow)
	if err != nil {
		outcomes[gangPhaseNegative].Status = "FAIL"
		outcomes[gangPhaseNegative].Detail = err.Error()
		return wrapGangPhase(err, "negative phase failed")
	}
	outcomes[gangPhaseNegative].Status = "PASS"
	outcomes[gangPhaseNegative].Detail = fmt.Sprintf(
		"%d members, 0 bound or Running for %s; KAI refusal: %s",
		run.negShape.Members, defaults.GangNegativeWindow, refusal)
	return nil
}

// recordGangVerdict writes the per-phase outcome so the evidence shows which
// phase produced the check's verdict.
func recordGangVerdict(ctx *validators.Context, run *gangTestRun, outcomes map[string]*gangPhaseOutcome) {
	var b strings.Builder
	for _, phase := range []string{gangPhasePositive, gangPhaseNegative} {
		o := outcomes[phase]
		fmt.Fprintf(&b, "%-8s %-7s %s\n", phase+":", o.Status, o.Detail)
	}
	recordRawTextArtifact(ctx, "Gang scheduling verdict",
		fmt.Sprintf("kubectl get pods,podgroups -n %s -o wide", run.namespace), b.String())
}

// createGangTestNamespace creates the per-run namespace (idempotent), stamped
// with the shared ownership label (gpuTestRunLabel=token) so an external
// sweeper can tell a leaked gang namespace from a foreign one, as it can for
// the GPU test namespaces.
func createGangTestNamespace(ctx context.Context, clientset kubernetes.Interface, run *gangTestRun) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   run.namespace,
			Labels: map[string]string{gpuTestRunLabel: run.suffix},
		},
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); k8s.IgnoreAlreadyExists(err) != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}
	// The member pods pull with the forwarded --image-pull-secret Secrets,
	// which a pod resolves only in its own namespace; the copies die with it.
	return copyImagePullSecrets(ctx, clientset, &gpuTestRun{
		token: run.suffix, namespace: run.namespace,
		imagePullSecrets: run.imagePullSecrets, validatorNamespace: run.validatorNamespace,
	})
}

// failOnGangCleanup is the check's final verdict once cleanup ran: a run that
// passed but could not delete its namespace fails, as every other per-run
// namespace check does (cleanupGPUTestNamespace), so a PASS never stands over
// a leaked namespace and its pods; a run that already failed keeps its own
// error, and the cleanup failure stays in the artifact.
//
// Errors:
//   - ErrCodeInternal wrapping nsErr when err is nil and nsErr is not.
func failOnGangCleanup(err, nsErr error) error {
	if err != nil || nsErr == nil {
		return err
	}
	return errors.Wrap(errors.ErrCodeInternal, "gang scheduling test namespace cleanup failed after a passing run", nsErr)
}

// wrapGangPhase names the failed phase in the CTRF message while keeping the
// phase error's code: PropagateOrWrap would return a structured error
// unchanged, dropping the phase name.
func wrapGangPhase(err error, phase string) error {
	if se, ok := stderrors.AsType[*errors.StructuredError](err); ok {
		return errors.Wrap(se.Code, phase, err)
	}
	return errors.Wrap(errors.ErrCodeInternal, phase, err)
}

// runGangPositivePhase deploys the fitting PodGroup and verifies every member
// Succeeded, bound, and co-scheduled within the completion bound.
func runGangPositivePhase(ctx *validators.Context, dynClient dynamic.Interface, run *gangTestRun) (*gangSchedulingReport, error) {
	recordRawTextArtifact(ctx, "Positive phase: apply test manifest",
		"kubectl apply generated CPU-only PodGroup test resources",
		fmt.Sprintf("Created PodGroup=%s (minMember=%d) Pods=%s,%s (%s CPU, %s each) in namespace=%s",
			run.groupName, gangMinMembers, run.pods[0], run.pods[1],
			gangWorkerCPURequest, gangWorkerMemoryRequest, run.namespace))

	if err := deployGangTestResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations); err != nil {
		return nil, err
	}

	pods, err := waitForGangTestPods(ctx.Ctx, ctx.Clientset, run)
	if err != nil {
		collectGangTestArtifacts(ctx, dynClient, pods, nil, run)
		return nil, err
	}

	gangReport, err := validateGangPatterns(pods, run)
	if err != nil {
		collectGangTestArtifacts(ctx, dynClient, pods, nil, run)
		return nil, err
	}

	collectGangTestArtifacts(ctx, dynClient, pods, gangReport, run)
	return gangReport, nil
}

// runGangNegativePhase sizes, deploys, and watches the non-fitting PodGroup
// for window. It returns the KAI refusal message that credited the phase.
func runGangNegativePhase(ctx *validators.Context, dynClient dynamic.Interface, run *gangTestRun, window time.Duration) (string, error) {
	nodes, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return "", classifyK8sReadError(err, "nodes for gang negative sizing")
	}
	shape, err := sizeGangNegativeShape(nodes.Items)
	if err != nil {
		return "", err
	}
	run.setNegativeShape(shape)
	recordRawTextArtifact(ctx, "Negative phase: sizing",
		"kubectl get nodes -o custom-columns=NAME:.metadata.name,CPU:.status.allocatable.cpu",
		fmt.Sprintf("Schedulable nodes: %d\nAllocatable CPU (sum): %s\nMembers (minMember): %d\n"+
			"CPU per member: %s (total %s exceeds allocatable)\nMemory per member: %s\n",
			shape.SchedulableNodes, shape.AllocatableCPU.String(), shape.Members,
			shape.CPUPerMember.String(), totalCPU(shape).String(), shape.MemoryPerMember.String()))

	recordRawTextArtifact(ctx, "Negative phase: apply test manifest",
		"kubectl apply generated non-fitting PodGroup test resources",
		fmt.Sprintf("Created PodGroup=%s (minMember=%d) Pods=%s..%s in namespace=%s",
			run.negGroupName, shape.Members, run.negPods[0], run.negPods[len(run.negPods)-1], run.namespace))

	if err = deployGangNegativeResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations); err != nil {
		return "", err
	}

	selector := gangPhaseSelector(gangPhaseNegative)
	observation, windowErr := holdGangNegativeWindow(ctx.Ctx, ctx.Clientset, run.namespace, selector, run.negPods, window)

	// Record the binding state and PodGroup status whatever the window saw, so
	// a failing run shows which member bound and what the scheduler said.
	collectGangNegativeArtifacts(ctx, dynClient, run, observation, window)
	if windowErr != nil {
		return "", windowErr
	}

	pg, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Get(ctx.Ctx, run.negGroupName, metav1.GetOptions{})
	if err != nil {
		return "", classifyK8sReadError(err, fmt.Sprintf("PodGroup %s/%s", run.namespace, run.negGroupName))
	}
	refusal, err := evaluateGangRefusal(pg, shape.Members)
	if err != nil {
		return "", err
	}
	return refusal, nil
}

// sizeGangNegativeShape sizes a PodGroup that cannot fit: with S Ready,
// schedulable nodes and A their summed allocatable CPU, it takes
// N = min(S+1, gangNegativeMaxMembers) members each requesting
// floor(A/(N-1)) + 1m CPU, so N members demand more than A whatever the
// packing and whatever frees up later. Allocatable, not free, capacity is
// the bound so that workloads finishing during the window cannot make the
// group fit.
//
// The negative phase's oracle (the PodGroup refusal condition versus bound-pod
// detection, and why homogeneous clusters have only the former) is described
// in docs/contributor/validator.md.
func sizeGangNegativeShape(nodes []corev1.Node) (gangNegativeShape, error) {
	var schedulable int
	var allocatable resource.Quantity
	for i := range nodes {
		node := &nodes[i]
		if node.Spec.Unschedulable || !nodeIsReady(node) {
			continue
		}
		cpu, ok := node.Status.Allocatable[corev1.ResourceCPU]
		if !ok || cpu.IsZero() {
			continue
		}
		schedulable++
		allocatable.Add(cpu)
	}
	if schedulable == 0 {
		return gangNegativeShape{}, errors.New(errors.ErrCodeUnavailable,
			"no Ready, schedulable node with allocatable CPU to size the non-fitting PodGroup against")
	}
	members := min(schedulable+1, gangNegativeMaxMembers)
	perMemberMilli := allocatable.MilliValue()/int64(members-1) + 1
	return gangNegativeShape{
		Members:          members,
		CPUPerMember:     *resource.NewMilliQuantity(perMemberMilli, resource.DecimalSI),
		MemoryPerMember:  resource.MustParse(gangWorkerMemoryRequest),
		SchedulableNodes: schedulable,
		AllocatableCPU:   allocatable,
	}, nil
}

func totalCPU(shape gangNegativeShape) *resource.Quantity {
	return resource.NewMilliQuantity(shape.CPUPerMember.MilliValue()*int64(shape.Members), resource.DecimalSI)
}

func nodeIsReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gangPhaseSelector(phase string) string {
	return gangPhaseLabel + "=" + phase
}

// gangBoundPodViolation reports why a pod breaks the all-or-nothing contract:
// bound to a node or Running. Empty means the pod is still unbound.
func gangBoundPodViolation(pod *corev1.Pod) string {
	switch {
	case pod.Spec.NodeName != "" && pod.Status.Phase == corev1.PodRunning:
		return fmt.Sprintf("bound to node %s and Running", pod.Spec.NodeName)
	case pod.Spec.NodeName != "":
		return fmt.Sprintf("bound to node %s (phase=%s)", pod.Spec.NodeName, pod.Status.Phase)
	case pod.Status.Phase == corev1.PodRunning:
		return "Running"
	}
	return ""
}

// holdGangNegativeWindow watches the expected negative members for the whole
// window and fails on the first member that binds, runs, or disappears,
// naming it; every list must be exactly the expected set. The window elapsing
// is the pass signal; the parent context ending is an abort.
func holdGangNegativeWindow(ctx context.Context, clientset kubernetes.Interface, namespace, selector string, expected []string, window time.Duration) (gangNegativeObservation, error) {
	var obs gangNegativeObservation

	windowCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	resumed := false
	for {
		// (Re)list on entry and after every watch closure: the list is the
		// authoritative state and seeds the watch's ResourceVersion.
		list, err := clientset.CoreV1().Pods(namespace).List(windowCtx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			if windowCtx.Err() != nil {
				break
			}
			return obs, classifyK8sReadError(err, "gang negative test pods")
		}
		obs.Pods, err = checkGangNegativeMembers(list, expected)
		if err != nil {
			return obs, err
		}

		if resumed {
			// The fresh list has been judged; pause before re-watching so a
			// flapping watch does not spin, then resume from the list's
			// ResourceVersion so events during the pause replay.
			select {
			case <-windowCtx.Done():
				return finishGangNegativeWindow(ctx, clientset, namespace, selector, expected, obs)
			case <-time.After(defaults.PodPollInterval):
			}
		}
		resumed = true

		watcher, err := clientset.CoreV1().Pods(namespace).Watch(windowCtx, metav1.ListOptions{
			LabelSelector:   selector,
			ResourceVersion: list.ResourceVersion,
		})
		if err != nil {
			if windowCtx.Err() != nil {
				break
			}
			return obs, errors.Wrap(errors.ErrCodeInternal, "failed to watch gang negative test pods", err)
		}

		closed, err := consumeGangNegativeWatch(windowCtx, watcher, expected, &obs)
		watcher.Stop()
		if err != nil {
			return obs, err
		}
		if !closed {
			break // window elapsed
		}
		// Watch channel closed with the window still open (apiserver hiccup,
		// LB drop): re-list and re-watch rather than declaring a verdict.
	}

	return finishGangNegativeWindow(ctx, clientset, namespace, selector, expected, obs)
}

// finishGangNegativeWindow is the verdict after the window elapsed: an abort
// if the caller canceled, otherwise one final authoritative read that must
// show exactly the expected members, all unbound.
func finishGangNegativeWindow(ctx context.Context, clientset kubernetes.Interface, namespace, selector string, expected []string, obs gangNegativeObservation) (gangNegativeObservation, error) {
	// Caller cancellation is an external abort, not a verdict.
	if ctx.Err() != nil {
		return obs, errors.Wrap(errors.ErrCodeTimeout, "gang negative verification window canceled", ctx.Err())
	}

	// Final authoritative read after the window: the watch may have been
	// torn down between the last event and the deadline.
	finalCtx, finalCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer finalCancel()
	list, err := clientset.CoreV1().Pods(namespace).List(finalCtx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return obs, classifyK8sReadError(err, "gang negative test pods after the verification window")
	}
	obs.Pods, err = checkGangNegativeMembers(list, expected)
	if err != nil {
		return obs, err
	}
	return obs, nil
}

// checkGangNegativeMembers requires list to be exactly the expected members,
// none bound or Running, and returns the names seen. A missing member is a
// failure (not a vacuous pass), as is a pod the phase did not create.
func checkGangNegativeMembers(list *corev1.PodList, expected []string) ([]string, error) {
	want := make(map[string]bool, len(expected))
	for _, name := range expected {
		want[name] = false
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		names = append(names, pod.Name)
		if _, ok := want[pod.Name]; !ok {
			return names, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("unexpected pod %s among the gang negative members (%d listed, %d expected)",
					pod.Name, len(list.Items), len(expected)))
		}
		want[pod.Name] = true
		if v := gangBoundPodViolation(pod); v != "" {
			return names, gangPartialSchedulingErr(pod.Name, v)
		}
	}
	for _, name := range expected {
		if !want[name] {
			return names, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang negative member %s is missing (%d of %d members present); "+
					"a member that disappears is not evidence it was held unbound",
					name, len(list.Items), len(expected)))
		}
	}
	if len(expected) == 0 {
		return names, errors.New(errors.ErrCodeInternal,
			"no gang negative members expected (vacuous: nothing was held unbound)")
	}
	return names, nil
}

// consumeGangNegativeWatch drains watch events until the window ends (closed
// false), the channel closes (closed true), or a member binds or is deleted
// (error).
func consumeGangNegativeWatch(windowCtx context.Context, watcher watch.Interface, expected []string, obs *gangNegativeObservation) (closed bool, err error) {
	members := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		members[name] = struct{}{}
	}
	for {
		select {
		case <-windowCtx.Done():
			return false, nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return true, nil
			}
			obs.Events++
			if event.Type == watch.Error {
				slog.Warn("gang negative watch error event; re-listing", "object", event.Object)
				return true, nil
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if event.Type == watch.Deleted {
				if _, isMember := members[pod.Name]; isMember {
					return false, errors.New(errors.ErrCodeInternal,
						fmt.Sprintf("gang negative member %s was deleted during the verification window; "+
							"a member that disappears is not evidence it was held unbound", pod.Name))
				}
				continue
			}
			if v := gangBoundPodViolation(pod); v != "" {
				return false, gangPartialSchedulingErr(pod.Name, v)
			}
		}
	}
}

func gangPartialSchedulingErr(podName, violation string) error {
	return errors.New(errors.ErrCodeInternal,
		fmt.Sprintf("partial scheduling detected: non-fitting gang member %s is %s (all-or-nothing violated)",
			podName, violation))
}

// evaluateGangRefusal credits the negative phase only on KAI's affirmative
// all-or-nothing refusal for this group: a status.schedulingConditions entry
// whose message, or one of whose reasons, says minMember members are required
// for gang scheduling. Returns the matching message.
func evaluateGangRefusal(pg *unstructured.Unstructured, minMember int) (string, error) {
	want := fmt.Sprintf(kaiGangRefusalFormat, minMember)
	conditions, found, err := unstructured.NestedSlice(pg.Object, "status", "schedulingConditions")
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("PodGroup %s status.schedulingConditions is malformed", pg.GetName()), err)
	}
	if !found || len(conditions) == 0 {
		return "", errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("negative phase not credited: PodGroup %s carries no schedulingConditions — "+
				"members stayed unbound but the scheduler recorded no gang refusal (stalled or absent scheduler?)",
				pg.GetName()))
	}
	var seen []string
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		status, _, _ := unstructured.NestedString(cond, "status")
		if status != string(corev1.ConditionTrue) {
			continue
		}
		var messages []string
		if msg, _, _ := unstructured.NestedString(cond, "message"); msg != "" {
			messages = append(messages, msg)
		}
		if reasons, _, _ := unstructured.NestedSlice(cond, "reasons"); len(reasons) > 0 {
			for _, r := range reasons {
				rm, ok := r.(map[string]any)
				if !ok {
					continue
				}
				if msg, _, _ := unstructured.NestedString(rm, "message"); msg != "" {
					messages = append(messages, msg)
				}
			}
		}
		for _, msg := range messages {
			if strings.Contains(msg, want) {
				return msg, nil
			}
			seen = append(seen, msg)
		}
	}
	return "", errors.New(errors.ErrCodeInternal,
		fmt.Sprintf("negative phase not credited: PodGroup %s schedulingConditions carry no gang refusal %q (saw: %s)",
			pg.GetName(), want, strings.Join(seen, " | ")))
}

func collectGangNegativeArtifacts(ctx *validators.Context, dynClient dynamic.Interface, run *gangTestRun, obs gangNegativeObservation, window time.Duration) {
	pods, listErr := ctx.Clientset.CoreV1().Pods(run.namespace).List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: gangPhaseSelector(gangPhaseNegative),
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Verification window: %s (watch events: %d)\n", window, obs.Events)
	if listErr != nil {
		fmt.Fprintf(&b, "failed to list pods: %v\n", listErr)
	} else {
		for i := range pods.Items {
			pod := &pods.Items[i]
			bound := pod.Spec.NodeName
			if bound == "" {
				bound = "<unbound>"
			}
			var sched string
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled {
					sched = fmt.Sprintf("%s/%s", cond.Status, cond.Reason)
					break
				}
			}
			fmt.Fprintf(&b, "%-44s phase=%-9s node=%-44s PodScheduled=%s\n", pod.Name, pod.Status.Phase, bound, sched)
		}
	}
	recordRawTextArtifact(ctx, "Negative phase: pod binding state",
		fmt.Sprintf("kubectl get pods -n %s -l %s -o wide", run.namespace, gangPhaseSelector(gangPhaseNegative)),
		b.String())

	pg, getErr := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Get(ctx.Ctx, run.negGroupName, metav1.GetOptions{})
	equivalent := fmt.Sprintf("kubectl get podgroup %s -n %s -o yaml", run.negGroupName, run.namespace)
	if getErr != nil {
		recordRawTextArtifact(ctx, "Negative phase: PodGroup status", equivalent,
			fmt.Sprintf("failed to get PodGroup: %v", getErr))
		return
	}
	status, _, _ := unstructured.NestedMap(pg.Object, "status")
	recordObjectYAMLArtifact(ctx, "Negative phase: PodGroup status", equivalent, map[string]any{
		"name":   pg.GetName(),
		"spec":   pg.Object[keySpec],
		"status": status,
	})
}

func collectGangTestArtifacts(ctx *validators.Context, dynClient dynamic.Interface,
	pods [gangMinMembers]*corev1.Pod, gangReport *gangSchedulingReport, run *gangTestRun) {

	// PodGroup status.
	pg, getErr := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Get(
		ctx.Ctx, run.groupName, metav1.GetOptions{})
	equivalent := fmt.Sprintf("kubectl get podgroup %s -n %s -o yaml", run.groupName, run.namespace)
	if getErr != nil {
		recordRawTextArtifact(ctx, "Positive phase: PodGroup status", equivalent,
			fmt.Sprintf("failed to get PodGroup: %v", getErr))
	} else {
		status, _, _ := unstructured.NestedMap(pg.Object, "status")
		recordObjectYAMLArtifact(ctx, "Positive phase: PodGroup status", equivalent, map[string]any{
			"name":   pg.GetName(),
			"spec":   pg.Object[keySpec],
			"status": status,
		})
	}

	// Pod status and scheduling timestamps.
	var gangResults strings.Builder
	for i, pod := range pods {
		if pod == nil {
			fmt.Fprintf(&gangResults, "Pod %d: %s  (not terminal within %s)\n", i, run.pods[i], defaults.GangPositiveCompletionTimeout)
			continue
		}
		var schedTime string
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
				schedTime = cond.LastTransitionTime.Format(time.RFC3339)
				break
			}
		}
		fmt.Fprintf(&gangResults, "Pod %d: %s  phase=%s  node=%s  scheduler=%s  scheduled=%s\n",
			i, pod.Name, pod.Status.Phase, pod.Spec.NodeName, pod.Spec.SchedulerName, schedTime)
	}
	fmt.Fprintf(&gangResults, "Completion bound: %s\n", defaults.GangPositiveCompletionTimeout)
	if gangReport != nil {
		fmt.Fprintf(&gangResults, "Co-schedule span: %s\n", gangReport.CoScheduleSpan)
		fmt.Fprintf(&gangResults, "Allowed window:   %s\n", defaults.CoScheduleWindow)
		fmt.Fprintf(&gangResults, "Earliest/Latest:  %s / %s\n",
			gangReport.EarliestScheduled.Format(time.RFC3339),
			gangReport.LatestScheduled.Format(time.RFC3339))
	}
	recordRawTextArtifact(ctx, "Positive phase: pod status",
		fmt.Sprintf("kubectl get pods -n %s -l %s -o wide", run.namespace, gangPhaseSelector(gangPhasePositive)),
		gangResults.String())

	// Worker logs.
	for i := range gangMinMembers {
		logBytes, logErr := ctx.Clientset.CoreV1().Pods(run.namespace).GetLogs(
			run.pods[i], &corev1.PodLogOptions{}).DoRaw(ctx.Ctx)
		label := fmt.Sprintf("Positive phase: %s logs", run.pods[i])
		if logErr != nil {
			recordRawTextArtifact(ctx, label,
				fmt.Sprintf("kubectl logs %s -n %s", run.pods[i], run.namespace),
				fmt.Sprintf("failed to read logs: %v", logErr))
			continue
		}
		recordRawTextArtifact(ctx, label,
			fmt.Sprintf("kubectl logs %s -n %s", run.pods[i], run.namespace),
			string(logBytes))
	}
}

// deployGangTestResources creates the positive PodGroup and worker Pods in the
// run namespace. tolerations, when non-nil, replace the default tolerate-all
// policy on test pods.
func deployGangTestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gangTestRun, tolerations []corev1.Toleration) error {
	podGroup := buildPodGroup(run)
	if _, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Create(
		ctx, podGroup, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create PodGroup", err)
	}

	for i := range gangMinMembers {
		pod := buildGangTestPod(run, i, tolerations)
		if _, err := clientset.CoreV1().Pods(run.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create gang test pod %s", run.pods[i]), err)
		}
	}

	return nil
}

// deployGangNegativeResources creates the non-fitting PodGroup and its members.
func deployGangNegativeResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gangTestRun, tolerations []corev1.Toleration) error {
	podGroup := buildNegativePodGroup(run)
	if _, err := dynClient.Resource(podGroupGVR).Namespace(run.namespace).Create(
		ctx, podGroup, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create non-fitting PodGroup", err)
	}

	for i := range run.negPods {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeTimeout, "creating gang negative test pods canceled", err)
		}
		pod := buildGangNegativePod(run, i, tolerations)
		if _, err := clientset.CoreV1().Pods(run.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create gang negative test pod %s", run.negPods[i]), err)
		}
	}
	return nil
}

// waitForGangTestPods polls until all positive-case pods reach a terminal
// state or defaults.GangPositiveCompletionTimeout elapses.
func waitForGangTestPods(ctx context.Context, clientset kubernetes.Interface, run *gangTestRun) ([gangMinMembers]*corev1.Pod, error) {
	var result [gangMinMembers]*corev1.Pod

	waitCtx, cancel := context.WithTimeout(ctx, defaults.GangPositiveCompletionTimeout)
	defer cancel()

	// Per-pod, not a single latch: a pod whose read recovers must clear its
	// entry, otherwise one early blip would mislabel a genuine "never
	// completed" timeout as "unreadable" long after reads recovered.
	readErrs := make(map[string]error, gangMinMembers)
	err := wait.PollUntilContextCancel(waitCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			allDone := true
			for i := range gangMinMembers {
				if result[i] != nil {
					continue // already terminal
				}
				pod, err := clientset.CoreV1().Pods(run.namespace).Get(
					ctx, run.pods[i], metav1.GetOptions{})
				if err != nil {
					// A read that could not land is not a verdict. Returning a
					// non-nil error here aborts the whole poll, so one throttled
					// or timed-out call would fail a healthy cluster even though
					// the next interval would have succeeded — the same defect
					// #1513 fixed one step earlier in this function. Let the
					// enclosing completion bound decide instead.
					if isK8sTimeoutErr(err) {
						// The wait context ending during this Get is the poll's
						// terminal signal, not evidence of sustained read failures.
						if readFailedBecauseContextEnded(ctx, err) {
							allDone = false
							continue
						}
						readErrs[run.pods[i]] = err
						slog.Debug("transient read while polling gang test pod; retrying",
							"pod", run.pods[i], "error", err)
						allDone = false
						continue
					}
					return false, classifyK8sReadError(err,
						fmt.Sprintf("gang test pod %s", run.pods[i]))
				}
				delete(readErrs, run.pods[i]) // this read landed
				switch pod.Status.Phase {     //nolint:exhaustive // only terminal states matter
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
		// Caller cancellation is an external abort, not the gang timing out.
		if ctx.Err() != nil {
			return result, errors.Wrap(errors.ErrCodeTimeout, "waiting for gang test pods canceled", ctx.Err())
		}
		if waitCtx.Err() != nil {
			// Preserve the last transient read error: a sustained throttle
			// otherwise looks identical to pods that never completed.
			// Only if a still-pending pod's most recent read failed.
			for i := range gangMinMembers {
				if result[i] != nil {
					continue
				}
				if readErr, ok := readErrs[run.pods[i]]; ok {
					return result, errors.Wrap(errors.ErrCodeTimeout,
						fmt.Sprintf("gang test pod %s unreadable (reads kept failing)", run.pods[i]),
						readErr)
				}
			}
			return result, errors.Wrap(errors.ErrCodeTimeout,
				fmt.Sprintf("gang test pods did not complete within %s", defaults.GangPositiveCompletionTimeout), err)
		}
		return result, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "gang test pod polling failed")
	}

	return result, nil
}

// validateGangPatterns verifies all positive-case pods completed successfully,
// were bound by kai-scheduler, and were co-scheduled.
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

		// Pod must have been bound to a node (upstream asserts spec.nodeName).
		if pod.Spec.NodeName == "" {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s has no node assignment", run.pods[i]))
		}

		// Pod must use kai-scheduler.
		if pod.Spec.SchedulerName != kaiSchedulerName {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s schedulerName=%s (want kai-scheduler)",
					run.pods[i], pod.Spec.SchedulerName))
		}

		// Pod must have PodGroup label.
		if pod.Labels[podGroupNameLabel] != run.groupName {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s missing PodGroup label (want %s)",
					run.pods[i], run.groupName))
		}

		// Gang scheduling is intentionally CPU-only. DRA behavior is validated
		// separately by dra-support and secure-accelerator-access.
		if len(pod.Spec.ResourceClaims) != 0 {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("gang test pod %s unexpectedly uses resourceClaims", run.pods[i]))
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

// cleanupGangTestResources tears down both phases' pods and PodGroups and the
// namespace best-effort. It returns the namespace deletion error (nil on
// success) so the caller can record the true outcome instead of a false
// success; pod/PodGroup deletes stay best-effort as the namespace delete
// subsumes them.
func cleanupGangTestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gangTestRun) error {
	podNames := make([]string, 0, gangMinMembers+len(run.negPods))
	podNames = append(podNames, run.pods[:]...)
	podNames = append(podNames, run.negPods...)
	for _, name := range podNames {
		_ = k8s.IgnoreNotFound(clientset.CoreV1().Pods(run.namespace).Delete(
			ctx, name, metav1.DeleteOptions{}))
	}
	// One bounded wait for all of them: unbound pods are deleted immediately
	// by the apiserver, and a per-pod wait would multiply the budget.
	pollCtx, pollCancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer pollCancel()
	_ = wait.PollUntilContextCancel(pollCtx, defaults.PodPollInterval, true,
		func(c context.Context) (bool, error) {
			for _, name := range podNames {
				if c.Err() != nil {
					return false, c.Err()
				}
				_, err := clientset.CoreV1().Pods(run.namespace).Get(c, name, metav1.GetOptions{})
				if err == nil || k8s.IgnoreNotFound(err) != nil {
					return false, nil // still present, or unreadable: keep waiting within the bound
				}
			}
			return true, nil
		})
	for _, group := range []string{run.groupName, run.negGroupName} {
		_ = k8s.IgnoreNotFound(dynClient.Resource(podGroupGVR).Namespace(run.namespace).Delete(
			ctx, group, metav1.DeleteOptions{}))
	}
	// Delete the namespace so a cluster reset leaves no residue. Pods and the
	// PodGroups are already gone, so this is a single bounded (background
	// propagation) API call. tools/cleanup sweeps the gang-scheduling-test-
	// prefix as a backstop for interrupted runs; deleting it here is the
	// primary path.
	//
	// Use a dedicated deadline rather than the shared cleanup ctx: a pod stuck
	// on a finalizer can burn the whole budget in the wait above, and starving
	// this delete is precisely the leak we are fixing.
	nsCtx, nsCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer nsCancel()
	//nolint:contextcheck // fresh deadline so an earlier stuck wait cannot starve namespace teardown
	if err := k8s.IgnoreNotFound(clientset.CoreV1().Namespaces().Delete(nsCtx, run.namespace, metav1.DeleteOptions{})); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to delete gang test namespace", err)
	}
	// Confirm the namespace is gone within the budget, so the artifact reports
	// what was observed. An accepted deletion that is still Terminating when
	// the budget ends is not a leak: the namespace controller finishes it
	// server-side, as cleanupGPUTestNamespace also records. It is returned as
	// an error only when the namespace is still present and not Terminating.
	//nolint:contextcheck // same fresh deadline as the Delete above, so a stuck wait earlier cannot starve the confirmation
	return confirmGangNamespaceGone(nsCtx, clientset, run.namespace)
}

// confirmGangNamespaceGone polls until the namespace is absent or ctx ends.
//
// Errors:
//   - ErrCodeInternal when the budget ends with the namespace present and not
//     Terminating (no deletion in progress), or a read failed throughout.
func confirmGangNamespaceGone(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	var last *corev1.Namespace
	var lastErr error
	pollErr := wait.PollUntilContextCancel(ctx, defaults.PodPollInterval, true, func(c context.Context) (bool, error) {
		ns, err := clientset.CoreV1().Namespaces().Get(c, namespace, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			return true, nil
		case err != nil:
			// A Get cut off by the poll deadline is not a read failure of the
			// namespace; keep the last real observation for the verdict.
			if c.Err() == nil {
				lastErr = err
			}
			return false, nil
		}
		last, lastErr = ns, nil
		return false, nil
	})
	if pollErr == nil {
		return nil
	}
	if last != nil && last.DeletionTimestamp != nil {
		slog.Warn("gang test namespace deletion accepted but not confirmed within the cleanup budget; the namespace controller finishes it server-side",
			"namespace", namespace)
		return nil
	}
	if lastErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("gang test namespace %s could not be confirmed gone within the cleanup budget", namespace), lastErr)
	}
	return errors.New(errors.ErrCodeInternal, fmt.Sprintf("gang test namespace %s still exists with no deletion in progress after the cleanup budget", namespace))
}

// buildPodGroup returns the unstructured PodGroup for the positive case.
func buildPodGroup(run *gangTestRun) *unstructured.Unstructured {
	return newPodGroup(run.namespace, run.groupName, gangMinMembers)
}

// buildNegativePodGroup returns the unstructured PodGroup for the negative case.
func buildNegativePodGroup(run *gangTestRun) *unstructured.Unstructured {
	return newPodGroup(run.namespace, run.negGroupName, run.negShape.Members)
}

func newPodGroup(namespace, name string, minMember int) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: "scheduling.run.ai/v2alpha2",
			keyKind:       "PodGroup",
			keyMetadata: map[string]any{
				keyName:      name,
				keyNamespace: namespace,
			},
			keySpec: map[string]any{
				"minMember": int64(minMember),
				"queue":     "default-queue",
			},
		},
	}
}

// buildGangTestPod returns the Pod spec for a positive-case worker.
// tolerations, when non-nil, replace the default tolerate-all policy.
func buildGangTestPod(run *gangTestRun, index int, tolerations []corev1.Toleration) *corev1.Pod {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(gangWorkerCPURequest),
		corev1.ResourceMemory: resource.MustParse(gangWorkerMemoryRequest),
	}
	return newGangMemberPod(run, run.pods[index], run.groupName, gangPhasePositive,
		[]string{"sh", "-c", fmt.Sprintf("echo 'Gang worker %d completed successfully'", index)},
		requests, tolerations)
}

// buildGangNegativePod returns the Pod spec for a non-fitting member. It
// sleeps rather than exits so that, under a scheduler without gang semantics,
// a bound member is also observed Running inside the window.
func buildGangNegativePod(run *gangTestRun, index int, tolerations []corev1.Toleration) *corev1.Pod {
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    run.negShape.CPUPerMember.DeepCopy(),
		corev1.ResourceMemory: run.negShape.MemoryPerMember.DeepCopy(),
	}
	return newGangMemberPod(run, run.negPods[index], run.negGroupName, gangPhaseNegative,
		[]string{"sh", "-c", "sleep 300"}, requests, tolerations)
}

func newGangMemberPod(run *gangTestRun, name, groupName, phase string, command []string, requests corev1.ResourceList, tolerations []corev1.Toleration) *corev1.Pod {
	namespace := run.namespace
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				podGroupNameAnnotation: groupName,
			},
			Labels: map[string]string{
				podGroupNameLabel: groupName,
				podGroupIDLabel:   groupName,
				gangPhaseLabel:    phase,
			},
		},
		Spec: corev1.PodSpec{
			ImagePullSecrets: run.imagePullSecretRefs(),
			SchedulerName:    kaiSchedulerName,
			RestartPolicy:    corev1.RestartPolicyNever,
			Tolerations:      tolerations,
			Containers: []corev1.Container{
				{
					Name:    "worker",
					Image:   defaults.ProbeImage,
					Command: command,
					Resources: corev1.ResourceRequirements{
						Requests: requests,
						Limits:   requests.DeepCopy(),
					},
				},
			},
		},
	}
}
