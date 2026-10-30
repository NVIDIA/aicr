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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// twoReleases is a target plan of two releases, so a rollout has a step
// boundary in the middle.
func twoReleases(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime) string {
	t.Helper()
	return putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
}

// approve sets the rollout's approval as an approver does.
func approve(t *testing.T, ctx context.Context, name string, expires time.Time) {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(ro.DeepCopy())
	ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: "alice", ExpiresAt: metav1.NewTime(expires.Truncate(time.Second)), Reason: "maintenance window"}
	if err := k8s.Patch(ctx, ro, patch); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// revoke adds a revocation as an approver does.
func revoke(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: name}, ro); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(ro.DeepCopy())
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	ro.Spec.Approval.RevokedBy, ro.Spec.Approval.RevokedAt, ro.Spec.Approval.RevokeReason = "bob", &now, "incident"
	if err := k8s.Patch(ctx, ro, patch); err != nil {
		t.Fatalf("revoke: %v", err)
	}
}

func onlyRollout(t *testing.T, ctx context.Context, digest string) *v1alpha1.RuntimeRollout {
	t.Helper()
	ros := rolloutsTo(t, ctx, digest)
	if len(ros) != 1 {
		t.Fatalf("%d rollouts to %s", len(ros), digest)
	}
	return &ros[0]
}

// An approval proceeds the rollout and changes no digest it approves; the
// starting inventory is recorded with its digest.
func TestRolloutProceedsOnApproval(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	before := onlyRollout(t, ctx, target)
	if before.Status.Phase != v1alpha1.PhaseAwaitingApproval || len(engine.order) != 0 {
		t.Fatalf("before approval: %+v, writes %v", before.Status, engine.order)
	}
	if _, ok := recordedInventory(before); !ok {
		t.Fatalf("the starting inventory %v does not digest to %s", before.Spec.StartingInventory, before.Spec.StartingInventoryDigest)
	}

	approve(t, ctx, before.Name, clk.now.Add(time.Hour))
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	after := onlyRollout(t, ctx, target)
	if after.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || after.Status.Approval != v1alpha1.ApprovalApproved {
		t.Fatalf("after approval: %v %+v", after.Labels, after.Status)
	}
	unchanged := after.Spec.ToPlanDigest == before.Spec.ToPlanDigest && after.Spec.StartingInventoryDigest == before.Spec.StartingInventoryDigest
	if !unchanged || getRuntime(t, ctx).Status.ActivePlanDigest != target {
		t.Fatalf("approval changed a digest: %+v -> %+v", before.Spec, after.Spec)
	}
}

// A revocation mid-rollout lets the step in flight finish, starts no other
// step and ends the rollout Revoked; a retry makes a rollout that needs a
// fresh approval.
func TestRolloutRevokedAtStepBoundary(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))
	reconcileRollout(t, ctx, r) // first step in flight
	revoke(t, ctx, ro.Name)
	reconcileRollout(t, ctx, r)
	if ro = onlyRollout(t, ctx, target); ro.Labels[v1alpha1.LabelOutcome] != "" || ro.Status.Current == nil {
		t.Fatalf("the step in flight was abandoned: %v %+v", ro.Labels, ro.Status)
	}
	engine.state = argocd.StateReady
	reconcileRollout(t, ctx, r)
	ro = onlyRollout(t, ctx, target)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseRevoked) || ro.Status.Approval != v1alpha1.ApprovalRevoked {
		t.Fatalf("after the step: %v %+v", ro.Labels, ro.Status)
	}
	if !slices.Equal(engine.order, []string{"Ensure cert-manager"}) {
		t.Fatalf("writes after the revocation: %v", engine.order)
	}
	if releaseState(ro, "cert-manager") != v1alpha1.ReleaseReady {
		t.Fatalf("the step in flight did not finish: %+v", ro.Status.Releases)
	}

	rt = getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Annotations = map[string]string{v1alpha1.AnnotationRetry: "after-revoke"}
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	var retried *v1alpha1.RuntimeRollout
	for _, x := range rolloutsTo(t, ctx, target) {
		if x.Annotations[v1alpha1.AnnotationRetry] == "after-revoke" {
			retried = &x
		}
	}
	if retried == nil || retried.Spec.Approval != nil || retried.Status.Phase != v1alpha1.PhaseAwaitingApproval {
		t.Fatalf("retry rollout = %+v", retried)
	}
}

