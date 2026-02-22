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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

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

	return nil
}
