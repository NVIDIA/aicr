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
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// Each step's class decides approval; blocked verdicts and
// identity changes stop the transition.
func TestAssessSteps(t *testing.T) {
	t.Parallel()
	rel := func(id, component, ns, repo, chart, values string, crds bool) plan.Release {
		return plan.Release{ID: id, Component: component, TargetNamespace: ns, OwnsCRDs: crds,
			Chart:  plan.Chart{Repository: "oci://r/charts/" + repo, Digest: plan.DigestOf([]byte(chart))},
			Values: plan.DigestedRef{Digest: plan.DigestOf([]byte(values))}}
	}
	from := &plan.Document{Releases: []plan.Release{
		rel("cert-manager", "cert-manager", "cert-manager", "cert-manager", "c1", "v1", true),
		rel("gpu-operator", "gpu-operator", "gpu-operator", "gpu-operator", "g1", "v1", true),
		rel("nfd", "nfd", "nfd", "node-feature-discovery", "n1", "v1", false),
		rel("kai-scheduler", "kai-scheduler", "kai", "kai-scheduler", "k1", "v1", false),
		rel("nvsentinel", "nvsentinel", "nvsentinel", "nvsentinel", "s1", "v1", false),
	}}
	to := &plan.Document{Releases: []plan.Release{
		rel("cert-manager", "cert-manager", "cert-manager", "cert-manager", "c1", "v1", true),
		rel("gpu-operator", "gpu-operator", "gpu-operator", "gpu-operator", "g2", "v1", true),
		rel("nfd", "nfd", "nfd", "node-feature-discovery", "n1", "v2", false),
		rel("kai-scheduler", "kai-scheduler", "kai", "kai-scheduler", "k2", "v1", false),
		rel("nvsentinel", "nvsentinel", "nvsentinel", "nvsentinel", "s1", "v1", false),
	}}
	upgrade := func(id string) v1alpha1.RolloutStep {
		return v1alpha1.RolloutStep{Release: id, Operation: v1alpha1.OperationUpgrade}
	}
	report := &aicrjob.UpgradeReport{Components: []aicrjob.ComponentChange{
		{Component: "gpu-operator", Change: "version", Verdict: "safe"},
		{Component: "kai-scheduler", Change: "version", Verdict: "safe"},
	}}

	steps := []v1alpha1.RolloutStep{upgrade("cert-manager"), upgrade("nfd"), upgrade("kai-scheduler")}
	approval, err := assessSteps(steps, from, to, report)
	if err != nil || !approval {
		t.Fatalf("assessSteps() = %v, %v; a values change needs approval", approval, err)
	}
	if steps[0].Assessment != v1alpha1.AssessmentNotApplicable || steps[1].Assessment != v1alpha1.AssessmentUnknown ||
		steps[2].Assessment != v1alpha1.AssessmentSafe || steps[2].CRDChange {

		t.Fatalf("steps = %+v", steps)
	}

	// Safe, unchanged and annotation-only steps are automatic.
	if approval, err = assessSteps([]v1alpha1.RolloutStep{upgrade("cert-manager"), upgrade("kai-scheduler")}, from, to, report); err != nil || approval {
		t.Errorf("safe transition: approval=%v, %v", approval, err)
	}
	// A safe chart change of a CRD owner needs approval.
	steps = []v1alpha1.RolloutStep{upgrade("gpu-operator")}
	if approval, _ = assessSteps(steps, from, to, report); !approval || !steps[0].CRDChange || steps[0].Assessment != v1alpha1.AssessmentSafe {
		t.Errorf("CRD owner: approval=%v, %+v", approval, steps[0])
	}
	// No verdict for a changed chart is unknown.
	steps = []v1alpha1.RolloutStep{upgrade("kai-scheduler")}
	if approval, _ = assessSteps(steps, from, to, nil); !approval || steps[0].Assessment != v1alpha1.AssessmentUnknown {
		t.Errorf("no report: approval=%v, %+v", approval, steps[0])
	}
	for name, step := range map[string]v1alpha1.RolloutStep{
		"install": {Release: "x", Operation: v1alpha1.OperationInstall},
		"remove":  {Release: "x", Operation: v1alpha1.OperationRemove, UID: "u"},
	} {
		if approval, _ = assessSteps([]v1alpha1.RolloutStep{step}, from, to, report); !approval {
			t.Errorf("%s does not need approval", name)
		}
	}
	if approval, _ = assessSteps([]v1alpha1.RolloutStep{{Release: "x", Operation: v1alpha1.OperationVerify}}, from, to, report); approval {
		t.Error("a Verify step needs approval")
	}

	// A manual record's steps after the apply are recorded on its step, for
	// the approver to acknowledge once the release is ready.
	record := func(verdict string, ids ...string) *aicrjob.UpgradeReport {
		c := aicrjob.ComponentChange{Component: "kai-scheduler", Change: "version", Verdict: verdict}
		for _, id := range ids {
			c.Steps = append(c.Steps, aicrjob.UpgradeStep{ID: id, Description: id})
		}
		return &aicrjob.UpgradeReport{Components: []aicrjob.ComponentChange{c}}
	}
	for name, tc := range map[string]struct {
		report *aicrjob.UpgradeReport
		want   []string
	}{
		"steps on both sides": {record("manual", "before", "apply", "reset-breaker", "verify"), []string{"reset-breaker", "verify"}},
		"steps before only":   {record("manual", "before", "apply"), nil},
		"no apply step":       {record("manual", "before", "verify"), nil},
		"a safe verdict":      {record("safe", "apply", "verify"), nil},
	} {
		steps = []v1alpha1.RolloutStep{upgrade("kai-scheduler")}
		if _, err = assessSteps(steps, from, to, tc.report); err != nil || !slices.Equal(steps[0].PostApplySteps, tc.want) {
			t.Errorf("%s: postApplySteps %v, want %v (%v)", name, steps[0].PostApplySteps, tc.want, err)
		}
	}

	blocked := &aicrjob.UpgradeReport{Components: []aicrjob.ComponentChange{{Component: "kai-scheduler", Change: "version", Verdict: "blocked"}}}
	if _, err = assessSteps([]v1alpha1.RolloutStep{upgrade("kai-scheduler")}, from, to, blocked); operrors.ReasonOf(err) != ReasonTransitionBlocked {
		t.Errorf("blocked verdict: %v", err)
	}
	// A replacement surfaces as a removal and an install, yet its record's
	// blocked verdict still blocks the transition.
	replaced := &aicrjob.UpgradeReport{Components: []aicrjob.ComponentChange{{Component: "kai-scheduler", Change: "replaced", Verdict: "blocked"}}}
	steps = []v1alpha1.RolloutStep{{Release: "x", Operation: v1alpha1.OperationInstall}, {Release: "y", Operation: v1alpha1.OperationRemove, UID: "u"}}
	if _, err = assessSteps(steps, from, to, replaced); operrors.ReasonOf(err) != ReasonTransitionBlocked {
		t.Errorf("blocked replacement: %v", err)
	}
	moved := &plan.Document{Releases: []plan.Release{rel("nfd", "nfd", "other", "node-feature-discovery", "n1", "v1", false)}}
	_, err = assessSteps([]v1alpha1.RolloutStep{upgrade("nfd")}, from, moved, report)
	if operrors.ReasonOf(err) != ReasonTransitionBlocked || !strings.Contains(err.Error(), "nfd moves from nfd/node-feature-discovery to other/node-feature-discovery") {
		t.Errorf("namespace change: %v", err)
	}
}