// A revocation stops a retry whose engine objects already carried the target
// plan when it was created: those steps were not written by this rollout, so
// none of them may start after the revocation.
func TestRolloutRevokedWhenPlanAlreadyWritten(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	for _, id := range []string{"cert-manager", "nfd"} {
		engine.ensured[id] = target
		engine.inventory = append(engine.inventory, argocd.InventoryItem{ReleaseID: id, Name: id, UID: "u-" + id, PlanDigest: target})
	}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))
	reconcileRollout(t, ctx, r) // first step in flight
	revoke(t, ctx, ro.Name)
	engine.state = argocd.StateReady
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro = onlyRollout(t, ctx, target)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseRevoked) || releaseState(ro, "nfd") == v1alpha1.ReleaseReady {
		t.Fatalf("a step started after the revocation: %v %+v", ro.Labels, ro.Status)
	}
	if releaseState(ro, "cert-manager") != v1alpha1.ReleaseReady {
		t.Fatalf("the step in flight did not finish: %+v", ro.Status.Releases)
	}
}

// An approval that expires mid-rollout ends it Expired at the next step
// boundary, after the step in flight finishes.
func TestRolloutApprovalExpiresAtStepBoundary(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	approve(t, ctx, ro.Name, clk.now.Add(time.Minute))
	reconcileRollout(t, ctx, r) // first step in flight
	clk.now = clk.now.Add(2 * time.Minute)
	engine.state = argocd.StateReady
	reconcileRollout(t, ctx, r)
	ro = onlyRollout(t, ctx, target)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseExpired) || !slices.Equal(engine.order, []string{"Ensure cert-manager"}) {
		t.Fatalf("expired approval: %v %+v, writes %v", ro.Labels, ro.Status, engine.order)
	}
}

// Once every step has finished, a revocation no longer applies, even after a
// restore emptied the rollout's status and admission could not tell.
func TestRolloutIgnoresLateRevocation(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
		testRelease("cert-manager-readiness", plan.PhaseReadiness, "cert-manager", `{}`))
	rt = getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Validation.Deployment = v1alpha1.DeploymentValidationRequired
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	resolved(t, ctx, target)
	engine := newEngine()
	r, clk := newRollout(t, engine, false)
	r.Config.ValidationEnabled = true
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))
	for range 5 {
		reconcileRollout(t, ctx, r)
	}
	if ro = onlyRollout(t, ctx, target); ro.Status.Phase != v1alpha1.PhaseValidating {
		t.Fatalf("not validating: %+v", ro.Status)
	}
	writes := len(engine.order)

	ro.Status = v1alpha1.RuntimeRolloutStatus{}
	if err := k8s.Status().Update(ctx, ro); err != nil {
		t.Fatal(err)
	}
	revoke(t, ctx, ro.Name)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro = onlyRollout(t, ctx, target)
	if ro.Labels[v1alpha1.LabelOutcome] != "" || ro.Status.Phase != v1alpha1.PhaseValidating || len(engine.order) != writes {
		t.Fatalf("late revocation applied: %v %+v, writes %v", ro.Labels, ro.Status, engine.order[writes:])
	}
}

