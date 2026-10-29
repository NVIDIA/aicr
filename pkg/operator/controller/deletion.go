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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// ReasonRemovalFailed reports that the removal rollout of a runtime deleted
// with deletionPolicy Delete ended without removing everything; the cleanup
// finalizer stays until a retry succeeds.
const ReasonRemovalFailed = "RemovalFailed"

// removalDigest is the target of a runtime's removal rollout: no release.
// It names no plan ConfigMap.
func removalDigest(runtimeUID string) string {
	return plan.DigestOf([]byte("aicr-operator: remove every release of runtime " + runtimeUID))
}

// released is the view of a deleted runtime whose finalizer is gone.
func released() *runtimeView { return &runtimeView{released: true} }

// isRemoval reports a runtime's removal rollout.
func isRemoval(ro *v1alpha1.RuntimeRollout) bool {
	return ro.Spec.ToPlanDigest == removalDigest(ro.Spec.RuntimeUID)
}

// finalizerManager owns the cleanup finalizer. metadata.finalizers is a
// set, so a server-side apply adds or removes only this item: concurrent
// writers never conflict, and no other finalizer is touched.
const finalizerManager = "aicr-operator-cleanup"

// ensureFinalizer adds the cleanup finalizer to a live runtime, so that its
// deletion runs its deletion policy first.
func (r *RolloutReconciler) ensureFinalizer(ctx context.Context, rt *v1alpha1.ClusterRuntime) error {
	if controllerutil.ContainsFinalizer(rt, v1alpha1.FinalizerCleanup) {
		return nil
	}
	return r.applyFinalizer(ctx, rt, []string{v1alpha1.FinalizerCleanup}, "add the cleanup finalizer")
}

// releaseFinalizer lets the API server delete the runtime. It removes the
// finalizer by value with a JSON patch tested at its position, not by
// server-side apply: an apply removes an entry only if no other manager owns
// it, and a runtime restored with the finalizer has it owned elsewhere.
// A concurrent change fails the test, and the next reconcile reads again.
func (r *RolloutReconciler) releaseFinalizer(ctx context.Context, rt *v1alpha1.ClusterRuntime) error {
	i := slices.Index(rt.Finalizers, v1alpha1.FinalizerCleanup)
	if i < 0 {
		return nil
	}
	patch := fmt.Sprintf(`[{"op":"test","path":"/metadata/finalizers/%d","value":%q},{"op":"remove","path":"/metadata/finalizers/%d"}]`,
		i, v1alpha1.FinalizerCleanup, i)
	if err := r.Client.Patch(ctx, rt, client.RawPatch(types.JSONPatchType, []byte(patch))); client.IgnoreNotFound(err) != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "remove the cleanup finalizer")
	}
	return nil
}

func (r *RolloutReconciler) applyFinalizer(ctx context.Context, rt *v1alpha1.ClusterRuntime, finalizers []string, what string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("ClusterRuntime"))
	u.SetName(rt.Name)
	u.SetUID(rt.UID)
	u.SetFinalizers(finalizers)
	err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(finalizerManager), client.ForceOwnership)
	if client.IgnoreNotFound(err) != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, what)
	}
	return nil
}

// deleting runs a deleted runtime's deletion policy and removes the cleanup
// finalizer once it is done. It returns the view to report while a
// removal rollout runs; a released view has nothing to report.
func (r *RolloutReconciler) deleting(ctx context.Context, rt *v1alpha1.ClusterRuntime) (*runtimeView, error) {
	if !controllerutil.ContainsFinalizer(rt, v1alpha1.FinalizerCleanup) {
		return released(), nil
	}
	if rt.Spec.DeletionPolicy == v1alpha1.DeletionDelete {
		return r.removeRuntime(ctx, rt)
	}
	return released(), r.orphanRuntime(ctx, rt)
}

// orphanRuntime is deletionPolicy Orphan: start no step and wait for no
// engine operation, end any unfinished rollout Orphaned, annotate the engine
// objects with orphaned-from, and remove the finalizer. The engine keeps
// reconciling everything, and the objects are the record a later runtime
// adopts from.
func (r *RolloutReconciler) orphanRuntime(ctx context.Context, rt *v1alpha1.ClusterRuntime) error {
	rollouts, err := r.rollouts(ctx, rt)
	if err != nil {
		return err
	}
	for i := range rollouts {
		if ro := &rollouts[i]; ro.Labels[v1alpha1.LabelOutcome] == "" {
			if err = r.finish(ctx, ro, v1alpha1.PhaseOrphaned, "runtime "+rt.Name+" was deleted with deletionPolicy Orphan"); err != nil {
				return err
			}
		}
	}
	annotated, err := r.Engine.Orphan(ctx, string(rt.UID))
	if err != nil {
		return err
	}
	r.event(rt, corev1.EventTypeNormal, "RuntimeOrphaned", "%d engine objects keep reconciling, annotated orphaned-from %s: %s",
		len(annotated), rt.UID, orNone(strings.Join(annotated, ", ")))
	return r.releaseFinalizer(ctx, rt)
}

