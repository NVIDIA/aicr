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
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

// Naming of the permanent RBAC objects ProvisionServiceAccountRoles creates.
//
// The names are deterministic — the same (namespace, ServiceAccount) pair
// always resolves to the same four names, which is what makes re-running the
// provisioning idempotent — and they cannot collide with a run-scoped name.
//
// The non-collision is structural, not probabilistic. Every run-scoped name
// is "<prefix>-<runID>" and every runID ends in 16 lowercase-hex characters
// (runid.Generate emits "<YYYYMMDD>-<HHMMSS>-<16 hex>"). These names end in
// the literal "-rbac", and "r" is not a hex digit, so no run-scoped name can
// ever equal one of them regardless of what prefix a caller supplies.
const (
	provisionedNamePrefix = "aicr-agent-"
	provisionedNameSuffix = "-rbac"
)

// ProvisionOptions selects the ServiceAccount that
// ProvisionServiceAccountRoles grants the snapshot agent's permissions to.
type ProvisionOptions struct {
	// Namespace holds the ServiceAccount and is where the Role and
	// RoleBinding are created. Required.
	Namespace string

	// ServiceAccountName is the EXACT name of an already-existing
	// ServiceAccount. Required; provisioning fails with ErrCodeNotFound
	// when no such ServiceAccount exists, because the whole point is to
	// grant permissions to an identity the operator created and controls
	// (typically one carrying IRSA or GKE Workload Identity annotations).
	ServiceAccountName string

	// DiscoverNetwork also grants the cluster-scoped MUTATING rules that
	// `aicr snapshot --discover-network` needs — nodes: patch,
	// pods/exec: create, and CRD, namespace, DaemonSet and namespaced-RBAC
	// create/delete (see discoverNetworkClusterRules).
	//
	// Unlike a run-scoped grant, this one is permanent: the ServiceAccount
	// carries those permissions until the operator removes them, not for
	// one run's lifetime.
	DiscoverNetwork bool
}

// ProvisionResult names what ProvisionServiceAccountRoles created or
// updated, so a caller can report it without rebuilding the names.
type ProvisionResult struct {
	Namespace          string
	ServiceAccountName string
	Role               string
	RoleBinding        string
	ClusterRole        string
	ClusterRoleBinding string

	// DiscoverNetwork echoes ProvisionOptions.DiscoverNetwork: it is the
	// difference between a read-only grant and one carrying cluster-scoped
	// mutating rules, so a caller reporting the result must be able to say
	// which was provisioned.
	DiscoverNetwork bool
}