// Live engine objects that changed out of band since the rollout was
// created stall it before the next step; restoring them lets it continue.
func TestRolloutStallsOnInventoryDrift(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	// cert-manager is upgraded, nfd installed and other removed.
	engine.inventory = []argocd.InventoryItem{
		{ReleaseID: "cert-manager", UID: "u-cert-manager", PlanDigest: digestOther},
		{ReleaseID: "other", UID: "u-other", PlanDigest: digestOther},
	}
	engine.gone["other"] = true
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))

	cases := []struct {
		name  string
		drift func()
		want  string
	}{
		{"an object recreated", func() { engine.inventory[1].UID = "u-other-2" }, "other was recreated"},
		{"an object at another plan", func() { engine.inventory[1].PlanDigest = target }, "other is at plan"},
		{"a new object", func() {
			engine.inventory = append(engine.inventory, argocd.InventoryItem{ReleaseID: "stray", Name: "stray", UID: "u-stray"})
		}, "stray appeared"},
		{"an upgraded object deleted", func() { engine.inventory = engine.inventory[1:] }, "cert-manager disappeared"},
	}
	for _, tc := range cases {
		saved := slices.Clone(engine.inventory)
		tc.drift()
		reconcileRollout(t, ctx, r)
		ro = onlyRollout(t, ctx, target)
		c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
		if ro.Status.Phase != v1alpha1.PhaseStalled || c.Reason != ReasonInventoryDrift || !strings.Contains(c.Message, tc.want) || len(engine.order) != 0 {
			t.Fatalf("%s: phase %s, Stalled %+v, writes %v", tc.name, ro.Status.Phase, c, engine.order)
		}
		engine.inventory = saved
	}
	for range 6 {
		reconcileRollout(t, ctx, r)
	}
	if ro = onlyRollout(t, ctx, target); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) {
		t.Fatalf("after the drift was undone: %v %+v", ro.Labels, ro.Status)
	}
	if !slices.Equal(engine.order, []string{"Ensure cert-manager", "Ensure nfd", "Remove other"}) {
		t.Fatalf("writes = %v", engine.order)
	}
}

const digestOther = "sha256:0000000000000000000000000000000000000000000000000000000000000001"

// The steps' own writes, and a pending step's write whose record a restart
// lost, are not drift.
func TestInventoryDriftAllowsTheRolloutsOwnWrites(t *testing.T) {
	t.Parallel()
	from, to := digestOther, "sha256:0000000000000000000000000000000000000000000000000000000000000002"
	start := []argocd.InventoryItem{{ReleaseID: "a", Name: "a", UID: "u-a", PlanDigest: from}, {ReleaseID: "gone", Name: "gone", UID: "u-g", PlanDigest: from}}
	ro := &v1alpha1.RuntimeRollout{Spec: v1alpha1.RuntimeRolloutSpec{
		ToPlanDigest: to, StartingInventory: startingInventory(start), StartingInventoryDigest: inventoryDigest(t, start),
		Steps: []v1alpha1.RolloutStep{
			{Release: "a", Operation: v1alpha1.OperationUpgrade},
			{Release: "b", Operation: v1alpha1.OperationInstall},
			{Release: "gone", Operation: v1alpha1.OperationRemove, UID: "u-g"},
		},
	}}
	setReleaseState(ro, "a", v1alpha1.ReleaseReady, "")
	live := []argocd.InventoryItem{
		{ReleaseID: "a", Name: "a", UID: "u-a", PlanDigest: to},
		{ReleaseID: "b", Name: "b", UID: "u-b", PlanDigest: to},
	}
	if d := driftOf(t, ro, live); len(d) != 0 {
		t.Fatalf("own writes reported as drift: %v", d)
	}
	ro.Status.Releases = nil
	if d := driftOf(t, ro, start); len(d) != 0 {
		t.Fatalf("the starting state reported as drift: %v", d)
	}
	setReleaseState(ro, "a", v1alpha1.ReleaseReady, "")
	if d := driftOf(t, ro, start); len(d) != 1 || !strings.Contains(d[0], "a is at plan") {
		t.Fatalf("a completed step back at its starting plan: %v", d)
	}
	ro.Spec.StartingInventoryDigest = digestOther
	if d := driftOf(t, ro, nil); d != nil {
		t.Fatalf("a rollout without a recorded inventory: %v", d)
	}
}

