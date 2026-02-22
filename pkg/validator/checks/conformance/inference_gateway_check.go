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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "inference-gateway",
		Description: "Verify Gateway API for AI/ML inference routing (GatewayClass, Gateway, CRDs)",
		Phase:       phaseConformance,
		Func:        CheckInferenceGateway,
		TestName:    "TestInferenceGateway",
	})
}

// CheckInferenceGateway validates CNCF requirement #6: Inference Gateway.
// Verifies GatewayClass "kgateway" is accepted, Gateway "inference-gateway" is programmed,
// and required Gateway API + InferencePool CRDs exist.
func CheckInferenceGateway(ctx *checks.ValidationContext) error {
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	// 1. GatewayClass "kgateway" accepted
	gcGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses",
	}
	gc, err := dynClient.Resource(gcGVR).Get(ctx.Context, "kgateway", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "GatewayClass 'kgateway' not found", err)
	}
	if condErr := checkCondition(gc, "Accepted", "True"); condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "GatewayClass not accepted", condErr)
	}

	// 2. Gateway "inference-gateway" programmed
	gwGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
	}
	gw, err := dynClient.Resource(gwGVR).Namespace("kgateway-system").Get(
		ctx.Context, "inference-gateway", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Gateway 'inference-gateway' not found", err)
	}
	if condErr := checkCondition(gw, "Programmed", "True"); condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "Gateway not programmed", condErr)
	}

	// 3. Required CRDs exist
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	requiredCRDs := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.x-k8s.io",
	}
	for _, crdName := range requiredCRDs {
		_, err := dynClient.Resource(crdGVR).Get(ctx.Context, crdName, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("CRD %s not found", crdName), err)
		}
	}

	return nil
}
