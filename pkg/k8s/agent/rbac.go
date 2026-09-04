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

// resolveServiceAccount decides, once per Deploy, whether this run creates
// and owns its own run-scoped ServiceAccount or runs as an
// already-existing one the operator named exactly.
//
// Config.ServiceAccountName is exact-if-exists. When it is set and a
// ServiceAccount of exactly that name already exists in the namespace, the
// agent pod runs as that ServiceAccount verbatim and this run creates NO
// ServiceAccount, Role, RoleBinding, ClusterRole or ClusterRoleBinding —
// aicr manages no permissions for an identity it did not create. That is
// what keeps a pre-created ServiceAccount carrying IRSA
// (eks.amazonaws.com/role-arn) or GKE Workload Identity
// (iam.gke.io/gcp-service-account) annotations usable: both providers pin
// trust to the ServiceAccount NAME, so a per-run name can never be trusted
// by either and copying the annotations onto one would not help.
//
// When the name does not exist, the value stays a prefix and the run
// creates <prefix>-<RunID> plus its RBAC, exactly as before. An unset
// Config.ServiceAccountName is never probed: the fallback base ("aicr") is
// aicr's own default, not something the operator asked for, so a stray
// ServiceAccount sitting at that name must not silently capture the run.
//
// It is called from CheckPermissions, not from Deploy directly: the verb set
// the pre-flight demands depends on which of the two modes this run is in,
// so the gate has to resolve before it can finish. The Get is read-only, so
// resolving inside the gate does not weaken fail-before-mutate — no write is
// issued until Deploy's ensure* chain, which runs only once the gate passes.
//
// Every error fails closed, Forbidden included. `serviceaccounts: get` is a
// REQUIRED check in CheckPermissions, evaluated before this runs, so a
// caller that reaches here has been told by the apiserver's own authorizer
// that it may read ServiceAccounts; a Forbidden anyway means the answer and
// the behavior disagree, which is not a state to guess through. The
// previous downgrade — log at debug, continue in prefix mode — is the exact
// credential-loss seam this package exists to close: an operator who passed
// --service-account-name pointing at their IRSA or Workload Identity
// ServiceAccount would silently run under a fresh "<prefix>-<runID>" account
// carrying none of its cloud annotations, with no visible signal.
func (d *Deployer) resolveServiceAccount(ctx context.Context) error {
	name := d.config.ServiceAccountName
	if name == "" {
		return nil
	}

	switch _, err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Get(ctx, name, metav1.GetOptions{}); {
	case err == nil:
		d.setExistingServiceAccount(name)
		slog.Info("using the existing ServiceAccount named by --service-account-name; aicr manages no RBAC for this run",
			attrServiceAccount, name,
			attrNamespace, d.config.Namespace,
			attrRunID, d.config.RunID,
			"note", "no ServiceAccount, Role, RoleBinding, ClusterRole or ClusterRoleBinding is created or deleted; generate this ServiceAccount's RBAC manifests with 'aicr snapshot --add-roles-to-service-account "+name+"' and apply them yourself")
	case apierrors.IsNotFound(err):
		// Normal path: the value is a prefix and this run creates its own
		// run-scoped ServiceAccount below.
	case apierrors.IsForbidden(err):
		return errors.WrapWithContext(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot read ServiceAccount %q in namespace %q, so it is impossible to tell whether "+
				"--service-account-name names an existing ServiceAccount to run as verbatim or is a prefix "+
				"for one this run creates; refusing to guess, because guessing \"prefix\" would run the agent "+
				"under a generated ServiceAccount carrying none of that account's cloud credentials. "+
				"Grant 'get serviceaccounts' in this namespace and re-run", name, d.config.Namespace),
			err, map[string]any{attrName: name, attrNamespace: d.config.Namespace})
	default:
		return errors.Wrap(errors.ErrCodeInternal, "failed to check for an existing ServiceAccount", err)
	}
	return nil
}

// ensureServiceAccount creates the run-scoped ServiceAccount for the agent.
// Deploy calls it only in prefix mode; see resolveServiceAccount.
func (d *Deployer) ensureServiceAccount(ctx context.Context) error {
	name := d.saName()
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.config.Namespace,
			Labels:    d.objectLabels(),
		},
	}

	// Record the intent before the Create so a committed create whose
	// response is lost still enters Cleanup's delete list (see recordIntent).
	d.recordIntent(kindServiceAccount, name)
	created, err := d.clientset.CoreV1().ServiceAccounts(d.config.Namespace).Create(ctx, sa, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		d.discardIntent(kindServiceAccount, name)
		return errors.Wrap(errors.ErrCodeInternal, "ServiceAccount already exists under run-scoped name (duplicate RunID?)", err)
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ServiceAccount", err)
	}
	d.recordCreated(kindServiceAccount, created.Name, created.UID)
	return nil
}

// namespacedRules returns the namespace-scoped policy rules the agent needs:
// writing its snapshot result into a staging ConfigMap, and reading pods.
//
// It is the single definition consumed by both the run-scoped Role
// ensureRole creates and the Role BuildServiceAccountRoleManifests renders
// for an operator-supplied ServiceAccount, so the two can never drift.
func namespacedRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{resourceCM},
			Verbs:     []string{verbCreate, verbGet, verbUpdate, verbPatch},
		},
		{
			APIGroups: []string{""},
			Resources: []string{resourcePods},
			Verbs:     []string{verbGet, verbList},
		},
	}
}