// A recipe change runs AICR's upgrade-check before the rollout exists; the
// report is kept, referenced from the rollout, and decides approval. A
// blocked verdict stalls without a rollout.
func TestRolloutAssessesRecipeChange(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	gpu := func(chart string) releaseWithValues {
		r := testRelease("gpu-operator", plan.PhasePrimary, "", `{}`)
		r.Chart.Digest, r.OwnsCRDs = plan.DigestOf([]byte(chart)), true
		return r
	}
	active := putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "1"}, gpu("g1"))
	target := putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "2"}, gpu("g2"))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "gpu-operator", UID: "u-g", PlanDigest: active}}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	jobs := liveJobs(t, ctx, "assessment")
	if len(jobs) != 1 || len(rollouts(t, ctx)) != 1 {
		t.Fatalf("assessment Jobs = %d, rollouts = %d; want one assessment and no new rollout", len(jobs), len(rollouts(t, ctx)))
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling); c.Reason != ReasonAssessing {
		t.Fatalf("Reconciling = %+v", c)
	}
	report := `{"deployer":"argocd","objectNamesCompared":false,"components":[` +
		`{"component":"gpu-operator","change":"version","from":"v25.10.1","to":"v26.7.1","verdict":"safe","breaking":false,"downgrade":false,"failsRun":false}],` +
		`"summary":{"components":1,"failing":0}}`
	succeed(t, ctx, &jobs[0], []byte(report))
	reconcileRollout(t, ctx, r)
	var ro *v1alpha1.RuntimeRollout
	for _, x := range rollouts(t, ctx) {
		if x.Spec.ToPlanDigest == target {
			ro = x.DeepCopy()
		}
	}
	if ro == nil || ro.Spec.AssessmentRef == nil || ro.Spec.Steps[0].Assessment != v1alpha1.AssessmentSafe || !ro.Spec.Steps[0].CRDChange ||
		!ro.Spec.ApprovalRequired {

		t.Fatalf("rollout = %+v", ro)
	}
	if len(liveJobs(t, ctx, "assessment")) != 0 {
		t.Error("the assessment Job outlived its stored report")
	}

	// The same transition, assessed blocked, stalls.
	rt = freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	active = putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "1"}, gpu("g1"))
	target = putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "2"}, gpu("g2"))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, target)
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "gpu-operator", UID: "u-g", PlanDigest: active}}
	reconcileRollout(t, ctx, r)
	job := liveJobs(t, ctx, "assessment")[0]
	succeed(t, ctx, &job, []byte(strings.Replace(report, `"safe"`, `"blocked"`, 1)))
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason != ReasonTransitionBlocked {
		t.Fatalf("Stalled = %+v", c)
	}
	for _, x := range rollouts(t, ctx) {
		if x.Spec.ToPlanDigest == target {
			t.Fatal("a blocked transition got a rollout")
		}
	}
}

