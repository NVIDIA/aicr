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

package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/evidence"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// RolloutFieldManager owns the rollout controller's runtime status fields.
const RolloutFieldManager = "aicr-operator-rollout"

// Runtime and rollout condition reasons.
const (
	ReasonRolledOut        = "RolledOut"
	ReasonRollingOut       = "RollingOut"
	ReasonStabilizing      = "Stabilizing"
	ReasonAwaitingApproval = "AwaitingApproval"
	// ReasonAwaitingAcknowledgement: a manual release is applied and its
	// post-apply steps await the approver's acknowledgement.
	ReasonAwaitingAcknowledgement = "AwaitingAcknowledgement"
	ReasonActionTimeout           = "ActionTimeout"
	ReasonValidationMissing       = "ValidationUnavailable"
	ReasonNotReady                = "NotReady"
	ReasonNoActivePlan            = "NoActivePlan"
	ReasonHealthy                 = "Healthy"
	ReasonEngineAvailable         = "EngineAvailable"
	ReasonPaused                  = "Paused"
	// ReasonEngineSuspended: spec.suspendEngine stopped automated engine
	// syncs; no step starts and no drift is repaired.
	ReasonEngineSuspended = "EngineSuspended"
	// ReasonPlanMissing: the active plan ConfigMap is gone and the live
	// engine objects do not rebuild to it.
	ReasonPlanMissing = "PlanMissing"
	// ReasonPlanOfAnotherRuntime: the target plan records another runtime's
	// UID, as an earlier runtime's plan does; it is never rolled out.
	ReasonPlanOfAnotherRuntime = "PlanOfAnotherRuntime"
	// ReasonPlanWithoutRecipe: the target plan was rebuilt from engine
	// objects and records no recipe, so deployment validation cannot run
	// against it; it is not rolled out while validation is required.
	ReasonPlanWithoutRecipe = "PlanWithoutRecipe"
	// ReasonMovedReleaseMissing: the Application of a release that a rollout
	// which did not succeed may have moved is gone; repairing it from the
	// active plan would roll it back without approval.
	ReasonMovedReleaseMissing = "MovedReleaseMissing"
)

// resyncInterval re-observes a steady runtime: it covers what no watch
// delivers, such as Applications before the engine kind is served and the
// clock-driven windows and deadlines.
const resyncInterval = 30 * time.Second

// Event reasons the rollout controller records on engine objects and the
// runtime.
const (
	// EventFieldOwnershipOverridden: an Ensure forced fields that other
	// managers had changed back to the plan.
	EventFieldOwnershipOverridden = "FieldOwnershipOverridden"
	// EventApplicationRecreated: an active release's Application was deleted
	// by hand and written again; its resources were never deleted.
	EventApplicationRecreated = "ApplicationRecreated"
	// EventReconcileFailed, on the runtime: a reconcile ended in an error it
	// retries, named so an outage is visible without logs.
	EventReconcileFailed = "ReconcileFailed"
	// EventAICRRunFailed, on the runtime: an AICR run failed, with its
	// stage, run, retry class and error.
	EventAICRRunFailed = "AICRRunFailed"
	// EventStalled, on the runtime: it turned Stalled, or stalled for
	// another reason.
	EventStalled = "Stalled"
	// EventEngineSuspensionRefused, on the runtime: the API server refused
	// for good to change an Application's automated sync.
	EventEngineSuspensionRefused = "EngineSuspensionRefused"
)

// Engine is the adapter contract the rollout controller drives.
type Engine interface {
	Capabilities(ctx context.Context) error
	Ensure(ctx context.Context, p *plan.Document, r *plan.Release, valuesJSON []byte) (*argocd.Ensured, error)
	Verify(ctx context.Context, p *plan.Document, r *plan.Release, valuesJSON []byte) error
	Observe(ctx context.Context, p *plan.Document, r *plan.Release, valuesJSON []byte, since time.Time, verify bool) (argocd.Observation, error)
	Remove(ctx context.Context, runtimeUID, name, uid, rollout string) error
	Retired(ctx context.Context, name, uid string) (argocd.Observation, error)
	Inventory(ctx context.Context, runtimeUID string) ([]argocd.InventoryItem, error)
	// RebuildLive rebuilds the plan from the Applications of runtimeUID and
	// of the runtimes named in also.
	RebuildLive(ctx context.Context, runtimeUID string, also ...string) (*plan.Document, []*plan.Content, error)
	Suspend(ctx context.Context, runtimeUID string, suspended bool) ([]string, error)
	Orphan(ctx context.Context, runtimeUID string) ([]string, error)
	Foreign(ctx context.Context, runtimeUID string) (map[string][]string, error)
	Adopt(ctx context.Context, fromUID, toUID, name, uid string) error
}

// RolloutReconciler moves the runtime from its active plan to its target
// plan through RuntimeRollouts, one engine mutation per step. It
// owns every runtime condition except Resolved.
type RolloutReconciler struct {
	Client client.Client
	Reader client.Reader
	Config *config.Config
	Engine Engine
	Now    func() time.Time
	// Credentials, when set, reports whether the engine's registry
	// credentials are current (aws provider).
	Credentials interface{ Check(context.Context) error }
	// Events, when set, records Events on engine objects.
	Events events.EventRecorder
	// Registry holds deployment validation evidence.
	Registry EvidenceRegistry
	// JobCredentials supplies the validation Job's registry credentials;
	// nil is an anonymous registry.
	JobCredentials registry.Provider
	// Operator reports the operator's own image and the digest it runs as,
	// for the evidence; nil records none.
	Operator func(context.Context) (evidence.Image, error)
}

// runtimeView is what one reconcile concluded for the runtime's status.
type runtimeView struct {
	// released: a deleted runtime's finalizer is gone; nothing to report.
	released        bool
	active, rollout string
	// inProgress is the rollout still in progress after this reconcile's
	// step: Ready reports its progress and Available observes the releases
	// it moved at its target plan.
	inProgress *v1alpha1.RuntimeRollout
	// retired are releases a rollout ended since the newest success
	// started removing; nothing writes them again until a rollout does.
	retired map[string]bool
	// moved are releases such a rollout may have written to its target
	// plan; repair never writes them back to the active plan.
	moved                       map[string]movedBy
	ready, available            bool
	readyReason, readyMessage   string
	availableMessage            string
	reconciling                 bool
	reconcilingReason           string
	stalled                     bool
	stalledReason, stalledMsg   string
	engineOK                    bool
	engineReason, engineMessage string
	requeue                     time.Duration
	// blockedReport is the assessment report ConfigMap of a transition that
	// stalled blocked: no rollout references it, yet retention keeps it so
	// the transition is not assessed again.
	blockedReport string
}

// Reconcile drives the runtime named in req.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Uncached: the status picks the rollout target, and the status
	// apply removes what it omits.
	rt := &v1alpha1.ClusterRuntime{}
	if err := r.Reader.Get(ctx, req.NamespacedName, rt); err != nil {
		if apierrors.IsNotFound(err) {
			// The runtime is gone: its unfinished rollouts are orphaned.
			return ctrl.Result{}, r.orphan(ctx, req.Name, "")
		}
		return ctrl.Result{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read ClusterRuntime "+req.Name)
	}
	if rt.Name != v1alpha1.SingletonName {
		return ctrl.Result{}, nil
	}
	switch stalled, err := r.unreadableRollouts(ctx, rt); {
	case err != nil:
		return ctrl.Result{}, err
	case stalled:
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}
	if !rt.DeletionTimestamp.IsZero() {
		view, err := r.deleting(ctx, rt)
		if err != nil || view.released {
			return ctrl.Result{}, err
		}
		if err = r.applyStatus(ctx, rt, view); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: view.requeue}, nil
	}
	if err := r.ensureFinalizer(ctx, rt); err != nil {
		return ctrl.Result{}, err
	}
	view, err := r.reconcile(ctx, rt)
	if err != nil {
		r.reconcileFailed(rt, err)
		return ctrl.Result{}, err
	}
	if err = r.applyStatus(ctx, rt, view); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: view.requeue}, nil
}

