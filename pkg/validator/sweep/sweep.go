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

package sweep

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/labels"
)

// Options configures a sweep. The zero value selects the validation namespace
// and rejects: an age floor is mandatory, so OlderThan must be set explicitly
// (the facade defaults it to defaults.ValidationSweepMinAge).
type Options struct {
	// Namespace is the validation namespace run-scoped objects live in.
	// Empty selects defaults.ValidationNamespace.
	Namespace string

	// OlderThan is the minimum age an object must reach before the sweeper
	// will delete it. Must be positive; defaults.ValidationSweepMinAge is the
	// value every caller should have a reason to override.
	OlderThan time.Duration
}

// normalized applies defaults and validates. A non-positive OlderThan is
// rejected rather than defaulted: the age floor is the backstop that keeps
// the sweeper from racing a live run, so it is never silently weakened.
func (o Options) normalized() (Options, error) {
	if o.Namespace == "" {
		o.Namespace = defaults.ValidationNamespace
	}
	if o.OlderThan <= 0 {
		return o, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("OlderThan must be positive, got %v", o.OlderThan))
	}
	return o, nil
}

// Status is what the sweeper did or would do with one object.
type Status string

const (
	// StatusWouldDelete marks a planned candidate (Plan, or a dry run).
	StatusWouldDelete Status = "would-delete"
	// StatusDeleted marks an object whose delete the API server accepted, or
	// which was already gone (idempotent re-run).
	StatusDeleted Status = "deleted"
	// StatusDeleting marks a namespace whose delete was accepted; the
	// sweeper never waits for termination.
	StatusDeleting Status = "deleting"
	// StatusSkipped marks an object a guard kept, with the guard named in
	// Reason.
	StatusSkipped Status = "skipped"
	// StatusTerminating marks an object whose deletion already started.
	StatusTerminating Status = "terminating"
	// StatusFailed marks an object whose delete (or delete-time guard)
	// failed.
	StatusFailed Status = "failed"
)

// Object is one cluster object the sweeper examined.
type Object struct {
	Kind      string
	Namespace string // empty for cluster-scoped kinds
	Name      string
	UID       types.UID
	Age       time.Duration
	RunID     string // the owning run, when the object carries one
	Status    Status
	Reason    string
}

// Report is the outcome of a sweep plan or apply: every object examined, in
// deletion order, with what was (or would be) done with it.
type Report struct {
	Namespace string
	OlderThan time.Duration
	Objects   []Object
}

// Candidates returns the objects marked StatusWouldDelete — the exact set
// Execute will attempt.
func (r *Report) Candidates() []Object {
	var out []Object
	for _, o := range r.Objects {
		if o.Status == StatusWouldDelete {
			out = append(out, o)
		}
	}
	return out
}

