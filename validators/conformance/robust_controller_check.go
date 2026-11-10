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
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const (
	robustTestPrefix = "robust-test-"
	// apiGroupTrainer is the Kubeflow Trainer API group.
	apiGroupTrainer = "trainer.kubeflow.org"
	// kubeflowTrainerNamespace is the namespace the kubeflow-trainer component
	// deploys into.
	kubeflowTrainerNamespace = "kubeflow"
	// trainJobDefaultRuntime is the ClusterTrainingRuntime every
	// kubeflow-trainer release ships and the upstream
	// TestRobustCRDControllerOperation reconciles against.
	trainJobDefaultRuntime = "torch-distributed"
	// trainJobPatchManager is the spec.runtimePatches manager AICR writes:
	// Trainer requires a domain-prefixed path and keys the patch list by it.
	trainJobPatchManager = "aicr.nvidia.com/conformance"
	// trainJobNodeReplicatedJob is the replicated Job of the default runtime
	// whose pod template the image pull Secret patch targets.
	trainJobNodeReplicatedJob = "node"
	// keyTemplate is the template key at each level of a runtime patch.
	keyTemplate = "template"
	// robustTrainJobName is the valid TrainJob created in the per-run test
	// namespace for the reconciliation case.
	robustTrainJobName = robustTestPrefix + "trainjob"
	// trainJobConditionComplete and trainJobConditionFailed are the TrainJob
	// terminal condition types the reconciliation wait decides on.
	trainJobConditionComplete = "Complete"
	trainJobConditionFailed   = "Failed"
	// crdConditionEstablished is the CRD condition that says the API server
	// serves the resource.
	crdConditionEstablished = "Established"
)

var dgdGVR = schema.GroupVersionResource{
	Group: apiGroupNVIDIA, Version: versionV1beta1, Resource: "dynamographdeployments",
}

var dcdGVR = schema.GroupVersionResource{
	Group: apiGroupNVIDIA, Version: versionV1alpha1, Resource: "dynamocomponentdeployments",
}

var trainJobGVR = schema.GroupVersionResource{
	Group: apiGroupTrainer, Version: versionV1alpha1, Resource: "trainjobs",
}

var clusterTrainingRuntimeGVR = schema.GroupVersionResource{
	Group: apiGroupTrainer, Version: versionV1alpha1, Resource: "clustertrainingruntimes",
}

// trainJobReconcileTimeout bounds the TrainJob reconciliation wait (a
// package variable so tests can shorten it, mirroring cleanupTimeout).
var trainJobReconcileTimeout = defaults.TrainJobReconcileTimeout

// trainJobReconcileReport is what the reconciliation wait observed on the
// valid TrainJob.
type trainJobReconcileReport struct {
	Namespace  string
	Name       string
	Runtime    string
	Conditions []conditionObservation
	Elapsed    time.Duration
}

type webhookRejectionReport struct {
	ResourceName string
	Namespace    string
	StatusCode   int32
	Reason       string
	Message      string
}

// recipeHasComponent checks if a named component exists and is enabled in the
// validation's componentRefs.
func recipeHasComponent(ctx *validators.Context, name string) bool {
	if ctx.ValidationInput == nil {
		return false
	}
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == name && ref.IsEnabled() {
			return true
		}
	}
	return false
}

// CheckRobustController validates CNCF requirement: Robust Controller.
// Proves that at least one complex AI operator with a CRD is installed and
// functions reliably, including running pods, an operational webhook that
// rejects an invalid custom resource, and reconciliation of a valid one to a
// terminal condition.
//
// Checks are selected based on recipe components:
//   - kubeflow-trainer in recipe → validate Kubeflow Trainer, including a
//     TrainJob reconciled to Complete (the full upstream
//     TestRobustCRDControllerOperation shape)
//   - dynamo-platform in recipe (no kubeflow-trainer) → validate the Dynamo
//     operator; CR reconciliation is recorded as an observation only, since
//     a DynamoGraphDeployment cannot reach a terminal condition without a
//     model and GPU workers
//   - neither → skip
func CheckRobustController(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	if recipeHasComponent(ctx, "kubeflow-trainer") {
		slog.Info("robust-controller: validating Kubeflow Trainer (kubeflow-trainer in recipe)")
		// The Kubeflow path creates a per-run namespace for the TrainJob, so
		// its work is bounded to leave the cleanup reserve intact — see
		// gpuCheckWorkBudget in consts.go. Fail fast before creating anything
		// when the deadline cannot fit the reserve.
		budget, err := gpuCheckWorkBudget(ctx.Ctx, robustControllerWorkBudget)
		if err != nil {
			return err
		}
		workCtx, cancelWork := ctx.Timeout(budget)
		defer cancelWork()
		work := *ctx
		work.Ctx = workCtx
		return checkRobustKubeflowTrainer(&work)
	}

	if recipeHasComponent(ctx, "dynamo-platform") {
		slog.Info("robust-controller: validating Dynamo operator (dynamo-platform in recipe)")
		return checkRobustDynamo(ctx)
	}

	return validators.SkipWithReason(validators.SkipReasonComponentNotInRecipe, "no supported AI operator found in recipe (requires dynamo-platform or kubeflow-trainer)")
}

