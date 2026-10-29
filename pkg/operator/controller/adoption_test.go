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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

const earlierUID = "0b1c2d3e-4f50-6172-8394-000000000001"

// orphanedPlan is a plan whose objects an earlier runtime left: the plan
// ConfigMap is gone, as after the operator namespace was deleted, and the
// engine reports the objects and what they rebuild to.
func orphanedPlan(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, engine *fakeEngine) string {
	t.Helper()
	digest := twoReleases(t, ctx, rt)
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := plan.Name(digest)
	if err = k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
	rebuilt := *doc
	rebuilt.Provenance, rebuilt.Recipe, rebuilt.RuntimeUID = plan.Provenance{Source: plan.SourceRebuilt}, nil, earlierUID
	engine.rebuilt = &rebuilt
	engine.foreign = map[string][]argocd.InventoryItem{earlierUID: {
		{ReleaseID: "cert-manager", Name: "cert-manager", UID: "u-c", PlanDigest: digest},
		{ReleaseID: "nfd", Name: "nfd", UID: "u-n", PlanDigest: digest},
	}}
	return digest
}

// adopting names the earlier runtime in spec.adoption.
func adopting(rt *v1alpha1.ClusterRuntime) {
	stabilized(rt)
	rt.Spec.Adoption = &v1alpha1.Adoption{RuntimeUID: earlierUID}
}

// A runtime that finds another runtime's engine objects never adopts them
// silently: it stalls naming the earlier UID and writes nothing.
func TestOrphanedObjectsStallANewRuntime(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	engine := newEngine()
	orphanedPlan(t, ctx, rt, engine)
	resolved(t, ctx, twoReleases(t, ctx, rt))
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
	if c.Reason != ReasonOrphanedObjectsPresent || !strings.Contains(c.Message, earlierUID) || len(rollouts(t, ctx)) != 0 || len(engine.order) != 0 {
		t.Fatalf("Stalled %+v, %d rollouts, writes %v", c, len(rollouts(t, ctx)), engine.order)
	}
}

// A stalled resolution keeps its reason next to another runtime's objects:
// Stalled names the resolver's failure and still points at the objects.
func TestOrphanedObjectsKeepAStalledResolutionVisible(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	engine := newEngine()
	orphanedPlan(t, ctx, rt, engine)
	res := &resolution{status: metav1.ConditionFalse, reason: "AICRUnauthorized", message: "[UNAUTHORIZED] aicr bundle exited 4"}
	if err := (&ResolverReconciler{Client: k8s, Reader: k8s, Config: testConfig()}).applyStatus(ctx, getRuntime(t, ctx), res); err != nil {
		t.Fatal(err)
	}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
	if c.Reason != "AICRUnauthorized" || !strings.Contains(c.Message, "exited 4") || !strings.Contains(c.Message, earlierUID) {
		t.Fatalf("Stalled %+v", c)
	}
}

// Adoption by the earlier UID rebuilds the plan from the objects alone,
// writes it, and moves the objects with an Adopt rollout that requires
// approval; the rebuilt plan becomes active with the original digest.
func TestAdoptionMovesObjectsAfterApproval(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, adopting)
	engine := newEngine()
	original := orphanedPlan(t, ctx, rt, engine)
	resolved(t, ctx, original)
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	if restored, err := readPlan(ctx, k8s, operatorNS, original); err != nil || restored.Provenance.Source != plan.SourceRebuilt {
		t.Fatalf("rebuilt plan = %+v, %v", restored, err)
	}
	ro := onlyRollout(t, ctx, original)
	steps := ro.Spec.Steps
	adoptSteps := len(steps) == 2 && steps[0].Operation == v1alpha1.OperationAdopt
	if !isAdopt(ro) || ro.Annotations[v1alpha1.AnnotationAdoptedFrom] != earlierUID || !ro.Spec.ApprovalRequired || !adoptSteps {
		t.Fatalf("Adopt rollout = %+v %+v", ro.Annotations, ro.Spec)
	}
	reconcileRollout(t, ctx, r)
	if ro = onlyRollout(t, ctx, original); ro.Status.Phase != v1alpha1.PhaseAwaitingApproval || len(engine.order) != 0 {
		t.Fatalf("before approval: %+v, writes %v", ro.Status, engine.order)
	}

	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	ro = onlyRollout(t, ctx, original)
	got := getRuntime(t, ctx)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || got.Status.ActivePlanDigest != original || len(engine.foreign) != 0 {
		t.Fatalf("after approval: %v %+v, active %s, foreign %v", ro.Labels, ro.Status, got.Status.ActivePlanDigest, engine.foreign)
	}
	if want := []string{"Adopt cert-manager", "Adopt nfd"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
}