// A failed rollout to a plan on another recipe
// left its release there, and the target is the active plan again.
// upgrade-check runs from the recipe of the plan the release is on, and its
// verdict on the downgrade decides approval; nothing is written before it.
func TestConvergenceAssessesFromTheLivePlansRecipe(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	gpu := func(chart string) releaseWithValues {
		r := testRelease("gpu-operator", plan.PhasePrimary, "", `{}`)
		r.Chart.Digest, r.OwnsCRDs = plan.DigestOf([]byte(chart)), true
		return r
	}
	active := putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "1"}, gpu("g1"))
	failed := putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "2"}, gpu("g2"))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["gpu-operator"] = failed
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "gpu-operator", UID: "u-g", PlanDigest: failed}}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	jobs := liveJobs(t, ctx, "assessment")
	if len(jobs) != 1 {
		t.Fatalf("%d assessment Jobs; want one from the failed plan's recipe", len(jobs))
	}
	from := &corev1.ConfigMap{}
	name := aicrjob.InputName(aicrjob.StageAssessment, jobs[0].Labels[v1alpha1.LabelRunID], aicrjob.InputFrom)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: jobs[0].Namespace, Name: name}, from); err != nil {
		t.Fatal(err)
	}
	var fromRecipe string
	for _, data := range from.Data {
		fromRecipe += data
	}
	if !strings.Contains(fromRecipe, `"version":"2"`) {
		t.Fatalf("assessment from recipe %s; want the failed plan's (version 2)", fromRecipe)
	}
	report := `{"deployer":"argocd","objectNamesCompared":false,"components":[` +
		`{"component":"gpu-operator","change":"version","from":"v26.7.2","to":"v26.7.1","verdict":"unknown","breaking":false,"downgrade":true,"failsRun":false}],` +
		`"summary":{"components":1,"failing":0}}`
	succeed(t, ctx, &jobs[0], []byte(report))
	reconcileRollout(t, ctx, r)
	var converge *v1alpha1.RuntimeRollout
	for _, x := range rollouts(t, ctx) {
		if x.Spec.ToPlanDigest == active && x.Spec.FromPlanDigest == active {
			converge = x.DeepCopy()
		}
	}
	if converge == nil || converge.Spec.AssessmentRef == nil || converge.Spec.Steps[0].Assessment != v1alpha1.AssessmentUnknown ||
		!converge.Spec.Steps[0].CRDChange || !converge.Spec.ApprovalRequired || len(engine.order) != 0 {

		t.Fatalf("convergence = %+v, writes %v", converge, engine.order)
	}
}

