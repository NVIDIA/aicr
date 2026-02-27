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

package agent

import (
	"context"
	"fmt"
	"log/slog"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ensureServiceAccount creates the ServiceAccount if it doesn't exist.
func (d *Deployer) ensureServiceAccount(ctx context.Context) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.ServiceAccountName,
			Namespace: d.config.Namespace,
		},
	}

	_, err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Create(ctx, sa, metav1.CreateOptions{})
	return k8s.IgnoreAlreadyExists(err)
}

// deleteServiceAccount deletes the ServiceAccount.
func (d *Deployer) deleteServiceAccount(ctx context.Context) error {
	err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// ensureRole creates the Role if it doesn't exist.
// The Role grants permissions to read cluster state and write result ConfigMaps.
// Note: This is namespace-scoped. For cluster-wide resources, see ensureClusterRole.
func (d *Deployer) ensureRole(ctx context.Context) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.ServiceAccountName,
			Namespace: d.config.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces", "events", "services", "endpoints", "nodes"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "create", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/log", "pods/status"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"batch"},
				Resources: []string{"jobs"},
				Verbs:     []string{"get", "list"},
			},
		},
	}

	_, err := d.clientset.RbacV1().Roles(d.config.Namespace).Create(ctx, role, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		_, err = d.clientset.RbacV1().Roles(d.config.Namespace).Update(ctx, role, metav1.UpdateOptions{})
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to update Role", err)
		}
		return nil
	}
	return err
}

