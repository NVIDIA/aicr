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

package helper

import (
	"context"
	"fmt"

	aicrErrors "aicr.run/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// NodeResourceRequests returns, per resource, the sum of the effective
// requests (PodEffectiveRequests) of every non-terminal pod bound to nodeName:
// what the scheduler subtracts from the node's allocatable. It is
// IsNodeGpuBusy generalized from "any GPU in use" to "how much of each
// resource is in use", so a check can tell whether one unit of an extended
// resource is still free.
//
// Pods are filtered by node on the client as well as by the field selector,
// so a pod bound elsewhere is never counted against nodeName.
func NodeResourceRequests(ctx context.Context, client kubernetes.Interface, nodeName string) (v1.ResourceList, error) {
	podList, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to list pods on node %s", nodeName), err)
	}

	total := v1.ResourceList{}
	for i := range podList.Items {
		select {
		case <-ctx.Done():
			return nil, aicrErrors.WrapCtxErr(ctx.Err(), aicrErrors.ErrCodeTimeout,
				fmt.Sprintf("summing pod requests on node %s", nodeName))
		default:
		}
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		addResources(total, PodEffectiveRequests(pod))
	}
	return total, nil
}

// PodEffectiveRequests returns the pod's effective request per resource, as
// the scheduler computes it: the larger of the app containers plus sidecars
// (restartable init containers), and each regular init container plus the
// sidecars started before it. A container that sets only a limit is counted
// by its limit, which is the request the API server defaults for it.
func PodEffectiveRequests(pod *v1.Pod) v1.ResourceList {
	sidecars := v1.ResourceList{}
	initPeak := v1.ResourceList{}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		req := containerRequests(c)
		if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
			addResources(sidecars, req)
			continue
		}
		withSidecars := req.DeepCopy()
		addResources(withSidecars, sidecars)
		maxResources(initPeak, withSidecars)
	}

	effective := sidecars.DeepCopy()
	for i := range pod.Spec.Containers {
		addResources(effective, containerRequests(&pod.Spec.Containers[i]))
	}
	maxResources(effective, initPeak)
	return effective
}

// containerRequests returns the container's requests, filling each resource
// that has only a limit with that limit.
func containerRequests(c *v1.Container) v1.ResourceList {
	out := v1.ResourceList{}
	for name, q := range c.Resources.Limits {
		out[name] = q.DeepCopy()
	}
	for name, q := range c.Resources.Requests {
		out[name] = q.DeepCopy()
	}
	return out
}

// addResources adds every quantity in add to dst.
func addResources(dst, add v1.ResourceList) {
	for name, q := range add {
		sum := dst[name]
		sum.Add(q)
		dst[name] = sum
	}
}

// maxResources raises every quantity in dst to at least the one in other.
func maxResources(dst, other v1.ResourceList) {
	for name, q := range other {
		if cur, ok := dst[name]; !ok || q.Cmp(cur) > 0 {
			dst[name] = q.DeepCopy()
		}
	}
}
