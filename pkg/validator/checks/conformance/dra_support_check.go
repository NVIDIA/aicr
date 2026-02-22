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
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "dra-support",
		Description: "Verify DRA driver controller, kubelet plugin, and ResourceSlices exist",
		Phase:       phaseConformance,
		Func:        CheckDRASupport,
		TestName:    "TestDRASupport",
	})
}

// CheckDRASupport validates CNCF requirement #2: DRA Support.
// Verifies DRA driver controller deployment, kubelet plugin DaemonSet,
// and that ResourceSlices (resource.k8s.io/v1 GA) exist advertising GPU resources.
func CheckDRASupport(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. DRA driver controller Deployment available
	if err := verifyDeploymentAvailable(ctx, "nvidia-dra-driver", "nvidia-dra-driver-gpu-controller"); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "DRA driver controller check failed", err)
	}

	// 2. DRA kubelet plugin DaemonSet ready
	if err := verifyDaemonSetReady(ctx, "nvidia-dra-driver", "nvidia-dra-driver-gpu-kubelet-plugin"); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "DRA kubelet plugin check failed", err)
	}

	// 3. ResourceSlices exist (GPU resources advertised via resource.k8s.io/v1 — GA)
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices",
	}
	slices, err := dynClient.Resource(gvr).List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceSlices", err)
	}
	if len(slices.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no ResourceSlices found (GPU resources not advertised)")
	}

	return nil
}