// checkRobustKubeflowTrainer validates the Kubeflow Trainer operator:
// 1. Controller deployment running
// 2. Validating webhook operational with reachable endpoint
// 3. TrainJob CRD exists and is Established
// 4. Webhook rejects invalid TrainJob
// 5. A valid TrainJob on the default ClusterTrainingRuntime reconciles to
// Complete within defaults.TrainJobReconcileTimeout
func checkRobustKubeflowTrainer(ctx *validators.Context) error {
	// 1. Controller deployment running
	deploy, deployErr := getDeploymentIfAvailable(ctx, kubeflowTrainerNamespace, "kubeflow-trainer-controller-manager")
	if deployErr != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Kubeflow Trainer controller not found", deployErr)
	}
	expected := int32(1)
	if deploy.Spec.Replicas != nil {
		expected = *deploy.Spec.Replicas
	}
	recordRawTextArtifact(ctx, "Kubeflow Trainer Deployment",
		"kubectl get deploy -n kubeflow",
		fmt.Sprintf("Name:      %s/%s\nReplicas:  %d/%d available\nImage:     %s",
			deploy.Namespace, deploy.Name,
			deploy.Status.AvailableReplicas, expected,
			firstContainerImage(deploy.Spec.Template.Spec.Containers)))

	operatorPods, podErr := ctx.Clientset.CoreV1().Pods(kubeflowTrainerNamespace).List(ctx.Ctx, metav1.ListOptions{})
	if podErr != nil {
		recordRawTextArtifact(ctx, "Kubeflow Trainer pods", "kubectl get pods -n kubeflow",
			fmt.Sprintf("failed to list pods: %v", podErr))
	} else {
		var podSummary strings.Builder
		for _, p := range operatorPods.Items {
			fmt.Fprintf(&podSummary, "%-46s ready=%s phase=%s node=%s\n",
				p.Name, podReadyCount(p), p.Status.Phase, valueOrUnknown(p.Spec.NodeName))
		}
		recordRawTextArtifact(ctx, "Kubeflow Trainer pods", "kubectl get pods -n kubeflow", podSummary.String())
	}

	// 2. Validating webhook operational
	webhooks, err := ctx.Clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(
		ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list validating webhook configurations", err)
	}
	var foundWebhook bool
	var webhookName string
	var webhookSummary strings.Builder
	for _, wh := range webhooks.Items {
		if wh.Name == "validator.trainer.kubeflow.org" {
			foundWebhook = true
			webhookName = wh.Name
			fmt.Fprintf(&webhookSummary, "WebhookConfig: %s\n", wh.Name)
			for _, w := range wh.Webhooks {
				if w.ClientConfig.Service != nil {
					svcName := w.ClientConfig.Service.Name
					svcNs := w.ClientConfig.Service.Namespace
					slices, listErr := ctx.Clientset.DiscoveryV1().EndpointSlices(svcNs).List(
						ctx.Ctx, metav1.ListOptions{
							LabelSelector: "kubernetes.io/service-name=" + svcName,
						})
					if listErr != nil {
						return errors.Wrap(errors.ErrCodeNotFound,
							fmt.Sprintf("webhook endpoint %s/%s not found", svcNs, svcName), listErr)
					}
					if len(slices.Items) == 0 {
						return errors.New(errors.ErrCodeNotFound,
							fmt.Sprintf("no EndpointSlice for webhook service %s/%s", svcNs, svcName))
					}
					fmt.Fprintf(&webhookSummary, "  service=%s/%s endpointSlices=%d\n", svcNs, svcName, len(slices.Items))
				}
			}
			break
		}
	}
	if !foundWebhook {
		return errors.New(errors.ErrCodeNotFound, "Kubeflow Trainer validating webhook not found")
	}
	recordRawTextArtifact(ctx, "Validating webhooks",
		"kubectl get validatingwebhookconfigurations | grep trainer",
		strings.TrimSpace(webhookSummary.String()))
	recordRawTextArtifact(ctx, "Validating Webhook",
		"kubectl get validatingwebhookconfigurations",
		fmt.Sprintf("Name:      %s\nEndpoint:  reachable", webhookName))

	// 3. TrainJob CRD exists and is Established
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	established, err := requireCRDEstablished(ctx.Ctx, dynClient, "trainjobs.trainer.kubeflow.org")
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Kubeflow Trainer CRDs",
		"kubectl get crd trainjobs.trainer.kubeflow.org -o jsonpath='{.status.conditions}'",
		fmt.Sprintf("Required CRD present: trainjobs.trainer.kubeflow.org\nEstablished: %s (reason=%s)",
			established.Status, established.Reason))

	// 4. Webhook rejects invalid TrainJob
	rejectionReport, err := validateKubeflowWebhookRejects(ctx)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Webhook Rejection Test",
		"kubectl apply -f <invalid trainjob>",
		fmt.Sprintf("Resource:    %s/%s\nHTTPStatus:  %d\nReason:      %s\nMessage:     %s",
			rejectionReport.Namespace, rejectionReport.ResourceName,
			rejectionReport.StatusCode, rejectionReport.Reason, rejectionReport.Message))

	// 5. A valid TrainJob reconciles to Complete.
	return reconcileKubeflowTrainJob(ctx, dynClient)
}