// Plan lists the stale validation resources in scope without deleting
// anything. Every read is bounded by defaults.K8sCleanupTimeout per call.
//
// Errors:
//   - ErrCodeInvalidRequest when opts are invalid (non-positive OlderThan).
//   - ErrCodeUnauthorized when a list is Forbidden.
//   - ErrCodeTimeout/ErrCodeCanceled when ctx dies.
//   - ErrCodeInternal for any other API failure.
func Plan(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, opts Options) (*Report, error) {
	o, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, "sweep plan interrupted before it started")
	}

	r := &Report{Namespace: o.Namespace, OlderThan: o.OlderThan}

	live, err := liveRunIDs(ctx, kube)
	if err != nil {
		return nil, err
	}

	// Check namespaces first: the accepted set also scopes the Run:ai
	// residue pass, which must leave their contents to the namespace delete.
	namespaces, accepted, err := planCheckNamespaces(ctx, kube, o)
	if err != nil {
		return nil, err
	}
	r.Objects = append(r.Objects, namespaces...)

	for _, family := range []func(context.Context, kubernetes.Interface, Options, map[string]bool) ([]Object, error){
		planValidatorRBAC,
		planDataConfigMaps,
		planAgentObjects,
	} {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, "sweep plan interrupted")
		}
		objects, familyErr := family(ctx, kube, o, live)
		if familyErr != nil {
			return nil, familyErr
		}
		r.Objects = append(r.Objects, objects...)
	}

	residue, err := planRunAIResidue(ctx, kube, dyn, o, accepted)
	if err != nil {
		return nil, err
	}
	r.Objects = append(r.Objects, residue...)

	sort.Slice(r.Objects, func(i, j int) bool {
		a, b := r.Objects[i], r.Objects[j]
		if ra, rb := deletionRank(a.Kind), deletionRank(b.Kind); ra != rb {
			return ra < rb
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return r, nil
}

// Execute deletes the plan's candidates, but only those a fresh Plan still
// marks would-delete: the caller's report is a claim, never proof. Execute
// re-plans against the cluster's current state with the plan's namespace and
// age floor, and any candidate the re-plan no longer selects (a namespace
// gained a live pod, a run gained a Job, an object was replaced, or the
// report was hand-built or tampered with) is marked StatusSkipped instead of
// deleted. Objects the caller's plan did not mark StatusWouldDelete are
// never deleted.
//
// Job liveness is a point-in-time signal — with cleanup on, a live run shows
// no Job at all while it waits between checks — so when run-scoped
// candidates remain, Execute then lists the run-labeled Jobs and watches
// from the list's ResourceVersion for defaults.ValidationSweepSettleWindow
// before deleting any of them; the window is a conservative margin over the
// usual between-checks gap, not a proven bound, and a run whose Job appears
// during it keeps all of its objects. The observation fails closed: a watch
// the apiserver ends before the window fires (an anomaly — apiserver watch
// timeouts are far longer) aborts the apply with ErrCodeUnavailable and
// nothing is deleted. Because the wait widens the gap between the re-plan's
// reads and the deletes to the whole window, a second re-plan runs after it
// and re-applies every guard against current state; an object it no longer
// selects is skipped with the fresh guard's reason.
//
// Every delete carries metav1.Preconditions{UID} so an object recreated
// since planning is left alone, and background propagation so dependent
// garbage collection is the apiserver's, not ours.
//
// A delete answered NotFound (already gone) is not a failure, which makes a
// re-run idempotent. A delete refused with a Conflict does not prove the
// object was replaced (admission and storage can conflict too), so it is
// reported skipped and left alone — re-run to retry.
//
// The plan is mutated in place: each candidate's Status becomes
// StatusDeleted, StatusDeleting (namespaces, whose termination is never
// awaited), StatusSkipped (a guard now keeps it), or StatusFailed.
//
// Errors: the re-plans' errors propagate (including ErrCodeInvalidRequest
// for a non-positive age floor and ErrCodeUnauthorized for a forbidden
// list), as do a failed liveness read or watch during the settle window and
// the ErrCodeUnavailable of a settle watch the apiserver ended early — all
// abort before anything is deleted. Partial delete failures are joined
// and wrapped in ErrCodeInternal naming the failed objects; a delete that is
// Forbidden is additionally reported per object with ErrCodeUnauthorized.
func Execute(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, plan *Report) error {
	if plan == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "no sweep plan to execute")
	}
	if err := ctx.Err(); err != nil {
		return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "sweep interrupted before it started")
	}

	if err := intersectFreshPlan(ctx, kube, dyn, plan); err != nil {
		return err
	}

	// Give a live run the settle window to reveal itself before any
	// run-scoped object is deleted. When the observation ran, the window sat
	// between the re-plan's reads and the deletes, so re-plan once more: a
	// namespace that gained a live pod, or a run that gained a Job just after
	// the window, is caught before anything is removed.
	waited, err := settleQuiet(ctx, kube, plan)
	if err != nil {
		return err
	}
	if waited {
		if err := intersectFreshPlan(ctx, kube, dyn, plan); err != nil {
			return err
		}
	}

	var errs []error
	for i := range plan.Objects {
		obj := &plan.Objects[i]
		if obj.Status != StatusWouldDelete {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout,
				fmt.Sprintf("sweep interrupted before deleting %s %s", obj.Kind, obj.Name))
		}

		if err := deleteObject(ctx, kube, dyn, obj); err != nil {
			switch {
			case apierrors.IsNotFound(err):
				// Already gone — the outcome a delete exists to reach.
				obj.Status = StatusDeleted
				obj.Reason = "already gone"
			case apierrors.IsConflict(err):
				// A conflicted delete usually means the UID precondition
				// did not match, but admission and storage can conflict
				// too — so this proves nothing about the object.
				obj.Status = StatusSkipped
				obj.Reason = "delete refused with a conflict (the object may have been replaced or modified); left alone — re-run to retry"
			default:
				obj.Status = StatusFailed
				obj.Reason = err.Error()
				errs = append(errs, classifyDeleteError(ctx, obj, err))
			}
			continue
		}
		if obj.Kind == kindNamespace {
			obj.Status = StatusDeleting
			obj.Reason = "deletion accepted; termination is not awaited"
		} else {
			obj.Status = StatusDeleted
		}
	}

	// An interruption that lands while the LAST candidate's delete is in
	// flight is classified into errs by classifyDeleteError; report the
	// interruption itself rather than the partial-failure Internal.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, "sweep interrupted while deleting")
	}

	if len(errs) > 0 {
		names := make([]string, 0, len(errs))
		for _, o := range plan.Objects {
			if o.Status == StatusFailed {
				names = append(names, fmt.Sprintf("%s %s/%s", o.Kind, o.Namespace, o.Name))
			}
		}
		return errors.WrapWithContext(errors.ErrCodeInternal,
			fmt.Sprintf("sweep failed to delete %d object(s): %v", len(errs), names),
			stderrors.Join(errs...),
			map[string]any{"namespace": plan.Namespace, "failures": len(errs)})
	}
	return nil
}