func (r *RolloutReconciler) reconcile(ctx context.Context, rt *v1alpha1.ClusterRuntime) (*runtimeView, error) {
	v := &runtimeView{requeue: resyncInterval, engineOK: true, engineReason: ReasonEngineAvailable}
	if ready, err := r.prepare(ctx, rt, v); err != nil || !ready {
		return v, err
	}
	rollouts, err := r.rollouts(ctx, rt)
	if err != nil {
		return nil, err
	}
	if err = r.restoreOutcomes(ctx, rollouts); err != nil {
		return nil, err
	}
	active, current, last := classify(rollouts)
	v.active = active
	retry := retryToken(rt, rollouts)

	// Only a plan resolved for the current generation and inputs is a
	// target; while a resolution runs, the previous candidate is not rolled.
	resolved := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionResolved)
	resolvedNow := resolved != nil && resolved.ObservedGeneration == rt.Generation && resolved.Status == metav1.ConditionTrue
	target := r.target(rt)
	// A stalled resolution, such as a cluster that no longer matches its
	// criteria, holds a rollout in progress at its next step boundary.
	hold := ""
	switch {
	case resolved != nil && resolved.Status == metav1.ConditionFalse && resolved.Reason != ReasonResolving:
		v.stalled, v.stalledReason, v.stalledMsg = true, resolved.Reason, resolved.Message
		hold = resolved.Reason
	case !resolvedNow:
		v.reconciling, v.reconcilingReason = true, ReasonResolving
	}

	if current == nil {
		// Another runtime's engine objects stop everything until they are
		// adopted or removed.
		if waiting, aErr := r.adoption(ctx, rt, rollouts, last, v); aErr != nil || waiting {
			return v, aErr
		}
	}
	converge := target != "" && target != active
	if !converge && target != "" && current == nil && resolvedNow && !v.stalled {
		if converge, err = r.diverged(ctx, rt, target, rollouts); err != nil {
			return nil, err
		}
	}
	switch {
	case current != nil:
		v.rollout, v.inProgress = current.Name, current
		v.reconciling, v.reconcilingReason = true, ReasonRollingOut
		// A newer target, or a retry of a stalled rollout, ends it at the
		// next step boundary. An Adopt rollout moves the objects
		// at the plan they carry; the target rolls forward after it, not
		// instead of it.
		var supersede, token string
		requested := rt.Annotations[v1alpha1.AnnotationRetry]
		switch {
		case target != "" && resolvedNow && target != current.Spec.ToPlanDigest && !isAdopt(current):
			supersede = "superseded by plan " + target
		case current.Status.Phase == v1alpha1.PhaseStalled && requested != "" &&
			(retry == requested || current.Annotations[v1alpha1.AnnotationRetryRequested] == requested):
			// The token is consumed on this rollout now, so one the rollout
			// recovers from before a step boundary acts on no later rollout;
			// the rollout is superseded while it stays stalled.
			if err = r.retryRequested(ctx, current, requested); err != nil {
				return nil, err
			}
			supersede, token = "superseded by retry "+requested, requested
		}
		if err = r.advance(ctx, rt, current, v, supersede, token, hold); err != nil {
			return nil, err
		}
		if current.Status.Phase.IsTerminal() {
			v.rollout, v.inProgress = "", nil
			if current.Status.Phase == v1alpha1.PhaseSucceeded {
				v.active = current.Spec.ToPlanDigest
			}
		}
	case converge && resolvedNow && !v.stalled:
		token := ""
		if last != nil && last.Spec.ToPlanDigest == target && failedOutcome(last) {
			// A failed transition waits for a new target or a retry request.
			if retry == "" {
				v.stalled, v.stalledReason, v.stalledMsg = true, "Rollout"+last.Labels[v1alpha1.LabelOutcome], last.Status.Message
				break
			}
			token = retry
		}
		if halt := haltedRuntime(rt); halt != "" {
			v.readyReason = halt
			break
		}
		if err = r.create(ctx, rt, rollouts, active, target, token, v); err != nil {
			return nil, err
		}
	}
	// After advance, which may have just ended the rollout in progress.
	v.retired, v.moved = leftBehind(rollouts)
	if err = r.observeActive(ctx, rt, target, v); err != nil {
		return nil, err
	}
	if v.rollout == "" {
		if err = r.retain(ctx, rt, v.active, target, v.blockedReport); err != nil {
			log.FromContext(ctx).Info("retention will retry", "error", err.Error())
		}
	}
	return v, nil
}

// unreadableRollouts stops the runtime when any of its rollouts, of any
// runtime UID, has a schema this operator does not read, as after a
// downgrade past a newer operator: it reports Stalled with
// PlanUnreadable, keeps the rest of its status, and writes nothing else,
// the engine and the rollouts included. Upgrading again recovers.
func (r *RolloutReconciler) unreadableRollouts(ctx context.Context, rt *v1alpha1.ClusterRuntime) (bool, error) {
	list := &v1alpha1.RuntimeRolloutList{}
	if err := r.Reader.List(ctx, list, client.MatchingLabels{v1alpha1.LabelRuntime: rt.Name}); err != nil {
		return false, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list rollouts")
	}
	var unreadable []string
	for i := range list.Items {
		if ro := &list.Items[i]; !v1alpha1.SupportedRolloutSchemaVersions[ro.Spec.SchemaVersion] {
			unreadable = append(unreadable, fmt.Sprintf("%s (schema %d)", ro.Name, ro.Spec.SchemaVersion))
		}
	}
	if len(unreadable) == 0 {
		return false, nil
	}
	sort.Strings(unreadable)
	message := "rollouts written by a newer operator: " + strings.Join(unreadable, ", ") + "; upgrade the operator"
	v := &runtimeView{
		engineOK: true, engineReason: ReasonEngineAvailable,
		stalled: true, stalledReason: plan.ReasonPlanUnreadable, stalledMsg: message,
		readyReason: plan.ReasonPlanUnreadable, readyMessage: message,
	}
	keepObserved(rt, v)
	return true, r.applyStatus(ctx, rt, v)
}

// keepObserved carries the active plan, the rollout and availability the
// runtime last reported into a view that observed none of them, so a failed
// check never erases what the runtime runs.
func keepObserved(rt *v1alpha1.ClusterRuntime, v *runtimeView) {
	v.active, v.rollout = rt.Status.ActivePlanDigest, rt.Status.Rollout
	if available := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionAvailable); available != nil {
		v.available, v.availableMessage = available.Status == metav1.ConditionTrue, available.Message
	}
}

// prepare checks the engine and the registry credentials, orphans earlier
// runtimes' unfinished rollouts and applies spec.suspendEngine. It reports
// whether the runtime can be reconciled further. A check that fails with a
// reason reports it, and so does one refused for good, such as a forbidden
// read; one that could not read what it checks for now returns its error, so
// one failed read flips no condition.
func (r *RolloutReconciler) prepare(ctx context.Context, rt *v1alpha1.ClusterRuntime, v *runtimeView) (bool, error) {
	err := r.Engine.Capabilities(ctx)
	if err == nil && r.Credentials != nil {
		err = r.Credentials.Check(ctx)
	}
	if err != nil {
		if operrors.ReasonOf(err) == "" && !operrors.IsPermanent(err) {
			return false, err
		}
		v.engineOK, v.engineReason, v.engineMessage = false, reasonOf(err), errorMessage(err)
		v.stalled, v.stalledReason, v.stalledMsg = true, v.engineReason, v.engineMessage
		v.readyReason, v.readyMessage = v.engineReason, v.engineMessage
		keepObserved(rt, v)
		return false, nil
	}
	if err = r.orphan(ctx, rt.Name, string(rt.UID)); err != nil {
		return false, err
	}
	if err = r.suspension(ctx, rt); err != nil {
		return false, err
	}
	return true, nil
}

// diverged reports whether an owned engine object carries a plan other than
// target, as a failed rollout leaves the releases it already moved, or
// whether a release of the target plan that such a rollout moved elsewhere
// or removed is gone, which repair does not restore. When the target is the
// active plan, rolling out to it from live state is moving to the declared
// target, not an automatic rollback.
func (r *RolloutReconciler) diverged(ctx context.Context, rt *v1alpha1.ClusterRuntime, target string,
	rollouts []v1alpha1.RuntimeRollout) (bool, error) {

	inventory, err := r.Engine.Inventory(ctx, string(rt.UID))
	if err != nil {
		return false, err
	}
	live := map[string]bool{}
	for _, i := range inventory {
		if i.PlanDigest != target {
			return true, nil
		}
		live[i.ReleaseID] = true
	}
	retired, moved := leftBehind(rollouts)
	gone := map[string]bool{}
	for id := range retired {
		if !live[id] {
			gone[id] = true
		}
	}
	for id, m := range moved {
		if m.plan != target && !live[id] {
			gone[id] = true
		}
	}
	if len(gone) == 0 {
		return false, nil
	}
	p, err := readPlan(ctx, r.Reader, r.Config.OperatorNamespace, target)
	if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
		// observeActive restores the active plan or stalls without it.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for i := range p.Releases {
		if gone[p.Releases[i].ID] {
			return true, nil
		}
	}
	return false, nil
}

// target is the plan the runtime should run: the newest candidate, or the
// pinned plan.
func (r *RolloutReconciler) target(rt *v1alpha1.ClusterRuntime) string {
	if rt.Spec.Selection.Policy == v1alpha1.SelectionPinned {
		return rt.Spec.Selection.PinnedPlanDigest
	}
	return rt.Status.CandidatePlanDigest
}

