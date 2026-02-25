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
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
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

type webhookRejectionReport struct {
	ResourceName string
	Namespace    string
	StatusCode   int32
	Reason       string
	Message      string
}

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:                  "robust-controller",
		Description:           "Verify Dynamo operator deployment, validating webhook, and DynamoGraphDeployment CRD",
		Phase:                 phaseConformance,
		Func:                  CheckRobustController,
		TestName:              "TestRobustController",
		RequirementID:         "robust_controller",
		EvidenceTitle:         "Robust AI Operator (Dynamo Platform)",
		EvidenceDescription:   "Demonstrates that a complex AI operator (Dynamo) can be installed and functions reliably, including operator pods, webhooks, and custom resource reconciliation.",
		EvidenceFile:          "robust-operator.md",
		SubmissionRequirement: true,
	})
}

// CheckRobustController validates CNCF requirement #9: Robust Controller.
// Verifies the Dynamo operator is deployed, its validating webhook is operational,
// and the DynamoGraphDeployment CRD exists.
func CheckRobustController(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Dynamo operator controller-manager deployment running
	// Name from: tests/chainsaw/ai-conformance/cluster/assert-dynamo.yaml:29
	deploy, deployErr := getDeploymentIfAvailable(ctx, "dynamo-system", "dynamo-platform-dynamo-operator-controller-manager")
	if deploy != nil {
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
	}
	if deployErr != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Dynamo operator controller-manager check failed", deployErr)
	}
	operatorPods, podErr := ctx.Clientset.CoreV1().Pods("dynamo-system").List(ctx.Context, metav1.ListOptions{})
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
		ctx.Context, metav1.ListOptions{})
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
			// Verify webhook service endpoint exists via EndpointSlice
			for _, w := range wh.Webhooks {
				if w.ClientConfig.Service != nil {
					svcName := w.ClientConfig.Service.Name
					svcNs := w.ClientConfig.Service.Namespace
					slices, listErr := ctx.Clientset.DiscoveryV1().EndpointSlices(svcNs).List(
						ctx.Context, metav1.ListOptions{
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

	// 3. DynamoGraphDeployment CRD exists (proves operator manages CRs)
	// API group: nvidia.com (v1alpha1) — from tests/manifests/dynamo-vllm-smoke-test.yaml:28
	// CRD name: dynamographdeployments.nvidia.com — from docs/conformance/cncf/evidence/robust-operator.md:57
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	crdObj, err := dynClient.Resource(crdGVR).Get(ctx.Context,
		"dynamographdeployments.nvidia.com", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"DynamoGraphDeployment CRD not found", err)
	}
	recordRawTextArtifact(ctx, "Dynamo CRDs",
		"kubectl get crds | grep -i dynamo",
		fmt.Sprintf("Required CRD present: %s", crdObj.GetName()))

	// Optional evidence: capture DynamoGraphDeployment and component inventories if available.
	dgdList, dgdListErr := dynClient.Resource(dgdGVR).Namespace("").List(ctx.Context, metav1.ListOptions{})
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

	workloadPods, workloadPodErr := ctx.Clientset.CoreV1().Pods("dynamo-workload").List(ctx.Context, metav1.ListOptions{})
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

	componentList, componentErr := dynClient.Resource(dcdGVR).Namespace("dynamo-workload").List(ctx.Context, metav1.ListOptions{})
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

	// 4. Validating webhook actively rejects invalid resources (behavioral test).
	rejectionReport, err := validateWebhookRejects(ctx)
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

// validateWebhookRejects verifies that the Dynamo validating webhook actively rejects
// invalid DynamoGraphDeployment resources. This proves the webhook is not just present
// but functionally operational.
func validateWebhookRejects(ctx *checks.ValidationContext) (*webhookRejectionReport, error) {
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
	}

	// Generate unique test resource name.
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	name := robustTestPrefix + hex.EncodeToString(b)

	// Build an intentionally invalid DynamoGraphDeployment (empty services).
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

	// Attempt to create the invalid resource — the webhook should reject it.
	_, createErr := dynClient.Resource(dgdGVR).Namespace("dynamo-system").Create(
		ctx.Context, dgd, metav1.CreateOptions{})

	if createErr == nil {
		// Webhook did not reject — clean up the accidentally created resource.
		_ = dynClient.Resource(dgdGVR).Namespace("dynamo-system").Delete(
			ctx.Context, name, metav1.DeleteOptions{})
		return nil, errors.New(errors.ErrCodeInternal,
			"validating webhook did not reject invalid DynamoGraphDeployment")
	}

	report := &webhookRejectionReport{
		ResourceName: name,
		Namespace:    "dynamo-system",
		Reason:       "unknown",
		Message:      createErr.Error(),
	}

	// Webhook rejections produce Forbidden (403) or Invalid (422) API errors.
	// Use k8serrors type predicates instead of brittle string matching.
	// IsForbidden can also match RBAC denials, so we explicitly exclude those
	// by checking the structured status message for RBAC patterns.
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
		}
		return report, nil // PASS — webhook rejected the invalid resource
	}

	// Non-admission error (network, CRD not installed, server error, etc).
	return nil, errors.Wrap(errors.ErrCodeInternal,
		"unexpected error testing webhook rejection", createErr)
}