// objectKey identifies one planned incarnation of an object for the
// apply-time re-plan intersection: identity plus UID, so an object replaced
// since planning is not recognized.
type objectKey struct {
	kind, namespace, name string
	uid                   types.UID
}

func keyOf(o Object) objectKey {
	return objectKey{o.Kind, o.Namespace, o.Name, o.UID}
}

// reasonNoLongerCandidate is why an apply-time candidate is kept when the
// fresh plan Execute runs no longer selects it.
const reasonNoLongerCandidate = "no longer a sweep candidate (re-planned at apply)"

// intersectFreshPlan re-plans against the cluster's current state and
// intersects: the fresh plan runs every guard anew, so only candidates it
// still selects (same identity AND same UID) keep StatusWouldDelete. A
// survivor's RunID and Age come from the fresh plan — the caller's report is
// a claim, never proof, so a report with a cleared or altered RunID cannot
// hide a live run from the settle observation. Anything else is kept: with
// the fresh guard's reason when the fresh plan still names the object, or as
// no longer a candidate. A failed re-plan aborts before any delete.
func intersectFreshPlan(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, plan *Report) error {
	fresh, err := Plan(ctx, kube, dyn, Options{Namespace: plan.Namespace, OlderThan: plan.OlderThan})
	if err != nil {
		return err
	}
	freshByKey := make(map[objectKey]Object, len(fresh.Objects))
	for _, o := range fresh.Objects {
		freshByKey[keyOf(o)] = o
	}
	for i := range plan.Objects {
		obj := &plan.Objects[i]
		if obj.Status != StatusWouldDelete {
			continue
		}
		freshObj, known := freshByKey[keyOf(*obj)]
		switch {
		case known && freshObj.Status == StatusWouldDelete:
			obj.RunID = freshObj.RunID
			obj.Age = freshObj.Age
		case known && freshObj.Reason != "":
			// The fresh plan still names the object and says which guard
			// now keeps it.
			obj.Status = StatusSkipped
			obj.Reason = freshObj.Reason
		default:
			obj.Status = StatusSkipped
			obj.Reason = reasonNoLongerCandidate
		}
	}
	return nil
}

// settleWindow governs the settle observation; it is a package-level
// variable so tests can compress the wait, the same seam
// validators/conformance uses for cleanupTimeout.
var settleWindow = defaults.ValidationSweepSettleWindow

