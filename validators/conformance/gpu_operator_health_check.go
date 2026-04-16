// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CheckGPUOperatorHealth validates CNCF requirement #1: GPU Management.
// Verifies GPU operator deployment, ClusterPolicy state=ready, and DCGM exporter DaemonSet.
func CheckGPUOperatorHealth(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. GPU Operator Deployment running
	// Check by listing deployments with the gpu-operator label in the namespace.
	deploys, err := ctx.Clientset.AppsV1().Deployments(defaults.GPUOperatorNamespace).List(ctx.Ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=gpu-operator",
	})
	if err != nil || len(deploys.Items) == 0 {
		// Fallback: try exact name match.
		if verifyErr := verifyDeploymentAvailable(ctx, defaults.GPUOperatorNamespace, "gpu-operator"); verifyErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "GPU operator deployment not found (checked label app.kubernetes.io/name=gpu-operator and exact name)", verifyErr)
		}
	}

	// 2. ClusterPolicy state = ready (dynamic client — CRD type)
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies",
	}
	cp, err := dynClient.Resource(gvr).Get(ctx.Ctx, "cluster-policy", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "ClusterPolicy not found", err)
	}
	state, found, err := unstructured.NestedString(cp.Object, "status", "state")
	if err != nil || !found {
		return errors.New(errors.ErrCodeInternal, "ClusterPolicy status.state not found")
	}
	if state != "ready" {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("ClusterPolicy state=%s (want ready)", state))
	}

	// 3. DCGM exporter DaemonSet running
	if err := verifyDaemonSetReady(ctx, defaults.GPUOperatorNamespace, "nvidia-dcgm-exporter"); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "DCGM exporter check failed", err)
	}

	return nil
}