// A candidate other than the rebuilt plan never supersedes the Adopt
// rollout: one Adopt rollout waits for approval and moves the objects at
// their plan, and only then does the runtime roll forward to the candidate,
// in a rollout of its own that requires approval.
func TestAdoptionPrecedesTheRollForward(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, adopting)
	engine := newEngine()
	original := orphanedPlan(t, ctx, rt, engine)
	candidate := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"40m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	resolved(t, ctx, candidate)
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	if ros := rollouts(t, ctx); len(ros) != 1 {
		ended := make([]string, 0, len(ros))
		for _, x := range ros {
			ended = append(ended, x.Name+" "+x.Labels[v1alpha1.LabelOutcome]+": "+x.Status.Message)
		}
		t.Fatalf("%d rollouts before approval, want the one Adopt rollout: %v", len(ros), ended)
	}
	ro := onlyRollout(t, ctx, original)
	if !isAdopt(ro) || ro.Status.Phase != v1alpha1.PhaseAwaitingApproval || len(engine.order) != 0 {
		t.Fatalf("before approval: %v %+v, writes %v", ro.Labels, ro.Status, engine.order)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason != ReasonAwaitingApproval {
		t.Fatalf("Stalled before approval = %+v", c)
	}

	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	ro = onlyRollout(t, ctx, original)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || len(engine.foreign) != 0 {
		t.Fatalf("after approval: %v %+v, foreign %v", ro.Labels, ro.Status, engine.foreign)
	}
	if want := []string{"Adopt cert-manager", "Adopt nfd"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
	forward := onlyRollout(t, ctx, candidate)
	if isAdopt(forward) || forward.Spec.FromPlanDigest != original || !forward.Spec.ApprovalRequired ||
		forward.Status.Phase != v1alpha1.PhaseAwaitingApproval {

		t.Fatalf("roll-forward = %+v %+v", forward.Spec, forward.Status)
	}
}

// Objects that all carry one plan but rebuild to another were edited out of
// band, and so are objects the rebuild itself rejects: adoption stalls,
// naming them, and writes nothing.
func TestAdoptionStallsOnOutOfBandEdits(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, adopting)
	engine := newEngine()
	original := orphanedPlan(t, ctx, rt, engine)
	resolved(t, ctx, original)
	for i := range engine.foreign[earlierUID] {
		engine.foreign[earlierUID][i].PlanDigest = digestOther
	}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
	if c.Reason != ReasonAdoptionBlocked || !strings.Contains(c.Message, "edited out of band") || len(rollouts(t, ctx)) != 0 {
		t.Fatalf("uniform digest mismatch: Stalled %+v, %d rollouts", c, len(rollouts(t, ctx)))
	}

	engine.rebuilt = nil
	engine.rebuildErr = operrors.New(operrors.ErrCodeConflict, "Application argocd/nfd: was edited out of band").WithReason(argocd.ReasonApplicationEdited)
	reconcileRollout(t, ctx, r)
	c = cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
	if c.Reason != ReasonAdoptionBlocked || !strings.Contains(c.Message, "argocd/nfd") || len(rollouts(t, ctx)) != 0 {
		t.Fatalf("rejected rebuild: Stalled %+v", c)
	}
}

// Objects orphaned mid-rollout carry different plans: the rebuilt plan is a
// new mixed plan, adoption proceeds, and the runtime then rolls the object
// still on the other plan forward to its target.
func TestAdoptionOfMixedPlans(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, adopting)
	engine := newEngine()
	original := orphanedPlan(t, ctx, rt, engine)
	resolved(t, ctx, original)
	engine.foreign[earlierUID][1].PlanDigest = digestOther
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	var adopt, forward *v1alpha1.RuntimeRollout
	for _, ro := range rolloutsTo(t, ctx, original) {
		if isAdopt(&ro) {
			adopt = &ro
		} else {
			forward = &ro
		}
	}
	if adopt == nil || adopt.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || adopt.Status.Approval != v1alpha1.ApprovalBypassed {
		t.Fatalf("mixed adoption: %+v", adopt)
	}
	if forward == nil || forward.Spec.FromPlanDigest != original {
		t.Fatalf("no roll-forward after the adoption: %+v", forward)
	}
}

// An Adopt rollout that ends after moving some objects is retried by a new
// Adopt rollout that starts from both sets, the objects already moved and
// those still left: it moves the rest and adopts the original plan, instead
// of failing on a moved release.
func TestAdoptionRetryAfterAPartialMove(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, adopting)
	engine := newEngine()
	original := orphanedPlan(t, ctx, rt, engine)
	resolved(t, ctx, original)
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r) // Adopt rollout
	first := onlyRollout(t, ctx, original)
	approve(t, ctx, first.Name, clk.now.Add(time.Hour))
	// cert-manager moves; moving nfd fails, as in a partition.
	engine.fault = func(op string, _ bool) error {
		if op == "Adopt nfd" {
			return operrors.New(operrors.ErrCodeUnavailable, "partition")
		}
		return nil
	}
	if _, err := r.Reconcile(ctx, request); err == nil || !slices.Equal(engine.order, []string{"Adopt cert-manager"}) {
		t.Fatalf("Reconcile() = %v, engine calls = %v", err, engine.order)
	}
	engine.fault = nil
	revoke(t, ctx, first.Name)
	reconcileRollout(t, ctx, r)
	if ro := onlyRollout(t, ctx, original); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseRevoked) {
		t.Fatalf("after revocation: %v %+v", ro.Labels, ro.Status)
	}

	retry := getRuntime(t, ctx)
	patch := client.MergeFrom(retry.DeepCopy())
	retry.Annotations = map[string]string{v1alpha1.AnnotationRetry: "adopt-the-rest"}
	if err := k8s.Patch(ctx, retry, patch); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, ctx, r)
	var second *v1alpha1.RuntimeRollout
	for _, ro := range rolloutsTo(t, ctx, original) {
		if ro.Name != first.Name {
			second = ro.DeepCopy()
		}
	}
	if second == nil || len(second.Spec.StartingInventory) != 2 {
		t.Fatalf("retry rollout = %+v; want a starting inventory of both objects", second)
	}
	approve(t, ctx, second.Name, clk.now.Add(time.Hour))
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(second), second); err != nil {
		t.Fatal(err)
	}
	if active := getRuntime(t, ctx).Status.ActivePlanDigest; second.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || active != original {
		t.Fatalf("retry: %v %+v, active %s", second.Labels, second.Status, active)
	}
	if want := []string{"Adopt cert-manager", "Adopt nfd"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
}