// A permanent assessment failure stalls the transition with AICR's exit
// category, before any rollout or engine write; a transient one waits.
func TestRolloutAssessmentFailures(t *testing.T) {
	ctx := testContext(t)
	for exit, want := range map[int32]string{aicrjob.ExitInvalidInput: "AICRInvalidRequest", aicrjob.ExitUnavailable: ReasonAssessing} {
		rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
			rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
		})
		active := putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "1"}, testRelease("gpu-operator", plan.PhasePrimary, "", `{"a":1}`))
		target := putResolved(t, ctx, rt, map[string]string{"service": "eks", "version": "2"}, testRelease("gpu-operator", plan.PhasePrimary, "", `{"a":2}`))
		succeedTo(t, ctx, rt, active)
		resolved(t, ctx, target)
		engine := newEngine()
		engine.inventory = []argocd.InventoryItem{{ReleaseID: "gpu-operator", UID: "u-g", PlanDigest: active}}
		r, clk := newRollout(t, engine, true)
		clk.now = time.Now().Add(2 * time.Second)
		reconcileRollout(t, ctx, r)
		job := liveJobs(t, ctx, "assessment")[0]
		fail(t, ctx, &job, exit)
		reconcileRollout(t, ctx, r)
		got := getRuntime(t, ctx)
		reason := cond(got, v1alpha1.ConditionStalled).Reason
		if want == ReasonAssessing {
			reason = cond(got, v1alpha1.ConditionReconciling).Reason
		}
		if reason != want || len(engine.order) != 0 {
			t.Fatalf("assessment exiting %d: reason %q, engine writes %v", exit, reason, engine.order)
		}
		for _, x := range rollouts(t, ctx) {
			if x.Spec.ToPlanDigest == target {
				t.Fatalf("assessment exiting %d: a rollout started", exit)
			}
		}
	}
}

// A manual record with steps after the apply pauses the rollout once that
// release is ready: the plan is not active until the approver acknowledges
// those steps, and then the rollout finishes.
func TestManualPostApplyStepsAwaitAcknowledgement(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	sentinel := func(chart string) releaseWithValues {
		r := testRelease("nvsentinel", plan.PhasePrimary, "", `{}`)
		r.Chart.Digest = plan.DigestOf([]byte(chart))
		return r
	}
	active := putResolved(t, ctx, rt, map[string]string{"service": "gke", "version": "1"}, sentinel("s1"))
	target := putResolved(t, ctx, rt, map[string]string{"service": "gke", "version": "2"}, sentinel("s2"))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.ensured["nvsentinel"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "nvsentinel", UID: "u-s", PlanDigest: active}}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	job := liveJobs(t, ctx, "assessment")[0]
	succeed(t, ctx, &job, []byte(`{"deployer":"argocd","objectNamesCompared":false,"components":[`+
		`{"component":"nvsentinel","change":"version","from":"v1.25.0","to":"v1.28.0","verdict":"manual","breaking":false,"downgrade":false,"failsRun":false,`+
		`"steps":[{"id":"before","description":"no GPUReset in progress"},{"id":"apply","description":"sync"},`+
		`{"id":"reset-breaker","description":"reset a tripped circuit breaker"},{"id":"verify","description":"check the CRD"}]}],`+
		`"summary":{"components":1,"failing":0}}`))
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	if got := ro.Spec.Steps[0].PostApplySteps; !slices.Equal(got, []string{"reset-breaker", "verify"}) || !ro.Spec.ApprovalRequired {
		t.Fatalf("rollout steps = %+v", ro.Spec.Steps)
	}
	approve(t, ctx, ro.Name, clk.now.Add(time.Hour))
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro = onlyRollout(t, ctx, target)
	got := getRuntime(t, ctx)
	stalled := cond(got, v1alpha1.ConditionStalled)
	if ro.Status.Phase != v1alpha1.PhaseAwaitingAcknowledgement || !strings.Contains(ro.Status.Message, "reset-breaker, verify") ||
		stalled.Reason != ReasonAwaitingAcknowledgement || got.Status.ActivePlanDigest != active {

		t.Fatalf("after the apply: rollout %+v, Stalled %+v, active %s", ro.Status, stalled, got.Status.ActivePlanDigest)
	}

	patch := client.MergeFrom(ro.DeepCopy())
	ro.Spec.Approval.Acknowledgements = []v1alpha1.Acknowledgement{{Release: "nvsentinel", AcknowledgedBy: "alice"}}
	if err := k8s.Patch(ctx, ro, patch); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	if ro = onlyRollout(t, ctx, target); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || getRuntime(t, ctx).Status.ActivePlanDigest != target {
		t.Fatalf("after the acknowledgement: %v %+v", ro.Labels, ro.Status)
	}
}

