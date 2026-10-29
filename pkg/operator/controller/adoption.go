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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Re-adoption reasons.
const (
	// ReasonOrphanedObjectsPresent: engine objects of another runtime UID
	// exist and spec.adoption does not name it; the runtime never adopts
	// silently and rolls nothing out meanwhile.
	ReasonOrphanedObjectsPresent = "OrphanedObjectsPresent"
	// ReasonAdoptionBlocked: the named runtime's objects do not rebuild to a
	// plan, or were edited out of band.
	ReasonAdoptionBlocked = "AdoptionBlocked"
)

// isAdopt reports an Adopt rollout, which records the earlier runtime UID.
func isAdopt(ro *v1alpha1.RuntimeRollout) bool {
	return ro.Annotations[v1alpha1.AnnotationAdoptedFrom] != ""
}

// adoption handles engine objects of other runtimes before anything rolls
// out. Without spec.adoption naming them, the runtime stalls with
// OrphanedObjectsPresent. When it names them, the plan is rebuilt from the
// objects alone, checked, written with provenance Rebuilt, and an Adopt
// rollout that requires approval moves them to this runtime. It reports
// whether the runtime must wait.
func (r *RolloutReconciler) adoption(ctx context.Context, rt *v1alpha1.ClusterRuntime, rollouts []v1alpha1.RuntimeRollout,
	last *v1alpha1.RuntimeRollout, v *runtimeView) (bool, error) {

	foreign, err := r.Engine.Foreign(ctx, string(rt.UID))
	if err != nil || len(foreign) == 0 {
		return false, err
	}
	want := ""
	if rt.Spec.Adoption != nil {
		want = rt.Spec.Adoption.RuntimeUID
	}
	others := make([]string, 0, len(foreign))
	for owner, names := range foreign {
		if owner != want {
			others = append(others, fmt.Sprintf("%s (%s)", owner, strings.Join(names, ", ")))
		}
	}
	sort.Strings(others)
	if len(others) > 0 || len(foreign[want]) == 0 {
		msg := "engine objects of other runtimes are present: " + strings.Join(others, "; ") +
			"; set spec.adoption.runtimeUID to adopt them, or delete them"
		if v.stalled {
			// A stalled resolution keeps its reason: hiding it would leave the
			// runtime waiting on a failure nobody sees.
			v.stalledMsg += "; " + msg
			return true, nil
		}
		v.stalled, v.stalledReason, v.stalledMsg = true, ReasonOrphanedObjectsPresent, msg
		return true, nil
	}
	if last != nil && isAdopt(last) && failedOutcome(last) && retryToken(rt, rollouts) == "" {
		v.stalled, v.stalledReason, v.stalledMsg = true, "Rollout"+last.Labels[v1alpha1.LabelOutcome], last.Status.Message
		return true, nil
	}
	blocked := func(msg string) (bool, error) {
		v.stalled, v.stalledReason, v.stalledMsg = true, ReasonAdoptionBlocked, msg
		return true, nil
	}
	// An Adopt rollout that ended after moving some objects leaves them
	// with this runtime: the plan and the starting inventory span both sets,
	// so a retry adopts the rest and treats the moved ones as done.
	doc, contents, err := r.Engine.RebuildLive(ctx, want, string(rt.UID))
	if err != nil {
		if operrors.IsPermanent(err) {
			return blocked("the engine objects of runtime " + want + " do not rebuild to a plan: " + errorMessage(err))
		}
		return false, err
	}
	inventory, err := r.bothInventories(ctx, string(rt.UID), want)
	if err != nil {
		return false, err
	}
	written := map[string][]string{}
	for _, it := range inventory {
		written[it.PlanDigest] = append(written[it.PlanDigest], it.ReleaseID)
	}
	if _, same := written[doc.DesiredStateDigest]; len(written) == 1 && !same {
		return blocked(fmt.Sprintf("the engine objects of runtime %s were written by plan %s but rebuild to %s: an object was edited out of band",
			want, inventory[0].PlanDigest, doc.DesiredStateDigest))
	}
	doc.RuntimeUID = string(rt.UID)
	if err = r.writeRebuilt(ctx, rt, doc, contents); err != nil {
		return false, err
	}
	return true, r.createAdopt(ctx, rt, rollouts, want, doc, inventory, written, v)
}

// writeRebuilt stores a rebuilt plan and its values for this runtime.
func (r *RolloutReconciler) writeRebuilt(ctx context.Context, rt *v1alpha1.ClusterRuntime, doc *plan.Document, contents []*plan.Content) error {
	for _, c := range contents {
		cm, err := c.ConfigMap(r.Config.OperatorNamespace, rt.Name, string(rt.UID), nil)
		if err != nil {
			return err
		}
		if err = createOnce(ctx, r.Client, r.Reader, cm, v1alpha1.AnnotationDigest, c.Digest); err != nil {
			return err
		}
	}
	cm, err := plan.ToConfigMap(doc, r.Config.OperatorNamespace, rt.Name)
	if err != nil {
		return err
	}
	return createPlanOnce(ctx, r.Client, r.Reader, cm)
}