// requireCRDEstablished reads the named CRD and requires its Established
// condition to be True: a registered CRD the API server does not serve yet
// (or no longer serves) is not an operating controller's API.
func requireCRDEstablished(ctx context.Context, dynClient dynamic.Interface, crdName string) (*conditionObservation, error) {
	crdGVR := schema.GroupVersionResource{
		Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs,
	}
	crdObj, err := dynClient.Resource(crdGVR).Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("CRD %s not found", crdName), err)
		}
		return nil, classifyK8sReadError(err, "CRD "+crdName)
	}
	cond, condErr := getConditionObservation(crdObj, crdConditionEstablished)
	if condErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("CRD %s carries no Established condition", crdName), condErr)
	}
	if cond.Status != conditionStatusTrue {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("CRD %s is registered but not Established: status=%s reason=%s message=%s",
				crdName, cond.Status, cond.Reason, cond.Message))
	}
	return cond, nil
}

// reconcileKubeflowTrainJob creates a valid TrainJob on the default
// ClusterTrainingRuntime in a per-run namespace and waits for the Trainer
// controller to reconcile it to Complete=True, failing on Failed=True or on
// the defaults.TrainJobReconcileTimeout bound (the upstream
// TestRobustCRDControllerOperation shape). The TrainJob requests no
// accelerator: the requirement is that the controller reconciles a custom
// resource, and a GPU allocation would compete with the other conformance
// checks for the cell's GPUs. Its one pod runs defaults.ProbeImage with a
// trivial command instead of the runtime's PyTorch image, so the reconcile
// bound is spent on the controller (TrainJob → JobSet → Job → Pod) and not
// on a multi-gigabyte image pull.
//
// The TrainJob sets no scheduling of its own: Trainer v2.2.0 offers
// spec.runtimePatches for per-TrainJob tolerations, but AICR's
// torch-distributed manifest already carries the accelerated node selector
// and tolerations the bundler bakes in from --accelerated-node-selector and
// --accelerated-node-toleration, and that path holds across Trainer v2.1 and
// v2.2. That is the chosen design: the probe exercises the runtime as a user's
// bare TrainJob would, so a bundle generated without the accelerated
// toleration surfaces here as a Timeout verdict naming the Pending pod and
// its Unschedulable reason. The one runtimePatches entry it carries, and only
// when --image-pull-secret named Secrets, sets imagePullSecrets on the node
// pod template, since the Secrets copied into the per-run namespace are
// reachable from the pod only through that field.
//
// The return is NAMED so the deferred namespace cleanup can fail an
// otherwise-passing check when it terminally fails; a primary error is always
// preserved.
func reconcileKubeflowTrainJob(ctx *validators.Context, dynClient dynamic.Interface) (err error) {
	// The runtime is the reconciliation's prerequisite: without it the
	// webhook rejects the TrainJob, which would read as a webhook result,
	// not the missing-runtime failure it is.
	if _, rtErr := dynClient.Resource(clusterTrainingRuntimeGVR).Get(ctx.Ctx, trainJobDefaultRuntime, metav1.GetOptions{}); rtErr != nil {
		if k8serrors.IsNotFound(rtErr) {
			return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf(
				"ClusterTrainingRuntime %s not found — the kubeflow-trainer chart ships it; reconciliation cannot be exercised", trainJobDefaultRuntime), rtErr)
		}
		return classifyK8sReadError(rtErr, "ClusterTrainingRuntime "+trainJobDefaultRuntime)
	}

	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}
	// Register cleanup BEFORE the first Create (see runDRASecureAccessTest):
	// deleting the per-run namespace removes the TrainJob and the JobSet,
	// Job and pod the controller derived from it in one bounded operation.
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		status, cleanupErr := cleanupGPUTestNamespace(ctx.Clientset, run)
		recordRawTextArtifact(ctx, "Cleanup test namespace",
			cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = createGPUTestNamespace(ctx.Ctx, ctx.Clientset, run); err != nil {
		return err
	}

	tj := buildValidTrainJob(run.namespace, run.imagePullSecretRefs())
	recordObjectYAMLArtifact(ctx, "TrainJob reconciliation: apply test manifest",
		fmt.Sprintf("kubectl apply -n %s -f <valid trainjob>", run.namespace), tj.Object)
	if _, err = dynClient.Resource(trainJobGVR).Namespace(run.namespace).Create(ctx.Ctx, tj, metav1.CreateOptions{}); err != nil {
		return classifyTrainJobCreateError(err, run.namespace)
	}

	report, waitErr := waitForTrainJobReconciled(ctx.Ctx, dynClient, run.namespace)
	report.Runtime = trainJobDefaultRuntime
	// Evidence BEFORE the verdict: the observed conditions and derived pods
	// are the diagnostic when the wait fails.
	recordRawTextArtifact(ctx, "TrainJob reconciliation",
		fmt.Sprintf("kubectl get trainjob %s -n %s -o jsonpath='{.status.conditions}'", robustTrainJobName, run.namespace),
		report.summary(waitErr))
	podsObserved := recordTrainJobPods(ctx, run.namespace)
	if waitErr != nil {
		// The verdict names what the pods were doing when the wait ended
		// (Pending, image pull, running), not an inferred cause: a TrainJob
		// that the controller did reconcile (Created=True, pod exists) and a
		// pod that stalled read the same at the TrainJob level.
		return wrapWithCode(waitErr, errors.ErrCodeInternal, "TrainJob pods at the verdict: "+podsObserved)
	}
	return nil
}

