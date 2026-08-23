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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
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

// ensureServiceAccount creates the run-scoped ServiceAccount for the agent.
//
// Before creating, it checks whether a ServiceAccount already exists under
// the bare (unscoped) prefix name. Previously, a caller passing
// --service-account-name to target a ServiceAccount they created out of
// band (e.g. with cloud IAM annotations for IRSA/Workload Identity) got it
// silently adopted via IgnoreAlreadyExists. Now every run gets its own
// run-scoped ServiceAccount, so that adoption no longer happens; warn
// loudly instead of leaving the caller to discover it the hard way. A
// NotFound Get is the normal path and stays silent.
//
// That Get is a diagnostic courtesy and must not gate the deployment:
// `serviceaccounts get` is deliberately absent from CheckPermissions'
// requiredChecks (permissions.go), so an identity scoped to exactly the
// pre-flight verb set would otherwise pass the pre-flight and then fail
// Deploy with an ErrCodeInternal — a permission problem reported as an
// internal error. Forbidden therefore downgrades to a debug line and the
// deployment proceeds; every other unexpected error still fails closed.
func (d *Deployer) ensureServiceAccount(ctx context.Context) error {
	name := d.saName()
	bareName := d.config.ServiceAccountName
	if bareName == "" {
		bareName = d.base()
	}
	switch _, err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Get(ctx, bareName, metav1.GetOptions{}); {
	case err == nil:
		slog.Warn("ServiceAccount already exists under the unscoped name; aicr is creating a run-scoped ServiceAccount instead of adopting it",
			"existing", bareName, "creating", name)
	case apierrors.IsNotFound(err):
		// Normal path: nothing to warn about.
	case apierrors.IsForbidden(err):
		slog.Debug("skipping adoption-drift check: not permitted to read ServiceAccounts in this namespace",
			"name", bareName, "namespace", d.config.Namespace, "error", err)
	default:
		return errors.Wrap(errors.ErrCodeInternal, "failed to check for pre-existing ServiceAccount", err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.config.Namespace,
			Labels:    d.objectLabels(),
		},
	}

	created, err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Create(ctx, sa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "ServiceAccount already exists under run-scoped name (duplicate RunID?)", err)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ServiceAccount", err)
	}
	d.recordCreated(kindServiceAccount, created.Name, created.UID)
	return nil
}

// ensureRole creates the run-scoped Role for ConfigMap access.
func (d *Deployer) ensureRole(ctx context.Context) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.roleName(),
			Namespace: d.config.Namespace,
			Labels:    d.objectLabels(),
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

	created, err := d.clientset.RbacV1().Roles(d.config.Namespace).Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "Role already exists under run-scoped name (duplicate RunID?)", err)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create Role", err)
	}
	d.recordCreated(kindRole, created.Name, created.UID)
	return nil
}

// ensureRoleBinding creates the run-scoped RoleBinding binding the Role to the ServiceAccount.
func (d *Deployer) ensureRoleBinding(ctx context.Context) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      d.roleName(),
			Namespace: d.config.Namespace,
			Labels:    d.objectLabels(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      d.saName(),
				Namespace: d.config.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     "Role",
			Name:     d.roleName(),
		},
	}

	created, err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).Create(ctx, rb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "RoleBinding already exists under run-scoped name (duplicate RunID?)", err)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create RoleBinding", err)
	}
	d.recordCreated(kindRoleBinding, created.Name, created.UID)
	return nil
}

// ensureClusterRole creates the run-scoped ClusterRole for node and cluster-wide resource access.
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
		{
			APIGroups: []string{slinkyAPIGroup},
			Resources: []string{
				slinkyControllerResource,
				slinkyNodeSetResource,
				slinkyLoginSetResource,
				slinkyRestAPIResource,
				slinkyAccountingResource,
			},
			Verbs: []string{verbList},
		},
		{
			APIGroups: []string{mariaDBAPIGroup},
			Resources: []string{mariaDBResource},
			Verbs:     []string{verbList},
		},
	}

	// Live l8k network discovery stands up a nic-configuration-daemon
	// DaemonSet in its own namespace, exec's into the daemon pods,
	// writes nvidia.kubernetes-launch-kit.{machine,gpu} labels onto
	// nodes, and patches mellanox.com NicClusterPolicy via server-side
	// apply. Grant the extra cluster-scoped rules only when the snapshot
	// opted into discovery so non-network snapshots stay minimal-priv.
	if d.config.DiscoverNetwork {
		rules = append(rules, discoverNetworkClusterRules()...)
	}

	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   d.clusterRoleName(),
			Labels: d.objectLabels(),
		},
		Rules: rules,
	}

	created, err := d.clientset.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "ClusterRole already exists under run-scoped name (duplicate RunID?)", err)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ClusterRole", err)
	}
	d.recordCreated(kindClusterRole, created.Name, created.UID)
	return nil
}