// rollouts lists the runtime's rollouts through the uncached reader, keeping
// only those of this runtime UID, newest first.
func (r *RolloutReconciler) rollouts(ctx context.Context, rt *v1alpha1.ClusterRuntime) ([]v1alpha1.RuntimeRollout, error) {
	list := &v1alpha1.RuntimeRolloutList{}
	if err := r.Reader.List(ctx, list, client.MatchingLabels{v1alpha1.LabelRuntime: rt.Name}); err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list rollouts")
	}
	var out []v1alpha1.RuntimeRollout
	for i := range list.Items {
		if list.Items[i].Spec.RuntimeUID == string(rt.UID) {
			out = append(out, list.Items[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := &out[i], &out[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return b.CreationTimestamp.Before(&a.CreationTimestamp)
		}
		// Timestamps have one-second resolution: within a second, a rollout
		// that starts from another's target is the newer one.
		return a.Spec.FromPlanDigest != "" && a.Spec.FromPlanDigest == b.Spec.ToPlanDigest
	})
	return out, nil
}

// classify finds the active plan (the newest rollout labeled Succeeded), the
// rollout in progress (one without an outcome) and the newest rollout.
func classify(rollouts []v1alpha1.RuntimeRollout) (active string, current, last *v1alpha1.RuntimeRollout) {
	for i := range rollouts {
		ro := &rollouts[i]
		if last == nil {
			last = ro
		}
		outcome := ro.Labels[v1alpha1.LabelOutcome]
		if outcome == "" && current == nil {
			current = ro
		}
		if outcome == string(v1alpha1.PhaseSucceeded) && active == "" {
			active = ro.Spec.ToPlanDigest
		}
	}
	return active, current, last
}

func failedOutcome(ro *v1alpha1.RuntimeRollout) bool {
	switch v1alpha1.RolloutPhase(ro.Labels[v1alpha1.LabelOutcome]) {
	case v1alpha1.PhaseFailed, v1alpha1.PhaseExpired, v1alpha1.PhaseRevoked:
		return true
	case v1alpha1.PhaseSucceeded, v1alpha1.PhaseSuperseded, v1alpha1.PhaseOrphaned, v1alpha1.PhasePending,
		v1alpha1.PhaseAwaitingApproval, v1alpha1.PhaseProgressing, v1alpha1.PhaseAwaitingAcknowledgement,
		v1alpha1.PhaseValidating, v1alpha1.PhaseStalled:
		return false
	default:
		return false
	}
}

// refused stalls the runtime on a target plan it never rolls out: another
// runtime's, or one without a recipe while deployment validation is
// required. It also stalls, before any step, a runtime that requires
// deployment validation from an installation without it.
func (r *RolloutReconciler) refused(rt *v1alpha1.ClusterRuntime, p *plan.Document, v *runtimeView) bool {
	switch {
	case p.RuntimeUID != string(rt.UID):
		// Its engine objects would carry the other UID, which this runtime's
		// inventory never sees.
		v.stalled, v.stalledReason = true, ReasonPlanOfAnotherRuntime
		v.stalledMsg = fmt.Sprintf("plan %s records runtime UID %s, not this runtime's %s", p.DesiredStateDigest, p.RuntimeUID, rt.UID)
	case p.Recipe == nil && rt.Spec.Validation.Deployment != v1alpha1.DeploymentValidationDisabled:
		// Every step would be applied and then never validated.
		v.stalled, v.stalledReason = true, ReasonPlanWithoutRecipe
		v.stalledMsg = fmt.Sprintf("plan %s was rebuilt from engine objects and records no recipe, so deployment validation cannot run: "+
			"change the inputs so resolution reproduces it, which restores its recipe; select another plan; "+
			"or set spec.validation.deployment: Disabled", p.DesiredStateDigest)
	case !r.Config.ValidationEnabled && rt.Spec.Validation.Deployment != v1alpha1.DeploymentValidationDisabled:
		// Every step would be applied and then wait for a validation that
		// cannot run.
		v.stalled, v.stalledReason, v.stalledMsg = true, ReasonValidationMissing, validationNotInstalled
	default:
		return false
	}
	// Nothing rolls out until the cause is fixed, so Ready names it too.
	v.readyReason, v.readyMessage = v.stalledReason, v.stalledMsg
	return true
}

// judge orders and assesses the transition to p from what is deployed:
// its steps, whether it needs approval, and the stored AICR
// assessment. Nil steps and no error mean the runtime waits or stalls, as v
// records.
func (r *RolloutReconciler) judge(ctx context.Context, rt *v1alpha1.ClusterRuntime, from, p *plan.Document,
	inventory []argocd.InventoryItem, v *runtimeView) ([]v1alpha1.RolloutStep, bool, *v1alpha1.DigestedConfigMapRef, error) {

	base, err := r.liveBaseline(ctx, from, p, inventory)
	if err != nil {
		return nil, false, nil, err
	}
	var report *aicrjob.UpgradeReport
	var assessment *v1alpha1.DigestedConfigMapRef
	if assessed(base.from, p) {
		var wait time.Duration
		if report, assessment, wait, err = r.assess(ctx, rt, base.from, p); err != nil {
			if isTerminal(err) {
				v.stalled, v.stalledReason, v.stalledMsg = true, reasonOf(err), errorMessage(err)
				return nil, false, nil, nil
			}
			return nil, false, nil, err
		}
		if report == nil {
			v.reconciling, v.reconcilingReason, v.requeue = true, ReasonAssessing, wait
			return nil, false, nil, nil
		}
	}
	steps := steps(p, base.doc, inventory)
	approvalRequired, err := assessSteps(steps, base.doc, p, without(report, base.unassessed))
	if err != nil {
		v.stalled, v.stalledReason, v.stalledMsg = true, reasonOf(err), errorMessage(err)
		if assessment != nil {
			v.blockedReport = assessment.ConfigMap
		}
		return nil, false, nil, nil
	}
	return steps, approvalRequired, assessment, nil
}

// create makes the rollout from active to target, named deterministically
// over the runtime UID and both digests, with a suffix from rollouts, the
// uncached list that found none in progress (api.md). A lost create is found
// by the next list; a concurrent creator, whatever live state it read, makes
// the same name and collides.
func (r *RolloutReconciler) create(ctx context.Context, rt *v1alpha1.ClusterRuntime, rollouts []v1alpha1.RuntimeRollout,
	active, target, retry string, v *runtimeView) error {

	p, err := readPlan(ctx, r.Reader, r.Config.OperatorNamespace, target)
	if err != nil {
		if operrors.IsPermanent(err) {
			v.stalled, v.stalledReason, v.stalledMsg = true, reasonOf(err), errorMessage(err)
			return nil
		}
		return err
	}
	if r.refused(rt, p, v) {
		return nil
	}
	planCM, err := plan.Name(target)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{}
	if err = r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Config.OperatorNamespace, Name: planCM}, cm); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read plan "+planCM)
	}
	window := defaults.RolloutStabilizationWindow
	if d := rt.Spec.Rollout.StabilizationWindow; d != nil {
		window = d.Duration
	}
	if wait := cm.CreationTimestamp.Add(window).Sub(r.Now()); wait > 0 {
		v.reconciling, v.reconcilingReason, v.requeue = true, ReasonStabilizing, min(wait, resyncInterval)
		return nil
	}
	inventory, err := r.Engine.Inventory(ctx, string(rt.UID))
	if err != nil {
		return err
	}
	var from *plan.Document
	if active != "" {
		if from, err = readPlan(ctx, r.Reader, r.Config.OperatorNamespace, active); err != nil {
			if !operrors.IsPermanent(err) {
				return err
			}
			// Without the active plan nothing is verified and removals
			// follow the inventory.
			from = nil
		}
	}
	steps, approvalRequired, assessment, err := r.judge(ctx, rt, from, p, inventory, v)
	if err != nil || steps == nil {
		return err
	}
	// Live state hides what an unfinished rollout to the same target already
	// applied, so a retry is judged against those rollouts too.
	if earlier := unfinished(rollouts, target); len(earlier) > 0 {
		approvalRequired = true
		carryPostApplySteps(steps, earlier)
	}
	invJSON, err := json.Marshal(inventory)
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "encode inventory", err)
	}
	inventoryDigest := plan.DigestOf(invJSON)
	transition := strings.TrimPrefix(plan.DigestOf([]byte(strings.Join([]string{string(rt.UID), active, target}, "\n"))), "sha256:")[:16]
	n := nextSuffix(rollouts, transition)
	deadline, actionTimeout := defaults.RolloutDeadline, defaults.RolloutActionTimeout
	if d := rt.Spec.Rollout.Deadline; d != nil {
		deadline = d.Duration
	}
	if d := rt.Spec.Rollout.ActionTimeout; d != nil {
		actionTimeout = d.Duration
	}
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("rollout-%s-%d", transition, n),
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:  v1alpha1.ManagedByValue,
				v1alpha1.LabelRuntime:    rt.Name,
				v1alpha1.LabelToPlan:     strings.TrimPrefix(target, "sha256:")[:16],
				v1alpha1.LabelTransition: transition,
			},
		},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion:           v1alpha1.RolloutSchemaVersion,
			RuntimeUID:              string(rt.UID),
			FromPlanDigest:          active,
			ToPlanDigest:            target,
			StartingInventoryDigest: inventoryDigest,
			StartingInventory:       startingInventory(inventory),
			AssessmentRef:           assessment,
			Deadline:                metav1.NewTime(r.Now().Add(deadline)),
			ActionTimeout:           metav1.Duration{Duration: actionTimeout},
			ApprovalRequired:        approvalRequired,
			Steps:                   steps,
		},
	}
	if retry != "" {
		ro.Annotations = map[string]string{v1alpha1.AnnotationRetry: retry}
	}
	switch err = r.Client.Create(ctx, ro); {
	case apierrors.IsAlreadyExists(err):
		// A concurrent creator or a lost response: the next list finds it.
	case err != nil:
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create rollout "+ro.Name)
	default:
		r.event(rt, corev1.EventTypeNormal, "RolloutCreated", "rollout %s from %s to plan %s (approval required: %t)",
			ro.Name, orNone(active), target, ro.Spec.ApprovalRequired)
	}
	v.rollout, v.inProgress, v.reconciling, v.reconcilingReason, v.requeue = ro.Name, ro, true, ReasonRollingOut, time.Second
	return nil
}

