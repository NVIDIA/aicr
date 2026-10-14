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

package job

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyrbacv1 "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/kubernetes"
)

// rbacNamePrefix is the shared prefix for the validator ServiceAccount and
// ClusterRoleBinding. ServiceAccountName and ClusterRoleBindingName suffix
// this with the runID so concurrent validation runs against the same cluster
// (or, for the ClusterRoleBinding, against any cluster they share at all) do
// not delete each other's RBAC during end-of-run cleanup.
const rbacNamePrefix = "aicr-validator"

// ServiceAccountName returns the per-run ServiceAccount name used by the
// validator Jobs deployed for runID. Each `aicr validate` invocation generates
// a unique runID, so the SA created at run start is the same one deleted at
// run end — overlapping runs cannot clobber each other.
func ServiceAccountName(runID string) string {
	return rbacNamePrefix + "-" + runID
}

// ClusterRoleBindingName returns the per-run ClusterRoleBinding name. The CRB
// is cluster-scoped, so name uniqueness across concurrent runs (even on
// different namespaces) is what prevents cross-run cleanup races.
func ClusterRoleBindingName(runID string) string {
	return rbacNamePrefix + "-" + runID
}

const (
	// clusterAdminRole is the built-in Kubernetes ClusterRole bound to validators.
	//
	// Why cluster-admin is safe here:
	//
	// 1. Kubernetes RBAC has built-in privilege escalation prevention (KEP-1850,
	//    k8s.io/apiserver/pkg/authorization/rbac). A user can only create a
	//    ClusterRoleBinding to cluster-admin if they ALREADY have cluster-admin
	//    permissions themselves. The person running `aicr validate` must be a
	//    cluster administrator. This is not a backdoor — it is a reflection of
	//    the permissions the caller already has.
	//
	// 2. Validators need to inspect arbitrary resources across the cluster:
	//    CRDs (DRA, Karpenter, KAI scheduler), custom metrics APIs, discovery
	//    APIs, ResourceSlices, PodGroups, and resources from operators that may
	//    not exist at compile time. A scoped ClusterRole requires enumerating
	//    every resource upfront, which breaks whenever a new validator or CRD is
	//    added. This is the core reason the scoped approach fails in practice.
	//
	// 3. The ServiceAccount is ephemeral — created at the start of a validation
	//    run and deleted at the end. It exists for minutes, not permanently.
	//
	// 4. The validator containers are built and signed by the AICR CI pipeline.
	//    They are not arbitrary user code. The ServiceAccount cannot be used by
	//    other workloads because it lives in the aicr-validation namespace which
	//    is also ephemeral.
	//
	// 5. This matches the pattern used by other cluster validation tools:
	//    Sonobuoy uses cluster-admin for conformance tests, and the Kubernetes
	//    e2e test suite itself requires cluster-admin.
	//
	// Points 1, 3 and 4 describe per-run RBAC only. With an
	// administrator-provisioned ServiceAccount (see VerifyServiceAccount) the
	// administrator grants cluster-admin once, to a long-lived identity in a
	// namespace they own, and the caller need not hold it.
	clusterAdminRole = "cluster-admin"

	// fieldManager is the SSA field manager name for all RBAC resources.
	fieldManager = labels.ValueAICR

	// serviceAccountUserPrefix is the username the apiserver authenticates a
	// ServiceAccount as, and therefore the SubjectAccessReview subject that
	// answers for the validator pod rather than for the caller.
	serviceAccountUserPrefix = "system:serviceaccount:"

	// Virtual groups every ServiceAccount belongs to. Bindings may target
	// them instead of the individual ServiceAccount, so a review that omits
	// them under-reports what the validator pod can do.
	groupServiceAccounts       = "system:serviceaccounts"
	groupServiceAccountsPrefix = "system:serviceaccounts:"
	groupAuthenticated         = "system:authenticated"
)

