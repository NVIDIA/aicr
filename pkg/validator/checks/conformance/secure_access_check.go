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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "secure-accelerator-access",
		Description: "Verify DRA-mediated GPU access (no device plugin, no hostPath)",
		Phase:       phaseConformance,
		Func:        CheckSecureAcceleratorAccess,
		TestName:    "TestSecureAcceleratorAccess",
	})
}

// CheckSecureAcceleratorAccess validates CNCF requirement #3: Secure Accelerator Access.
// Verifies that a DRA-based GPU workload uses proper access patterns:
// resourceClaims instead of device plugin, no hostPath to GPU devices,
// and ResourceClaim is allocated.
func CheckSecureAcceleratorAccess(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. Get the DRA test pod (deployed by workflow before aicr validate runs)
	pod, err := ctx.Clientset.CoreV1().Pods("dra-test").Get(
		ctx.Context, "dra-gpu-test", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"DRA test pod not found (deploy dra-gpu-test.yaml first)", err)
	}

	// 2. Pod uses resourceClaims (DRA pattern)
	if len(pod.Spec.ResourceClaims) == 0 {
		return errors.New(errors.ErrCodeInternal,
			"pod does not use DRA resourceClaims")
	}

	// 3. No nvidia.com/gpu in resources.limits (device plugin pattern)
	for _, c := range pod.Spec.Containers {
		if c.Resources.Limits != nil {
			if _, hasGPU := c.Resources.Limits["nvidia.com/gpu"]; hasGPU {
				return errors.New(errors.ErrCodeInternal,
					"pod uses device plugin (nvidia.com/gpu in limits) instead of DRA")
			}
		}
	}

	// 4. No hostPath volumes to /dev/nvidia*
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil && strings.Contains(vol.HostPath.Path, "/dev/nvidia") {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("pod has hostPath volume to %s", vol.HostPath.Path))
		}
	}

	// 5. ResourceClaim exists
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims",
	}
	_, err = dynClient.Resource(gvr).Namespace("dra-test").Get(
		ctx.Context, "gpu-claim", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "ResourceClaim gpu-claim not found", err)
	}

	// 6. Pod completed successfully — proves DRA allocation worked.
	// Note: status.allocation may be cleared after pod completion, so we verify
	// success via the pod phase rather than the claim's allocation status.
	if pod.Status.Phase != corev1.PodSucceeded {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DRA test pod phase=%s (want Succeeded), GPU allocation may have failed",
				pod.Status.Phase))
	}

	return nil
}