// createAdopt creates the Adopt rollout: one Adopt step per release, whose
// only mutation is moving the object to this runtime; it requires approval.
func (r *RolloutReconciler) createAdopt(ctx context.Context, rt *v1alpha1.ClusterRuntime, rollouts []v1alpha1.RuntimeRollout,
	from string, doc *plan.Document, inventory []argocd.InventoryItem, written map[string][]string, v *runtimeView) error {

	invJSON, err := json.Marshal(inventory)
	if err != nil {
		return operrors.Wrap(operrors.ErrCodeInternal, "encode inventory", err)
	}
	steps := make([]v1alpha1.RolloutStep, 0, len(doc.Releases))
	for i := range doc.Releases {
		steps = append(steps, v1alpha1.RolloutStep{Release: doc.Releases[i].ID, Operation: v1alpha1.OperationAdopt,
			Assessment: v1alpha1.AssessmentNotApplicable})
	}
	target := doc.DesiredStateDigest
	transition := strings.TrimPrefix(plan.DigestOf([]byte(strings.Join([]string{string(rt.UID), "", target}, "\n"))), "sha256:")[:16]
	deadline, actionTimeout := defaults.RolloutDeadline, defaults.RolloutActionTimeout
	if d := rt.Spec.Rollout.Deadline; d != nil {
		deadline = d.Duration
	}
	if d := rt.Spec.Rollout.ActionTimeout; d != nil {
		actionTimeout = d.Duration
	}
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("rollout-%s-%d", transition, nextSuffix(rollouts, transition)),
			Labels: map[string]string{
				v1alpha1.LabelManagedBy:  v1alpha1.ManagedByValue,
				v1alpha1.LabelRuntime:    rt.Name,
				v1alpha1.LabelToPlan:     strings.TrimPrefix(target, "sha256:")[:16],
				v1alpha1.LabelTransition: transition,
			},
			Annotations: map[string]string{v1alpha1.AnnotationAdoptedFrom: from},
		},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion:           v1alpha1.RolloutSchemaVersion,
			RuntimeUID:              string(rt.UID),
			ToPlanDigest:            target,
			StartingInventoryDigest: plan.DigestOf(invJSON),
			StartingInventory:       startingInventory(inventory),
			Deadline:                metav1.NewTime(r.Now().Add(deadline)),
			ActionTimeout:           metav1.Duration{Duration: actionTimeout},
			ApprovalRequired:        true,
			Steps:                   steps,
		},
	}
	if token := retryToken(rt, rollouts); token != "" {
		ro.Annotations[v1alpha1.AnnotationRetry] = token
	}
	switch err = r.Client.Create(ctx, ro); {
	case apierrors.IsAlreadyExists(err):
	case err != nil:
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create Adopt rollout "+ro.Name)
	default:
		r.event(rt, corev1.EventTypeNormal, "RolloutCreated", "Adopt rollout %s moves %d objects of runtime %s to plan %s; %s",
			ro.Name, len(steps), from, target, writtenBy(written))
	}
	v.rollout, v.reconciling, v.reconcilingReason, v.requeue = ro.Name, true, ReasonRollingOut, pollInterval
	return nil
}

// writtenBy lists which plan wrote which objects: one plan, or a mix left
// by a rollout that was orphaned mid-way, which the approver sees.
func writtenBy(written map[string][]string) string {
	plans := make([]string, 0, len(written))
	for digest, ids := range written {
		sort.Strings(ids)
		plans = append(plans, fmt.Sprintf("%s: %s", orNone(digest), strings.Join(ids, ", ")))
	}
	sort.Strings(plans)
	if len(plans) == 1 {
		return "every object was written by " + plans[0]
	}
	return "the objects were written by different plans: " + strings.Join(plans, "; ")
}

// adopt runs one Adopt step: the object recorded in the starting inventory
// moves to this runtime. An object already moved is done; a halt stops
// before an object that has not moved.
func (r *RolloutReconciler) adopt(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout,
	step v1alpha1.RolloutStep, stop *halt) (bool, error) {

	start, ok := recordedInventory(ro)
	entry, found := start[step.Release]
	if !ok || !found {
		return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, "release "+step.Release+" is not in the rollout's starting inventory")
	}
	live, err := r.Engine.Inventory(ctx, string(rt.UID))
	if err != nil {
		return false, err
	}
	moved := false
	for _, it := range live {
		moved = moved || (it.ReleaseID == step.Release && it.UID == entry.UID)
	}
	if !moved {
		if stop != nil {
			return false, r.finish(ctx, ro, stop.phase, stop.message)
		}
		if err = r.Engine.Adopt(ctx, ro.Annotations[v1alpha1.AnnotationAdoptedFrom], string(rt.UID), entry.Name, entry.UID); err != nil {
			if operrors.IsPermanent(err) {
				setReleaseState(ro, step.Release, v1alpha1.ReleaseFailed, "")
				return false, r.finish(ctx, ro, v1alpha1.PhaseFailed, errorMessage(err))
			}
			return false, err
		}
	}
	setReleaseState(ro, step.Release, v1alpha1.ReleaseReady, entry.Revision)
	// Adoption changes only ownership, so the spec stays as it started.
	recordSpec(ro, step.Release, entry.SpecDigest)
	ro.Status.Current = nil
	return true, r.saveStatus(ctx, ro, "")
}

// adoptedInventory is the live inventory an Adopt rollout is judged by: the
// objects still owned by the earlier runtime and those already moved.
func (r *RolloutReconciler) adoptedInventory(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout) ([]argocd.InventoryItem, error) {
	return r.bothInventories(ctx, string(rt.UID), ro.Annotations[v1alpha1.AnnotationAdoptedFrom])
}

// bothInventories is this runtime's objects followed by those the earlier
// runtime still owns.
func (r *RolloutReconciler) bothInventories(ctx context.Context, uid, from string) ([]argocd.InventoryItem, error) {
	moved, err := r.Engine.Inventory(ctx, uid)
	if err != nil {
		return nil, err
	}
	left, err := r.Engine.Inventory(ctx, from)
	if err != nil {
		return nil, err
	}
	return append(moved, left...), nil
}