// classifyTrainJobCreateError turns a failed TrainJob Create into the
// check's verdict. Only a definitive apiserver rejection (admission or
// validation) means the valid TrainJob was rejected — and an RBAC denial is
// reported as such rather than as a webhook rejection; a timeout, transport
// failure or 5xx (the single-replica Trainer webhook restarting mid-run) is
// ambiguous — the object may have been persisted, which the per-run
// namespace cleanup covers — and keeps its classified code.
func classifyTrainJobCreateError(createErr error, namespace string) error {
	what := fmt.Sprintf("TrainJob %s/%s on runtime %s", namespace, robustTrainJobName, trainJobDefaultRuntime)
	if createDefinitelyRejected(createErr) {
		if strings.Contains(createErr.Error(), "cannot create resource") {
			return errors.Wrap(errors.ErrCodeUnauthorized,
				fmt.Sprintf("RBAC denied creating the valid %s", what), createErr)
		}
		return errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("valid %s was rejected by admission or validation", what), createErr)
	}
	// Keep the classifier's code (Timeout, Unavailable, Internal) but build
	// the message directly: the classifier prefixes its subject with "failed
	// to read", which does not describe a create.
	code := errors.ErrCodeInternal
	if se, ok := stderrors.AsType[*errors.StructuredError](classifyK8sReadError(createErr, what)); ok {
		code = se.Code
	}
	return errors.Wrap(code,
		fmt.Sprintf("create of the valid %s did not complete (ambiguous: the object may have been persisted; the per-run namespace cleanup removes it)", what), createErr)
}

// summary renders the reconciliation observation for the evidence page.
func (r *trainJobReconcileReport) summary(waitErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TrainJob:   %s/%s\nRuntime:    ClusterTrainingRuntime/%s\nAccelerator: none requested\nElapsed:    %s\n",
		r.Namespace, r.Name, r.Runtime, r.Elapsed.Round(time.Second))
	if waitErr == nil {
		fmt.Fprintf(&b, "Verdict:    reconciled to %s=True within %s\n", trainJobConditionComplete, trainJobReconcileTimeout)
	} else {
		fmt.Fprintf(&b, "Verdict:    FAIL — %v\n", waitErr)
	}
	if len(r.Conditions) == 0 {
		b.WriteString("Conditions: none observed\n")
		return b.String()
	}
	b.WriteString("Conditions:\n")
	for _, c := range r.Conditions {
		fmt.Fprintf(&b, "  %-9s status=%-7s reason=%s message=%s\n", c.Type, c.Status, c.Reason, c.Message)
	}
	return b.String()
}

// recordTrainJobPods records the pods the controller derived from the
// TrainJob (through JobSet and Job) so the evidence shows how far the
// reconciliation reached, on which node, and what a non-terminal pod was
// waiting on. It returns a one-line summary of the same for the verdict.
func recordTrainJobPods(ctx *validators.Context, namespace string) string {
	pods, err := ctx.Clientset.CoreV1().Pods(namespace).List(ctx.Ctx, metav1.ListOptions{})
	equivalent := fmt.Sprintf("kubectl get pods -n %s -o wide", namespace)
	if err != nil {
		summary := fmt.Sprintf("failed to list pods: %v", err)
		recordRawTextArtifact(ctx, "TrainJob pods", equivalent, summary)
		return summary
	}
	if len(pods.Items) == 0 {
		const summary = "no pods derived from the TrainJob were observed"
		recordRawTextArtifact(ctx, "TrainJob pods", equivalent, summary)
		return summary
	}
	var b strings.Builder
	lines := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		state := string(p.Status.Phase)
		if reason := podStuckReason(p); reason != "" {
			state += " (" + reason + ")"
		} else if p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodRunning {
			if waiting := podWaitingStatus(p); waiting != valueNone {
				state += " (waiting: " + waiting + ")"
			}
		}
		fmt.Fprintf(&b, "%-52s phase=%-9s node=%s image=%s state=%s\n",
			p.Name, p.Status.Phase, valueOrUnknown(p.Spec.NodeName), firstContainerImage(p.Spec.Containers), state)
		lines = append(lines, fmt.Sprintf("%s %s on node %s", p.Name, state, valueOrUnknown(p.Spec.NodeName)))
	}
	recordRawTextArtifact(ctx, "TrainJob pods", equivalent, b.String())
	return strings.Join(lines, "; ")
}

