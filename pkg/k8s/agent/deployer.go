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
	"strings"
	"sync"
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

	// Step 2: Create this run's RBAC. Every name carries the run ID, so
	// nothing here can already exist; an AlreadyExists is reported as an
	// error rather than adopted or overwritten.
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

	// Step 3: Create this run's Job under its run-scoped name.
	if err := d.ensureJob(ctx); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to create Job", err)
	}

	return nil
}

// JobName returns the run-scoped name of the Job this Deployer deploys —
// Config.JobName (or the name base) suffixed with Config.RunID. Callers that
// surface the Job to an operator (log lines, kubectl hints) must use this
// rather than Config.JobName, which is only the optional prefix and is empty
// by default.
func (d *Deployer) JobName() string {
	return d.jobName()
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

// Cleanup removes exactly the objects this Deployer created: the Job, the
// RBAC resources, and — when this Deployer owns the output ConfigMap — the
// staging ConfigMap. If opts.Enabled is false, no cleanup is performed
// (resources are kept for debugging). All resources are attempted for
// deletion even if some fail, and a combined error is returned. Deletions
// are fanned out concurrently so a slow apiserver does not serialize the
// wall clock.
func (d *Deployer) Cleanup(ctx context.Context, opts CleanupOptions) error {
	if !opts.Enabled {
		return nil
	}

	// Build the task list from what this Deployer actually created, not
	// from configured names — a run must never delete an object it did
	// not create (e.g. a same-named object left behind by an unrelated
	// run or user). Each delete is additionally pinned to the recorded
	// UID via metav1.Preconditions.
	created := d.createdSnapshot()

	type result struct {
		label string
		err   error
	}

	type task struct {
		label string
		op    func(context.Context) error
	}

	tasks := make([]task, len(created))
	for i, obj := range created {
		tasks[i].label = fmt.Sprintf("%s %q", obj.kind, obj.name)
		tasks[i].op = func(ctx context.Context) error {
			return d.deleteCreatedObject(ctx, obj)
		}
	}

	// The staging ConfigMap is written by the in-pod agent, so it only
	// enters the created-set when getSnapshotFromConfigMap got far enough
	// to observe its UID. A run that fails after the agent wrote it (Job
	// timeout, wait error, canceled context) would otherwise leak it — and
	// with run-scoped naming that is one leaked object per failed run, not
	// one shared object. Sweep it here: the name is run-unique, and the
	// delete is still UID-pinned against the UID observed by the Get.
	if d.config.OwnsOutputConfigMap && !d.hasCreated(kindConfigMap) {
		name := d.stagingConfigMapName()
		tasks = append(tasks, task{
			label: fmt.Sprintf("%s %q", kindConfigMap, name),
			op:    d.deleteUnrecordedStagingConfigMap,
		})
	}

	// sync.WaitGroup (not errgroup) is intentional here: cleanup must
	// attempt every delete even if earlier ones fail, AND surface every
	// failure in the combined error message below. errgroup.WithContext
	// would cancel siblings on first error; plain errgroup.Group would
	// only surface the first error. The indexed result slice gives us
	// per-task attribution without locking.
	results := make([]result, len(tasks))
	var wg sync.WaitGroup
	for i := range tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = result{label: tasks[i].label, err: tasks[i].op(ctx)}
		}(i)
	}
	wg.Wait()

	var errs []string
	var deleted []string
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", r.label, r.err))
		} else {
			deleted = append(deleted, r.label)
		}
	}

	if len(deleted) > 0 {
		slog.Debug("cleanup completed", slog.Int("deleted", len(deleted)), slog.Any("resources", deleted))
	}

	if len(errs) > 0 {
		return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to delete %d resource(s):\n  - %s", len(errs), strings.Join(errs, "\n  - ")))
	}

	return nil
}

// deleteCreatedObject deletes a single created-set entry by dispatching to
// the resource-specific delete call for obj.kind, passing obj.name and
// obj.uid through so every delete is UID-pinned.
func (d *Deployer) deleteCreatedObject(ctx context.Context, obj createdObject) error {
	switch obj.kind {
	case kindJob:
		return d.deleteJob(ctx, obj.name, obj.uid)
	case kindServiceAccount:
		return d.deleteServiceAccount(ctx, obj.name, obj.uid)
	case kindRole:
		return d.deleteRole(ctx, obj.name, obj.uid)
	case kindRoleBinding:
		return d.deleteRoleBinding(ctx, obj.name, obj.uid)
	case kindClusterRole:
		return d.deleteClusterRole(ctx, obj.name, obj.uid)
	case kindClusterRoleBinding:
		return d.deleteClusterRoleBinding(ctx, obj.name, obj.uid)
	case kindConfigMap:
		return d.deleteStagingConfigMap(ctx, obj.name, obj.uid)
	default:
		return aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf("cleanup: unknown created-object kind %q", obj.kind))
	}
}

// ignoreNotFoundOrConflict returns nil when err is "not found" (already
// deleted) or "conflict" (the UID precondition did not match — some other
// object now holds this name; it has already been replaced and is not
// ours to delete). Both are success from Cleanup's perspective: the object
// this run created is gone.
func ignoreNotFoundOrConflict(err error) error {
	if k8serrors.IsNotFound(err) || k8serrors.IsConflict(err) {
		return nil
	}
	return err
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