// steps orders a transition: in the target plan's order, installs
// for releases the inventory lacks and upgrades for the others, where a
// readiness release that is unchanged while its parent changes is a Verify
// that re-runs its gate; then removals of releases the target plan lacks,
// in reverse order of the active plan, each bound to the object's UID.
func steps(p, from *plan.Document, inventory []argocd.InventoryItem) []v1alpha1.RolloutStep {
	live := map[string]bool{}
	for _, i := range inventory {
		live[i.ReleaseID] = true
	}
	// changed reports a release that differs from the active plan's, or that
	// the active plan lacks.
	changed := func(id string) bool {
		a, b := release(from, id), release(p, id)
		return a == nil || b == nil || a.Chart.Digest != b.Chart.Digest || a.Values.Digest != b.Values.Digest ||
			a.TargetNamespace != b.TargetNamespace
	}
	out := make([]v1alpha1.RolloutStep, 0, len(p.Releases)+len(inventory))
	for i := range p.Releases {
		rel := &p.Releases[i]
		step := v1alpha1.RolloutStep{Release: rel.ID, Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable}
		switch {
		case !live[rel.ID]:
		case rel.Phase == plan.PhaseReadiness && from != nil && !changed(rel.ID) && changed(rel.ReadinessFor):
			step.Operation = v1alpha1.OperationVerify
		default:
			step.Operation = v1alpha1.OperationUpgrade
		}
		out = append(out, step)
	}
	var removed []argocd.InventoryItem
	for _, i := range inventory {
		if release(p, i.ReleaseID) == nil {
			removed = append(removed, i)
		}
	}
	// Reverse active-plan order; releases the active plan lacks go first,
	// in reverse inventory order.
	position := func(id string) int {
		if from != nil {
			for k := range from.Releases {
				if from.Releases[k].ID == id {
					return k
				}
			}
		}
		return len(inventory)
	}
	slices.Reverse(removed)
	sort.SliceStable(removed, func(i, j int) bool { return position(removed[i].ReleaseID) > position(removed[j].ReleaseID) })
	for _, i := range removed {
		out = append(out, v1alpha1.RolloutStep{Release: i.ReleaseID, Operation: v1alpha1.OperationRemove,
			Assessment: v1alpha1.AssessmentNotApplicable, UID: i.UID})
	}
	return out
}

// nextSuffix is one more than the highest suffix among the listed rollouts
// of a transition. Creators that listed the same rollouts derive the same
// name; a rollout created since makes a stale creator collide with it.
func nextSuffix(rollouts []v1alpha1.RuntimeRollout, transition string) int {
	n := 1
	for i := range rollouts {
		if rollouts[i].Labels[v1alpha1.LabelTransition] != transition {
			continue
		}
		idx := strings.LastIndex(rollouts[i].Name, "-")
		if k, err := strconv.Atoi(rollouts[i].Name[idx+1:]); err == nil && k >= n {
			n = k + 1
		}
	}
	return n
}

// advance moves a rollout one step at a time: deadline, approval,
// pause, then the steps in order with one engine mutation each. A non-empty
// supersede ends it at the next step boundary, recording the token of the
// retry that asked for it, if any. A non-empty hold names why no further step
// starts, as a pause does; a removal rollout is never held, so a runtime
// with broken inputs can still be deleted.
func (r *RolloutReconciler) advance(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout, v *runtimeView,
	supersede, retry, hold string) error {

	now := r.Now()
	if now.After(ro.Spec.Deadline.Time) {
		return r.finish(ctx, ro, v1alpha1.PhaseExpired, "the rollout deadline passed")
	}
	// A step in flight is never abandoned mid-write; validation is not
	// superseded.
	atBoundary := ro.Status.Current == nil && ro.Status.Phase != v1alpha1.PhaseValidating
	if supersede != "" && atBoundary {
		return r.superseded(ctx, ro, supersede, retry)
	}
	approval := r.approval(ro, now)
	if approval == v1alpha1.ApprovalPending {
		ro.Status.Approval, ro.Status.Phase = approval, v1alpha1.PhaseAwaitingApproval
		v.stalled, v.stalledReason, v.stalledMsg = true, ReasonAwaitingApproval, "rollout "+ro.Name+" needs approval"
		return r.saveStatus(ctx, ro, "")
	}
	ro.Status.Approval = approval
	// A revoked or expired approval starts no further step; the step in
	// flight finishes, and once every step has, nothing is left to stop.
	stop := halted(approval)
	if halt := haltedRuntime(rt); halt != "" {
		v.readyReason = halt
		return r.observeHalted(ctx, ro, halt, v)
	}
	if hold != "" && !isRemoval(ro) {
		return r.observeHalted(ctx, ro, hold, v)
	}
	if cur := ro.Status.Current; cur != nil && releaseState(ro, cur.Release) == v1alpha1.ReleaseSuspended {
		// The halt ended: the step's action timeout starts again.
		from := metav1.NewTime(now)
		cur.TimeoutFrom = &from
		state := v1alpha1.ReleaseProgressing
		if cur.Operation == v1alpha1.OperationRemove {
			state = v1alpha1.ReleaseDeleting
		}
		setReleaseState(ro, cur.Release, state, "")
	}
	// A removal rollout targets no plan: its steps are all Remove.
	p := &plan.Document{RuntimeUID: ro.Spec.RuntimeUID, DesiredStateDigest: ro.Spec.ToPlanDigest}
	if !isRemoval(ro) {
		var err error
		if p, err = readPlan(ctx, r.Reader, r.Config.OperatorNamespace, ro.Spec.ToPlanDigest); err != nil {
			if operrors.IsPermanent(err) {
				ro.Status.Phase = v1alpha1.PhaseStalled
				v.stalled, v.stalledReason, v.stalledMsg = true, reasonOf(err), errorMessage(err)
				return r.saveStatus(ctx, ro, errorMessage(err))
			}
			return err
		}
	}
	ro.Status.Phase = v1alpha1.PhaseProgressing
	for _, step := range ro.Spec.Steps {
		if s := releaseState(ro, step.Release); s == v1alpha1.ReleaseReady || s == v1alpha1.ReleaseRemoved {
			if awaitsAcknowledgement(ro, approval, step) {
				return r.awaitAcknowledgement(ctx, ro, step, stop, v)
			}
			continue
		}
		if supersede != "" && ro.Status.Current == nil {
			return r.superseded(ctx, ro, supersede, retry)
		}
		if cur := ro.Status.Current; cur == nil || cur.Release != step.Release {
			// Before each step, live state must be the starting inventory
			// plus the completed steps.
			if drifted, dErr := r.checkDrift(ctx, rt, ro, p, v); dErr != nil || drifted {
				return dErr
			}
		}
		var done bool
		var err error
		switch step.Operation {
		case v1alpha1.OperationRemove:
			done, err = r.remove(ctx, rt, ro, step, now, stop, v)
		case v1alpha1.OperationAdopt:
			done, err = r.adopt(ctx, rt, ro, step, stop)
		case v1alpha1.OperationInstall, v1alpha1.OperationUpgrade, v1alpha1.OperationVerify:
			done, err = r.step(ctx, p, ro, step, now, stop, v)
		default:
			return r.finish(ctx, ro, v1alpha1.PhaseFailed, "step "+step.Release+" has unknown operation "+string(step.Operation))
		}
		if err != nil || !done {
			return err
		}
		if awaitsAcknowledgement(ro, approval, step) {
			return r.awaitAcknowledgement(ctx, ro, step, stop, v)
		}
	}
	if validationRequired(rt, ro) {
		return r.validate(ctx, rt, ro, p, v)
	}
	return r.finish(ctx, ro, v1alpha1.PhaseSucceeded, "")
}

