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
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// ReasonInventoryDrift reports that the live engine objects changed out of
// band since the rollout was created and approved.
const ReasonInventoryDrift = "InventoryDrift"

// halt is a revocation or an expired approval: no further step starts, and
// the rollout ends with phase once the step in flight has finished.
type halt struct {
	phase   v1alpha1.RolloutPhase
	message string
}

// halted returns the halt an approval state calls for, if any.
func halted(state v1alpha1.ApprovalState) *halt {
	switch state {
	case v1alpha1.ApprovalRevoked:
		return &halt{phase: v1alpha1.PhaseRevoked, message: "the approval was revoked"}
	case v1alpha1.ApprovalExpired:
		return &halt{phase: v1alpha1.PhaseExpired, message: "the approval expired"}
	case v1alpha1.ApprovalNotRequired, v1alpha1.ApprovalPending, v1alpha1.ApprovalApproved, v1alpha1.ApprovalBypassed:
		return nil
	default:
		return nil
	}
}

// startingInventory converts the live inventory for the rollout spec.
func startingInventory(items []argocd.InventoryItem) []v1alpha1.InventoryEntry {
	if len(items) == 0 {
		return nil
	}
	out := make([]v1alpha1.InventoryEntry, len(items))
	for i, it := range items {
		out[i] = v1alpha1.InventoryEntry{ReleaseID: it.ReleaseID, Name: it.Name, UID: it.UID, PlanDigest: it.PlanDigest, Revision: it.Revision,
			SpecDigest: it.SpecDigest}
	}
	return out
}

// recordedInventory returns the rollout's starting inventory when the spec
// records it: the entries digest to startingInventoryDigest. A rollout
// created before the inventory was recorded has none to compare.
func recordedInventory(ro *v1alpha1.RuntimeRollout) (map[string]v1alpha1.InventoryEntry, bool) {
	items := make([]argocd.InventoryItem, 0, len(ro.Spec.StartingInventory))
	for _, e := range ro.Spec.StartingInventory {
		items = append(items, argocd.InventoryItem{ReleaseID: e.ReleaseID, Name: e.Name, UID: e.UID, PlanDigest: e.PlanDigest, Revision: e.Revision,
			SpecDigest: e.SpecDigest})
	}
	candidates := [][]argocd.InventoryItem{items}
	if len(items) == 0 {
		candidates = append(candidates, nil)
	}
	for _, c := range candidates {
		raw, err := json.Marshal(c)
		if err == nil && plan.DigestOf(raw) == ro.Spec.StartingInventoryDigest {
			out := make(map[string]v1alpha1.InventoryEntry, len(items))
			for _, e := range ro.Spec.StartingInventory {
				out[e.ReleaseID] = e
			}
			return out, true
		}
	}
	return nil, false
}

// inventoryDrift compares the live inventory with the rollout's starting
// inventory plus its completed steps. A release outside the steps
// must be as it started; a completed step's release must be at its end
// state; a pending step's release may be either, as after a write whose
// record a restart lost. The release of the step in flight is not judged.
// The engine's observed revision is not compared: it follows the spec.
// "As it started" includes the spec fingerprint the starting inventory
// recorded, and a completed step's end state the fingerprint recorded when
// it became ready; a missing fingerprint is not compared.
func inventoryDrift(ro *v1alpha1.RuntimeRollout, live []argocd.InventoryItem) []string {
	start, ok := recordedInventory(ro)
	if !ok {
		return nil
	}
	var drift []string
	now := map[string]argocd.InventoryItem{}
	for _, it := range live {
		if _, dup := now[it.ReleaseID]; dup {
			drift = append(drift, it.ReleaseID+" has more than one object")
			continue
		}
		now[it.ReleaseID] = it
	}
	steps := map[string]v1alpha1.RolloutStep{}
	releases := map[string]bool{}
	for _, s := range ro.Spec.Steps {
		steps[s.Release], releases[s.Release] = s, true
	}
	for id := range start {
		releases[id] = true
	}
	for id := range now {
		releases[id] = true
	}
	ids := make([]string, 0, len(releases))
	for id := range releases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if cur := ro.Status.Current; cur != nil && cur.Release == id {
			continue
		}
		s, started := start[id]
		l, present := now[id]
		identical := started && present && l.UID == s.UID && l.Name == s.Name && l.PlanDigest == s.PlanDigest
		original := started == present && (!started || (identical && sameSpec(s.SpecDigest, l.SpecDigest)))
		edited := identical && !sameSpec(s.SpecDigest, l.SpecDigest)
		step, stepped := steps[id]
		var ok bool
		switch state := releaseState(ro, id); {
		case !stepped:
			ok = original
		case state == v1alpha1.ReleaseReady || state == v1alpha1.ReleaseRemoved:
			ok = ended(ro, step, s, started, l, present)
			if written := writtenSpec(ro, id); ok && present && !sameSpec(written, l.SpecDigest) {
				ok, edited = false, true
			}
		default:
			// Not written by this rollout yet, so a changed spec on the
			// object it started from is an edit even when that object is
			// already at the target plan, as on a retry.
			ok = !edited && (original || ended(ro, step, s, started, l, present))
		}
		if !ok {
			drift = append(drift, describeDrift(id, s, started, l, present, edited))
		}
	}
	return drift
}

// sameSpec compares a recorded spec fingerprint with a live one; a missing
// record, as in a rollout created before fingerprints were recorded, is not
// compared.
func sameSpec(recorded, live string) bool {
	return recorded == "" || recorded == live
}