// ensureClusterRoleBinding creates the run-scoped ClusterRoleBinding binding the ClusterRole to the ServiceAccount.
func (d *Deployer) ensureClusterRoleBinding(ctx context.Context) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   d.clusterRoleName(),
			Labels: d.objectLabels(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      d.saName(),
				Namespace: d.config.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     "ClusterRole",
			Name:     d.clusterRoleName(),
		},
	}

	created, err := d.clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "ClusterRoleBinding already exists under run-scoped name (duplicate RunID?)", err)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ClusterRoleBinding", err)
	}
	d.recordCreated(kindClusterRoleBinding, created.Name, created.UID)
	return nil
}

// deleteServiceAccount deletes the ServiceAccount, pinning the delete to uid
// so a same-named ServiceAccount belonging to a different run is never
// collected. If the ServiceAccount is already gone, or uid no longer
// matches (already replaced, not ours), this is a no-op (idempotent).
func (d *Deployer) deleteServiceAccount(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	return ignoreNotFoundOrConflict(err)
}

// deleteRole deletes the Role, pinning the delete to uid. If the Role is
// already gone, or uid no longer matches, this is a no-op (idempotent).
func (d *Deployer) deleteRole(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().Roles(d.config.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	return ignoreNotFoundOrConflict(err)
}

// deleteRoleBinding deletes the RoleBinding, pinning the delete to uid. If
// the RoleBinding is already gone, or uid no longer matches, this is a
// no-op (idempotent).
func (d *Deployer) deleteRoleBinding(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	return ignoreNotFoundOrConflict(err)
}

// deleteClusterRole deletes the ClusterRole, pinning the delete to uid. If
// the ClusterRole is already gone, or uid no longer matches, this is a
// no-op (idempotent).
func (d *Deployer) deleteClusterRole(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().ClusterRoles().
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	return ignoreNotFoundOrConflict(err)
}

// deleteClusterRoleBinding deletes the ClusterRoleBinding, pinning the
// delete to uid. If the ClusterRoleBinding is already gone, or uid no
// longer matches, this is a no-op (idempotent).
func (d *Deployer) deleteClusterRoleBinding(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().ClusterRoleBindings().
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	return ignoreNotFoundOrConflict(err)
}

// discoverNetworkClusterRules returns the cluster-scoped policy rules
// required by l8k's live network discovery (--discover-network). Each
// rule maps to a concrete cluster-side step in the discovery flow:
//
//   - customresourcedefinitions: l8k installs the
//     nic-configuration-operator CRDs (NicDevice, NicClusterPolicy)
//     if they're absent.
//   - namespaces / daemonsets / serviceaccounts / configmaps /
//     roles / rolebindings: l8k creates a bootstrap namespace
//     (nvidia-k8s-launch-kit) and deploys the nic-configuration-daemon
//     DaemonSet plus its supporting RBAC, then deletes the namespace
//     when done.
//   - pods/exec: l8k exec's into each daemon pod to read VPD / link
//     metadata via the in-pod CLI.
//   - nodes/patch: l8k writes nvidia.kubernetes-launch-kit.machine
//     and .gpu labels onto matched nodes.
//   - nicdevices: l8k consumes the NicDevice CRs the daemon publishes.
//   - nicclusterpolicies: l8k patches the user's NicClusterPolicy
//     (NicConfigurationOperator section) via server-side apply.
func discoverNetworkClusterRules() []rbacv1.PolicyRule {
	const verbUpdate, verbPatch, verbWatch, verbDelete = "update", "patch", "watch", "delete"
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"apiextensions.k8s.io"},
			Resources: []string{"customresourcedefinitions"},
			Verbs:     []string{verbGet, verbList, verbCreate, verbUpdate, verbPatch},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"namespaces"},
			Verbs:     []string{verbGet, verbCreate, verbDelete},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"daemonsets"},
			Verbs:     []string{verbGet, verbList, verbWatch, verbCreate, verbDelete},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"serviceaccounts", "configmaps"},
			Verbs:     []string{verbGet, verbCreate, verbDelete},
		},
		{
			APIGroups: []string{rbacAPIGroup},
			Resources: []string{"roles", "rolebindings"},
			Verbs:     []string{verbGet, verbCreate, verbDelete},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods/exec"},
			Verbs:     []string{verbCreate},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{verbPatch},
		},
		{
			APIGroups: []string{"configuration.net.nvidia.com"},
			Resources: []string{"nicdevices"},
			Verbs:     []string{verbGet, verbList},
		},
		{
			APIGroups: []string{"mellanox.com"},
			Resources: []string{"nicclusterpolicies"},
			Verbs:     []string{verbGet, verbPatch},
		},
	}
}