// ProvisionServiceAccountRoles grants the snapshot agent's permissions to an
// already-existing, operator-supplied ServiceAccount by creating a Role,
// RoleBinding, ClusterRole and ClusterRoleBinding for it.
//
// These four objects are PERMANENT and deliberately outside every run's
// lifecycle: they carry no run-ID label, they never enter any Deployer's
// created-set, and no run's Cleanup deletes them. Removing them is the
// operator's job. That is the counterpart to Config.ServiceAccountName's
// exact-if-exists behavior, where a run that adopts an existing
// ServiceAccount creates and deletes no RBAC of its own.
//
// The call is idempotent: each object is created, or updated in place when
// it already exists, so re-running it after an aicr upgrade refreshes the
// rules rather than failing or leaving a stale rule set behind.
//
// Trade-off the caller must surface to the operator: an adopted
// ServiceAccount waives per-run permission isolation. Concurrent runs using
// it share its grants, and a DiscoverNetwork provisioning leaves mutating
// cluster permissions in place permanently rather than for one run.
func ProvisionServiceAccountRoles(ctx context.Context, clientset kubernetes.Interface, opts ProvisionOptions) (*ProvisionResult, error) {
	if strings.TrimSpace(opts.Namespace) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"namespace is required: it is where the ServiceAccount, Role and RoleBinding live")
	}
	if strings.TrimSpace(opts.ServiceAccountName) == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"ServiceAccount name is required: provisioning grants permissions to an existing ServiceAccount, it does not create one")
	}

	// Both halves of each composed name are valid on their own, but their
	// concatenation can exceed the length ceiling. Reject that here, before
	// anything is created, rather than as an opaque apiserver "Invalid
	// value: metadata.name" partway through.
	roleName := provisionedRoleName(opts.ServiceAccountName)
	clusterRoleName := provisionedClusterRoleName(opts.Namespace, opts.ServiceAccountName)
	for _, name := range []string{roleName, clusterRoleName} {
		problems := validation.IsDNS1123Subdomain(name)
		if len(problems) == 0 {
			continue
		}
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("ServiceAccount %q yields the RBAC object name %q, which is not a valid Kubernetes object name: %s",
				opts.ServiceAccountName, name, strings.Join(problems, "; ")),
			map[string]any{ctxKeyValue: opts.ServiceAccountName, ctxKeyResolvedName: name})
	}

	// Fail closed before creating anything when the ServiceAccount is
	// absent. Provisioning permissions for an identity that does not exist
	// would leave four dangling objects and a binding to nothing, and the
	// most likely cause is a typo the operator needs to see.
	if _, err := clientset.CoreV1().ServiceAccounts(opts.Namespace).
		Get(ctx, opts.ServiceAccountName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.NewWithContext(errors.ErrCodeNotFound,
				fmt.Sprintf("ServiceAccount %q not found in namespace %q; create it first (aicr grants permissions to an existing ServiceAccount, it never creates one)",
					opts.ServiceAccountName, opts.Namespace),
				map[string]any{attrServiceAccount: opts.ServiceAccountName, attrNamespace: opts.Namespace})
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read the target ServiceAccount", err)
	}

	subjects := []rbacv1.Subject{{
		Kind:      kindServiceAccount,
		Name:      opts.ServiceAccountName,
		Namespace: opts.Namespace,
	}}

	if err := provisionRole(ctx, clientset, opts.Namespace, roleName); err != nil {
		return nil, err
	}
	if err := provisionRoleBinding(ctx, clientset, opts.Namespace, roleName, subjects); err != nil {
		return nil, err
	}
	if err := provisionClusterRole(ctx, clientset, clusterRoleName, opts.DiscoverNetwork); err != nil {
		return nil, err
	}
	if err := provisionClusterRoleBinding(ctx, clientset, clusterRoleName, subjects); err != nil {
		return nil, err
	}

	return &ProvisionResult{
		Namespace:          opts.Namespace,
		ServiceAccountName: opts.ServiceAccountName,
		Role:               roleName,
		RoleBinding:        roleName,
		ClusterRole:        clusterRoleName,
		ClusterRoleBinding: clusterRoleName,
		DiscoverNetwork:    opts.DiscoverNetwork,
	}, nil
}

// provisionedRoleName returns the permanent Role and RoleBinding name for a
// ServiceAccount. It is injective within a namespace — the name is a pure
// function of the ServiceAccount name, and a ServiceAccount name is unique
// in its namespace — so two ServiceAccounts can never share these objects.
func provisionedRoleName(serviceAccount string) string {
	return provisionedNamePrefix + serviceAccount + provisionedNameSuffix
}

// provisionedClusterRoleName returns the permanent ClusterRole and
// ClusterRoleBinding name for a ServiceAccount. The namespace is part of the
// name because these objects are cluster-scoped and the same ServiceAccount
// name can exist in several namespaces.
//
// Joining two "-"-bearing segments is not injective ("a-b"/"c" and
// "a"/"b-c" compose the same string), so provisionClusterRoleBinding
// additionally refuses to retarget a binding that already names a different
// subject rather than silently revoking the first ServiceAccount's grants.
func provisionedClusterRoleName(namespace, serviceAccount string) string {
	return provisionedNamePrefix + namespace + "-" + serviceAccount + provisionedNameSuffix
}

// provisionedLabels is the label set stamped on every permanent object.
// It deliberately omits labels.RunID: these objects belong to no run, so
// Deployer.createdByThisRun can never match one and no run's Cleanup can
// reclaim it. The component value is what distinguishes them from the
// run-scoped snapshot-agent objects in selectors and sweeps.
func provisionedLabels() map[string]string {
	return map[string]string{
		labels.Name:      labels.ValueAICR,
		labels.ManagedBy: labels.ValueAICR,
		labels.Component: labels.ValueAgentRBAC,
	}
}

