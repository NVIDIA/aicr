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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
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
		Name:        "robust-controller",
		Description: "Verify Dynamo operator deployment, validating webhook, and DynamoGraphDeployment CRD",
		Phase:       phaseConformance,
		Func:        CheckRobustController,
		TestName:    "TestRobustController",
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

	// Check if the error is specifically a webhook admission rejection.
	// We intentionally do NOT check k8serrors.IsForbidden() here because Forbidden
	// can also come from RBAC denials, which would produce false positives.
	errMsg := createErr.Error()
	if strings.Contains(errMsg, "admission webhook") || strings.Contains(errMsg, "denied the request") {
		return nil // PASS — webhook properly rejected the invalid resource
	}

	// Non-admission error (RBAC, network, CRD not installed, etc).
	return errors.Wrap(errors.ErrCodeInternal,
		"unexpected error testing webhook rejection", createErr)
}