// step starts or observes one step. It reports done when the release is
// current-revision ready.
func (r *RolloutReconciler) step(ctx context.Context, p *plan.Document, ro *v1alpha1.RuntimeRollout, step v1alpha1.RolloutStep,
	now time.Time, stop *halt, v *runtimeView) (bool, error) {

	rel := release(p, step.Release)
	if rel == nil {
		ro.Status.Phase = v1alpha1.PhaseStalled
		v.stalled, v.stalledReason, v.stalledMsg = true, "StepUnknown", "step "+step.Release+" is not in the target plan"
		return false, r.saveStatus(ctx, ro, v.stalledMsg)
	}
	values, err := r.values(ctx, rel)
	if err != nil {
		if operrors.IsPermanent(err) {
			return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, errorMessage(err))
		}
		return false, err
	}
	cur := ro.Status.Current
	if cur == nil || cur.Release != step.Release || cur.StartedAt == nil {
		landed := false
		if stop != nil {
			if landed, err = r.landed(ctx, p, rel, ro, step, values, now); err != nil {
				return false, err
			}
			if !landed {
				return false, r.finish(ctx, ro, stop.phase, stop.message)
			}
		}
		start := metav1.NewTime(now)
		if landed && syncs(step, rel) {
			// Observe the sync this rollout already started.
			start = ro.CreationTimestamp
		}
		ro.Status.Current = &v1alpha1.CurrentStep{Release: step.Release, Operation: step.Operation, StartedAt: &start}
		setReleaseState(ro, step.Release, v1alpha1.ReleaseProgressing, "")
		// Record the step before its mutation, so a restart observes it.
		if err = r.saveStatus(ctx, ro, ""); err != nil {
			return false, err
		}
		if landed {
			v.requeue = pollInterval
			return false, nil
		}
		if written, wErr := r.written(ctx, p, rel, step, values, now); wErr != nil || written {
			// Written before a restart lost the step: observation continues.
			v.requeue = pollInterval
			return false, wErr
		}
		if err = r.mutate(ctx, p, rel, step, values); err != nil {
			if operrors.IsPermanent(err) {
				return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, errorMessage(err))
			}
			return false, err
		}
		v.requeue = pollInterval
		return false, nil
	}
	verify := syncs(step, rel)
	obs, err := r.Engine.Observe(ctx, p, rel, values, cur.StartedAt.Time, verify)
	if err == nil && verify && obs.Reason == argocd.ReasonSyncNotStarted {
		// Argo CD was busy or lost the request: ask again.
		err = r.Engine.Verify(ctx, p, rel, values)
	}
	if err == nil && !verify && obs.Reason == argocd.ReasonPriorSyncFailed {
		// The Application already carries the plan, as in a retry, but its
		// sync of this revision failed before the step, and automated sync
		// never retries a failed revision: start the sync.
		err = r.Engine.Verify(ctx, p, rel, values)
	}
	if err == nil && !verify && (obs.State == argocd.StateMissing || obs.Reason == argocd.ReasonSpecNotWritten) {
		// The step was recorded, then the operator stopped before its write
		// landed: make it now, unless the approval ended since,
		// which no landed mutation outlives.
		if stop != nil {
			return false, r.finish(ctx, ro, stop.phase, stop.message)
		}
		if err = r.mutate(ctx, p, rel, step, values); err == nil {
			v.requeue = pollInterval
			return false, r.saveStatus(ctx, ro, "release "+step.Release+": written again after an interrupted step")
		}
	}
	if err != nil {
		if ended, finishErr := r.timedOut(ctx, ro, step, cur, now, "ready", err); ended {
			return false, finishErr
		}
		if operrors.IsPermanent(err) {
			return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, errorMessage(err))
		}
		return false, err
	}
	switch {
	case obs.State == argocd.StateReady:
		setReleaseState(ro, step.Release, v1alpha1.ReleaseReady, rel.Chart.Digest)
		recordSpec(ro, step.Release, obs.SpecDigest)
		ro.Status.Current = nil
		return true, r.saveStatus(ctx, ro, "")
	case obs.State == argocd.StateFailed:
		setReleaseState(ro, step.Release, v1alpha1.ReleaseFailed, "")
		return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, "release "+step.Release+": "+obs.Reason)
	case now.Sub(timeoutFrom(cur)) > ro.Spec.ActionTimeout.Duration:
		setReleaseState(ro, step.Release, v1alpha1.ReleaseFailed, "")
		return false, r.finish(ctx, ro, v1alpha1.PhaseFailed,
			fmt.Sprintf("release %s not ready within %s (%s)", step.Release, ro.Spec.ActionTimeout.Duration, obs.Reason))
	}
	v.requeue = pollInterval
	return false, r.saveStatus(ctx, ro, "release "+step.Release+": "+obs.Reason)
}

// timedOut ends a step whose action timeout passed while a transient error
// kept it from being observed, naming that error, so a lasting outage fails
// the step with its cause instead of waiting out the rollout's deadline.
// It reports whether it ended the step.
func (r *RolloutReconciler) timedOut(ctx context.Context, ro *v1alpha1.RuntimeRollout, step v1alpha1.RolloutStep,
	cur *v1alpha1.CurrentStep, now time.Time, outcome string, err error) (bool, error) {

	if operrors.IsPermanent(err) || now.Sub(timeoutFrom(cur)) <= ro.Spec.ActionTimeout.Duration {
		return false, nil
	}
	setReleaseState(ro, step.Release, v1alpha1.ReleaseFailed, "")
	return true, r.finish(ctx, ro, v1alpha1.PhaseFailed,
		fmt.Sprintf("release %s not %s within %s (%s)", step.Release, outcome, ro.Spec.ActionTimeout.Duration, errorMessage(err)))
}

// awaitsAcknowledgement reports a ready step whose manual record has steps
// after the apply that the approver has not acknowledged. The
// development bypass skips it, as it skips the approval.
func awaitsAcknowledgement(ro *v1alpha1.RuntimeRollout, approval v1alpha1.ApprovalState, step v1alpha1.RolloutStep) bool {
	if len(step.PostApplySteps) == 0 || approval == v1alpha1.ApprovalBypassed || approval == v1alpha1.ApprovalNotRequired {
		return false
	}
	a := ro.Spec.Approval
	return a == nil || !slices.ContainsFunc(a.Acknowledgements, func(k v1alpha1.Acknowledgement) bool { return k.Release == step.Release })
}

// awaitAcknowledgement holds the rollout after a manual release's apply
// until the approver acknowledges the record's remaining steps; nothing
// further starts and the plan does not become active meanwhile. A
// revoked or expired approval ends it here, as at any step boundary.
func (r *RolloutReconciler) awaitAcknowledgement(ctx context.Context, ro *v1alpha1.RuntimeRollout, step v1alpha1.RolloutStep,
	stop *halt, v *runtimeView) error {

	if stop != nil {
		return r.finish(ctx, ro, stop.phase, stop.message)
	}
	message := "release " + step.Release + " is applied; acknowledge its post-apply steps: " + strings.Join(step.PostApplySteps, ", ")
	ro.Status.Phase = v1alpha1.PhaseAwaitingAcknowledgement
	v.stalled, v.stalledReason, v.stalledMsg = true, ReasonAwaitingAcknowledgement, "rollout "+ro.Name+": "+message
	return r.saveStatus(ctx, ro, message)
}

// timeoutFrom is when the step's action timeout started: the step's start,
// or the end of the last halt.
func timeoutFrom(cur *v1alpha1.CurrentStep) time.Time {
	if cur.TimeoutFrom != nil {
		return cur.TimeoutFrom.Time
	}
	return cur.StartedAt.Time
}

// observeHalted keeps observing the step in flight while the runtime is
// paused or its engine suspended, and writes nothing to the engine. Its release reads Suspended until the halt ends; one that
// becomes ready meanwhile is recorded, and nothing else is judged.
func (r *RolloutReconciler) observeHalted(ctx context.Context, ro *v1alpha1.RuntimeRollout, halt string, v *runtimeView) error {
	message := "no step starts: " + halt
	cur := ro.Status.Current
	if cur == nil || cur.StartedAt == nil || cur.Operation == v1alpha1.OperationAdopt {
		return r.saveStatus(ctx, ro, message)
	}
	obs, done, revision, err := r.observeInFlight(ctx, ro, cur)
	if err != nil {
		if !operrors.IsPermanent(err) {
			return err
		}
		return r.saveStatus(ctx, ro, message+"; release "+cur.Release+": "+errorMessage(err))
	}
	if obs.State == argocd.StateReady {
		setReleaseState(ro, cur.Release, done, revision)
		recordSpec(ro, cur.Release, obs.SpecDigest)
		ro.Status.Current = nil
		return r.saveStatus(ctx, ro, message)
	}
	setReleaseState(ro, cur.Release, v1alpha1.ReleaseSuspended, "")
	v.requeue = pollInterval
	return r.saveStatus(ctx, ro, message+"; release "+cur.Release+": "+obs.Reason)
}

// observeInFlight observes the step in flight without writing, with the
// release state and revision it records once ready.
func (r *RolloutReconciler) observeInFlight(ctx context.Context, ro *v1alpha1.RuntimeRollout,
	cur *v1alpha1.CurrentStep) (argocd.Observation, v1alpha1.ReleaseState, string, error) {

	idx := slices.IndexFunc(ro.Spec.Steps, func(s v1alpha1.RolloutStep) bool { return s.Release == cur.Release })
	if idx < 0 {
		return argocd.Observation{}, "", "", operrors.New(operrors.ErrCodeInvalidRequest, "step "+cur.Release+" is not in the rollout")
	}
	step := ro.Spec.Steps[idx]
	if step.Operation == v1alpha1.OperationRemove {
		obs, err := r.Engine.Retired(ctx, step.Release, step.UID)
		return obs, v1alpha1.ReleaseRemoved, "", err
	}
	p, err := readPlan(ctx, r.Reader, r.Config.OperatorNamespace, ro.Spec.ToPlanDigest)
	if err != nil {
		return argocd.Observation{}, "", "", err
	}
	rel := release(p, step.Release)
	if rel == nil {
		return argocd.Observation{}, "", "", operrors.New(operrors.ErrCodeInvalidRequest, "step "+step.Release+" is not in the target plan")
	}
	values, err := r.values(ctx, rel)
	if err != nil {
		return argocd.Observation{}, "", "", err
	}
	obs, err := r.Engine.Observe(ctx, p, rel, values, cur.StartedAt.Time, syncs(step, rel))
	return obs, v1alpha1.ReleaseReady, rel.Chart.Digest, err
}

// syncs reports whether a step needs an explicit sync: a Verify step, and
// every step of a readiness release. Argo CD starts no sync for a change
// that touches only hook resources, and the gate Job is a hook, so only a
// sync started in the step proves the gate ran for it.
func syncs(step v1alpha1.RolloutStep, rel *plan.Release) bool {
	return step.Operation == v1alpha1.OperationVerify || rel.Phase == plan.PhaseReadiness
}

// written reports whether a step's Application already carries the target
// plan exactly, as when the operator stopped after the write and before
// recording it: the step observes instead of writing again. Steps
// that sync always sync.
func (r *RolloutReconciler) written(ctx context.Context, p *plan.Document, rel *plan.Release, step v1alpha1.RolloutStep,
	values []byte, now time.Time) (bool, error) {

	if syncs(step, rel) {
		return false, nil
	}
	obs, err := r.Engine.Observe(ctx, p, rel, values, now, false)
	if err != nil {
		if operrors.IsPermanent(err) {
			return false, nil
		}
		return false, err
	}
	return obs.State != argocd.StateMissing && obs.Reason != argocd.ReasonSpecNotWritten && obs.Reason != argocd.ReasonSpecDrifted, nil
}