// provisionRole creates the permanent Role, or refreshes its rules in place
// when it already exists so an aicr upgrade that changes the rule set takes
// effect on a re-run instead of leaving stale rules behind.
func provisionRole(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    provisionedLabels(),
		},
		Rules: namespacedRules(),
	}
	_, err := clientset.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		if _, err = clientset.RbacV1().Roles(namespace).Update(ctx, role, metav1.UpdateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update the permanent Role", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create the permanent Role", err)
	}
	return nil
}

// provisionRoleBinding creates or refreshes the permanent RoleBinding.
// It needs no subject-collision guard: the name is a pure function of the
// ServiceAccount name within one namespace, so it can only ever refer to
// the ServiceAccount it is being written for.
func provisionRoleBinding(ctx context.Context, clientset kubernetes.Interface, namespace, name string, subjects []rbacv1.Subject) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    provisionedLabels(),
		},
		Subjects: subjects,
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     kindRole,
			Name:     name,
		},
	}
	_, err := clientset.RbacV1().RoleBindings(namespace).Create(ctx, rb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		if _, err = clientset.RbacV1().RoleBindings(namespace).Update(ctx, rb, metav1.UpdateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update the permanent RoleBinding", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create the permanent RoleBinding", err)
	}
	return nil
}

// provisionClusterRole creates or refreshes the permanent ClusterRole.
func provisionClusterRole(ctx context.Context, clientset kubernetes.Interface, name string, discoverNetwork bool) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: provisionedLabels(),
		},
		Rules: clusterRules(discoverNetwork),
	}
	_, err := clientset.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		if _, err = clientset.RbacV1().ClusterRoles().Update(ctx, cr, metav1.UpdateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update the permanent ClusterRole", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create the permanent ClusterRole", err)
	}
	return nil
}

// provisionClusterRoleBinding creates or refreshes the permanent
// ClusterRoleBinding.
//
// Before updating an existing one it checks the subject: because the
// cluster-scoped name joins namespace and ServiceAccount with "-", two
// distinct pairs can compose the same name (see
// provisionedClusterRoleName). Overwriting a binding that names a different
// ServiceAccount would silently revoke that ServiceAccount's cluster grants,
// so refuse with ErrCodeConflict — the request is well formed, the cluster
// state is what makes it unserviceable.
func provisionClusterRoleBinding(ctx context.Context, clientset kubernetes.Interface, name string, subjects []rbacv1.Subject) error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: provisionedLabels(),
		},
		Subjects: subjects,
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacAPIGroup,
			Kind:     kindClusterRole,
			Name:     name,
		},
	}
	_, err := clientset.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := clientset.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to read the existing ClusterRoleBinding", getErr)
		}
		if other := conflictingSubject(existing.Subjects, subjects[0]); other != "" {
			return errors.NewWithContext(errors.ErrCodeConflict,
				fmt.Sprintf("ClusterRoleBinding %q already grants these permissions to %s; updating it would revoke them. Rename one of the two ServiceAccounts or namespaces so the generated names differ",
					name, other),
				map[string]any{ctxKeyResolvedName: name, "existingSubject": other})
		}
		if _, err = clientset.RbacV1().ClusterRoleBindings().Update(ctx, crb, metav1.UpdateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update the permanent ClusterRoleBinding", err)
		}
		return nil
	}
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create the permanent ClusterRoleBinding", err)
	}
	return nil
}

// conflictingSubject returns a description of the first subject in existing
// that is not want, or "" when every subject is want (the idempotent
// re-provisioning case) or existing is empty.
func conflictingSubject(existing []rbacv1.Subject, want rbacv1.Subject) string {
	for _, s := range existing {
		if s.Kind == want.Kind && s.Name == want.Name && s.Namespace == want.Namespace {
			continue
		}
		return fmt.Sprintf("%s %q in namespace %q", s.Kind, s.Name, s.Namespace)
	}
	return ""
}
