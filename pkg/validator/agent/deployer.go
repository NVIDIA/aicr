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
	"fmt"
	"log/slog"
	"strings"
	"time"

	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
)

// Deploy deploys the validation agent with all required resources (RBAC + Job).
// For multi-phase validation, prefer using EnsureRBAC() once, then DeployJob() per phase.
func (d *Deployer) Deploy(ctx context.Context) error {
	slog.Debug("deploying validation agent",
		"namespace", d.config.Namespace,
		"job", d.config.JobName)

	// Create RBAC
	if err := d.EnsureRBAC(ctx); err != nil {
		return err
	}

	// Deploy Job
	if err := d.DeployJob(ctx); err != nil {
		return err
	}

	slog.Info("validation agent deployed successfully",
		"job", d.config.JobName,
		"namespace", d.config.Namespace)

	return nil
}

// EnsureRBAC creates RBAC resources (ServiceAccount, Role, RoleBinding).
// This is idempotent - safe to call multiple times, reuses existing resources.
// For multi-phase validation, call this once before running multiple Jobs.
func (d *Deployer) EnsureRBAC(ctx context.Context) error {
	slog.Debug("creating RBAC resources",
		"namespace", d.config.Namespace,
		"serviceAccount", d.config.ServiceAccountName)

	if err := d.ensureServiceAccount(ctx); err != nil {
		slog.Error("Failed to create ServiceAccount", "error", err)
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create ServiceAccount", err)
	}

	if err := d.ensureRole(ctx); err != nil {
		slog.Error("Failed to create Role", "error", err)
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create Role", err)
	}

	if err := d.ensureRoleBinding(ctx); err != nil {
		slog.Error("Failed to create RoleBinding", "error", err)
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create RoleBinding", err)
	}

	// Create ClusterRole for cluster-wide read access (needed for deployment phase)
	if err := d.ensureClusterRole(ctx); err != nil {
		slog.Error("Failed to create ClusterRole", "error", err)
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create ClusterRole", err)
	}

	if err := d.ensureClusterRoleBinding(ctx); err != nil {
		slog.Error("Failed to create ClusterRoleBinding", "error", err)
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create ClusterRoleBinding", err)
	}

	slog.Debug("RBAC resources created",
		"serviceAccount", d.config.ServiceAccountName)

	return nil
}

// DeployJob deploys the validation Job.
// Assumes RBAC resources already exist (call EnsureRBAC first).
// This deletes any existing Job with the same name before creating a new one.
func (d *Deployer) DeployJob(ctx context.Context) error {
	slog.Debug("deploying validation Job",
		"namespace", d.config.Namespace,
		"job", d.config.JobName)

	// Ensure ConfigMaps for snapshot and recipe inputs
	if err := d.ensureInputConfigMaps(ctx); err != nil {
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to verify input ConfigMaps", err)
	}

	// Ensure Job (delete existing + recreate)
	if err := d.ensureJob(ctx); err != nil {
		return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to create Job", err)
	}

	slog.Debug("validation Job deployed",
		"job", d.config.JobName)

	return nil
}

// WaitForCompletion waits for the validation Job to complete successfully.
func (d *Deployer) WaitForCompletion(ctx context.Context, timeout time.Duration) error {
	slog.Debug("waiting for validation Job completion",
		"job", d.config.JobName,
		"timeout", timeout)

	return d.waitForJobCompletion(ctx, timeout)
}

// GetResult retrieves the validation result from the Job's pod logs.
func (d *Deployer) GetResult(ctx context.Context) (*ValidationResult, error) {
	slog.Debug("retrieving validation result",
		"job", d.config.JobName,
		"namespace", d.config.Namespace)

	return d.getResultFromJobLogs(ctx)
}

// Cleanup removes the validation Job and RBAC resources.
// For multi-phase validation, prefer CleanupJob() per phase, then CleanupRBAC() once at end.
func (d *Deployer) Cleanup(ctx context.Context, opts CleanupOptions) error {
	if !opts.Enabled {
		slog.Debug("cleanup disabled, keeping resources",
			"job", d.config.JobName)
		return nil
	}

	slog.Debug("cleaning up validation agent resources",
		"job", d.config.JobName,
		"namespace", d.config.Namespace)

	var errs []error

	// Cleanup Job
	if err := d.CleanupJob(ctx); err != nil {
		errs = append(errs, err)
	}

	// Cleanup RBAC
	if err := d.CleanupRBAC(ctx); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return eidoserrors.New(eidoserrors.ErrCodeInternal,
			fmt.Sprintf("cleanup failed with %d error(s)", len(errs)))
	}

	return nil
}

// CleanupJob removes the validation Job.
// Use this after each phase in multi-phase validation to clean up per-phase Jobs.
func (d *Deployer) CleanupJob(ctx context.Context) error {
	slog.Debug("cleaning up validation Job",
		"job", d.config.JobName,
		"namespace", d.config.Namespace)

	var errs []string
	var deleted []string

	// Delete the Job
	if err := d.deleteJob(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("Job %q: %v", d.config.JobName, err))
	} else {
		deleted = append(deleted, fmt.Sprintf("Job %q", d.config.JobName))
	}

	// Log successful deletions
	if len(deleted) > 0 {
		slog.Debug("Job cleanup completed",
			"deleted", len(deleted),
			"resources", deleted)
	}

	// Return combined error if any deletions failed
	if len(errs) > 0 {
		return eidoserrors.New(eidoserrors.ErrCodeInternal,
			fmt.Sprintf("failed to delete %d resource(s):\n  - %s",
				len(errs), strings.Join(errs, "\n  - ")))
	}

	return nil
}

// CleanupRBAC removes RBAC resources (ServiceAccount, Role, RoleBinding).
// Use this once at the end of multi-phase validation after all Jobs are done.
func (d *Deployer) CleanupRBAC(ctx context.Context) error {
	slog.Debug("cleaning up RBAC resources",
		"namespace", d.config.Namespace,
		"serviceAccount", d.config.ServiceAccountName)

	var errs []string
	var deleted []string

	// Delete RBAC resources
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

	// Delete ClusterRole resources
	clusterRoleName := d.config.ServiceAccountName + "-cluster"
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
		slog.Debug("RBAC cleanup completed",
			"deleted", len(deleted),
			"resources", deleted)
	}

	// Return combined error if any deletions failed
	if len(errs) > 0 {
		return eidoserrors.New(eidoserrors.ErrCodeInternal,
			fmt.Sprintf("failed to delete %d resource(s):\n  - %s",
				len(errs), strings.Join(errs, "\n  - ")))
	}

	return nil
}

// StreamLogs streams logs from the validation Job pod to the provided writer.
func (d *Deployer) StreamLogs(ctx context.Context) error {
	return d.streamPodLogs(ctx)
}

// GetPodLogs retrieves all pod logs as a string.
// This is useful for capturing logs when a Job fails for debugging.
func (d *Deployer) GetPodLogs(ctx context.Context) (string, error) {
	return d.getPodLogsAsString(ctx)
}