// EnsureRBAC applies the ServiceAccount and ClusterRoleBinding for validator
// Jobs using server-side apply. Call once per validation run before deploying
// any Jobs. The runID scopes the resource names so overlapping runs do not
// clobber each other.
func EnsureRBAC(ctx context.Context, clientset kubernetes.Interface, namespace, runID string) error {
	saName := ServiceAccountName(runID)
	crbName := ClusterRoleBindingName(runID)

	if err := applyServiceAccount(ctx, clientset, namespace, saName); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to ensure ServiceAccount", err)
	}

	if err := applyClusterRoleBinding(ctx, clientset, namespace, saName, crbName); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to ensure ClusterRoleBinding", err)
	}

	slog.Debug("RBAC resources applied",
		"serviceAccount", saName,
		"namespace", namespace,
		"clusterRole", clusterAdminRole)

	return nil
}

// CleanupRBAC removes the per-run ClusterRoleBinding and ServiceAccount.
// Ignores NotFound errors (idempotent). Call once at end of validation run.
//
// The ClusterRoleBinding is deleted BEFORE the ServiceAccount: revoking the
// binding immediately closes the cluster-admin escalation window, whereas an
// orphaned ServiceAccount with no binding carries no privileges. If the binding
// delete fails, the ServiceAccount delete still runs so we do not leave both
// behind.
//
// When both deletes fail, the returned StructuredError wraps the joined
// underlying errors via stderrors.Join so callers can inspect individual
// failures with errors.Is / errors.As.
func CleanupRBAC(ctx context.Context, clientset kubernetes.Interface, namespace, runID string) error {
	saName := ServiceAccountName(runID)
	crbName := ClusterRoleBindingName(runID)

	var errs []error

	if err := clientset.RbacV1().ClusterRoleBindings().Delete(ctx, crbName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, errors.Wrap(errors.ErrCodeInternal, "failed to delete ClusterRoleBinding", err))
		}
	}

	if err := clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, saName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, errors.Wrap(errors.ErrCodeInternal, "failed to delete ServiceAccount", err))
		}
	}

	if len(errs) > 0 {
		return errors.WrapWithContext(errors.ErrCodeInternal, "RBAC cleanup failed",
			stderrors.Join(errs...),
			map[string]any{
				"namespace":          namespace,
				"serviceAccount":     saName,
				"clusterRoleBinding": crbName,
			})
	}

	slog.Debug("RBAC resources cleaned up",
		"serviceAccount", saName,
		"namespace", namespace)

	return nil
}

