// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package nicconfigdaemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Cleanup removes everything Ensure created, except the CRDs. CRDs are left in
// place so any NicDevice CRs that other tools or future runs may rely on are
// not silently destroyed.
//
// Best effort: every delete is attempted even if an earlier one fails (the
// caller typically runs Cleanup from a defer in the discovery path). Errors
// are logged AND joined into the return value so callers — e.g. the discovery
// pre-clean — can react instead of polling a namespace that never started
// deleting. A NotFound on any object is treated as success (nil).
func Cleanup(ctx context.Context, c client.Client) error {
	var errs []error
	if err := deleteNamespace(ctx, c); err != nil {
		log.Log.Error(err, "failed to delete bootstrap namespace", "namespace", Namespace)
		errs = append(errs, err)
	}

	// ClusterRoleBinding and ClusterRole are cluster-scoped, so cascade deletion
	// of the namespace does NOT remove them — delete them explicitly.
	if err := deleteClusterRoleBinding(ctx, c); err != nil {
		log.Log.Error(err, "failed to delete cluster role binding", "name", ClusterRoleBindingName)
		errs = append(errs, err)
	}
	if err := deleteClusterRole(ctx, c); err != nil {
		log.Log.Error(err, "failed to delete cluster role", "name", ClusterRoleName)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// WaitForNamespaceDeleted polls until the bootstrap namespace is fully gone
// (Get returns NotFound) or the timeout fires. Used by the discovery pre-clean
// so a fresh bootstrap is not applied on top of a namespace that is still
// Terminating from a prior run. The bootstrap objects carry no finalizers, so
// deletion is normally fast; a namespace stuck Terminating past the timeout
// returns an error the caller can surface as a warning.
func WaitForNamespaceDeleted(ctx context.Context, c client.Client, timeout time.Duration) error {
	pollCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		pollCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		ns := &corev1.Namespace{}
		err := c.Get(pollCtx, types.NamespacedName{Name: Namespace}, ns)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			log.Log.V(1).Info("error while waiting for namespace deletion; will retry",
				"namespace", Namespace, "error", err.Error())
		}

		select {
		case <-pollCtx.Done():
			return fmt.Errorf("timeout waiting for namespace %s to be deleted", Namespace)
		case <-ticker.C:
		}
	}
}

func deleteNamespace(ctx context.Context, c client.Client) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace}}
	if err := c.Delete(ctx, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete namespace %s: %w", Namespace, err)
	}
	log.Log.Info("Deleted bootstrap namespace", "namespace", Namespace)
	return nil
}

func deleteClusterRole(ctx context.Context, c client.Client) error {
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleName}}
	if err := c.Delete(ctx, cr); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete ClusterRole %s: %w", ClusterRoleName, err)
	}
	log.Log.V(1).Info("Deleted ClusterRole", "name", ClusterRoleName)
	return nil
}

func deleteClusterRoleBinding(ctx context.Context, c client.Client) error {
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleBindingName}}
	if err := c.Delete(ctx, crb); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete ClusterRoleBinding %s: %w", ClusterRoleBindingName, err)
	}
	log.Log.V(1).Info("Deleted ClusterRoleBinding", "name", ClusterRoleBindingName)
	return nil
}