// mutate performs a step's one engine write: the Application with a sync
// operation when the step needs one, otherwise an Ensure.
func (r *RolloutReconciler) mutate(ctx context.Context, p *plan.Document, rel *plan.Release, step v1alpha1.RolloutStep, values []byte) error {
	if syncs(step, rel) {
		return r.Engine.Verify(ctx, p, rel, values)
	}
	ensured, err := r.Engine.Ensure(ctx, p, rel, values)
	if err != nil {
		return err
	}
	r.overridden(ensured, p.DesiredStateDigest)
	return nil
}

// remove starts or observes a Remove step: the Application recorded in the
// step is deleted with its resources, and the step is done once it is gone.
func (r *RolloutReconciler) remove(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout,
	step v1alpha1.RolloutStep, now time.Time, stop *halt, v *runtimeView) (bool, error) {

	// fail ends the rollout on a permanent error. Only a release whose delete
	// was issued is Failed, which keeps repair from writing it again; one
	// whose delete never landed is Pending, its Application as it was.
	fail := func(err error, issued bool) (bool, error) {
		if !operrors.IsPermanent(err) {
			return false, err
		}
		state := v1alpha1.ReleasePending
		if issued {
			state = v1alpha1.ReleaseFailed
		}
		setReleaseState(ro, step.Release, state, "")
		return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, errorMessage(err))
	}
	cur := ro.Status.Current
	if cur == nil || cur.Release != step.Release || cur.StartedAt == nil {
		if stop != nil {
			// Only a removal already made by this rollout finishes.
			obs, err := r.Engine.Retired(ctx, step.Release, step.UID)
			if err != nil {
				return fail(err, false)
			}
			if obs.State != argocd.StateReady {
				return false, r.finish(ctx, ro, stop.phase, stop.message)
			}
		}
		start := metav1.NewTime(now)
		ro.Status.Current = &v1alpha1.CurrentStep{Release: step.Release, Operation: step.Operation, StartedAt: &start}
		setReleaseState(ro, step.Release, v1alpha1.ReleaseDeleting, "")
		if err := r.saveStatus(ctx, ro, ""); err != nil {
			return false, err
		}
		if err := r.Engine.Remove(ctx, string(rt.UID), step.Release, step.UID, ro.Name); err != nil {
			return fail(err, false)
		}
		v.requeue = pollInterval
		return false, nil
	}
	obs, err := r.Engine.Retired(ctx, step.Release, step.UID)
	issued := true
	if err == nil && obs.Reason == "DeletePending" {
		// The mark or the delete did not land: issue it again.
		err, issued = r.Engine.Remove(ctx, string(rt.UID), step.Release, step.UID, ro.Name), false
	}
	if err != nil {
		if ended, finishErr := r.timedOut(ctx, ro, step, cur, now, "removed", err); ended {
			return false, finishErr
		}
		return fail(err, issued)
	}
	switch {
	case obs.State == argocd.StateReady:
		setReleaseState(ro, step.Release, v1alpha1.ReleaseRemoved, "")
		ro.Status.Current = nil
		return true, r.saveStatus(ctx, ro, "")
	case now.Sub(timeoutFrom(cur)) > ro.Spec.ActionTimeout.Duration:
		setReleaseState(ro, step.Release, v1alpha1.ReleaseFailed, "")
		return false, r.finish(ctx, ro, v1alpha1.PhaseFailed,
			fmt.Sprintf("release %s not removed within %s (%s)", step.Release, ro.Spec.ActionTimeout.Duration, obs.Reason))
	}
	v.requeue = pollInterval
	return false, r.saveStatus(ctx, ro, "release "+step.Release+": "+obs.Reason)
}

// reconcileFailed records a Warning Event on the runtime for a reconcile that
// ended in an error, other than shutdown. The runtime's conditions keep what
// was last observed: a failed reconcile observed nothing to replace them
// with, and one transient error must not flip Ready.
func (r *RolloutReconciler) reconcileFailed(rt *v1alpha1.ClusterRuntime, err error) {
	if r.Events == nil || operrors.CodeOf(err) == operrors.ErrCodeCanceled {
		return
	}
	emit(r.Events, rt, corev1.EventTypeWarning, EventReconcileFailed, "Reconcile", "%s", errorMessage(err))
}

// overridden records an Event on an Application whose fields other managers
// had changed, naming the managers, never the values.
func (r *RolloutReconciler) overridden(e *argocd.Ensured, planDigest string) {
	if r.Events == nil || e == nil || len(e.Overridden) == 0 {
		return
	}
	emit(r.Events, e.Application, corev1.EventTypeWarning, EventFieldOwnershipOverridden, "Ensure",
		"forced fields owned by %s back to plan %s", strings.Join(e.Overridden, ", "), planDigest)
}

// approval evaluates the set-once approval. Without one, only the
// development bypass lets a rollout that requires approval proceed.
func (r *RolloutReconciler) approval(ro *v1alpha1.RuntimeRollout, now time.Time) v1alpha1.ApprovalState {
	a := ro.Spec.Approval
	switch {
	case !ro.Spec.ApprovalRequired:
		return v1alpha1.ApprovalNotRequired
	case a != nil && a.RevokedBy != "":
		return v1alpha1.ApprovalRevoked
	case a != nil && now.After(a.ExpiresAt.Time):
		return v1alpha1.ApprovalExpired
	case a != nil:
		return v1alpha1.ApprovalApproved
	case r.Config.BypassApprovals:
		return v1alpha1.ApprovalBypassed
	default:
		return v1alpha1.ApprovalPending
	}
}

// finish ends a rollout: the durable outcome label first, then status.
func (r *RolloutReconciler) finish(ctx context.Context, ro *v1alpha1.RuntimeRollout, phase v1alpha1.RolloutPhase, message string) error {
	return r.finishWith(ctx, ro, phase, message, nil)
}

// retryRequested records on a stalled rollout the retry token that asked to
// supersede it, which consumes the token.
func (r *RolloutReconciler) retryRequested(ctx context.Context, ro *v1alpha1.RuntimeRollout, token string) error {
	if ro.Annotations[v1alpha1.AnnotationRetryRequested] == token {
		return nil
	}
	patch := client.MergeFrom(ro.DeepCopy())
	if ro.Annotations == nil {
		ro.Annotations = map[string]string{}
	}
	ro.Annotations[v1alpha1.AnnotationRetryRequested] = token
	if err := r.Client.Patch(ctx, ro, patch); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "record retry request on "+ro.Name)
	}
	return nil
}

// superseded ends a rollout Superseded, recording the token of the retry
// that superseded it, if any, in the outcome's patch: the token is consumed
// exactly when the rollout ends.
func (r *RolloutReconciler) superseded(ctx context.Context, ro *v1alpha1.RuntimeRollout, message, retry string) error {
	var annotations map[string]string
	if retry != "" {
		annotations = map[string]string{v1alpha1.AnnotationRetry: retry}
	}
	return r.finishWith(ctx, ro, v1alpha1.PhaseSuperseded, message, annotations)
}

// finishWith is finish recording annotations with the outcome, in the same
// patch: the evidence reference of a validated rollout, so it survives the
// loss of status, or the retry token that ended it. The outcome is counted
// once that patch lands, and its validation Jobs go.
func (r *RolloutReconciler) finishWith(ctx context.Context, ro *v1alpha1.RuntimeRollout, phase v1alpha1.RolloutPhase,
	message string, annotations map[string]string) error {

	if ro.Labels[v1alpha1.LabelOutcome] == "" {
		// The patch response carries the stored status; keep the one this
		// reconcile built.
		status := ro.Status.DeepCopy()
		patch := client.MergeFrom(ro.DeepCopy())
		if ro.Labels == nil {
			ro.Labels = map[string]string{}
		}
		ro.Labels[v1alpha1.LabelOutcome] = string(phase)
		if len(annotations) > 0 && ro.Annotations == nil {
			ro.Annotations = map[string]string{}
		}
		maps.Copy(ro.Annotations, annotations)
		if err := r.Client.Patch(ctx, ro, patch); err != nil {
			return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "record outcome of "+ro.Name)
		}
		ro.Status = *status
		rolloutOutcomes.WithLabelValues(string(phase)).Inc()
		if val := status.Validation; val != nil && (val.Result == resultPassed || val.Result == resultFailed) {
			validationRuns.WithLabelValues(strings.ToLower(val.Result)).Inc()
		}
	}
	ro.Status.Phase = phase
	ro.Status.Current = nil
	if err := r.saveStatus(ctx, ro, message); err != nil {
		return err
	}
	eventType := corev1.EventTypeNormal
	if phase != v1alpha1.PhaseSucceeded && phase != v1alpha1.PhaseSuperseded {
		eventType = corev1.EventTypeWarning
	}
	r.event(ro, eventType, "Rollout"+string(phase), "rollout %s to plan %s ended %s: %s", ro.Name, ro.Spec.ToPlanDigest, phase, orNone(message))
	if r.Config.ValidationEnabled {
		// Only one rollout is in progress, so every validation run of the
		// runtime is this rollout's or older; the TTL is the backstop.
		if err := r.pruneValidation(ctx, r.validationStages(), ro.Labels[v1alpha1.LabelRuntime], ""); err != nil {
			log.FromContext(ctx).Info("validation Job cleanup will be left to the TTL", "rollout", ro.Name, "error", err.Error())
		}
	}
	return nil
}

