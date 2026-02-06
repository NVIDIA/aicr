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

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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
	return ignoreAlreadyExists(err)
}

// deleteServiceAccount deletes the ServiceAccount.
func (d *Deployer) deleteServiceAccount(ctx context.Context) error {
	err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return ignoreNotFound(err)
}

// ensureRole creates the Role if it doesn't exist.
// The Role grants permissions to read cluster state and write result ConfigMaps.
func (d *Deployer) ensureRole(ctx context.Context) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.ServiceAccountName,
			Namespace: d.config.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list", "create", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "services", "endpoints"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments", "daemonsets", "statefulsets", "replicasets"},
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
	return ignoreAlreadyExists(err)
}

// deleteRole deletes the Role.
func (d *Deployer) deleteRole(ctx context.Context) error {
	err := d.clientset.RbacV1().Roles(d.config.Namespace).Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return ignoreNotFound(err)
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
	return ignoreAlreadyExists(err)
}

// deleteRoleBinding deletes the RoleBinding.
func (d *Deployer) deleteRoleBinding(ctx context.Context) error {
	err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return ignoreNotFound(err)
}

// ensureInputConfigMaps verifies that required input ConfigMaps exist.
func (d *Deployer) ensureInputConfigMaps(ctx context.Context) error {
	// Check snapshot ConfigMap
	if d.config.SnapshotConfigMap != "" {
		_, err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Get(ctx, d.config.SnapshotConfigMap, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("snapshot ConfigMap %q not found: %w", d.config.SnapshotConfigMap, err)
		}
	}

	// Check recipe ConfigMap
	if d.config.RecipeConfigMap != "" {
		_, err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Get(ctx, d.config.RecipeConfigMap, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("recipe ConfigMap %q not found: %w", d.config.RecipeConfigMap, err)
		}
	}

	return nil
}

// deleteResultConfigMap deletes the result ConfigMap.
func (d *Deployer) deleteResultConfigMap(ctx context.Context) error {
	resultConfigMapName := fmt.Sprintf("%s-result", d.config.JobName)
	err := d.clientset.CoreV1().ConfigMaps(d.config.Namespace).Delete(ctx, resultConfigMapName, metav1.DeleteOptions{})
	return ignoreNotFound(err)
}
