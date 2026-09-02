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
	"github.com/NVIDIA/aicr/pkg/k8s/labels"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Deploy deploys the agent with all required resources (RBAC + Job).
// This is the main entry point that orchestrates the deployment.
func (d *Deployer) Deploy(ctx context.Context) error {
	// Pre-flight, ahead of any cluster call: reject a run ID — or a
	// caller-supplied name prefix — that cannot be folded into a valid
	// object name. Every object created below is named "<prefix>-<RunID>",
	// so validating here is what keeps an invalid value from surfacing as
	// an apiserver "Invalid value: metadata.name" partway through the
	// ensure* chain, with some objects already created.
	if err := d.validateNames(); err != nil {
		return err
	}

	// Step 0: the authoritative permission gate for the whole run. It
	// verifies every permission this run will exercise — for the caller AND
	// for the ServiceAccount the agent pod runs as — and resolves which
	// ServiceAccount mode the run is in, which is what decides the verb set
	// it demands. Everything it does is a read, so nothing below has been
	// written yet when it fails.
	if _, err := d.CheckPermissions(ctx); err != nil {
		if aicrerrors.IsNetworkError(err) {
			return aicrerrors.Wrap(aicrerrors.ErrCodeUnavailable,
				"cannot reach Kubernetes API server\n\nCheck your network connectivity:\n  - Is your VPN connected?\n  - Is the cluster endpoint correct in your kubeconfig?\n  - Are firewall rules allowing egress to the API server?", err)
		}
		// Propagated as-is: CheckPermissions already returns
		// ErrCodeUnauthorized carrying the complete list of what is
		// missing, for which subject, and how to fix it. Re-wrapping would
		// bury that behind a generic sentence.
		return err
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

	// ServiceAccount mode is already decided at this point: Step 0's gate
	// resolves it (resolveServiceAccount) because the permissions it must
	// demand differ between the two modes. Resolving there rather than here
	// also means the decision is made before ensureNamespace, which is
	// harmless — a ServiceAccount cannot exist in a namespace that does
	// not, so the Get correctly reports NotFound and the run stays in
	// prefix mode.

	// Step 2: Create this run's RBAC. Every name carries the run ID, so
	// nothing here can already exist; an AlreadyExists is reported as an
	// error rather than adopted or overwritten.
	//
	// Skipped entirely in exact-ServiceAccount mode: aicr will not add or
	// remove permissions on a ServiceAccount it did not create. Nothing is
	// created, so nothing enters the created-set, so Cleanup deletes none
	// of these kinds — the operator's grants outlive the run. Generate its
	// RBAC manifests with BuildServiceAccountRoleManifests and apply them
	// out of band.
	if d.managesRBAC() {
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
	// one shared object. Sweep it here.
	//
	// The name alone does not license that delete: Config.RunID is caller-
	// settable, so a second run reusing a RunID resolves the same staging
	// name as the first. stagingSweepJob therefore requires this run to hold
	// a CONFIRMED Job entry — the only way this run could have produced a
	// staging ConfigMap at all — stillHoldsJob requires that Job to still be
	// the live one at its name, and deleteUnrecordedStagingConfigMap
	// re-checks the object it finds before deleting it.
	//
	// stillHoldsJob is evaluated HERE, before the fan-out, and deliberately
	// not inside the sweep task: the created-set delete of this run's own Job
	// runs concurrently with the sweep, so a check made inside the task would
	// race its sibling and see the Job already gone.
	if job, sweep := stagingSweepJob(created); d.config.OwnsOutputConfigMap && sweep && d.stillHoldsJob(ctx, job) {
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
//
// An unconfirmed entry carries no UID (no Create response ever named the
// object), so its ownership is re-established first — resolveIntentUID either
// returns the live object's UID after proving the object carries this run's
// labels, or reports that there is nothing this run may delete.
func (d *Deployer) deleteCreatedObject(ctx context.Context, obj createdObject) error {
	if !obj.confirmed {
		uid, ours, err := d.resolveIntentUID(ctx, obj)
		if err != nil || !ours {
			return err
		}
		obj.uid = uid
	}

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

// resolveIntentUID re-establishes ownership of an unconfirmed created-set
// entry — one recordIntent added whose Create response never arrived — and
// returns the UID to pin its delete to. ours is false when this run may not
// delete the object, in which case the caller must issue no delete at all.
//
// The entry's run-scoped name is not evidence: it says what this run WOULD
// have created, not what is standing there now. Get the live object and
// require it to carry the label set objectLabels() stamps on everything Deploy
// creates — including aicr.run/invocation-id, which is what makes that set
// evidence at all (see createdByThisInvocation). So:
//
//   - Labels match: the Create did commit and this is our object. Delete it,
//     pinned to the UID this Get observed. (If it is replaced again between
//     the Get and the Delete, the precondition turns that into a Conflict,
//     which ignoreNotFoundOrConflict treats as "already gone".)
//   - Labels do not match: whatever holds the name was not created by this
//     invocation — an operator's object, another subsystem's, a replacement
//     made after this run's object was deleted, or another invocation that
//     reused this run's public RunID. Deleting it would collect an object
//     this invocation never created, so fail closed and warn instead.
//   - NotFound: nothing to reclaim.
//
// UID pinning alone would not cover the third case. It protects against a
// replacement made after THIS Get, but the entry carries no UID from creation
// time, so a replacement standing there BEFORE the Get is simply what the Get
// returns — and its UID is what the delete would then be pinned to. Only the
// invocation ID separates "the object I created" from "a same-named,
// same-labeled object another invocation created", which is why the label
// check, not the precondition, is the gate.
func (d *Deployer) resolveIntentUID(ctx context.Context, obj createdObject) (uid types.UID, ours bool, err error) {
	live, err := d.getCreatedObject(ctx, obj.kind, obj.name)
	if k8serrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
			fmt.Sprintf("failed to read %s %q to confirm this run created it before deleting it", obj.kind, obj.name), err)
	}

	// A real apiserver always assigns a UID, so an empty one here means the
	// delete could not be pinned. Refuse rather than fall back to a
	// bare-name delete: that is exactly the blind delete this path exists
	// to prevent.
	if !d.createdByThisInvocation(live.GetLabels()) || live.GetUID() == "" {
		slog.Warn("cleanup left behind an object it cannot prove this run created; if it is a stale orphan of this run, remove it by hand",
			slog.String("kind", obj.kind),
			slog.String(attrName, obj.name),
			slog.String(attrNamespace, live.GetNamespace()),
			slog.String("uid", string(live.GetUID())),
			slog.String(attrRunID, d.config.RunID),
			slog.String("objectRunID", live.GetLabels()[labels.RunID]),
			slog.String("objectInvocationID", live.GetLabels()[labels.InvocationID]))
		return "", false, nil
	}
	return live.GetUID(), true, nil
}

// getCreatedObject reads the live object a created-set entry names. The
// apiserver error is returned unwrapped so callers can classify it with
// k8serrors.IsNotFound before wrapping.
func (d *Deployer) getCreatedObject(ctx context.Context, kind, name string) (metav1.Object, error) {
	ns := d.config.Namespace
	switch kind {
	case kindJob:
		return d.clientset.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	case kindServiceAccount:
		return d.clientset.CoreV1().ServiceAccounts(ns).Get(ctx, name, metav1.GetOptions{})
	case kindRole:
		return d.clientset.RbacV1().Roles(ns).Get(ctx, name, metav1.GetOptions{})
	case kindRoleBinding:
		return d.clientset.RbacV1().RoleBindings(ns).Get(ctx, name, metav1.GetOptions{})
	case kindClusterRole:
		return d.clientset.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{})
	case kindClusterRoleBinding:
		return d.clientset.RbacV1().ClusterRoleBindings().Get(ctx, name, metav1.GetOptions{})
	case kindConfigMap:
		return d.clientset.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	default:
		return nil, aicrerrors.New(aicrerrors.ErrCodeInternal, fmt.Sprintf("cleanup: cannot read unknown created-object kind %q", kind))
	}
}

// createdByThisInvocation reports whether objLabels is the label set
// objectLabels() stamps on every object THIS Deployer creates.
//
// labels.InvocationID is the load-bearing key, and the only one of the five
// that answers the question. The other four are reusable by construction: any
// invocation sharing Config.RunID stamps byte-identical
// Name/ManagedBy/Component/RunID values, and sharing a RunID is a designed
// scenario, not misuse — `aicr validate` hands one ID to its live-capture
// agent and its validator Jobs, and e2e/chainsaw runs pin one deliberately.
// Matching on those four would therefore let this run adopt an object a
// different invocation stood up at the same name, which is exactly the delete
// resolveIntentUID exists to refuse. labels.InvocationID is generated per
// Deployer and settable through no Config field, so a same-RunID invocation
// carries a different value there and cannot be mistaken for this one.
//
// The remaining four stay required, because a match on the invocation ID
// alone would be a match on an unvalidated string: they keep the predicate a
// statement about a snapshot-agent object of this run.
//
// Both IDs must be non-empty. An empty Config.RunID must never match a
// label-less object, and an empty invocationID (a Deployer built as a struct
// literal rather than through NewDeployer) has no authorship to prove — in
// both cases "" == "" would otherwise read as agreement.
func (d *Deployer) createdByThisInvocation(objLabels map[string]string) bool {
	if d.config.RunID == "" || d.invocationID == "" {
		return false
	}
	return objLabels[labels.InvocationID] == d.invocationID &&
		objLabels[labels.RunID] == d.config.RunID &&
		objLabels[labels.Name] == labels.ValueAICR &&
		objLabels[labels.ManagedBy] == labels.ValueAICR &&
		objLabels[labels.Component] == labels.ValueSnapshotAgent
}

// uidPreconditions returns the DeleteOptions precondition pinning a delete
// to uid, or nil when uid is the zero UID.
//
// Only a confirmed entry can reach here with the zero UID: a Create response
// named the object — establishing this run's ownership — but carried no UID.
// A real apiserver always assigns one, so that shape belongs to fake
// clientsets in tests; the delete then falls back to the run-scoped name the
// Create response confirmed. Unconfirmed entries never reach here without a
// UID (see resolveIntentUID).
//
// Omitting the precondition entirely is required in that case —
// metav1.Preconditions{UID: &""} is NOT equivalent to no precondition: the
// apiserver compares it against the live object's UID and rejects every
// delete with a Conflict, which ignoreNotFoundOrConflict then swallows as
// success, leaking the object this entry exists to reclaim.
func uidPreconditions(uid types.UID) *metav1.Preconditions {
	if uid == "" {
		return nil
	}
	return &metav1.Preconditions{UID: &uid}
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