// writtenSpec is the spec fingerprint recordSpec kept for a release.
func writtenSpec(ro *v1alpha1.RuntimeRollout, id string) string {
	for _, r := range ro.Status.Releases {
		if r.ID == id {
			return r.SpecDigest
		}
	}
	return ""
}

// ended reports whether a release is at the end state of its step.
func ended(ro *v1alpha1.RuntimeRollout, step v1alpha1.RolloutStep, s v1alpha1.InventoryEntry, started bool,
	l argocd.InventoryItem, present bool) bool {

	switch step.Operation {
	case v1alpha1.OperationRemove:
		return !present
	case v1alpha1.OperationInstall:
		return present && l.PlanDigest == ro.Spec.ToPlanDigest
	case v1alpha1.OperationUpgrade, v1alpha1.OperationVerify:
		return present && started && l.UID == s.UID && l.PlanDigest == ro.Spec.ToPlanDigest
	case v1alpha1.OperationAdopt:
		// Adoption moves the object to this runtime and changes nothing else.
		return present && started && l.UID == s.UID && l.PlanDigest == s.PlanDigest
	default:
		return false
	}
}

func describeDrift(id string, s v1alpha1.InventoryEntry, started bool, l argocd.InventoryItem, present, edited bool) string {
	switch {
	case edited:
		return fmt.Sprintf("%s was edited (object %s: its source, values or destination changed)", id, l.UID)
	case !started:
		return fmt.Sprintf("%s appeared (object %s at plan %s)", id, l.UID, orNone(l.PlanDigest))
	case !present:
		return fmt.Sprintf("%s disappeared (object %s)", id, s.UID)
	case l.UID != s.UID:
		return fmt.Sprintf("%s was recreated (object %s, was %s)", id, l.UID, s.UID)
	default:
		return fmt.Sprintf("%s is at plan %s (was %s)", id, orNone(l.PlanDigest), orNone(s.PlanDigest))
	}
}

// checkDrift stalls the rollout when the live inventory drifted; the next
// reconcile checks again, and a retry replaces the rollout.
func (r *RolloutReconciler) checkDrift(ctx context.Context, rt *v1alpha1.ClusterRuntime, ro *v1alpha1.RuntimeRollout, v *runtimeView) (bool, error) {
	var live []argocd.InventoryItem
	var err error
	if isAdopt(ro) {
		live, err = r.adoptedInventory(ctx, rt, ro)
	} else {
		live, err = r.Engine.Inventory(ctx, string(rt.UID))
	}
	if err != nil {
		return false, err
	}
	drift := inventoryDrift(ro, live)
	if len(drift) == 0 {
		return false, nil
	}
	msg := "the engine objects changed since the rollout was created: " + strings.Join(drift, "; ")
	ro.Status.Phase = v1alpha1.PhaseStalled
	v.stalled, v.stalledReason, v.stalledMsg = true, ReasonInventoryDrift, msg
	return true, r.saveStatus(ctx, ro, msg)
}

// landed reports whether a step's mutation was already made by this
// rollout, so that a halt lets it finish instead of starting it: the spec
// is written, or, for a step that syncs, a sync of the current revision
// passed since the rollout was created. An object that already carried the
// target plan when the rollout was created, as after a retry, was not
// written by this rollout, so its step has not landed.
func (r *RolloutReconciler) landed(ctx context.Context, p *plan.Document, rel *plan.Release, ro *v1alpha1.RuntimeRollout,
	step v1alpha1.RolloutStep, values []byte, now time.Time) (bool, error) {

	if !syncs(step, rel) {
		if carriedAtStart(ro, step.Release) {
			return false, nil
		}
		return r.written(ctx, p, rel, step, values, now)
	}
	obs, err := r.Engine.Observe(ctx, p, rel, values, ro.CreationTimestamp.Time, true)
	if err != nil {
		return false, err
	}
	return obs.State == argocd.StateReady, nil
}

// carriedAtStart reports whether a release's engine object already carried
// the rollout's target plan in its starting inventory.
func carriedAtStart(ro *v1alpha1.RuntimeRollout, release string) bool {
	for _, e := range ro.Spec.StartingInventory {
		if e.ReleaseID == release && e.PlanDigest == ro.Spec.ToPlanDigest {
			return true
		}
	}
	return false
}

// haltedRuntime is why the runtime starts no step: an engine suspension,
// which also stops Argo CD's own syncs, or a pause.
func haltedRuntime(rt *v1alpha1.ClusterRuntime) string {
	switch {
	case rt.Spec.SuspendEngine:
		return ReasonEngineSuspended
	case rt.Spec.Paused:
		return ReasonPaused
	default:
		return ""
	}
}

// suspension keeps every owned engine object's automated sync off while
// spec.suspendEngine is set and on otherwise, recording each change.
func (r *RolloutReconciler) suspension(ctx context.Context, rt *v1alpha1.ClusterRuntime) error {
	changed, err := r.Engine.Suspend(ctx, string(rt.UID), rt.Spec.SuspendEngine)
	if len(changed) > 0 {
		reason, verb := "EngineResumed", "resumed"
		if rt.Spec.SuspendEngine {
			reason, verb = ReasonEngineSuspended, "suspended"
		}
		r.event(rt, corev1.EventTypeNormal, reason, "automated sync %s for %s", verb, strings.Join(changed, ", "))
	}
	return err
}