// settleQuiet holds the deletion of run-scoped objects while a live run
// could be between its validator Jobs and therefore invisible to the Job
// liveness guard: with cleanup on, the validator deletes each Job right
// after its check, so a quiet cluster proves nothing at one instant. While
// any run-scoped candidate remains, the run-labeled Jobs are listed once and
// then watched from the list's ResourceVersion until the window elapses, so
// a Job that appears only briefly — created and deleted inside the gap by a
// fast check — is still observed; a run whose Job appears keeps every one of
// its candidates. The window is a conservative margin over the usual
// between-checks gap, not a proven bound — a run stalled between checks for
// longer cannot be told from a dead one.
//
// The observation fails closed. An apiserver watch outlasts this window by
// far (the routine --min-request-timeout expiry is 30+ minutes), so a watch
// that ends first — a closed channel, an error event — is an anomaly after
// which quiet was not observed for the whole window: settleQuiet returns an
// error and nothing is deleted. The same holds for a failed list or watch.
//
// waited reports whether the observation ran at all: with no run-scoped
// candidates there is nothing to wait for, and Execute skips the post-settle
// re-plan.
//
// Namespace and Run:ai residue candidates do not gate on this signal —
// their guards are live pods and name shape.
func settleQuiet(ctx context.Context, kube kubernetes.Interface, plan *Report) (waited bool, err error) {
	pending := make(map[string][]*Object) // run ID -> its waiting candidates
	for i := range plan.Objects {
		obj := &plan.Objects[i]
		if obj.Status == StatusWouldDelete && isRunScoped(obj.Kind) {
			pending[obj.RunID] = append(pending[obj.RunID], obj)
		}
	}
	if len(pending) == 0 {
		return false, nil
	}

	slog.Info("sweep observing the settle window before deleting run-scoped objects: "+
		"a live run between its validator Jobs shows none, so quiet must be observed first",
		"window", settleWindow, "namespace", plan.Namespace, "runs", len(pending))
	window := time.NewTimer(settleWindow)
	defer window.Stop()

	jobs, err := listRunJobs(ctx, kube)
	if err != nil {
		return true, err
	}
	for i := range jobs.Items {
		keepRun(pending, jobs.Items[i].Labels[labels.RunID])
	}
	if len(pending) == 0 {
		return true, nil
	}
	return true, watchOnce(ctx, kube, jobs.ResourceVersion, window, pending)
}

// watchOnce opens the settle observation's one Job watch from
// resourceVersion and consumes it with watchRunJobs. The watch context is
// bounded by the window so a hung apiserver cannot hold the sweep past it;
// the timeout classifies as ErrCodeTimeout and nothing is deleted.
func watchOnce(ctx context.Context, kube kubernetes.Interface, resourceVersion string, window *time.Timer, pending map[string][]*Object) error {
	watchCtx, cancel := context.WithTimeout(ctx, settleWindow)
	defer cancel()
	watcher, err := kube.BatchV1().Jobs(metav1.NamespaceAll).Watch(watchCtx, metav1.ListOptions{
		LabelSelector:   runJobSelector,
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return classifyWatchError(ctx, "jobs", err)
	}
	defer watcher.Stop()
	return watchRunJobs(ctx, watcher, window, pending)
}

// watchRunJobs consumes Job events until the window elapses or every pending
// run is accounted for. Any event whose Job carries a pending run's
// aicr.run/run-id label keeps that run; a DELETED event still proves the run
// was alive inside the window. The result channel ending before the window
// fires — a closed channel or an error event — means liveness was not
// observed for the whole window, so it fails closed.
func watchRunJobs(ctx context.Context, watcher watch.Interface, window *time.Timer, pending map[string][]*Object) error {
	for {
		select {
		case <-ctx.Done():
			return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout,
				"sweep interrupted while waiting for validation runs to go quiet")
		case <-window.C:
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return errors.New(errors.ErrCodeUnavailable,
					"the apiserver ended the job watch before the settle window did, "+
						"so liveness was not observed for the whole window; nothing was deleted — re-run")
			}
			if event.Type == watch.Error {
				return classifyWatchError(ctx, "jobs", apierrors.FromObject(event.Object))
			}
			if j, isJob := event.Object.(*batchv1.Job); isJob {
				keepRun(pending, j.Labels[labels.RunID])
			}
			if len(pending) == 0 {
				return nil
			}
		}
	}
}

// keepRun keeps every waiting candidate of runID: a validator Job carrying
// its aicr.run/run-id label was observed during the settle window, so the
// run is live.
func keepRun(pending map[string][]*Object, runID string) {
	if runID == "" {
		return
	}
	objects, waiting := pending[runID]
	if !waiting {
		return
	}
	for _, obj := range objects {
		obj.Status = StatusSkipped
		obj.Reason = fmt.Sprintf("run %s started a validator Job while the sweep waited for it to go quiet", runID)
	}
	delete(pending, runID)
}