// buildValidTrainJob returns the TrainJob (robustTrainJobName) the webhook admits: it references
// the default ClusterTrainingRuntime, runs one node and requests no
// accelerator. The trainer image and command override the runtime's PyTorch
// container with the small probe image and a command that exits 0, so the
// reconciliation reaches Complete without a GPU or a large image pull. When
// pullSecrets is non-empty, a spec.runtimePatches entry sets them as the
// imagePullSecrets of the node pod template (trainJobPodSpecPatch).
func buildValidTrainJob(namespace string, pullSecrets []corev1.LocalObjectReference) *unstructured.Unstructured {
	spec := map[string]any{
		"runtimeRef": map[string]any{
			keyName:    trainJobDefaultRuntime,
			"apiGroup": apiGroupTrainer,
			keyKind:    "ClusterTrainingRuntime",
		},
		"trainer": map[string]any{
			"numNodes": int64(1),
			"image":    defaults.ProbeImage,
			"command":  []any{"sh", "-c", "echo 'robust-controller: TrainJob reconciled'"},
		},
	}
	if len(pullSecrets) > 0 {
		spec["runtimePatches"] = []any{trainJobPodSpecPatch(map[string]any{
			"imagePullSecrets": localObjectReferences(pullSecrets),
		})}
	}
	return &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: apiGroupTrainer + "/" + versionV1alpha1,
			keyKind:       "TrainJob",
			keyMetadata: map[string]any{
				keyName:      robustTrainJobName,
				keyNamespace: namespace,
			},
			keySpec: spec,
		},
	}
}

// trainJobPodSpecPatch wraps podSpec, the pod-spec fields to patch, in the
// Trainer v2.2 spec.runtimePatches shape addressing the default runtime's
// node Job: runtimePatches[].trainingRuntimeSpec.template.spec.replicatedJobs
// [name=node].template.spec.template.spec. The entry is keyed by
// trainJobPatchManager, as Trainer requires.
func trainJobPodSpecPatch(podSpec map[string]any) map[string]any {
	return map[string]any{
		"manager": trainJobPatchManager,
		"trainingRuntimeSpec": map[string]any{
			keyTemplate: map[string]any{
				keySpec: map[string]any{
					"replicatedJobs": []any{map[string]any{
						keyName: trainJobNodeReplicatedJob,
						keyTemplate: map[string]any{
							keySpec: map[string]any{
								keyTemplate: map[string]any{
									keySpec: podSpec,
								},
							},
						},
					}},
				},
			},
		},
	}
}

// localObjectReferences renders refs as the unstructured form of a
// []corev1.LocalObjectReference.
func localObjectReferences(refs []corev1.LocalObjectReference) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]any{keyName: ref.Name})
	}
	return out
}

// waitForTrainJobReconciled polls the TrainJob (robustTrainJobName) until Complete=True (done),
// Failed=True (terminal failure) or trainJobReconcileTimeout. Every
// observed condition set is kept for the evidence. A read error that is not
// the wait's own deadline aborts with the classified code; the fake-client
// and apiserver NotFound right after a create is retried within the bound.
func waitForTrainJobReconciled(ctx context.Context, dynClient dynamic.Interface, namespace string) (*trainJobReconcileReport, error) {
	start := time.Now()
	name := robustTrainJobName
	report := &trainJobReconcileReport{Namespace: namespace, Name: name}
	waitCtx, cancel := context.WithTimeout(ctx, trainJobReconcileTimeout)
	defer cancel()

	// The poll interval scales down with a shortened bound (tests) so the
	// condition is evaluated more than once before the bound.
	interval := min(defaults.TrainJobReconcilePollInterval, trainJobReconcileTimeout/10)
	err := wait.PollUntilContextCancel(waitCtx, interval, true,
		func(c context.Context) (bool, error) {
			tj, getErr := dynClient.Resource(trainJobGVR).Namespace(namespace).Get(c, name, metav1.GetOptions{})
			if getErr != nil {
				if k8serrors.IsNotFound(getErr) || readFailedBecauseContextEnded(c, getErr) || isK8sTimeoutErr(getErr) {
					return false, nil
				}
				return false, classifyK8sReadError(getErr, fmt.Sprintf("TrainJob %s/%s", namespace, name))
			}
			report.Conditions = trainJobConditions(tj)
			for _, cond := range report.Conditions {
				if cond.Type == trainJobConditionFailed && cond.Status == conditionStatusTrue {
					return false, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
						"TrainJob %s/%s reached %s=True: reason=%s message=%s",
						namespace, name, trainJobConditionFailed, cond.Reason, cond.Message))
				}
			}
			for _, cond := range report.Conditions {
				if cond.Type == trainJobConditionComplete && cond.Status == conditionStatusTrue {
					return true, nil
				}
			}
			return false, nil
		})
	report.Elapsed = time.Since(start)
	if err == nil {
		return report, nil
	}
	if ctx.Err() != nil {
		// WrapCtxErr maps an operator abort (Ctrl-C, SIGTERM) to Canceled and
		// an expired work budget to Timeout.
		return report, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, fmt.Sprintf(
			"waiting for TrainJob %s/%s stopped; conditions observed: %s", namespace, name, report.conditionsLine()))
	}
	if waitCtx.Err() != nil {
		return report, errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
			"TrainJob %s/%s did not reach %s=True within %s; conditions observed: %s (see the TrainJob reconciliation and TrainJob pods artifacts)",
			namespace, name, trainJobConditionComplete, trainJobReconcileTimeout, report.conditionsLine()), err)
	}
	return report, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
		fmt.Sprintf("failed waiting for TrainJob %s/%s", namespace, name))
}

