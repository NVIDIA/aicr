// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Deploy deploys the agent with all required resources (RBAC + Job).
// This is the main entry point that orchestrates the deployment.
func (d *Deployer) Deploy(ctx context.Context) error {
	// Step 0: Check permissions before attempting deployment
	_, err := d.CheckPermissions(ctx)
	if err != nil {
		if aicrerrors.IsNetworkError(err) {
			return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable,
				"cannot reach Kubernetes API server\n\nCheck your network connectivity:\n  - Is your VPN connected?\n  - Is the cluster endpoint correct in your kubeconfig?\n  - Are firewall rules allowing egress to the API server?", err)
		}
		return aicrerrors.Wrap(aicrerrors.ErrCodeUnauthorized, "insufficient permissions to deploy agent\n\nTo deploy the agent, you need cluster admin privileges.\nRun: aicr snapshot", err)
	}

	// Step 0.5: Validate RuntimeClass exists if configured
	if d.config.RuntimeClassName != "" {
		if err := d.validateRuntimeClass(ctx); err != nil {
			return err
		}
	}

	// Step 1: Ensure namespace exists
	if err := d.ensureNamespace(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to ensure namespace", err)
	}

	// Step 2: Ensure RBAC resources (idempotent - reuses if already exists)
	if err := d.ensureServiceAccount(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ServiceAccount", err)
	}

	if err := d.ensureRole(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create Role", err)
	}

	if err := d.ensureRoleBinding(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create RoleBinding", err)
	}

	if err := d.ensureClusterRole(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ClusterRole", err)
	}

	if err := d.ensureClusterRoleBinding(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create ClusterRoleBinding", err)
	}

	// Step 2: Ensure Job (delete existing + recreate)
	if err := d.ensureJob(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create Job", err)
	}

	return nil
}

// WaitForCompletion waits for the agent Job to complete successfully.
// Returns error if the Job fails or times out.
func (d *Deployer) WaitForCompletion(ctx context.Context, timeout time.Duration) error {
	return d.waitForJobCompletion(ctx, timeout)
}

// GetSnapshot retrieves the snapshot data from the ConfigMap created by the agent.
// Returns the snapshot YAML content.
func (d *Deployer) GetSnapshot(ctx context.Context) ([]byte, error) {
	return d.getSnapshotFromConfigMap(ctx)
}

// Cleanup removes the agent Job and RBAC resources.
// If opts.Enabled is false, no cleanup is performed (resources are kept for debugging).
// All resources are attempted for deletion even if some fail, and a combined error is returned.
func (d *Deployer) Cleanup(ctx context.Context, opts CleanupOptions) error {
	// Skip cleanup if not enabled (keep resources for debugging)
	if !opts.Enabled {
		return nil
	}

	var errs []string
	var deleted []string

	// Delete the Job
	if err := d.deleteJob(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("Job %q: %v", d.config.JobName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("Job %q", d.config.JobName))
	}

	// Delete RBAC resources - attempt all even if some fail
	if err := d.deleteServiceAccount(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("ServiceAccount %q: %v", d.config.ServiceAccountName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("ServiceAccount %q", d.config.ServiceAccountName))
	}

	if err := d.deleteRole(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("Role %q: %v", d.config.ServiceAccountName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("Role %q", d.config.ServiceAccountName))
	}

	if err := d.deleteRoleBinding(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("RoleBinding %q: %v", d.config.ServiceAccountName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("RoleBinding %q", d.config.ServiceAccountName))
	}

	if err := d.deleteClusterRole(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("ClusterRole %q: %v", clusterRoleName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("ClusterRole %q", clusterRoleName))
	}

	if err := d.deleteClusterRoleBinding(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("ClusterRoleBinding %q: %v", clusterRoleName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("ClusterRoleBinding %q", clusterRoleName))
	}

	// Log successful deletions
	if len(deleted) > 0 {
		slog.Debug("cleanup completed", slog.Int("deleted", len(deleted)), slog.Any("resources", deleted))
	}

	// Return combined error if any deletions failed
	if len(errs) > 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to delete %d resource(s):\n  - %s", len(errs), strings.Join(errs, "\n  - ")))
	}

	return nil
}

// validateRuntimeClass checks that the specified RuntimeClass exists in the cluster.
func (d *Deployer) validateRuntimeClass(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.RuntimeClassCheckTimeout)
	defer cancel()

	_, err := d.clientset.NodeV1().RuntimeClasses().Get(ctx, d.config.RuntimeClassName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return aicrerrors.New(aicrerrors.ErrCodeNotFound,
			fmt.Sprintf("RuntimeClass %q not found in cluster; the GPU Operator may not be installed yet.\n\n"+
				"The --runtime-class flag requires a RuntimeClass to be registered in the cluster.\n"+
				"If GPU Operator is not yet installed, omit --runtime-class and use --node-selector\n"+
				"to target a GPU node instead.", d.config.RuntimeClassName))
	}
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
			fmt.Sprintf("failed to check RuntimeClass %q", d.config.RuntimeClassName), err)
	}

	slog.Debug("RuntimeClass validated", slog.String("runtimeClass", d.config.RuntimeClassName))
	return nil
}
