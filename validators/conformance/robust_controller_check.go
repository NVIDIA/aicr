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
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const robustTestPrefix = "robust-test-"

var dgdGVR = schema.GroupVersionResource{
	Group: "nvidia.com", Version: "v1alpha1", Resource: "dynamographdeployments",
}

var dcdGVR = schema.GroupVersionResource{
	Group: "nvidia.com", Version: "v1alpha1", Resource: "dynamocomponentdeployments",
}

var trainJobGVR = schema.GroupVersionResource{
	Group: "trainer.kubeflow.org", Version: "v1alpha1", Resource: "trainjobs",
}

type webhookRejectionReport struct {
	ResourceName string
	Namespace    string
	StatusCode   int32
	Reason       string
	Message      string
}

// recipeHasComponent checks if a named component exists in the recipe's componentRefs.
func recipeHasComponent(ctx *validators.Context, name string) bool {
	if ctx.Recipe == nil {
		return false
	}
	for _, ref := range ctx.Recipe.ComponentRefs {
		if ref.Name == name {
			return true
		}
	}
	return false
}

// CheckRobustController validates CNCF requirement: Robust Controller.
// Proves that at least one complex AI operator with a CRD is installed and
// functions reliably, including running pods, operational webhooks, and
// custom resource reconciliation.
//
// Checks are selected based on recipe components:
//   - dynamo-platform in recipe → validate Dynamo operator
//   - kubeflow-trainer in recipe → validate Kubeflow Trainer operator
//   - neither → skip
func CheckRobustController(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	if recipeHasComponent(ctx, "dynamo-platform") {
		slog.Info("robust-controller: validating Dynamo operator (dynamo-platform in recipe)")
		return checkRobustDynamo(ctx)
	}

	if recipeHasComponent(ctx, "kubeflow-trainer") {
		slog.Info("robust-controller: validating Kubeflow Trainer (kubeflow-trainer in recipe)")
		return checkRobustKubeflowTrainer(ctx)
	}

	return validators.Skip("no supported AI operator found in recipe (requires dynamo-platform or kubeflow-trainer)")
}

// checkRobustKubeflowTrainer validates the Kubeflow Trainer operator:
// 1. Controller deployment running
// 2. Validating webhook operational with reachable endpoint
// 3. TrainJob CRD exists
// 4. Webhook rejects invalid TrainJob
func checkRobustKubeflowTrainer(ctx *validators.Context) error {
	// 1. Controller deployment running
	deploy, deployErr := getDeploymentIfAvailable(ctx, "kubeflow", "kubeflow-trainer-controller-manager")
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

	operatorPods, podErr := ctx.Clientset.CoreV1().Pods("kubeflow").List(ctx.Ctx, metav1.ListOptions{})
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

	// 3. TrainJob CRD exists
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	crdObj, err := dynClient.Resource(crdGVR).Get(ctx.Ctx, "trainjobs.trainer.kubeflow.org", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "TrainJob CRD not found", err)
	}
	recordRawTextArtifact(ctx, "Kubeflow Trainer CRDs",
		"kubectl get crds | grep trainer",
		fmt.Sprintf("Required CRD present: %s", crdObj.GetName()))

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
	return nil
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
		Object: map[string]interface{}{
			"apiVersion": "trainer.kubeflow.org/v1alpha1",
			"kind":       "TrainJob",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "default",
			},
			"spec": map[string]interface{}{
				"runtimeRef": map[string]interface{}{
					"name":     robustTestPrefix + "nonexistent-runtime",
					"apiGroup": "trainer.kubeflow.org",
					"kind":     "ClusterTrainingRuntime",
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
		Reason:       "unknown",
		Message:      createErr.Error(),
	}

	if k8serrors.IsForbidden(createErr) || k8serrors.IsInvalid(createErr) {
		var statusErr *k8serrors.StatusError
		if stderrors.As(createErr, &statusErr) {
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

	// 3. DynamoGraphDeployment CRD exists
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	crdObj, err := dynClient.Resource(crdGVR).Get(ctx.Ctx,
		"dynamographdeployments.nvidia.com", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"DynamoGraphDeployment CRD not found", err)
	}
	recordRawTextArtifact(ctx, "Dynamo CRDs",
		"kubectl get crds | grep -i dynamo",
		fmt.Sprintf("Required CRD present: %s", crdObj.GetName()))

	// Optional evidence: capture inventories.
	dgdList, dgdListErr := dynClient.Resource(dgdGVR).Namespace("").List(ctx.Ctx, metav1.ListOptions{})
	if dgdListErr != nil {
		recordRawTextArtifact(ctx, "DynamoGraphDeployments", "kubectl get dynamographdeployments -A",
			fmt.Sprintf("unable to list DynamoGraphDeployments: %v", dgdListErr))
	} else {
		var dgdSummary strings.Builder
		fmt.Fprintf(&dgdSummary, "Count: %d\n", len(dgdList.Items))
		for _, item := range dgdList.Items {
			fmt.Fprintf(&dgdSummary, "- %s/%s\n", item.GetNamespace(), item.GetName())
		}
		recordRawTextArtifact(ctx, "DynamoGraphDeployments", "kubectl get dynamographdeployments -A", dgdSummary.String())
	}

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
		Object: map[string]interface{}{
			"apiVersion": "nvidia.com/v1alpha1",
			"kind":       "DynamoGraphDeployment",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "dynamo-system",
			},
			"spec": map[string]interface{}{
				"services": map[string]interface{}{},
			},
		},
	}

	_, createErr := dynClient.Resource(dgdGVR).Namespace("dynamo-system").Create(
		ctx.Ctx, dgd, metav1.CreateOptions{})

	if createErr == nil {
		_ = dynClient.Resource(dgdGVR).Namespace("dynamo-system").Delete(
			ctx.Ctx, name, metav1.DeleteOptions{})
		return nil, errors.New(errors.ErrCodeInternal,
			"validating webhook did not reject invalid DynamoGraphDeployment")
	}

	report := &webhookRejectionReport{
		ResourceName: name,
		Namespace:    "dynamo-system",
		Reason:       "unknown",
		Message:      createErr.Error(),
	}

	if k8serrors.IsForbidden(createErr) || k8serrors.IsInvalid(createErr) {
		var statusErr *k8serrors.StatusError
		if stderrors.As(createErr, &statusErr) {
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
