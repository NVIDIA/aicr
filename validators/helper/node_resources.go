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
	resourcehelper "k8s.io/component-helpers/resource"
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
	others, _, err := NodeResourceRequestsSplit(ctx, client, nodeName, nil)
	return others, err
}

// NodeResourceRequestsSplit is NodeResourceRequests with the pods own selects
// summed apart from the others, so a check can tell the units its own pods
// hold from the units other workloads hold. A nil own selects no pod.
func NodeResourceRequestsSplit(ctx context.Context, client kubernetes.Interface, nodeName string,
	own func(*v1.Pod) bool) (others, owned v1.ResourceList, err error) {

	podList, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to list pods on node %s", nodeName), err)
	}

	others, owned = v1.ResourceList{}, v1.ResourceList{}
	for i := range podList.Items {
		select {
		case <-ctx.Done():
			return nil, nil, aicrErrors.WrapCtxErr(ctx.Err(), aicrErrors.ErrCodeTimeout,
				fmt.Sprintf("summing pod requests on node %s", nodeName))
		default:
		}
		pod := &podList.Items[i]
		if pod.Spec.NodeName != nodeName || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			continue
		}
		if own != nil && own(pod) {
			addResources(owned, PodEffectiveRequests(pod))
			continue
		}
		addResources(others, PodEffectiveRequests(pod))
	}
	return others, owned, nil
}

// PodEffectiveRequests returns the pod's effective request per resource, as
// the scheduler computes it (k8s.io/component-helpers resource.PodRequests):
// the larger of the app containers plus sidecars (restartable init
// containers) and each regular init container plus the sidecars started
// before it, replaced by pod-level requests for the resources those cover,
// plus the pod overhead. A container that sets only a limit is counted by its
// limit, which is the request the API server defaults for it.
func PodEffectiveRequests(pod *v1.Pod) v1.ResourceList {
	defaulted := pod.DeepCopy()
	for i := range defaulted.Spec.InitContainers {
		defaultRequestsFromLimits(&defaulted.Spec.InitContainers[i])
	}
	for i := range defaulted.Spec.Containers {
		defaultRequestsFromLimits(&defaulted.Spec.Containers[i])
	}
	return resourcehelper.PodRequests(defaulted, resourcehelper.PodResourcesOptions{})
}

// defaultRequestsFromLimits sets the request of every resource c limits
// without requesting to that limit.
func defaultRequestsFromLimits(c *v1.Container) {
	for name, q := range c.Resources.Limits {
		if _, ok := c.Resources.Requests[name]; ok {
			continue
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = v1.ResourceList{}
		}
		c.Resources.Requests[name] = q.DeepCopy()
	}
}

// addResources adds every quantity in add to dst.
func addResources(dst, add v1.ResourceList) {
	for name, q := range add {
		sum := dst[name]
		sum.Add(q)
		dst[name] = sum
	}
}
