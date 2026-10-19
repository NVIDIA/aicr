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

// ensureNamespace creates or labels the namespace. Deploy calls it only in
// prefix mode; in exact-ServiceAccount mode the namespace belongs to whoever
// provisioned that ServiceAccount and is not touched.
//
// It reads before it writes. The apiserver authorizes a request before it
// looks the object up, so a Create from a caller without cluster-scoped
// `namespaces: create` is Forbidden, not AlreadyExists, even when the
// namespace exists. The flow is:
//  1. Get. If the namespace exists, label it (labelNamespace) and return.
//  2. On NotFound, Create it. A Forbidden Get takes this path too, because
//     a caller may hold create on namespaces without get.
//  3. On AlreadyExists, the namespace appeared after step 1 or could not be
//     read there: Get it again and label it.
//
// Every refused request is reported as ErrCodeUnauthorized naming the verb
// to grant. Whether the namespace is missing is known only when the first
// Get was answered.
func (d *Deployer) ensureNamespace(ctx context.Context) error {
	name := d.config.Namespace
	existing, getErr := d.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case getErr == nil:
		return d.labelNamespace(ctx, existing)
	case !apierrors.IsNotFound(getErr) && !apierrors.IsForbidden(getErr):
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read Namespace %q", name), getErr)
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				labelAppManagedBy: appName,
			},
		},
	}
	_, err := d.clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	switch {
	case err == nil:
		return nil
	case apierrors.IsAlreadyExists(err):
		return d.labelExistingNamespace(ctx, name)
	case apierrors.IsForbidden(err) && apierrors.IsNotFound(getErr):
		return namespaceForbidden(name, err, fmt.Sprintf(
			"namespace %q does not exist and the caller may not create it; "+
				"create it beforehand, or grant 'create namespaces'", name))
	case apierrors.IsForbidden(err):
		return namespaceForbidden(name, err, fmt.Sprintf(
			"the caller may neither read nor create namespace %q; grant 'get' on it (%s), grant 'create namespaces', "+
				"or pass --service-account-name naming an existing ServiceAccount, which leaves the namespace untouched",
			name, namespaceGrantHint))
	default:
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create Namespace %q", name), err)
	}
}

// labelExistingNamespace reads a namespace whose Create reported
// AlreadyExists, then labels it.
func (d *Deployer) labelExistingNamespace(ctx context.Context, name string) error {
	existing, err := d.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return d.labelNamespace(ctx, existing)
	case apierrors.IsForbidden(err):
		return namespaceForbidden(name, err, fmt.Sprintf(
			"namespace %q exists but the caller may not read it; grant 'get' on it (%s)", name, namespaceGrantHint))
	default:
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to get existing Namespace %q", name), err)
	}
}

// labelNamespace sets the managed-by label on an existing namespace,
// replacing any other value. A namespace already labeled managed-by aicr is
// left alone, so the patch permission is needed only when it is not.
func (d *Deployer) labelNamespace(ctx context.Context, ns *corev1.Namespace) error {
	if ns.Labels[labelAppManagedBy] == appName {
		return nil
	}

	patch := fmt.Appendf(nil, `{"metadata":{"labels":{%q:%q}}}`, labelAppManagedBy, appName)
	_, err := d.clientset.CoreV1().Namespaces().Patch(ctx, ns.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	switch {
	case err == nil:
		return nil
	case apierrors.IsForbidden(err):
		return namespaceForbidden(ns.Name, err, fmt.Sprintf(
			"the caller may not label namespace %q %s=%s; grant 'patch' on it (%s)",
			ns.Name, labelAppManagedBy, appName, namespaceGrantHint))
	default:
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to label existing Namespace %q", ns.Name), err)
	}
}

// namespaceGrantHint tells an operator how to grant a verb on one namespace
// rather than on all of them.
const namespaceGrantHint = "a ClusterRole rule can limit it to this namespace with resourceNames"

// namespaceForbidden reports a namespace request the apiserver refused as a
// permission the operator can grant.
func namespaceForbidden(name string, cause error, msg string) error {
	return errors.WrapWithContext(errors.ErrCodeUnauthorized, msg, cause, map[string]any{attrNamespace: name})
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

// clusterRules returns the cluster-scoped policy rules the agent needs. Every
// rule is read-only; discoverNetwork appends the one additional read rule
// l8k's read-only network discovery requires (see discoverNetworkReadRules).
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
			APIGroups: []string{storageAPIGroup},
			Resources: []string{storageClassResource},
			Verbs:     []string{verbList},
		},
		{
			APIGroups: []string{perconaAPIGroup},
			Resources: []string{perconaResource},
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

	// Read-only l8k network discovery lists nodes and pods (granted above)
	// and the NicDevice objects the Network Operator's
	// nic-configuration-daemon already publishes. Grant that one extra read
	// only when discovery was opted into so non-network snapshots stay
	// minimal-priv.
	if discoverNetwork {
		rules = append(rules, discoverNetworkReadRules()...)
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

// discoverNetworkReadRules returns the cluster-scoped rules l8k's read-only
// network discovery (--discover-network) needs beyond the baseline. Discovery
// lists nodes and pods, both already granted, and the NicDevice objects an
// existing nic-configuration-daemon publishes. It creates, patches, and
// deletes nothing, and aicr deliberately grants no pods/exec: a daemon node
// without a NicDevice fails the collector rather than being probed.
func discoverNetworkReadRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"configuration.net.nvidia.com"},
			Resources: []string{"nicdevices"},
			Verbs:     []string{verbGet, verbList},
		},
	}
}