// VerifyServiceAccount vets an administrator-provisioned ServiceAccount that
// validator Jobs will run as in place of the per-run one EnsureRBAC creates.
// It issues only reads: a Get, and a SubjectAccessReview asking whether the
// ServiceAccount may perform any verb on any resource cluster-wide, which is
// what a binding to cluster-admin grants (see clusterAdminRole for why
// validators need it).
//
// The ServiceAccount must also leave token automount enabled.
//
// Every outcome other than "exists and allowed" fails closed, including a
// caller that may not create SubjectAccessReviews. A validator denied access
// does not always fail: a check asserting that something is refused can read
// its own Forbidden as the refusal it expected. Running under an unverified
// identity would trade a clear error here for a result nobody can trust.
func VerifyServiceAccount(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.K8sServiceAccountVerifyTimeout)
	defer cancel()

	sa, err := clientset.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("ServiceAccount %q does not exist in namespace %q. aicr does not create it in this mode, "+
				"because that would grant cluster-admin on the caller's behalf: create it and bind it to the "+
				"cluster-admin ClusterRole, or omit the validator ServiceAccount name to use per-run RBAC", name, namespace),
			err)
	case apierrors.IsForbidden(err):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot read ServiceAccount %q in namespace %q: grant the caller 'get serviceaccounts' in that namespace",
				name, namespace),
			err)
	case err != nil:
		return errors.Wrap(apiErrorCode(ctx, err), fmt.Sprintf("failed to read ServiceAccount %q in namespace %q", name, namespace), err)
	}
	// The validator pod spec leaves token automount to the ServiceAccount, so
	// a ServiceAccount that disables it yields pods with no credential to
	// reach the apiserver at all.
	if sa.AutomountServiceAccountToken != nil && !*sa.AutomountServiceAccountToken {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("ServiceAccount %q in namespace %q sets automountServiceAccountToken: false, so validator pods "+
				"would get no token to reach the API server; remove the field or set it to true", name, namespace))
	}

	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   serviceAccountUserPrefix + namespace + ":" + name,
			Groups: []string{groupServiceAccounts, groupServiceAccountsPrefix + namespace, groupAuthenticated},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     rbacv1.VerbAll,
				Group:    rbacv1.APIGroupAll,
				Resource: rbacv1.ResourceAll,
			},
		},
	}
	result, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	switch {
	case apierrors.IsForbidden(err):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot verify that ServiceAccount %q in namespace %q is bound to cluster-admin: the caller may "+
				"not create SubjectAccessReviews. Grant the caller 'create subjectaccessreviews' "+
				"(authorization.k8s.io, a read-only authorization query); aicr will not run validators under an "+
				"unverified identity", name, namespace),
			err)
	case err != nil:
		return errors.Wrap(apiErrorCode(ctx, err),
			fmt.Sprintf("failed to review the permissions of ServiceAccount %q in namespace %q", name, namespace), err)
	}
	if !result.Status.Allowed {
		msg := fmt.Sprintf("ServiceAccount %q in namespace %q is not granted \"*\" for every verb, API group and resource "+
			"cluster-wide, which validators require. Bind it with: "+
			"kubectl create clusterrolebinding <name> --clusterrole=%s --serviceaccount=%s:%s",
			name, namespace, clusterAdminRole, namespace, name)
		if result.Status.Reason != "" {
			msg += " (apiserver: " + result.Status.Reason + ")"
		}
		// An authorizer that errors also answers Allowed=false, often with
		// no Reason; without this the message would blame a missing binding.
		if result.Status.EvaluationError != "" {
			msg += " (authorizer error: " + result.Status.EvaluationError + ")"
		}
		return errors.New(errors.ErrCodeUnauthorized, msg)
	}

	slog.Debug("validator ServiceAccount verified",
		"serviceAccount", name,
		"namespace", namespace)
	return nil
}

// apiErrorCode classifies an unexpected apiserver error: a dead context is a
// cancellation or a timeout, rejected credentials are unauthorized, a network
// failure is retryable, and anything else is internal.
func apiErrorCode(ctx context.Context, err error) errors.ErrorCode {
	switch {
	case stderrors.Is(ctx.Err(), context.Canceled):
		return errors.ErrCodeCanceled
	case ctx.Err() != nil:
		return errors.ErrCodeTimeout
	case apierrors.IsUnauthorized(err):
		return errors.ErrCodeUnauthorized
	case errors.IsNetworkError(err):
		return errors.ErrCodeUnavailable
	default:
		return errors.ErrCodeInternal
	}
}

func applyServiceAccount(ctx context.Context, clientset kubernetes.Interface, namespace, saName string) error {
	sa := applycorev1.ServiceAccount(saName, namespace).
		WithLabels(map[string]string{
			labels.Name:      labels.ValueValidator,
			labels.ManagedBy: labels.ValueAICR,
		})

	_, err := clientset.CoreV1().ServiceAccounts(namespace).Apply(
		ctx, sa, metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply ServiceAccount", err)
	}
	return nil
}

func applyClusterRoleBinding(ctx context.Context, clientset kubernetes.Interface, namespace, saName, crbName string) error {
	crb := applyrbacv1.ClusterRoleBinding(crbName).
		WithLabels(map[string]string{
			labels.Name:      labels.ValueValidator,
			labels.ManagedBy: labels.ValueAICR,
		}).
		WithSubjects(
			applyrbacv1.Subject().
				WithKind("ServiceAccount").
				WithName(saName).
				WithNamespace(namespace),
		).
		WithRoleRef(
			applyrbacv1.RoleRef().
				WithAPIGroup("rbac.authorization.k8s.io").
				WithKind("ClusterRole").
				WithName(clusterAdminRole),
		)

	_, err := clientset.RbacV1().ClusterRoleBindings().Apply(
		ctx, crb, metav1.ApplyOptions{FieldManager: fieldManager, Force: true},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply ClusterRoleBinding", err)
	}
	return nil
}
