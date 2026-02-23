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
	if err := verifyDeploymentAvailable(ctx, "dynamo-system", "dynamo-platform-dynamo-operator-controller-manager"); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Dynamo operator controller-manager check failed", err)
	}

	// 2. Validating webhook operational
	webhooks, err := ctx.Clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(
		ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to list validating webhook configurations", err)
	}
	var foundDynamoWebhook bool
	for _, wh := range webhooks.Items {
		if strings.Contains(wh.Name, "dynamo") {
			foundDynamoWebhook = true
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
				}
			}
			break
		}
	}
	if !foundDynamoWebhook {
		return errors.New(errors.ErrCodeNotFound,
			"Dynamo validating webhook configuration not found")
	}

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
	_, err = dynClient.Resource(crdGVR).Get(ctx.Context,
		"dynamographdeployments.nvidia.com", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"DynamoGraphDeployment CRD not found", err)
	}

	// 4. Validating webhook actively rejects invalid resources (behavioral test).
	return validateWebhookRejects(ctx)
}

// validateWebhookRejects verifies that the Dynamo validating webhook actively rejects
// invalid DynamoGraphDeployment resources. This proves the webhook is not just present
// but functionally operational.
func validateWebhookRejects(ctx *checks.ValidationContext) error {
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	// Generate unique test resource name.
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
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
		return errors.New(errors.ErrCodeInternal,
			"validating webhook did not reject invalid DynamoGraphDeployment")
	}

	// Webhook rejections produce Forbidden (403) or Invalid (422) API errors.
	// Use k8serrors type predicates instead of brittle string matching.
	// IsForbidden can also match RBAC denials, so we explicitly exclude those
	// by checking the structured status message for RBAC patterns.
	if k8serrors.IsForbidden(createErr) || k8serrors.IsInvalid(createErr) {
		var statusErr *k8serrors.StatusError
		if stderrors.As(createErr, &statusErr) {
			msg := statusErr.Status().Message
			if strings.Contains(msg, "cannot create resource") {
				return errors.Wrap(errors.ErrCodeInternal,
					"RBAC denied the request, not an admission webhook rejection", createErr)
			}
		}
		return nil // PASS — webhook rejected the invalid resource
	}

	// Non-admission error (network, CRD not installed, server error, etc).
	return errors.Wrap(errors.ErrCodeInternal,
		"unexpected error testing webhook rejection", createErr)
}