// driftOf runs inventoryDrift with every Application at the target plan
// carrying the plan's write.
func driftOf(t *testing.T, ro *v1alpha1.RuntimeRollout, live []argocd.InventoryItem) []string {
	t.Helper()
	d, err := inventoryDrift(ro, live, func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A pending step's release at the target plan that this rollout has not
// recorded writing must carry the plan's write: an editor who changes the
// spec and sets the plan digest annotation is reported, not taken for a
// write a restart lost. Only such a release is checked against the plan.
func TestInventoryDriftChecksUnrecordedWritesAgainstThePlan(t *testing.T) {
	t.Parallel()
	from, to := digestOther, "sha256:0000000000000000000000000000000000000000000000000000000000000002"
	spec := "sha256:" + strings.Repeat("a", 64)
	start := []argocd.InventoryItem{
		{ReleaseID: "a", Name: "a", UID: "u-a", PlanDigest: from, SpecDigest: spec},
		{ReleaseID: "c", Name: "c", UID: "u-c", PlanDigest: from, SpecDigest: spec},
	}
	ro := &v1alpha1.RuntimeRollout{Spec: v1alpha1.RuntimeRolloutSpec{
		ToPlanDigest: to, StartingInventory: startingInventory(start), StartingInventoryDigest: inventoryDigest(t, start),
		Steps: []v1alpha1.RolloutStep{
			{Release: "a", Operation: v1alpha1.OperationUpgrade},
			{Release: "b", Operation: v1alpha1.OperationInstall},
			{Release: "c", Operation: v1alpha1.OperationUpgrade},
		},
	}}
	live := []argocd.InventoryItem{
		{ReleaseID: "a", Name: "a", UID: "u-a", PlanDigest: to, SpecDigest: "sha256:" + strings.Repeat("e", 64)},
		{ReleaseID: "b", Name: "b", UID: "u-b", PlanDigest: to},
		{ReleaseID: "c", Name: "c", UID: "u-c", PlanDigest: from, SpecDigest: spec},
	}
	var checked []string
	carries := func(written bool) func(string) (bool, error) {
		return func(id string) (bool, error) {
			checked = append(checked, id)
			return written, nil
		}
	}
	d, err := inventoryDrift(ro, live, carries(true))
	if err != nil || len(d) != 0 || !slices.Equal(checked, []string{"a", "b"}) {
		t.Fatalf("writes carrying the plan: drift %v, err %v, checked %v", d, err, checked)
	}
	checked = nil
	d, err = inventoryDrift(ro, live, carries(false))
	if err != nil || len(d) != 2 || !strings.Contains(d[0], "a was edited") || !strings.Contains(d[1], "b was edited") {
		t.Fatalf("objects at the target plan without its write: drift %v, err %v", d, err)
	}
	unavailable := operrors.New(operrors.ErrCodeUnavailable, "read values")
	if _, err = inventoryDrift(ro, live, func(string) (bool, error) { return false, unavailable }); !errors.Is(err, unavailable) {
		t.Fatalf("a failed check: %v", err)
	}
}

// An edit of a release's source, values or destination after the rollout
// was created is drift, whether the rollout has not reached the release yet
// or already wrote it. Entries recorded without a fingerprint, as by a
// rollout created before it was recorded, are not compared on spec.
func TestInventoryDriftNamesEditedSpecs(t *testing.T) {
	t.Parallel()
	from, to := digestOther, "sha256:0000000000000000000000000000000000000000000000000000000000000002"
	specA, specB, edited := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("e", 64)
	start := []argocd.InventoryItem{
		{ReleaseID: "a", Name: "a", UID: "u-a", PlanDigest: from, SpecDigest: specA},
		{ReleaseID: "b", Name: "b", UID: "u-b", PlanDigest: from, SpecDigest: specB},
	}
	rollout := func(start []argocd.InventoryItem) *v1alpha1.RuntimeRollout {
		return &v1alpha1.RuntimeRollout{Spec: v1alpha1.RuntimeRolloutSpec{
			ToPlanDigest: to, StartingInventory: startingInventory(start), StartingInventoryDigest: inventoryDigest(t, start),
			Steps: []v1alpha1.RolloutStep{{Release: "a", Operation: v1alpha1.OperationUpgrade}, {Release: "b", Operation: v1alpha1.OperationUpgrade}},
		}}
	}
	written := "sha256:" + strings.Repeat("c", 64)
	completed := func(ro *v1alpha1.RuntimeRollout) {
		setReleaseState(ro, "a", v1alpha1.ReleaseReady, "")
		recordSpec(ro, "a", written)
	}
	live := func(a, b string) []argocd.InventoryItem {
		return []argocd.InventoryItem{
			{ReleaseID: "a", Name: "a", UID: "u-a", PlanDigest: to, SpecDigest: a},
			{ReleaseID: "b", Name: "b", UID: "u-b", PlanDigest: from, SpecDigest: b},
		}
	}

	ro := rollout(start)
	completed(ro)
	if d := driftOf(t, ro, live(written, specB)); len(d) != 0 {
		t.Fatalf("the rollout's own write and an untouched release: %v", d)
	}
	if d := driftOf(t, ro, live(edited, specB)); len(d) != 1 || !strings.Contains(d[0], "a was edited") {
		t.Fatalf("a written release edited afterwards: %v", d)
	}
	if d := driftOf(t, ro, live(written, edited)); len(d) != 1 || !strings.Contains(d[0], "b was edited") {
		t.Fatalf("a release not reached yet edited: %v", d)
	}

	// A retry starts from releases an earlier rollout already moved to the
	// target plan; one not reached yet that is edited is still drift.
	atTarget := slices.Clone(start)
	atTarget[1].PlanDigest = to
	ro = rollout(atTarget)
	completed(ro)
	carried := live(written, specB)
	carried[1].PlanDigest = to
	if d := driftOf(t, ro, carried); len(d) != 0 {
		t.Fatalf("an untouched release already at the target: %v", d)
	}
	carried[1].SpecDigest = edited
	if d := driftOf(t, ro, carried); len(d) != 1 || !strings.Contains(d[0], "b was edited") {
		t.Fatalf("a release already at the target, not reached yet, edited: %v", d)
	}

	unrecorded := slices.Clone(start)
	for i := range unrecorded {
		unrecorded[i].SpecDigest = ""
	}
	ro = rollout(unrecorded)
	setReleaseState(ro, "a", v1alpha1.ReleaseReady, "")
	if d := driftOf(t, ro, live(edited, edited)); len(d) != 0 {
		t.Fatalf("a rollout recorded without fingerprints: %v", d)
	}
}

// A hand edit of a release the rollout already wrote, made between two
// steps, stalls the rollout before the next step instead of letting it
// proceed against state nobody approved; undoing the edit resumes it.
func TestRolloutStallsOnAnEditedRelease(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // cert-manager written

	// Hold between the steps so that cert-manager is recorded ready.
	setHalt(t, ctx, pause, true)
	engine.state = argocd.StateReady
	reconcileRollout(t, ctx, r)
	if ro := onlyRollout(t, ctx, target); releaseState(ro, "cert-manager") != v1alpha1.ReleaseReady {
		t.Fatalf("cert-manager not recorded ready: %+v", ro.Status)
	}
	written := engine.liveSpec("cert-manager")
	engine.inventory[0].SpecDigest = "sha256:" + strings.Repeat("e", 64)
	setHalt(t, ctx, pause, false)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
	if ro.Status.Phase != v1alpha1.PhaseStalled || c.Reason != ReasonInventoryDrift || !strings.Contains(c.Message, "cert-manager was edited") ||
		slices.Contains(engine.order, "Ensure nfd") {

		t.Fatalf("after the edit: phase %s, Stalled %+v, writes %v", ro.Status.Phase, c, engine.order)
	}

	engine.inventory[0].SpecDigest = written
	reconcileRollout(t, ctx, r)
	if !slices.Contains(engine.order, "Ensure nfd") {
		t.Fatalf("the edit was undone, but the next step did not start: %v", engine.order)
	}
}

func inventoryDigest(t *testing.T, items []argocd.InventoryItem) string {
	t.Helper()
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return plan.DigestOf(raw)
}

// setSuspended sets the runtime's spec.suspendEngine; the resolver then
// confirms the same candidate for the new generation.
func setSuspended(t *testing.T, ctx context.Context, suspended bool) {
	t.Helper()
	rt := getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.SuspendEngine = suspended
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	if candidate := getRuntime(t, ctx).Status.CandidatePlanDigest; candidate != "" {
		resolved(t, ctx, candidate)
	}
}

// suspendEngine turns automated sync off for the owned objects, starts no
// rollout and no step, repairs nothing and claims no readiness; clearing it
// turns sync back on and the rollout continues where it stopped.
func TestRolloutSuspendsEngine(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	setSuspended(t, ctx, true)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); !engine.suspended || len(rolloutsTo(t, ctx, target)) != 0 || c.Reason != ReasonEngineSuspended {
		t.Fatalf("suspended: engine %t, %d rollouts, Ready %+v", engine.suspended, len(rolloutsTo(t, ctx, target)), c)
	}

	setSuspended(t, ctx, false)
	reconcileRollout(t, ctx, r) // resume and create
	reconcileRollout(t, ctx, r) // first step in flight
	if engine.suspended || engine.ensured["cert-manager"] != target {
		t.Fatalf("after resume: suspended %t, writes %v", engine.suspended, engine.order)
	}

	setSuspended(t, ctx, true)
	engine.state = argocd.StateReady
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro := onlyRollout(t, ctx, target)
	if engine.ensured["nfd"] != "" || ro.Labels[v1alpha1.LabelOutcome] != "" {
		t.Fatalf("a step started while suspended: %v, %v %+v", engine.order, ro.Labels, ro.Status)
	}
	setSuspended(t, ctx, false)
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	ro = onlyRollout(t, ctx, target)
	want := []string{"Suspend true", "Suspend false", "Ensure cert-manager", "Suspend true", "Suspend false", "Ensure nfd"}
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || !slices.Equal(engine.order, want) {
		t.Fatalf("after the second resume: %v %+v, writes %v", ro.Labels, ro.Status, engine.order)
	}
}

// While the engine is suspended, a release that drifts or degrades is named
// on the runtime, and nothing repairs it.
func TestSuspendedEngineNamesDrift(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	digest := putPlan(t, ctx, rt, "50m")
	succeedTo(t, ctx, rt, digest)
	resolved(t, ctx, digest)
	engine := newEngine()
	engine.ensured["cert-manager"] = digest
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)
	setSuspended(t, ctx, true)

	engine.unhealthy["cert-manager"] = "OutOfSync"
	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	ready, available := cond(got, v1alpha1.ConditionReady), cond(got, v1alpha1.ConditionAvailable)
	if ready.Reason != ReasonEngineSuspended || !strings.Contains(ready.Message, "release cert-manager: OutOfSync") ||
		available.Status != metav1.ConditionFalse || !strings.Contains(available.Message, "release cert-manager: OutOfSync") {

		t.Fatalf("suspended with a degraded release: Ready %+v, Available %+v", ready, available)
	}

	delete(engine.unhealthy, "cert-manager")
	engine.drifted["cert-manager"] = true
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); !strings.Contains(c.Message, "release cert-manager: "+argocd.ReasonSpecDrifted) ||
		slices.Contains(engine.order, "Ensure cert-manager") {

		t.Fatalf("suspended with a drifted release: Ready %+v, writes %v", c, engine.order)
	}
}

// Pinning an earlier plan is a new rollout from live state to it, assessed
// and approved like any other; nothing rolls back on its own.
func TestPinnedPlanIsAnAssessedRollout(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	earlier := putPlan(t, ctx, rt, "50m")
	current := putPlan(t, ctx, rt, "60m")
	succeedTo(t, ctx, rt, current)
	resolved(t, ctx, current)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: current}}
	engine.ensured["cert-manager"] = current
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	if n := len(rolloutsTo(t, ctx, earlier)); n != 0 {
		t.Fatalf("%d rollouts back to the earlier plan without a pin", n)
	}

	rt = getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Selection.Policy, rt.Spec.Selection.PinnedPlanDigest = v1alpha1.SelectionPinned, earlier
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	resolved(t, ctx, current)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	back := onlyRollout(t, ctx, earlier)
	assessed := back.Spec.ApprovalRequired && back.Spec.Steps[0].Assessment == v1alpha1.AssessmentUnknown
	if back.Spec.FromPlanDigest != current || !assessed || back.Status.Phase != v1alpha1.PhaseAwaitingApproval || len(engine.order) != 0 {
		t.Fatalf("pinned rollout = %+v %+v, writes %v", back.Spec, back.Status, engine.order)
	}
}