// conditionsLine renders the observed conditions as "Type=Status" pairs for
// a one-line verdict, or "none" when the TrainJob carried no status.
func (r *trainJobReconcileReport) conditionsLine() string {
	if len(r.Conditions) == 0 {
		return valueNone
	}
	pairs := make([]string, 0, len(r.Conditions))
	for _, c := range r.Conditions {
		pairs = append(pairs, c.Type+"="+c.Status)
	}
	return strings.Join(pairs, ", ")
}

// trainJobConditions flattens status.conditions into observations; a
// TrainJob with no status yet yields none.
func trainJobConditions(tj *unstructured.Unstructured) []conditionObservation {
	raw, found, err := unstructured.NestedSlice(tj.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	out := make([]conditionObservation, 0, len(raw))
	for _, c := range raw {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		kind, _, _ := unstructured.NestedString(cond, "type")
		status, _, _ := unstructured.NestedString(cond, "status")
		reason, _, _ := unstructured.NestedString(cond, "reason")
		message, _, _ := unstructured.NestedString(cond, "message")
		out = append(out, conditionObservation{
			Type: valueOrUnknown(kind), Status: valueOrUnknown(status),
			Reason: valueOrUnknown(reason), Message: valueOrUnknown(message),
		})
	}
	return out
}

// validateKubeflowWebhookRejects verifies the Kubeflow Trainer webhook actively rejects
// invalid TrainJob resources.
func validateKubeflowWebhookRejects(ctx *validators.Context) (*webhookRejectionReport, error) {
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
	}

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	name := robustTestPrefix + hex.EncodeToString(b)

	// Build an intentionally invalid TrainJob with a runtimeRef pointing to a
	// non-existent runtime. The Kubeflow Trainer validating webhook rejects this
	// because the referenced ClusterTrainingRuntime does not exist. This proves
	// the webhook is actively validating, not just schema validation.
	tj := &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: apiGroupTrainer + "/" + versionV1alpha1,
			keyKind:       "TrainJob",
			keyMetadata: map[string]any{
				keyName:      name,
				keyNamespace: "default",
			},
			keySpec: map[string]any{
				"runtimeRef": map[string]any{
					keyName:    robustTestPrefix + "nonexistent-runtime",
					"apiGroup": apiGroupTrainer,
					keyKind:    "ClusterTrainingRuntime",
				},
			},
		},
	}

	_, createErr := dynClient.Resource(trainJobGVR).Namespace("default").Create(
		ctx.Ctx, tj, metav1.CreateOptions{})

	if createErr == nil {
		_ = dynClient.Resource(trainJobGVR).Namespace("default").Delete(
			ctx.Ctx, name, metav1.DeleteOptions{})
		return nil, errors.New(errors.ErrCodeInternal,
			"validating webhook did not reject invalid TrainJob")
	}

	report := &webhookRejectionReport{
		ResourceName: name,
		Namespace:    "default",
		Reason:       statusUnknown,
		Message:      createErr.Error(),
	}

	if k8serrors.IsForbidden(createErr) || k8serrors.IsInvalid(createErr) {
		if statusErr, ok := stderrors.AsType[*k8serrors.StatusError](createErr); ok {
			status := statusErr.Status()
			report.StatusCode = status.Code
			report.Reason = string(status.Reason)
			report.Message = status.Message
			msg := status.Message
			if strings.Contains(msg, "cannot create resource") {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					"RBAC denied the request, not an admission webhook rejection", createErr)
			}
			// Verify the rejection came from the admission webhook.
			// Kubeflow Trainer webhook rejections contain "admission webhook" in the message.
			if !strings.Contains(msg, "admission webhook") {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					"rejection does not appear to be from admission webhook (missing 'admission webhook' in message)", createErr)
			}
		}

		return report, nil
	}

	return nil, errors.Wrap(errors.ErrCodeInternal,
		"unexpected error testing webhook rejection", createErr)
}