// deleteRole deletes the Role.
func (d *Deployer) deleteRole(ctx context.Context) error {
	err := d.clientset.RbacV1().Roles(d.config.Namespace).Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// ensureClusterRole creates a ClusterRole for cluster-wide read access.
// This is needed for deployment phase validators that need to read resources
// in other namespaces (e.g., GPU operator in gpu-operator namespace).
func (d *Deployer) ensureClusterRole(ctx context.Context) error {
	clusterRoleName := d.config.ServiceAccountName + "-cluster"
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleName,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "aicr-validator",
				"app.kubernetes.io/managed-by": "aicr",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "daemonsets", "statefulsets", "replicasets"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "services", "nodes"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: cluster-wide core resources (platform-health, robust-controller)
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces", "endpoints"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: CRD discovery (inference-gateway, dra-support, gang-scheduling, robust-controller)
			{
				APIGroups: []string{"apiextensions.k8s.io"},
				Resources: []string{"customresourcedefinitions"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: DRA support validation (resource.k8s.io/v1 — GA)
			{
				APIGroups: []string{"resource.k8s.io"},
				Resources: []string{"resourceslices", "resourceclaims"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: secure-accelerator-access — DRA test pod lifecycle
			{
				APIGroups: []string{""},
				Resources: []string{"namespaces", "pods"},
				Verbs:     []string{"create", "delete"},
			},
			{
				APIGroups: []string{"resource.k8s.io"},
				Resources: []string{"resourceclaims"},
				Verbs:     []string{"create", "delete"},
			},
			// Conformance: GPU operator ClusterPolicy
			{
				APIGroups: []string{"nvidia.com"},
				Resources: []string{"clusterpolicies"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: robust-controller webhook behavioral test
			{
				APIGroups: []string{"nvidia.com"},
				Resources: []string{"dynamographdeployments"},
				Verbs:     []string{"create", "delete"},
			},
			// Conformance: Gateway API validation
			{
				APIGroups: []string{"gateway.networking.k8s.io"},
				Resources: []string{"gatewayclasses", "gateways"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: inference-gateway data-plane validation (HTTPRoute discovery)
			{
				APIGroups: []string{"gateway.networking.k8s.io"},
				Resources: []string{"httproutes"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: Gang scheduling (KAI scheduler)
			{
				APIGroups: []string{"scheduling.run.ai"},
				Resources: []string{"queues", "podgroups"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: gang-scheduling — PodGroup lifecycle for functional test
			{
				APIGroups: []string{"scheduling.run.ai"},
				Resources: []string{"podgroups"},
				Verbs:     []string{"create", "delete"},
			},
			// Conformance: Cluster autoscaling (Karpenter)
			{
				APIGroups: []string{"karpenter.sh"},
				Resources: []string{"nodepools"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: Aggregated metrics APIs (pod-autoscaling, ai-service-metrics)
			{
				APIGroups: []string{"custom.metrics.k8s.io"},
				Resources: []string{"*"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"external.metrics.k8s.io"},
				Resources: []string{"*"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: Robust controller — webhook configurations and endpoint health
			{
				APIGroups: []string{"admissionregistration.k8s.io"},
				Resources: []string{"validatingwebhookconfigurations"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"discovery.k8s.io"},
				Resources: []string{"endpointslices"},
				Verbs:     []string{"get", "list"},
			},
			// Conformance: HPA behavioral tests (pod-autoscaling)
			{
				APIGroups: []string{"autoscaling"},
				Resources: []string{"horizontalpodautoscalers"},
				Verbs:     []string{"get", "list", "create", "update", "delete"},
			},
			// Conformance: HPA behavioral tests — Deployment lifecycle
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				Verbs:     []string{"create", "delete"},
			},
		},
	}

	slog.Debug("creating ClusterRole", "name", clusterRoleName)
	_, err := d.clientset.RbacV1().ClusterRoles().Create(ctx, clusterRole, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		slog.Error("failed to create ClusterRole", "name", clusterRoleName, "error", err)
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ClusterRole", err)
	}
	if errors.IsAlreadyExists(err) {
		slog.Debug("ClusterRole already exists", "name", clusterRoleName)
	} else {
		slog.Info("ClusterRole created successfully", "name", clusterRoleName)
	}
	return nil
}

// deleteClusterRole deletes the ClusterRole.
func (d *Deployer) deleteClusterRole(ctx context.Context) error {
	clusterRoleName := d.config.ServiceAccountName + "-cluster"
	err := d.clientset.RbacV1().ClusterRoles().Delete(ctx, clusterRoleName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// ensureClusterRoleBinding creates a ClusterRoleBinding.
func (d *Deployer) ensureClusterRoleBinding(ctx context.Context) error {
	clusterRoleName := d.config.ServiceAccountName + "-cluster"
	clusterRoleBindingName := d.config.ServiceAccountName + "-cluster"

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleBindingName,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "aicr-validator",
				"app.kubernetes.io/managed-by": "aicr",
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      d.config.ServiceAccountName,
				Namespace: d.config.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
	}

	slog.Debug("creating ClusterRoleBinding",
		"name", clusterRoleBindingName,
		"serviceAccount", d.config.ServiceAccountName,
		"namespace", d.config.Namespace)
	_, err := d.clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		slog.Error("failed to create ClusterRoleBinding", "name", clusterRoleBindingName, "error", err)
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ClusterRoleBinding", err)
	}
	if errors.IsAlreadyExists(err) {
		slog.Debug("ClusterRoleBinding already exists", "name", clusterRoleBindingName)
	} else {
		slog.Info("ClusterRoleBinding created successfully",
			"name", clusterRoleBindingName,
			"serviceAccount", fmt.Sprintf("%s/%s", d.config.Namespace, d.config.ServiceAccountName))
	}
	return nil
}

// deleteClusterRoleBinding deletes the ClusterRoleBinding.
func (d *Deployer) deleteClusterRoleBinding(ctx context.Context) error {
	clusterRoleBindingName := d.config.ServiceAccountName + "-cluster"
	err := d.clientset.RbacV1().ClusterRoleBindings().Delete(ctx, clusterRoleBindingName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// ensureRoleBinding creates the RoleBinding if it doesn't exist.
func (d *Deployer) ensureRoleBinding(ctx context.Context) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.ServiceAccountName,
			Namespace: d.config.Namespace,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      d.config.ServiceAccountName,
				Namespace: d.config.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     d.config.ServiceAccountName,
		},
	}

	_, err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).Create(ctx, rb, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		_, err = d.clientset.RbacV1().RoleBindings(d.config.Namespace).Update(ctx, rb, metav1.UpdateOptions{})
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to update RoleBinding", err)
		}
		return nil
	}
	return err
}

// deleteRoleBinding deletes the RoleBinding.
func (d *Deployer) deleteRoleBinding(ctx context.Context) error {
	err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// ensureInputConfigMaps verifies that required input ConfigMaps exist.
func (d *Deployer) ensureInputConfigMaps(ctx context.Context) error {
	// Check snapshot ConfigMap
	if d.config.SnapshotConfigMap != "" {
		_, err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Get(ctx, d.config.SnapshotConfigMap, metav1.GetOptions{})
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeNotFound, fmt.Sprintf("snapshot ConfigMap %q not found", d.config.SnapshotConfigMap), err)
		}
	}

	// Check recipe ConfigMap
	if d.config.RecipeConfigMap != "" {
		_, err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Get(ctx, d.config.RecipeConfigMap, metav1.GetOptions{})
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeNotFound, fmt.Sprintf("recipe ConfigMap %q not found", d.config.RecipeConfigMap), err)
		}
	}

	return nil
}

// deleteResultConfigMap deletes the result ConfigMap.
func (d *Deployer) deleteResultConfigMap(ctx context.Context) error {
	resultConfigMapName := fmt.Sprintf("%s-result", d.config.JobName)
	err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Delete(ctx, resultConfigMapName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}
