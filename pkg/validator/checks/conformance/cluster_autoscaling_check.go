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
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "cluster-autoscaling",
		Description: "Verify Karpenter controller is deployed and a GPU-aware NodePool exists",
		Phase:       phaseConformance,
		Func:        CheckClusterAutoscaling,
		TestName:    "TestClusterAutoscaling",
	})
}

// CheckClusterAutoscaling validates CNCF requirement #8a: Cluster Autoscaling.
// Verifies the Karpenter controller deployment is running and at least one
// NodePool has nvidia.com/gpu limits configured.
func CheckClusterAutoscaling(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Karpenter controller deployment running
	if err := verifyDeploymentAvailable(ctx, "karpenter", "karpenter"); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Karpenter controller check failed", err)
	}

	// 2. GPU NodePool exists with nvidia.com/gpu limits
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	npGVR := schema.GroupVersionResource{
		Group: "karpenter.sh", Version: "v1", Resource: "nodepools",
	}
	nps, err := dynClient.Resource(npGVR).List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "failed to list NodePools", err)
	}

	var hasGPUPool bool
	for _, np := range nps.Items {
		limits, found, _ := unstructured.NestedMap(np.Object, "spec", "limits")
		if found {
			if _, hasGPU := limits["nvidia.com/gpu"]; hasGPU {
				hasGPUPool = true
				break
			}
		}
	}
	if !hasGPUPool {
		return errors.New(errors.ErrCodeNotFound,
			"no NodePool with nvidia.com/gpu limits found")
	}

	return nil
}
