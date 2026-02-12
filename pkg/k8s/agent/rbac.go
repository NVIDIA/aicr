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

	"github.com/NVIDIA/eidos/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ensureServiceAccount creates the ServiceAccount for the agent.
// If the ServiceAccount already exists, this is a no-op (idempotent).
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

// ensureRole creates the Role for ConfigMap access.
// If the Role already exists, this is a no-op (idempotent).
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
				Verbs:     []string{"create", "get", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list"},
			},
		},
	}

	_, err := d.clientset.RbacV1().Roles(d.config.Namespace).Create(ctx, role, metav1.CreateOptions{})
	return k8s.IgnoreAlreadyExists(err)
}

// ensureRoleBinding creates the RoleBinding to bind the Role to the ServiceAccount.
// If the RoleBinding already exists, this is a no-op (idempotent).
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
	return k8s.IgnoreAlreadyExists(err)
}

// ensureClusterRole creates the ClusterRole for node and cluster-wide resource access.
// If the ClusterRole already exists, this is a no-op (idempotent).
func (d *Deployer) ensureClusterRole(ctx context.Context) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleName,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{"nvidia.com"},
				Resources: []string{"clusterpolicies"},
				Verbs:     []string{"get", "list"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"services"},
				Verbs:     []string{"get", "list"},
			},
		},
	}

	_, err := d.clientset.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
	return k8s.IgnoreAlreadyExists(err)
}

// ensureClusterRoleBinding creates the ClusterRoleBinding to bind the ClusterRole to the ServiceAccount.
// If the ClusterRoleBinding already exists, this is a no-op (idempotent).
func (d *Deployer) ensureClusterRoleBinding(ctx context.Context) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleName,
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
			Name:     "eidos-node-reader",
		},
	}

	_, err := d.clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	return k8s.IgnoreAlreadyExists(err)
}

// deleteServiceAccount deletes the ServiceAccount.
// If the ServiceAccount doesn't exist, this is a no-op (idempotent).
func (d *Deployer) deleteServiceAccount(ctx context.Context) error {
	err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).
		Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// deleteRole deletes the Role.
// If the Role doesn't exist, this is a no-op (idempotent).
func (d *Deployer) deleteRole(ctx context.Context) error {
	err := d.clientset.RbacV1().Roles(d.config.Namespace).
		Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// deleteRoleBinding deletes the RoleBinding.
// If the RoleBinding doesn't exist, this is a no-op (idempotent).
func (d *Deployer) deleteRoleBinding(ctx context.Context) error {
	err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).
		Delete(ctx, d.config.ServiceAccountName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// deleteClusterRole deletes the ClusterRole.
// If the ClusterRole doesn't exist, this is a no-op (idempotent).
func (d *Deployer) deleteClusterRole(ctx context.Context) error {
	err := d.clientset.RbacV1().ClusterRoles().
		Delete(ctx, "eidos-node-reader", metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// deleteClusterRoleBinding deletes the ClusterRoleBinding.
// If the ClusterRoleBinding doesn't exist, this is a no-op (idempotent).
func (d *Deployer) deleteClusterRoleBinding(ctx context.Context) error {
	err := d.clientset.RbacV1().ClusterRoleBindings().
		Delete(ctx, "eidos-node-reader", metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}