// A retry needs a fresh approval and keeps the post-apply steps nobody
// acknowledged: after a manual release is applied and the rollout is
// revoked while it awaits acknowledgement, the retry to the same target
// finds the release already at the target, yet still needs approval and
// still waits for the acknowledgement before the plan becomes active.
func TestRetriedManualRolloutKeepsItsAcknowledgementAndApproval(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	sentinel := func(chart string) releaseWithValues {
		r := testRelease("nvsentinel", plan.PhasePrimary, "", `{}`)
		r.Chart.Digest = plan.DigestOf([]byte(chart))
		return r
	}
	active := putResolved(t, ctx, rt, map[string]string{"service": "gke", "version": "1"}, sentinel("s1"))
	target := putResolved(t, ctx, rt, map[string]string{"service": "gke", "version": "2"}, sentinel("s2"))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.ensured["nvsentinel"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "nvsentinel", UID: "u-s", PlanDigest: active}}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	job := liveJobs(t, ctx, "assessment")[0]
	succeed(t, ctx, &job, []byte(`{"deployer":"argocd","objectNamesCompared":false,"components":[`+
		`{"component":"nvsentinel","change":"version","from":"v1.25.0","to":"v1.28.0","verdict":"manual","breaking":false,"downgrade":false,"failsRun":false,`+
		`"steps":[{"id":"before","description":"no GPUReset in progress"},{"id":"apply","description":"sync"},`+
		`{"id":"reset-breaker","description":"reset a tripped circuit breaker"},{"id":"verify","description":"check the CRD"}]}],`+
		`"summary":{"components":1,"failing":0}}`))
	reconcileRollout(t, ctx, r)
	first := onlyRollout(t, ctx, target)
	approve(t, ctx, first.Name, clk.now.Add(time.Hour))
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	if first = onlyRollout(t, ctx, target); first.Status.Phase != v1alpha1.PhaseAwaitingAcknowledgement {
		t.Fatalf("before the revocation: %+v", first.Status)
	}
	revoke(t, ctx, first.Name)
	reconcileRollout(t, ctx, r)
	if first = onlyRollout(t, ctx, target); first.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseRevoked) {
		t.Fatalf("after the revocation: %v %+v", first.Labels, first.Status)
	}

	got := getRuntime(t, ctx)
	patch := client.MergeFrom(got.DeepCopy())
	got.Annotations = map[string]string{v1alpha1.AnnotationRetry: "after-revoke"}
	if err := k8s.Patch(ctx, got, patch); err != nil {
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
	if retried == nil || !retried.Spec.ApprovalRequired || retried.Status.Phase != v1alpha1.PhaseAwaitingApproval ||
		!slices.Equal(retried.Spec.Steps[0].PostApplySteps, []string{"reset-breaker", "verify"}) ||
		retried.Spec.Steps[0].Assessment != v1alpha1.AssessmentManual {

		t.Fatalf("retry rollout = %+v", retried)
	}

	approve(t, ctx, retried.Name, clk.now.Add(time.Hour))
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro := &v1alpha1.RuntimeRollout{}
	if err := k8s.Get(ctx, client.ObjectKey{Name: retried.Name}, ro); err != nil {
		t.Fatal(err)
	}
	if ro.Status.Phase != v1alpha1.PhaseAwaitingAcknowledgement || getRuntime(t, ctx).Status.ActivePlanDigest != active {
		t.Fatalf("the retry finished without the acknowledgement: %v %+v", ro.Labels, ro.Status)
	}
}

// An assessment belongs to the AICR that ran it: a transition assessed, or
// refused, by one AICR image is assessed again by another, since its
// verdicts come from that release's transition records.
func TestAssessmentIsRunAgainForAnotherAICRImage(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	gpu := func(chart string) releaseWithValues {
		r := testRelease("gpu-operator", plan.PhasePrimary, "", `{}`)
		r.Chart.Digest = plan.DigestOf([]byte(chart))
		return r
	}
	active := putResolved(t, ctx, rt, map[string]string{"service": "gke", "version": "1"}, gpu("g1"))
	target := putResolved(t, ctx, rt, map[string]string{"service": "gke", "version": "2"}, gpu("g2"))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "gpu-operator", UID: "u-g", PlanDigest: active}}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	first := liveJobs(t, ctx, "assessment")
	if len(first) != 1 {
		t.Fatalf("assessment Jobs = %d", len(first))
	}
	fail(t, ctx, &first[0], 2)
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Status != metav1.ConditionTrue {
		t.Fatalf("a refused assessment: Stalled %+v", c)
	}

	r.Config.AICRImage = "ghcr.io/nvidia/aicr:v1.0.1@sha256:" + strings.Repeat("b", 64)
	reconcileRollout(t, ctx, r)
	again := slices.DeleteFunc(liveJobs(t, ctx, "assessment"), func(j batchv1.Job) bool { return j.Name == first[0].Name })
	if len(again) != 1 || again[0].Spec.Template.Spec.Containers[0].Image != r.Config.AICRImage {
		t.Fatalf("assessment Jobs for the new AICR image = %+v", again)
	}
}
