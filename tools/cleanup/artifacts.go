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

package main

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

// sweep deletes every object of one resource, in namespace when it is set,
// that match selects.
type sweep struct {
	gvr       schema.GroupVersionResource
	namespace string
	match     func(metav1.Object) bool
}

var (
	clusterRoleBindingGVR = rbacv1.SchemeGroupVersion.WithResource("clusterrolebindings")
	clusterRoleGVR        = rbacv1.SchemeGroupVersion.WithResource("clusterroles")
	roleBindingGVR        = rbacv1.SchemeGroupVersion.WithResource("rolebindings")
	roleGVR               = rbacv1.SchemeGroupVersion.WithResource("roles")
	jobGVR                = batchv1.SchemeGroupVersion.WithResource("jobs")
	serviceAccountGVR     = corev1.SchemeGroupVersion.WithResource("serviceaccounts")
)

// artifactSweeps covers cluster RBAC from validator and snapshot-agent runs,
// and the snapshot agent's namespaced objects in the gpu-operator namespace.
var artifactSweeps = []sweep{
	{gvr: clusterRoleBindingGVR, match: isAICRClusterRBAC},
	{gvr: clusterRoleGVR, match: isAICRClusterRBAC},
	{gvr: jobGVR, namespace: defaults.GPUOperatorNamespace, match: isSnapshotAgentObject},
	{gvr: serviceAccountGVR, namespace: defaults.GPUOperatorNamespace, match: isSnapshotAgentObject},
	{gvr: roleGVR, namespace: defaults.GPUOperatorNamespace, match: isSnapshotAgentObject},
	{gvr: roleBindingGVR, namespace: defaults.GPUOperatorNamespace, match: isSnapshotAgentObject},
}

func (c *cleaner) removeValidatorArtifacts(ctx context.Context) {
	c.log.Info("phase 2: removing AICR validator and snapshot-agent artifacts")
	for _, s := range artifactSweeps {
		if ctx.Err() != nil {
			return
		}
		c.runSweep(ctx, s)
	}
	c.deleteNamespace(ctx, defaults.ValidationNamespace)
}

func (c *cleaner) runSweep(ctx context.Context, s sweep) {
	res := c.dyn.Resource(s.gvr).Namespace(s.namespace)
	listCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	list, err := res.List(listCtx, metav1.ListOptions{})
	cancel()
	if err != nil {
		c.log.Warn("could not list objects, skipping", "resource", s.gvr.Resource, "error", err)
		return
	}
	background := metav1.DeletePropagationBackground
	for i := range list.Items {
		if ctx.Err() != nil {
			return
		}
		obj := &list.Items[i]
		if !s.match(obj) {
			continue
		}
		c.deleteObject(ctx, s.gvr.Resource, s.namespace, obj.GetName(), func(ctx context.Context) error {
			return res.Delete(ctx, obj.GetName(), metav1.DeleteOptions{PropagationPolicy: &background})
		})
	}
}