// removeRuntime is deletionPolicy Delete: a removal rollout of Remove steps
// over the live objects, in reverse order of the active plan, that requires
// approval and supersedes any rollout in progress at its next step boundary.
// Only a removal that retired everything removes the finalizer; a failed
// one stalls the runtime until a retry token makes a new one.
func (r *RolloutReconciler) removeRuntime(ctx context.Context, rt *v1alpha1.ClusterRuntime) (*runtimeView, error) {
	v := &runtimeView{requeue: resyncInterval, engineOK: true, engineReason: ReasonEngineAvailable}
	if err := r.Engine.Capabilities(ctx); err != nil {
		v.engineOK, v.engineReason, v.engineMessage = false, reasonOf(err), errorMessage(err)
		v.stalled, v.stalledReason, v.stalledMsg = true, v.engineReason, v.engineMessage
		keepObserved(rt, v)
		return v, nil
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
	v.readyReason, v.readyMessage = "Deleting", "runtime "+rt.Name+" is being deleted with deletionPolicy Delete"
	retry := retryToken(rt, rollouts)
	switch {
	case current != nil:
		v.rollout, v.reconciling, v.reconcilingReason = current.Name, true, ReasonRollingOut
		supersede := ""
		if !isRemoval(current) {
			supersede = "superseded by the deletion of runtime " + rt.Name
		}
		if err = r.advance(ctx, rt, current, v, supersede, ""); err != nil {
			return nil, err
		}
		if isRemoval(current) && current.Status.Phase == v1alpha1.PhaseSucceeded {
			return released(), r.releaseFinalizer(ctx, rt)
		}
		return v, nil
	case last != nil && isRemoval(last) && last.Labels[v1alpha1.LabelOutcome] == string(v1alpha1.PhaseSucceeded):
		return released(), r.releaseFinalizer(ctx, rt)
	case last != nil && isRemoval(last) && retry == "":
		v.stalled, v.stalledReason = true, ReasonRemovalFailed
		v.stalledMsg = fmt.Sprintf("removal rollout %s ended %s; the cleanup finalizer stays until a retry: %s",
			last.Name, last.Labels[v1alpha1.LabelOutcome], last.Status.Message)
		return v, nil
	}
	inventory, err := r.Engine.Inventory(ctx, string(rt.UID))
	if err != nil {
		return nil, err
	}
	if len(inventory) == 0 {
		return released(), r.releaseFinalizer(ctx, rt)
	}
	if err = r.createRemoval(ctx, rt, rollouts, active, inventory, retry, v); err != nil {
		return nil, err
	}
	return v, nil
}

// createRemoval creates the removal rollout, named as every rollout is.
func (r *RolloutReconciler) createRemoval(ctx context.Context, rt *v1alpha1.ClusterRuntime, rollouts []v1alpha1.RuntimeRollout,
	active string, inventory []argocd.InventoryItem, retry string, v *runtimeView) error {

	var from *plan.Document
	if active != "" {
		doc, err := readPlan(ctx, r.Reader, r.Config.OperatorNamespace, active)
		switch {
		case err == nil:
			from = doc
		case !operrors.IsPermanent(err):
			return err
		}
	}
	invJSON, err := json.Marshal(inventory)
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "encode inventory", err)
	}
	target := removalDigest(string(rt.UID))
	transition := strings.TrimPrefix(plan.DigestOf([]byte(strings.Join([]string{string(rt.UID), active, target}, "\n"))), "sha256:")[:16]
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("rollout-%s-%d", transition, nextSuffix(rollouts, transition)),
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
			StartingInventoryDigest: plan.DigestOf(invJSON),
			StartingInventory:       startingInventory(inventory),
			Deadline:                metav1.NewTime(r.Now().Add(defaults.RolloutDeadline)),
			ActionTimeout:           metav1.Duration{Duration: defaults.RolloutActionTimeout},
			// Removing a component requires approval.
			ApprovalRequired: true,
			Steps:            steps(&plan.Document{}, from, inventory),
		},
	}
	if d := rt.Spec.Rollout.Deadline; d != nil {
		ro.Spec.Deadline = metav1.NewTime(r.Now().Add(d.Duration))
	}
	if d := rt.Spec.Rollout.ActionTimeout; d != nil {
		ro.Spec.ActionTimeout = metav1.Duration{Duration: d.Duration}
	}
	if retry != "" {
		ro.Annotations = map[string]string{v1alpha1.AnnotationRetry: retry}
	}
	switch err = r.Client.Create(ctx, ro); {
	case apierrors.IsAlreadyExists(err):
	case err != nil:
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create removal rollout "+ro.Name)
	default:
		r.event(rt, corev1.EventTypeNormal, "RolloutCreated", "removal rollout %s of %d releases for the deletion of runtime %s",
			ro.Name, len(ro.Spec.Steps), rt.Name)
	}
	v.rollout, v.reconciling, v.reconcilingReason, v.requeue = ro.Name, true, ReasonRollingOut, pollInterval
	return nil
}
