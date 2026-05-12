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

package agent

import (
	"context"
	"fmt"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ensureNamespace creates or labels the namespace.
// We deliberately do not use IgnoreAlreadyExists alone here because the
// managed-by label is intent we want applied even when the user pre-created
// the namespace. The flow is:
//  1. Try Create — common path for fresh installs.
//  2. On AlreadyExists, Get the namespace and check if our managed-by label
//     is already set; if so, return early. This avoids requiring patch
//     permission for the (typical) case where the namespace was already
//     properly labeled by a prior run.
//  3. Otherwise, Patch the label on. This is the only path that requires
//     namespaces/patch.
func (d *Deployer) ensureNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: d.config.Namespace,
			Labels: map[string]string{
				labelAppManagedBy: appName,
			},
		},
	}
	_, err := d.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create Namespace", err)
	}

	// Pre-existing namespace: read the current labels first so we only Patch
	// when the label is actually missing or wrong (saves a round trip and
	// avoids requiring patch permission in the common case).
	existing, getErr := d.clientset.CoreV1().Namespaces().
		Get(ctx, d.config.Namespace, metav1.GetOptions{})
	if getErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get existing Namespace", getErr)
	}
	if existing.Labels[labelAppManagedBy] == appName {
		return nil
	}

	patch := []byte(fmt.Sprintf(
		`{"metadata":{"labels":{%q:%q}}}`,
		labelAppManagedBy, appName,
	))
	if _, err := d.clientset.CoreV1().Namespaces().Patch(
		ctx, d.config.Namespace, types.MergePatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to label existing Namespace", err)
	}
	return nil
}

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

// ensureRole creates or updates the Role for ConfigMap access.
func (d *Deployer) ensureRole(ctx context.Context) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.config.ServiceAccountName,
			Namespace: d.config.Namespace,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{resourceCM},
				Verbs:     []string{verbCreate, verbGet, "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", verbList},
			},
		},
	}

	_, err := d.clientset.RbacV1().Roles(d.config.Namespace).Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = d.clientset.RbacV1().Roles(d.config.Namespace).Update(ctx, role, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update Role", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create Role", err)
	}
	return nil
}

// ensureRoleBinding creates or updates the RoleBinding to bind the Role to the ServiceAccount.
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
	if apierrors.IsAlreadyExists(err) {
		_, err = d.clientset.RbacV1().RoleBindings(d.config.Namespace).Update(ctx, rb, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update RoleBinding", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create RoleBinding", err)
	}
	return nil
}

// ensureClusterRole creates or updates the ClusterRole for node and cluster-wide resource access.
func (d *Deployer) ensureClusterRole(ctx context.Context) error {
	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{verbGet, verbList},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{verbGet, verbList},
		},
		{
			APIGroups: []string{"nvidia.com"},
			Resources: []string{"clusterpolicies"},
			Verbs:     []string{verbGet, verbList},
		},
	}

	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleName,
		},
		Rules: rules,
	}

	_, err := d.clientset.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = d.clientset.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update ClusterRole", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ClusterRole", err)
	}
	return nil
}

// ensureClusterRoleBinding creates or updates the ClusterRoleBinding to bind the ClusterRole to the ServiceAccount.
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
			Name:     clusterRoleName,
		},
	}

	_, err := d.clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = d.clientset.RbacV1().ClusterRoleBindings().Update(ctx, crb, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update ClusterRoleBinding", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ClusterRoleBinding", err)
	}
	return nil
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
		Delete(ctx, clusterRoleName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// deleteClusterRoleBinding deletes the ClusterRoleBinding.
// If the ClusterRoleBinding doesn't exist, this is a no-op (idempotent).
func (d *Deployer) deleteClusterRoleBinding(ctx context.Context) error {
	err := d.clientset.RbacV1().ClusterRoleBindings().
		Delete(ctx, clusterRoleName, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}
