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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	applyrbacv1 "k8s.io/client-go/applyconfigurations/rbac/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ServiceAccountName is the name of the ServiceAccount used by all validator Jobs.
	ServiceAccountName = "aicr-validator"

	// ClusterRoleBindingName is the name of the ClusterRoleBinding that grants
	// cluster-admin to the validator ServiceAccount.
	ClusterRoleBindingName = "aicr-validator"

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
	clusterAdminRole = "cluster-admin"

	// fieldManager is the SSA field manager name for all RBAC resources.
	fieldManager = labels.ValueAICR
)

// EnsureRBAC applies the ServiceAccount and ClusterRoleBinding for validator
// Jobs using server-side apply. Call once per validation run before deploying
// any Jobs.
func EnsureRBAC(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	if err := applyServiceAccount(ctx, clientset, namespace); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to ensure ServiceAccount", err)
	}

	if err := applyClusterRoleBinding(ctx, clientset, namespace); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to ensure ClusterRoleBinding", err)
	}

	slog.Debug("RBAC resources applied",
		"serviceAccount", ServiceAccountName,
		"namespace", namespace,
		"clusterRole", clusterAdminRole)

	return nil
}

// CleanupRBAC removes the ServiceAccount and ClusterRoleBinding.
// Ignores NotFound errors (idempotent). Call once at end of validation run.
//
// When both deletes fail, the returned StructuredError wraps the joined
// underlying errors via stderrors.Join so callers can inspect individual
// failures with errors.Is / errors.As.
func CleanupRBAC(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	var errs []error

	if err := clientset.CoreV1().ServiceAccounts(namespace).Delete(ctx, ServiceAccountName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, errors.Wrap(errors.ErrCodeInternal, "failed to delete ServiceAccount", err))
		}
	}

	if err := clientset.RbacV1().ClusterRoleBindings().Delete(ctx, ClusterRoleBindingName, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, errors.Wrap(errors.ErrCodeInternal, "failed to delete ClusterRoleBinding", err))
		}
	}

	if len(errs) > 0 {
		return errors.WrapWithContext(errors.ErrCodeInternal, "RBAC cleanup failed",
			stderrors.Join(errs...),
			map[string]any{
				"namespace":          namespace,
				"serviceAccount":     ServiceAccountName,
				"clusterRoleBinding": ClusterRoleBindingName,
			})
	}

	slog.Debug("RBAC resources cleaned up",
		"serviceAccount", ServiceAccountName,
		"namespace", namespace)

	return nil
}

func applyServiceAccount(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	sa := applycorev1.ServiceAccount(ServiceAccountName, namespace).
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

func applyClusterRoleBinding(ctx context.Context, clientset kubernetes.Interface, namespace string) error {
	crb := applyrbacv1.ClusterRoleBinding(ClusterRoleBindingName).
		WithLabels(map[string]string{
			labels.Name:      labels.ValueValidator,
			labels.ManagedBy: labels.ValueAICR,
		}).
		WithSubjects(
			applyrbacv1.Subject().
				WithKind("ServiceAccount").
				WithName(ServiceAccountName).
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