// clusterRules returns the cluster-scoped policy rules the agent needs. The
// baseline set is read-only; discoverNetwork appends the mutating rules live
// l8k network discovery requires (see discoverNetworkClusterRules).
//
// It is the single definition consumed by both the run-scoped ClusterRole
// ensureClusterRole creates and the ClusterRole
// BuildServiceAccountRoleManifests renders.
func clusterRules(discoverNetwork bool) []rbacv1.PolicyRule {
	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{resourceNodes},
			Verbs:     []string{verbGet, verbList},
		},
		{
			APIGroups: []string{""},
			Resources: []string{resourcePods},
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
		{
			// OKE legacy device-plugin conflict evidence: the K8s collector
			// reads kube-system/nvidia-gpu-device-plugin (a single namespaced
			// Get; list kept for parity with the other read-only rules) —
			// pkg/collector/k8s/okelegacyplugin.go.
			APIGroups: []string{"apps"},
			Resources: []string{"daemonsets"},
			Verbs:     []string{verbGet, verbList},
		},
	}

	// Live l8k network discovery stands up a nic-configuration-daemon
	// DaemonSet in its own namespace, exec's into the daemon pods,
	// writes nvidia.kubernetes-launch-kit.{machine,gpu} labels onto
	// nodes, and patches mellanox.com NicClusterPolicy via server-side
	// apply. Grant the extra cluster-scoped rules only when discovery was
	// opted into so non-network snapshots stay minimal-priv.
	if discoverNetwork {
		rules = append(rules, discoverNetworkClusterRules()...)
	}
	return rules
}

// ensureRole creates the run-scoped Role for ConfigMap access.
func (d *Deployer) ensureRole(ctx context.Context) error {
	name := d.roleName()
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.config.Namespace,
			Labels:    d.objectLabels(),
		},
		Rules: namespacedRules(),
	}

	d.recordIntent(kindRole, name)
	created, err := d.clientset.RbacV1().Roles(d.config.Namespace).Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		d.discardIntent(kindRole, name)
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
	name := d.roleName()
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.config.Namespace,
			Labels:    d.objectLabels(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      kindServiceAccount,
				Name:      d.saName(),
				Namespace: d.config.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     kindRole,
			Name:     d.roleName(),
		},
	}

	d.recordIntent(kindRoleBinding, name)
	created, err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).Create(ctx, rb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		d.discardIntent(kindRoleBinding, name)
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
	name := d.clusterRoleName()
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: d.objectLabels(),
		},
		Rules: clusterRules(d.config.DiscoverNetwork),
	}

	d.recordIntent(kindClusterRole, name)
	created, err := d.clientset.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		d.discardIntent(kindClusterRole, name)
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
	name := d.clusterRoleName()
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: d.objectLabels(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      kindServiceAccount,
				Name:      d.saName(),
				Namespace: d.config.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     kindClusterRole,
			Name:     d.clusterRoleName(),
		},
	}

	d.recordIntent(kindClusterRoleBinding, name)
	created, err := d.clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		d.discardIntent(kindClusterRoleBinding, name)
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
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: uidPreconditions(uid)})
	return ignoreNotFoundOrConflict(err)
}

// deleteRole deletes the Role, pinning the delete to uid. If the Role is
// already gone, or uid no longer matches, this is a no-op (idempotent).
func (d *Deployer) deleteRole(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().Roles(d.config.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: uidPreconditions(uid)})
	return ignoreNotFoundOrConflict(err)
}

// deleteRoleBinding deletes the RoleBinding, pinning the delete to uid. If
// the RoleBinding is already gone, or uid no longer matches, this is a
// no-op (idempotent).
func (d *Deployer) deleteRoleBinding(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().RoleBindings(d.config.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: uidPreconditions(uid)})
	return ignoreNotFoundOrConflict(err)
}

// deleteClusterRole deletes the ClusterRole, pinning the delete to uid. If
// the ClusterRole is already gone, or uid no longer matches, this is a
// no-op (idempotent).
func (d *Deployer) deleteClusterRole(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().ClusterRoles().
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: uidPreconditions(uid)})
	return ignoreNotFoundOrConflict(err)
}

// deleteClusterRoleBinding deletes the ClusterRoleBinding, pinning the
// delete to uid. If the ClusterRoleBinding is already gone, or uid no
// longer matches, this is a no-op (idempotent).
func (d *Deployer) deleteClusterRoleBinding(ctx context.Context, name string, uid types.UID) error {
	err := d.clientset.RbacV1().ClusterRoleBindings().
		Delete(ctx, name, metav1.DeleteOptions{Preconditions: uidPreconditions(uid)})
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
			Resources: []string{resourceServiceAccounts, resourceCM},
			Verbs:     []string{verbGet, verbCreate, verbDelete},
		},
		{
			APIGroups: []string{rbacAPIGroup},
			Resources: []string{resourceRoles, resourceRoleBindings},
			Verbs:     []string{verbGet, verbCreate, verbDelete},
		},
		{
			APIGroups: []string{""},
			Resources: []string{resourcePods + "/exec"},
			Verbs:     []string{verbCreate},
		},
		{
			APIGroups: []string{""},
			Resources: []string{resourceNodes},
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