// isRunScoped reports whether a kind's liveness guard is the validator-Job
// signal: the families a whole uncapped `aicr validate` can outlive the age
// floor with, unlike namespaces and residue (guarded by live pods and age).
func isRunScoped(kind string) bool {
	switch kind {
	case kindServiceAccount, kindClusterRoleBinding, kindClusterRole,
		kindRole, kindRoleBinding, kindConfigMap:
		return true
	}
	return false
}

// deleteObject issues one UID-pinned, background-propagation delete bounded
// by defaults.K8sCleanupTimeout.
func deleteObject(ctx context.Context, kube kubernetes.Interface, dyn dynamic.Interface, obj *Object) error {
	ctx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()

	opts := metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &obj.UID},
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	}
	switch obj.Kind {
	case kindNamespace:
		return kube.CoreV1().Namespaces().Delete(ctx, obj.Name, opts)
	case kindPod:
		return kube.CoreV1().Pods(obj.Namespace).Delete(ctx, obj.Name, opts)
	case kindPodGroup:
		return dyn.Resource(podGroupGVR).Namespace(obj.Namespace).Delete(ctx, obj.Name, opts)
	case kindClusterRoleBinding:
		return kube.RbacV1().ClusterRoleBindings().Delete(ctx, obj.Name, opts)
	case kindClusterRole:
		return kube.RbacV1().ClusterRoles().Delete(ctx, obj.Name, opts)
	case kindServiceAccount:
		return kube.CoreV1().ServiceAccounts(obj.Namespace).Delete(ctx, obj.Name, opts)
	case kindRole:
		return kube.RbacV1().Roles(obj.Namespace).Delete(ctx, obj.Name, opts)
	case kindRoleBinding:
		return kube.RbacV1().RoleBindings(obj.Namespace).Delete(ctx, obj.Name, opts)
	case kindConfigMap:
		return kube.CoreV1().ConfigMaps(obj.Namespace).Delete(ctx, obj.Name, opts)
	default:
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("sweep bug: no deleter for kind %q", obj.Kind))
	}
}

// classifyDeleteError maps one failed delete onto the sweep's error codes:
// Forbidden means the caller's RBAC lacks delete on the resource, a dead
// context classifies through WrapCtxErr, and anything else is internal.
func classifyDeleteError(ctx context.Context, obj *Object, err error) error {
	what := fmt.Sprintf("%s %s/%s", obj.Kind, obj.Namespace, obj.Name)
	switch {
	case apierrors.IsForbidden(err):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot delete %s: grant the caller 'delete' on the resource", what), err)
	case ctx.Err() != nil:
		return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout,
			fmt.Sprintf("interrupted while deleting %s", what))
	case stderrors.Is(err, context.DeadlineExceeded):
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf("timed out deleting %s", what), err)
	default:
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to delete %s", what), err)
	}
}

// classifyListError maps a failed family List onto the sweep's error codes.
func classifyListError(ctx context.Context, resource string, err error) error {
	switch {
	case apierrors.IsForbidden(err):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot list %s: grant the caller 'list %s'", resource, resource), err)
	case ctx.Err() != nil:
		return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout,
			fmt.Sprintf("interrupted while listing %s", resource))
	case stderrors.Is(err, context.DeadlineExceeded):
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf("timed out listing %s", resource), err)
	default:
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to list %s", resource), err)
	}
}

// classifyWatchError maps a failed settle-window Watch onto the sweep's
// error codes. The liveness guard needs list on Jobs; the settle observation
// needs the watch verb, so the RBAC hint cannot reuse classifyListError.
func classifyWatchError(ctx context.Context, resource string, err error) error {
	switch {
	case apierrors.IsForbidden(err):
		return errors.Wrap(errors.ErrCodeUnauthorized,
			fmt.Sprintf("cannot watch %s: grant the caller 'watch %s'", resource, resource), err)
	case ctx.Err() != nil:
		return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout,
			fmt.Sprintf("interrupted while watching %s", resource))
	case stderrors.Is(err, context.DeadlineExceeded):
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf("timed out watching %s", resource), err)
	default:
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to watch %s", resource), err)
	}
}