// event records an Event when a recorder is set.
func (r *RolloutReconciler) event(regarding runtime.Object, eventType, reason, note string, args ...any) {
	emit(r.Events, regarding, eventType, reason, "Reconcile", note, args...)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (r *RolloutReconciler) saveStatus(ctx context.Context, ro *v1alpha1.RuntimeRollout, message string) error {
	ro.Status.Message = bounded(message)
	ro.Status.ObservedGeneration = ro.Generation
	if err := r.Client.Status().Update(ctx, ro); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "update status of "+ro.Name)
	}
	return nil
}

// values reads a release's values through the uncached reader, verified
// against the digest the plan records.
func (r *RolloutReconciler) values(ctx context.Context, rel *plan.Release) ([]byte, error) {
	return readContent(ctx, r.Reader, r.Config.OperatorNamespace, rel.Values, plan.ValuesDataKey, "values")
}

// readContent reads a content-addressed ConfigMap, verified against the
// digest the plan records. A missing ConfigMap is NotFound and one that does
// not match is a Conflict: both permanent, since waiting restores neither.
func readContent(ctx context.Context, reader client.Reader, namespace string, ref plan.DigestedRef, key, what string) ([]byte, error) {
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.ConfigMap}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, operrors.Wrap(operrors.ErrCodeNotFound, what+" "+ref.ConfigMap+" is missing", err)
		}
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read "+what+" "+ref.ConfigMap)
	}
	data, err := plan.ReadContent(cm, key)
	if err != nil {
		return nil, err
	}
	if plan.DigestOf(data) != ref.Digest {
		return nil, operrors.New(operrors.ErrCodeConflict, what+" "+ref.ConfigMap+" does not match the plan")
	}
	return data, nil
}

// contentUnreadable stalls the runtime on plan content it can no longer
// read, so status keeps reporting it; a transient read error is returned.
func contentUnreadable(err error, v *runtimeView) error {
	if !operrors.IsPermanent(err) {
		return err
	}
	v.stalled, v.stalledReason, v.stalledMsg = true, reasonOf(err), errorMessage(err)
	v.readyReason, v.readyMessage, v.availableMessage = v.stalledReason, v.stalledMsg, v.stalledMsg
	return nil
}

// observeActive sets Ready and Available. Available observes the active
// plan's releases; a release a rollout moved, in progress or failed, is
// observed at the plan its live object carries, and one a rollout removes no
// longer counts. Ready reports the rollout's progress until it succeeds.
func (r *RolloutReconciler) observeActive(ctx context.Context, rt *v1alpha1.ClusterRuntime, target string, v *runtimeView) error {
	suspended := rt.Spec.SuspendEngine
	if suspended {
		// Argo CD neither syncs nor heals: nothing is repaired, and readiness
		// is not claimed for a stack nobody reconciles, but a release that
		// drifts or degrades is still named.
		v.readyReason, v.readyMessage = ReasonEngineSuspended, "automated engine syncs are suspended (spec.suspendEngine)"
		v.availableMessage = v.readyMessage
	}
	ro := v.inProgress
	if ro != nil && v.readyMessage == "" {
		// A pause or an engine suspension keeps its reason.
		if v.readyReason == "" {
			v.readyReason = ReasonRollingOut
		}
		v.readyMessage = progress(ro)
	}
	if v.active == "" {
		if v.readyReason == "" {
			v.readyReason, v.readyMessage = ReasonNoActivePlan, "no plan has been rolled out"
		}
		v.availableMessage = "no plan has been rolled out"
		return nil
	}
	p, err := readPlan(ctx, r.Reader, r.Config.OperatorNamespace, v.active)
	if operrors.CodeOf(err) == operrors.ErrCodeNotFound {
		p, err = r.restorePlan(ctx, rt, v.active)
	}
	if err != nil {
		if operrors.IsPermanent(err) {
			v.stalled, v.stalledReason, v.stalledMsg = true, reasonOf(err), errorMessage(err)
			v.availableMessage = v.stalledMsg
			return nil
		}
		return err
	}
	carried, err := r.carriedPlans(ctx, rt, v.active)
	if err != nil {
		return err
	}
	for i := range p.Releases {
		rel := &p.Releases[i]
		if (ro != nil && removing(ro, rel.ID)) || v.retired[rel.ID] {
			continue
		}
		values, err := r.values(ctx, rel)
		if err != nil {
			return contentUnreadable(err, v)
		}
		obs, err := r.Engine.Observe(ctx, p, rel, values, time.Time{}, false)
		moved := release(carried[rel.ID], rel.ID)
		// Repair writes the active plan's release, so a release observed at
		// the plan a rollout moved it to is never repaired.
		atMoved := err == nil && moved != nil && obs.Reason == argocd.ReasonSpecNotWritten
		if atMoved {
			var movedValues []byte
			if movedValues, err = r.values(ctx, moved); err != nil {
				return contentUnreadable(err, v)
			}
			obs, err = r.Engine.Observe(ctx, carried[rel.ID], moved, movedValues, time.Time{}, false)
		}
		if err != nil {
			if operrors.IsPermanent(err) {
				v.readyReason, v.readyMessage = reasonOf(err), errorMessage(err)
				v.availableMessage = v.readyMessage
				return nil
			}
			return err
		}
		lost := !atMoved && v.lostMove(rel.ID, obs)
		if v.rollout == "" && !suspended && !atMoved && !lost && (obs.State == argocd.StateMissing || obs.Reason == argocd.ReasonSpecDrifted) {
			if err = r.repair(ctx, p, rel, values, obs); err != nil {
				if operrors.IsPermanent(err) {
					v.readyReason, v.readyMessage = reasonOf(err), errorMessage(err)
					v.availableMessage = v.readyMessage
					return nil
				}
				return err
			}
			v.requeue = pollInterval
		}
		if obs.State == argocd.StateProgressing && inFlight(ro, rel.ID) {
			// The step in flight is expected to progress; its action
			// timeout, not Available, reports one that never settles.
			continue
		}
		if obs.State != argocd.StateReady {
			v.availableMessage = "release " + rel.ID + ": " + obs.Reason
			if suspended {
				v.readyMessage += "; " + v.availableMessage
				v.availableMessage = v.readyMessage
			}
			if v.readyReason == "" {
				v.readyReason, v.readyMessage = ReasonNotReady, v.availableMessage
			}
			return nil
		}
	}
	if !suspended {
		markAvailable(rt, target, v)
	}
	return nil
}

// markAvailable records every active release healthy, and Ready once the target
// plan is the active one, nothing is in progress and nothing stalls; a stall
// gives Ready its reason.
func markAvailable(rt *v1alpha1.ClusterRuntime, target string, v *runtimeView) {
	v.available = true
	rolledOut := target == v.active && v.rollout == "" && !v.reconciling
	switch {
	case rolledOut && v.stalled:
		v.readyReason, v.readyMessage = v.stalledReason, v.stalledMsg
	case rolledOut:
		v.ready, v.readyReason, v.readyMessage = true, ReasonRolledOut, "plan "+v.active+" is rolled out and current-revision ready"
		if rt.Spec.Validation.Deployment == v1alpha1.DeploymentValidationDisabled {
			v.readyReason, v.readyMessage = ReasonValidationDisabled, v.readyMessage+"; deployment validation is disabled"
		}
	case v.readyReason == "":
		v.readyReason, v.readyMessage = ReasonRollingOut, "the target plan is not rolled out yet"
		if v.reconcilingReason == ReasonStabilizing {
			v.readyMessage += "; its stabilization window has not passed"
		}
	}
}

// carriedPlans maps each release whose live object carries a plan other
// than the active one, as a rollout in progress or one that failed leaves
// the releases it moved, to that plan. A plan that cannot be read is left
// out, and its release reports SpecNotWritten.
func (r *RolloutReconciler) carriedPlans(ctx context.Context, rt *v1alpha1.ClusterRuntime, active string) (map[string]*plan.Document, error) {
	plans, carried := map[string]*plan.Document{}, map[string]*plan.Document{}
	inventory, err := r.Engine.Inventory(ctx, string(rt.UID))
	if err != nil {
		if operrors.IsPermanent(err) {
			return carried, nil
		}
		return nil, err
	}
	for _, it := range inventory {
		if it.PlanDigest == "" || it.PlanDigest == active {
			continue
		}
		doc, ok := plans[it.PlanDigest]
		if !ok {
			if doc, err = readPlan(ctx, r.Reader, r.Config.OperatorNamespace, it.PlanDigest); err != nil {
				if !operrors.IsPermanent(err) {
					return nil, err
				}
				doc = nil
			}
			plans[it.PlanDigest] = doc
		}
		if doc != nil {
			carried[it.ReleaseID] = doc
		}
	}
	return carried, nil
}