// checkRobustDynamo validates the Dynamo operator (original implementation).
func checkRobustDynamo(ctx *validators.Context) error {
	// 1. Dynamo operator controller-manager deployment running
	deploy, deployErr := getDeploymentIfAvailable(ctx, "dynamo-system", "dynamo-platform-dynamo-operator-controller-manager")
	if deployErr != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Dynamo operator controller not found", deployErr)
	}
	expected := int32(1)
	if deploy.Spec.Replicas != nil {
		expected = *deploy.Spec.Replicas
	}
	recordRawTextArtifact(ctx, "Dynamo Operator Deployment",
		"kubectl get deploy -n dynamo-system",
		fmt.Sprintf("Name:      %s/%s\nReplicas:  %d/%d available\nImage:     %s",
			deploy.Namespace, deploy.Name,
			deploy.Status.AvailableReplicas, expected,
			firstContainerImage(deploy.Spec.Template.Spec.Containers)))

	operatorPods, podErr := ctx.Clientset.CoreV1().Pods("dynamo-system").List(ctx.Ctx, metav1.ListOptions{})
	if podErr != nil {
		recordRawTextArtifact(ctx, "Dynamo operator pods", "kubectl get pods -n dynamo-system",
			fmt.Sprintf("failed to list pods: %v", podErr))
	} else {
		var podSummary strings.Builder
		for _, p := range operatorPods.Items {
			fmt.Fprintf(&podSummary, "%-46s ready=%s phase=%s node=%s\n",
				p.Name, podReadyCount(p), p.Status.Phase, valueOrUnknown(p.Spec.NodeName))
		}
		recordRawTextArtifact(ctx, "Dynamo operator pods", "kubectl get pods -n dynamo-system", podSummary.String())
	}

	// 2. Validating webhook operational
	webhooks, err := ctx.Clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(
		ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to list validating webhook configurations", err)
	}
	var foundDynamoWebhook bool
	var webhookName string
	var webhookSummary strings.Builder
	for _, wh := range webhooks.Items {
		if strings.Contains(wh.Name, "dynamo") {
			foundDynamoWebhook = true
			webhookName = wh.Name
			fmt.Fprintf(&webhookSummary, "WebhookConfig: %s\n", wh.Name)
			for _, w := range wh.Webhooks {
				if w.ClientConfig.Service != nil {
					svcName := w.ClientConfig.Service.Name
					svcNs := w.ClientConfig.Service.Namespace
					slices, listErr := ctx.Clientset.DiscoveryV1().EndpointSlices(svcNs).List(
						ctx.Ctx, metav1.ListOptions{
							LabelSelector: "kubernetes.io/service-name=" + svcName,
						})
					if listErr != nil {
						return errors.Wrap(errors.ErrCodeNotFound,
							fmt.Sprintf("webhook endpoint %s/%s not found", svcNs, svcName), listErr)
					}
					if len(slices.Items) == 0 {
						return errors.New(errors.ErrCodeNotFound,
							fmt.Sprintf("no EndpointSlice for webhook service %s/%s", svcNs, svcName))
					}
					fmt.Fprintf(&webhookSummary, "  service=%s/%s endpointSlices=%d\n", svcNs, svcName, len(slices.Items))
				}
			}
			break
		}
	}
	if !foundDynamoWebhook {
		return errors.New(errors.ErrCodeNotFound,
			"Dynamo validating webhook configuration not found")
	}
	recordRawTextArtifact(ctx, "Validating webhooks",
		"kubectl get validatingwebhookconfigurations | grep dynamo",
		strings.TrimSpace(webhookSummary.String()))
	recordRawTextArtifact(ctx, "Validating Webhook",
		"kubectl get validatingwebhookconfigurations",
		fmt.Sprintf("Name:      %s\nEndpoint:  reachable", webhookName))

	// 3. DynamoGraphDeployment CRD exists and is Established
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	established, err := requireCRDEstablished(ctx.Ctx, dynClient, "dynamographdeployments.nvidia.com")
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Dynamo CRDs",
		"kubectl get crd dynamographdeployments.nvidia.com -o jsonpath='{.status.conditions}'",
		fmt.Sprintf("Required CRD present: dynamographdeployments.nvidia.com\nEstablished: %s (reason=%s)",
			established.Status, established.Reason))

	// Optional evidence: capture inventories. The existing
	// DynamoGraphDeployments and their conditions are the only reconciliation
	// evidence this path records: a minimal DynamoGraphDeployment cannot be
	// reconciled to a terminal condition without a model and GPU workers, so
	// no CR is created here and robust_controller's reconciliation case is
	// evidenced by the Kubeflow Trainer path on recipes that ship it.
	dgdList, dgdListErr := dynClient.Resource(dgdGVR).Namespace("").List(ctx.Ctx, metav1.ListOptions{})
	if dgdListErr != nil {
		recordRawTextArtifact(ctx, "DynamoGraphDeployments", "kubectl get dynamographdeployments -A",
			fmt.Sprintf("unable to list DynamoGraphDeployments: %v", dgdListErr))
	} else {
		var dgdSummary strings.Builder
		fmt.Fprintf(&dgdSummary, "Count: %d\n", len(dgdList.Items))
		for i := range dgdList.Items {
			item := &dgdList.Items[i]
			fmt.Fprintf(&dgdSummary, "- %s/%s\n", item.GetNamespace(), item.GetName())
			for _, c := range trainJobConditions(item) {
				fmt.Fprintf(&dgdSummary, "    %-12s status=%-7s reason=%s\n", c.Type, c.Status, c.Reason)
			}
		}
		recordRawTextArtifact(ctx, "DynamoGraphDeployments", "kubectl get dynamographdeployments -A -o yaml", dgdSummary.String())
	}
	recordRawTextArtifact(ctx, "CR reconciliation (observation)", "",
		"Not exercised on the Dynamo path: a minimal DynamoGraphDeployment has no terminal condition to reach without a served model and GPU workers. "+
			"The DynamoGraphDeployments inventory above records the conditions of the CRs the operator is reconciling; "+
			"the robust_controller reconciliation case (a valid CR reconciled to a terminal condition) is evidenced by the Kubeflow Trainer path on recipes that ship kubeflow-trainer.")

	workloadPods, workloadPodErr := ctx.Clientset.CoreV1().Pods("dynamo-workload").List(ctx.Ctx, metav1.ListOptions{})
	if workloadPodErr != nil {
		recordRawTextArtifact(ctx, "Dynamo workload pods", "kubectl get pods -n dynamo-workload -o wide",
			fmt.Sprintf("unable to list workload pods: %v", workloadPodErr))
	} else {
		var workloadSummary strings.Builder
		for _, p := range workloadPods.Items {
			fmt.Fprintf(&workloadSummary, "%-46s ready=%s phase=%s node=%s\n",
				p.Name, podReadyCount(p), p.Status.Phase, valueOrUnknown(p.Spec.NodeName))
		}
		recordRawTextArtifact(ctx, "Dynamo workload pods", "kubectl get pods -n dynamo-workload -o wide", workloadSummary.String())
	}

	componentList, componentErr := dynClient.Resource(dcdGVR).Namespace("dynamo-workload").List(ctx.Ctx, metav1.ListOptions{})
	if componentErr != nil {
		recordRawTextArtifact(ctx, "DynamoComponentDeployments",
			"kubectl get dynamocomponentdeployments -n dynamo-workload",
			fmt.Sprintf("unable to list DynamoComponentDeployments: %v", componentErr))
	} else {
		var componentSummary strings.Builder
		fmt.Fprintf(&componentSummary, "Count: %d\n", len(componentList.Items))
		for _, item := range componentList.Items {
			fmt.Fprintf(&componentSummary, "- %s/%s\n", item.GetNamespace(), item.GetName())
		}
		recordRawTextArtifact(ctx, "DynamoComponentDeployments",
			"kubectl get dynamocomponentdeployments -n dynamo-workload", componentSummary.String())
	}

	// 4. Validating webhook actively rejects invalid resources.
	rejectionReport, err := validateDynamoWebhookRejects(ctx)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Webhook Rejection Test",
		"kubectl apply -f <invalid dynamographdeployment>",
		fmt.Sprintf("Resource:    %s/%s\nHTTPStatus:  %d\nReason:      %s\nMessage:     %s",
			rejectionReport.Namespace, rejectionReport.ResourceName,
			rejectionReport.StatusCode, rejectionReport.Reason, rejectionReport.Message))
	return nil
}