// progress names a rollout in progress and where it is, for Ready's message.
func progress(ro *v1alpha1.RuntimeRollout) string {
	name := "rollout " + ro.Name
	switch ro.Status.Phase {
	case v1alpha1.PhaseAwaitingApproval:
		return name + " awaiting approval"
	case v1alpha1.PhaseAwaitingAcknowledgement:
		return name + ": " + ro.Status.Message
	case v1alpha1.PhaseStalled:
		return name + " stalled: " + ro.Status.Message
	case v1alpha1.PhaseValidating:
		if val := ro.Status.Validation; val != nil && val.RunID != "" {
			return name + ": deployment validation run " + val.RunID
		}
		if ro.Status.Message != "" {
			// Validation cannot start, and the message says why.
			return name + ": " + ro.Status.Message
		}
		return name + ": deployment validation"
	case v1alpha1.PhasePending, v1alpha1.PhaseProgressing, v1alpha1.PhaseSucceeded, v1alpha1.PhaseFailed,
		v1alpha1.PhaseExpired, v1alpha1.PhaseRevoked, v1alpha1.PhaseSuperseded, v1alpha1.PhaseOrphaned:
		// Pending and Progressing report their step; a terminal rollout is
		// not in progress.
	}
	// The step in flight, or the next one: advance runs them in order.
	for i, step := range ro.Spec.Steps {
		if s := releaseState(ro, step.Release); s != v1alpha1.ReleaseReady && s != v1alpha1.ReleaseRemoved {
			return fmt.Sprintf("%s: step %d/%d %s (%s)", name, i+1, len(ro.Spec.Steps), step.Release, step.Operation)
		}
	}
	return fmt.Sprintf("%s: %d/%d steps done", name, len(ro.Spec.Steps), len(ro.Spec.Steps))
}

// inFlight reports the release of the rollout's step in flight.
func inFlight(ro *v1alpha1.RuntimeRollout, id string) bool {
	return ro != nil && ro.Status.Current != nil && ro.Status.Current.Release == id
}

// lostMove reports, with no rollout in progress, a moved release whose
// Application is gone, and stalls the runtime naming it: it has no live plan
// to be observed at, and the active plan's release would roll it back.
func (v *runtimeView) lostMove(id string, obs argocd.Observation) bool {
	m, ok := v.moved[id]
	if !ok || v.rollout != "" || m.plan == v.active || obs.State != argocd.StateMissing {
		return false
	}
	v.stalled, v.stalledReason = true, ReasonMovedReleaseMissing
	v.stalledMsg = fmt.Sprintf("release %s: its Application is gone, and rollout %s, which did not succeed, may have moved it "+
		"to plan %s; repairing it from the active plan would roll it back. Retry to restore it at the target plan, or return the "+
		"target to the active plan to restore it there through an approved rollout", id, m.rollout, m.plan)
	return true
}

// movedBy is the ended rollout that may have written a release, and the plan
// it targeted.
type movedBy struct{ rollout, plan string }

// leftBehind returns what rollouts ended since the newest success left behind,
// from rollouts newest first: the releases they left removing or removed,
// and the others they may have written to their target plan. The newest such
// rollout to touch a release decides, so a later one that installed it again
// clears an earlier removal. Moves come from rollout history rather than the
// live objects, so they survive an Application deleted by hand; a rollout
// restored without its status keeps them as Unknown (restoreOutcomes).
func leftBehind(rollouts []v1alpha1.RuntimeRollout) (map[string]bool, map[string]movedBy) {
	retired, moved, seen := map[string]bool{}, map[string]movedBy{}, map[string]bool{}
	for i := range rollouts {
		ro := &rollouts[i]
		outcome := ro.Labels[v1alpha1.LabelOutcome]
		if outcome == string(v1alpha1.PhaseSucceeded) {
			break
		}
		if outcome == "" {
			continue
		}
		for _, rel := range ro.Status.Releases {
			if seen[rel.ID] || rel.State == "" || rel.State == v1alpha1.ReleasePending {
				continue
			}
			seen[rel.ID] = true
			if removing(ro, rel.ID) {
				retired[rel.ID] = true
			} else {
				moved[rel.ID] = movedBy{rollout: ro.Name, plan: ro.Spec.ToPlanDigest}
			}
		}
	}
	return retired, moved
}

// removing reports a release whose Remove step started: its Application is
// being deleted, possibly while halted, or is gone, or the step failed after
// its delete was issued, as at its action timeout.
func removing(ro *v1alpha1.RuntimeRollout, id string) bool {
	switch releaseState(ro, id) {
	case v1alpha1.ReleaseDeleting, v1alpha1.ReleaseRemoved:
		return true
	case v1alpha1.ReleaseSuspended:
		return inFlight(ro, id) && ro.Status.Current.Operation == v1alpha1.OperationRemove
	case v1alpha1.ReleaseFailed:
		return slices.ContainsFunc(ro.Spec.Steps, func(s v1alpha1.RolloutStep) bool {
			return s.Release == id && s.Operation == v1alpha1.OperationRemove
		})
	case v1alpha1.ReleasePending, v1alpha1.ReleaseProgressing, v1alpha1.ReleaseReady, v1alpha1.ReleaseUnknown:
		return false
	default:
		return false
	}
}

// repair writes an active release's Application again when it was deleted
// by hand or changed by someone else (drift repair continues while
// paused). Only Applications that carry the active plan, or none, are
// repaired: a release a failed rollout already moved is never rolled back.
func (r *RolloutReconciler) repair(ctx context.Context, p *plan.Document, rel *plan.Release, values []byte, obs argocd.Observation) error {
	ensured, err := r.Engine.Ensure(ctx, p, rel, values)
	if err != nil {
		return err
	}
	log.FromContext(ctx).Info("repaired engine drift", "release", rel.ID, "reason", obs.Reason, "overridden", ensured.Overridden)
	if obs.State == argocd.StateMissing && r.Events != nil {
		emit(r.Events, ensured.Application, corev1.EventTypeWarning, EventApplicationRecreated, "Ensure",
			"the Application of plan %s was deleted by hand and written again; its resources were left in place", p.DesiredStateDigest)
	}
	r.overridden(ensured, p.DesiredStateDigest)
	return nil
}

// applyStatus applies the rollout controller's runtime status fields.
func (r *RolloutReconciler) applyStatus(ctx context.Context, rt *v1alpha1.ClusterRuntime, v *runtimeView) error {
	boolStatus := func(b bool) metav1.ConditionStatus {
		if b {
			return metav1.ConditionTrue
		}
		return metav1.ConditionFalse
	}
	availableReason, availableMessage := ReasonHealthy, v.availableMessage
	if !v.available {
		availableReason = ReasonNotReady
		if v.active == "" {
			availableReason = ReasonNoActivePlan
		}
		if availableMessage == "" {
			// Nothing was observed: the engine is unavailable, objects wait
			// for adoption, or the runtime is being deleted.
			availableMessage = cmp.Or(v.readyMessage, v.stalledMsg, "the releases were not observed")
		}
	}
	readyReason := v.readyReason
	if readyReason == "" {
		// A stall found while observing the active plan sets no Ready reason.
		readyReason = ReasonNotReady
		if v.stalled {
			readyReason = v.stalledReason
		}
	}
	reconcilingReason := v.reconcilingReason
	if reconcilingReason == "" {
		reconcilingReason = ReasonRolledOut
	}
	stalledReason := v.stalledReason
	if stalledReason == "" {
		stalledReason = "Progressing"
	}
	conditions := []any{
		condition(rt, v1alpha1.ConditionReady, boolStatus(v.ready), readyReason, v.readyMessage),
		condition(rt, v1alpha1.ConditionAvailable, boolStatus(v.available), availableReason, availableMessage),
		condition(rt, v1alpha1.ConditionReconciling, boolStatus(v.reconciling), reconcilingReason, ""),
		condition(rt, v1alpha1.ConditionStalled, boolStatus(v.stalled), stalledReason, v.stalledMsg),
		condition(rt, v1alpha1.ConditionEngineAvailable, boolStatus(v.engineOK), v.engineReason, v.engineMessage),
	}
	status := map[string]any{"observedGeneration": rt.Generation, "conditions": conditions}
	if v.active != "" {
		status["activePlanDigest"] = v.active
	}
	if v.rollout != "" {
		status["rollout"] = v.rollout
	}
	before := meta.FindStatusCondition(rt.Status.Conditions, v1alpha1.ConditionStalled)
	if err := applyRuntimeStatus(ctx, r.Client, rt, status, RolloutFieldManager); err != nil {
		return err
	}
	if v.stalled && r.Events != nil && (before == nil || before.Status != metav1.ConditionTrue || before.Reason != stalledReason) {
		// Each stall, and each new cause, is a durable record.
		emit(r.Events, rt, corev1.EventTypeWarning, EventStalled, "Stall", "%s: %s", stalledReason, v.stalledMsg)
	}
	return nil
}

func release(p *plan.Document, id string) *plan.Release {
	if p == nil {
		return nil
	}
	for i := range p.Releases {
		if p.Releases[i].ID == id {
			return &p.Releases[i]
		}
	}
	return nil
}

func releaseState(ro *v1alpha1.RuntimeRollout, id string) v1alpha1.ReleaseState {
	for _, s := range ro.Status.Releases {
		if s.ID == id {
			return s.State
		}
	}
	return v1alpha1.ReleasePending
}

// recordSpec records the spec fingerprint a release had when it became
// ready, which is the spec this rollout wrote; inventoryDrift compares
// later observations with it. An empty digest records nothing.
func recordSpec(ro *v1alpha1.RuntimeRollout, id, digest string) {
	if digest == "" {
		return
	}
	for i := range ro.Status.Releases {
		if ro.Status.Releases[i].ID == id {
			ro.Status.Releases[i].SpecDigest = digest
			return
		}
	}
}

func setReleaseState(ro *v1alpha1.RuntimeRollout, id string, state v1alpha1.ReleaseState, revision string) {
	for i := range ro.Status.Releases {
		if ro.Status.Releases[i].ID == id {
			ro.Status.Releases[i].State, ro.Status.Releases[i].Revision = state, revision
			return
		}
	}
	ro.Status.Releases = append(ro.Status.Releases, v1alpha1.ReleaseStatus{ID: id, State: state, Revision: revision})
}