// validateDynamoWebhookRejects verifies that the Dynamo validating webhook actively rejects
// invalid DynamoGraphDeployment resources.
func validateDynamoWebhookRejects(ctx *validators.Context) (*webhookRejectionReport, error) {
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
	}

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	name := robustTestPrefix + hex.EncodeToString(b)

	dgd := &unstructured.Unstructured{
		Object: map[string]any{
			keyAPIVersion: "nvidia.com/v1beta1",
			keyKind:       "DynamoGraphDeployment",
			keyMetadata: map[string]any{
				keyName:      name,
				keyNamespace: namespaceDynamoSystem,
			},
			keySpec: map[string]any{
				"components": []any{},
			},
		},
	}

	_, createErr := dynClient.Resource(dgdGVR).Namespace(namespaceDynamoSystem).Create(
		ctx.Ctx, dgd, metav1.CreateOptions{})

	if createErr == nil {
		_ = dynClient.Resource(dgdGVR).Namespace(namespaceDynamoSystem).Delete(
			ctx.Ctx, name, metav1.DeleteOptions{})
		return nil, errors.New(errors.ErrCodeInternal,
			"validating webhook did not reject invalid DynamoGraphDeployment")
	}

	report := &webhookRejectionReport{
		ResourceName: name,
		Namespace:    "dynamo-system",
		Reason:       statusUnknown,
		Message:      createErr.Error(),
	}

	if k8serrors.IsForbidden(createErr) || k8serrors.IsInvalid(createErr) {
		if statusErr, ok := stderrors.AsType[*k8serrors.StatusError](createErr); ok {
			status := statusErr.Status()
			report.StatusCode = status.Code
			report.Reason = string(status.Reason)
			report.Message = status.Message
			if strings.Contains(status.Message, "cannot create resource") {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					"RBAC denied the request, not an admission webhook rejection", createErr)
			}
		}
		return report, nil
	}

	return nil, errors.Wrap(errors.ErrCodeInternal,
		"unexpected error testing webhook rejection", createErr)
}
