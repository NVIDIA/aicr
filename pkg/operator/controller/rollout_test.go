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
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	"github.com/NVIDIA/aicr/pkg/operator/discovery"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// fakeEngine records engine calls and reports what the test scripts.
type fakeEngine struct {
	// fault, when set, is asked before and after each mutation lands; an
	// error before skips the mutation, one after loses its response.
	fault        func(op string, landed bool) error
	capabilities error
	ensured      map[string]string // release -> plan digest
	ensureErr    error
	state        argocd.State
	inventory    []argocd.InventoryItem
	verified     map[string]int    // release -> Verify calls
	removed      map[string]string // release -> UID passed to Remove
	gone         map[string]bool   // release -> Retired reports it gone
	drifted      map[string]bool   // release -> Observe reports SpecDrifted
	missing      map[string]bool   // release -> Observe reports Missing
	priorFailed  map[string]bool   // release -> Observe reports PriorSyncFailed until a Verify
	unhealthy    map[string]string // release -> Observe reports Progressing with this reason at its written plan
	observeErr   error             // Observe fails with it
	overridden   []string
	order        []string // mutations, "<op> <release>"
	rebuilt      *plan.Document
	contents     []*plan.Content
	suspended    bool
	foreign      map[string][]argocd.InventoryItem // runtime UID -> objects it owns
	rebuildErr   error
}

func newEngine() *fakeEngine {
	return &fakeEngine{ensured: map[string]string{}, state: argocd.StateReady, verified: map[string]int{},
		removed: map[string]string{}, gone: map[string]bool{}, drifted: map[string]bool{}, missing: map[string]bool{},
		priorFailed: map[string]bool{}, unhealthy: map[string]string{}}
}

func (f *fakeEngine) Capabilities(context.Context) error { return f.capabilities }

func (f *fakeEngine) Ensure(_ context.Context, p *plan.Document, r *plan.Release, _ []byte) (*argocd.Ensured, error) {
	if f.ensureErr != nil {
		return nil, f.ensureErr
	}
	if err := f.inject("Ensure "+r.ID, false); err != nil {
		return nil, err
	}
	f.ensured[r.ID] = p.DesiredStateDigest
	f.drifted[r.ID], f.missing[r.ID] = false, false
	f.order = append(f.order, "Ensure "+r.ID)
	f.track(r.ID, p.DesiredStateDigest)
	if err := f.inject("Ensure "+r.ID, true); err != nil {
		return nil, err
	}
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argocd.ApplicationGVK)
	app.SetNamespace("argocd")
	app.SetName(r.ID)
	return &argocd.Ensured{Application: app, Overridden: f.overridden}, nil
}

// Verify writes the Application with a sync, as the adapter does.
func (f *fakeEngine) Verify(_ context.Context, p *plan.Document, r *plan.Release, _ []byte) error {
	f.ensured[r.ID] = p.DesiredStateDigest
	f.verified[r.ID]++
	f.order = append(f.order, "Verify "+r.ID)
	f.track(r.ID, p.DesiredStateDigest)
	return nil
}

func (f *fakeEngine) inject(op string, landed bool) error {
	if f.fault == nil {
		return nil
	}
	return f.fault(op, landed)
}

// track keeps the inventory as a real engine's would after a write,
// fingerprinting the spec it wrote.
func (f *fakeEngine) track(id, digest string) {
	spec := plan.DigestOf([]byte("spec " + id + " " + digest))
	for i := range f.inventory {
		if f.inventory[i].ReleaseID == id {
			f.inventory[i].PlanDigest, f.inventory[i].SpecDigest = digest, spec
			return
		}
	}
	f.inventory = append(f.inventory, argocd.InventoryItem{ReleaseID: id, Name: id, UID: "u-" + id, PlanDigest: digest, SpecDigest: spec})
}

// liveSpec is the live object's fingerprint, as the adapter's Observe
// reports it.
func (f *fakeEngine) liveSpec(id string) string {
	for _, it := range f.inventory {
		if it.ReleaseID == id {
			return it.SpecDigest
		}
	}
	return ""
}

// Observe is Ready only for what was ensured at this plan: never stale. A
// Verify step is Ready only after a Verify call.
func (f *fakeEngine) Observe(_ context.Context, p *plan.Document, r *plan.Release, _ []byte, _ time.Time, verify bool) (argocd.Observation, error) {
	if f.observeErr != nil {
		return argocd.Observation{}, f.observeErr
	}
	switch {
	case f.missing[r.ID]:
		return argocd.Observation{State: argocd.StateMissing, Reason: argocd.ReasonApplicationGone}, nil
	case f.drifted[r.ID]:
		return argocd.Observation{State: argocd.StateProgressing, Reason: argocd.ReasonSpecDrifted}, nil
	case f.ensured[r.ID] != p.DesiredStateDigest:
		return argocd.Observation{State: argocd.StateProgressing, Reason: argocd.ReasonSpecNotWritten}, nil
	case verify && f.verified[r.ID] == 0:
		return argocd.Observation{State: argocd.StateProgressing, Reason: argocd.ReasonSyncNotStarted}, nil
	case !verify && f.priorFailed[r.ID] && f.verified[r.ID] == 0:
		return argocd.Observation{State: argocd.StateProgressing, Reason: argocd.ReasonPriorSyncFailed}, nil
	case f.unhealthy[r.ID] != "":
		return argocd.Observation{State: argocd.StateProgressing, Reason: f.unhealthy[r.ID]}, nil
	}
	return argocd.Observation{State: f.state, Reason: string(f.state), SpecDigest: f.liveSpec(r.ID)}, nil
}

func (f *fakeEngine) Remove(_ context.Context, _, name, uid, _ string) error {
	f.removed[name] = uid
	f.order = append(f.order, "Remove "+name)
	return nil
}

func (f *fakeEngine) Retired(_ context.Context, name, uid string) (argocd.Observation, error) {
	if f.removed[name] == uid && f.gone[name] {
		f.inventory = slices.DeleteFunc(f.inventory, func(i argocd.InventoryItem) bool { return i.ReleaseID == name })
		return argocd.Observation{State: argocd.StateReady, Reason: argocd.ReasonRetired}, nil
	}
	return argocd.Observation{State: argocd.StateProgressing, Reason: "Deleting"}, nil
}

// Inventory names each object after its release, as Argo CD Applications are.
func (f *fakeEngine) Inventory(_ context.Context, runtimeUID string) ([]argocd.InventoryItem, error) {
	if items, ok := f.foreign[runtimeUID]; ok {
		return items, nil
	}
	for i := range f.inventory {
		if f.inventory[i].Name == "" {
			f.inventory[i].Name = f.inventory[i].ReleaseID
		}
	}
	return f.inventory, nil
}

// Suspend records the requested suspension and reports a change once per
// toggle, as the adapter does.
func (f *fakeEngine) Suspend(_ context.Context, _ string, suspended bool) ([]string, error) {
	if f.suspended == suspended {
		return nil, nil
	}
	f.suspended = suspended
	f.order = append(f.order, fmt.Sprintf("Suspend %t", suspended))
	return []string{"cert-manager"}, nil
}

// Orphan records the runtime whose objects were annotated.
func (f *fakeEngine) Orphan(_ context.Context, runtimeUID string) ([]string, error) {
	f.order = append(f.order, "Orphan "+runtimeUID)
	names := make([]string, 0, len(f.inventory))
	for _, i := range f.inventory {
		names = append(names, i.ReleaseID)
	}
	return names, nil
}

// Foreign lists the objects the test says other runtimes own.
func (f *fakeEngine) Foreign(context.Context, string) (map[string][]string, error) {
	out := map[string][]string{}
	for owner, items := range f.foreign {
		for _, i := range items {
			out[owner] = append(out[owner], i.ReleaseID)
		}
	}
	return out, nil
}

// Adopt moves an object of another runtime into this runtime's inventory.
func (f *fakeEngine) Adopt(_ context.Context, fromUID, _, name, uid string) error {
	if err := f.inject("Adopt "+name, false); err != nil {
		return err
	}
	f.order = append(f.order, "Adopt "+name)
	for i, it := range f.foreign[fromUID] {
		if it.Name == name && it.UID == uid {
			f.inventory = append(f.inventory, it)
			f.foreign[fromUID] = slices.Delete(f.foreign[fromUID], i, i+1)
			if len(f.foreign[fromUID]) == 0 {
				delete(f.foreign, fromUID)
			}
			return nil
		}
	}
	return operrors.New(operrors.ErrCodeNotFound, "no object "+name)
}

// RebuildLive returns the plan the test says the live objects rebuild to.
func (f *fakeEngine) RebuildLive(context.Context, string, ...string) (*plan.Document, []*plan.Content, error) {
	if f.rebuildErr != nil {
		return nil, nil, f.rebuildErr
	}
	if f.rebuilt == nil {
		return nil, nil, operrors.New(operrors.ErrCodeNotFound, "no Applications")
	}
	return f.rebuilt, f.contents, nil
}

// clock is a settable test clock.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newRollout(t *testing.T, engine *fakeEngine, bypass bool) (*RolloutReconciler, *clock) {
	t.Helper()
	cfg := testConfig()
	cfg.BypassApprovals = bypass
	c := &clock{now: time.Now()}
	return &RolloutReconciler{Client: k8s, Reader: k8s, Config: cfg, Engine: engine, Now: c.Now}, c
}

// resolved records a candidate for the runtime as the resolver would.
func resolved(t *testing.T, ctx context.Context, digest string) {
	t.Helper()
	rt := getRuntime(t, ctx)
	r := &ResolverReconciler{Client: k8s, Reader: k8s, Config: testConfig()}
	res := &resolution{status: metav1.ConditionTrue, reason: ReasonResolved, candidate: digest, inputDigest: plan.DigestOf([]byte(digest))}
	if err := r.applyStatus(ctx, rt, res); err != nil {
		t.Fatalf("apply resolver status: %v", err)
	}
}

func reconcileRollout(t *testing.T, ctx context.Context, r *RolloutReconciler) {
	t.Helper()
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
}

func rollouts(t *testing.T, ctx context.Context) []v1alpha1.RuntimeRollout {
	t.Helper()
	list := &v1alpha1.RuntimeRolloutList{}
	if err := k8s.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

var rolloutName = regexp.MustCompile(`^rollout-[a-f0-9]{16}-[0-9]+$`)

// A candidate becomes a rollout with a deterministic name; with the
// development bypass it installs one release per step and promotes the
// plan only once the engine reports current-revision readiness.
func TestRolloutInstallsCandidate(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	digest := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, digest)
	engine := newEngine()
	r, clk := newRollout(t, engine, true)

	reconcileRollout(t, ctx, r)
	ros := rollouts(t, ctx)
	if len(ros) != 1 || !rolloutName.MatchString(ros[0].Name) {
		t.Fatalf("rollouts = %+v", ros)
	}
	ro := ros[0]
	if ro.Spec.ToPlanDigest != digest || ro.Spec.FromPlanDigest != "" || !ro.Spec.ApprovalRequired ||
		len(ro.Spec.Steps) != 1 || ro.Spec.Steps[0].Operation != v1alpha1.OperationInstall {

		t.Fatalf("rollout spec = %+v", ro.Spec)
	}
	if got := getRuntime(t, ctx); got.Status.Rollout != ro.Name || cond(got, v1alpha1.ConditionReady).Status != metav1.ConditionFalse {
		t.Fatalf("runtime during rollout: %+v", got.Status)
	}

	reconcileRollout(t, ctx, r) // approval bypassed, step started, Ensure
	if engine.ensured["cert-manager"] != digest {
		t.Fatalf("Ensure = %v", engine.ensured)
	}
	reconcileRollout(t, ctx, r) // observed Ready, promoted
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(&ro), &ro); err != nil {
		t.Fatal(err)
	}
	if ro.Labels[v1alpha1.LabelOutcome] != "Succeeded" || ro.Status.Phase != v1alpha1.PhaseSucceeded || ro.Status.Approval != v1alpha1.ApprovalBypassed {
		t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
	}
	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	if got.Status.ActivePlanDigest != digest || cond(got, v1alpha1.ConditionReady).Status != metav1.ConditionTrue ||
		cond(got, v1alpha1.ConditionAvailable).Status != metav1.ConditionTrue || cond(got, v1alpha1.ConditionResolved).Status != metav1.ConditionTrue {

		t.Fatalf("runtime after rollout: %+v", got.Status)
	}

	// A new candidate upgrades the release from the live inventory.
	next := putPlan(t, ctx, rt, "40m")
	resolved(t, ctx, next)
	// The new plan's creationTimestamp has one-second resolution; move the
	// clock past its stabilization window.
	clk.now = time.Now().Add(2 * time.Second)
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", Name: "cert-manager", PlanDigest: digest}}
	reconcileRollout(t, ctx, r)
	var upgrade *v1alpha1.RuntimeRollout
	for _, x := range rollouts(t, ctx) {
		if x.Spec.ToPlanDigest == next {
			upgrade = &x
		}
	}
	if upgrade == nil || upgrade.Spec.FromPlanDigest != digest || upgrade.Spec.Steps[0].Operation != v1alpha1.OperationUpgrade ||
		upgrade.Spec.Steps[0].Assessment != v1alpha1.AssessmentUnknown {

		t.Fatalf("upgrade rollout = %+v", upgrade)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready during the upgrade = %+v", c)
	}
}

// Without an approval or the bypass, a rollout waits and the runtime stalls.
func TestRolloutWaitsForApproval(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	r, _ := newRollout(t, engine, false)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	ro := rollouts(t, ctx)[0]
	if ro.Status.Phase != v1alpha1.PhaseAwaitingApproval || ro.Status.Approval != v1alpha1.ApprovalPending || len(engine.ensured) != 0 {
		t.Fatalf("rollout = %+v, ensured %v", ro.Status, engine.ensured)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Status != metav1.ConditionTrue || c.Reason != ReasonAwaitingApproval {
		t.Fatalf("Stalled = %+v", c)
	}
}

// A new plan waits for the stabilization window before a rollout exists.
func TestRolloutWaitsForStabilization(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	r, _ := newRollout(t, newEngine(), true)
	reconcileRollout(t, ctx, r)
	if n := len(rollouts(t, ctx)); n != 0 {
		t.Fatalf("%d rollouts inside the stabilization window", n)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReconciling); c.Reason != ReasonStabilizing {
		t.Fatalf("Reconciling = %+v", c)
	}
}

// A plan belongs to the runtime UID it records: a target plan that
// records an earlier runtime's UID, as the candidate or pinned, is never
// rolled out. Its engine objects would carry that UID, which this runtime's
// inventory never sees.
func TestRolloutRefusesAnotherRuntimesPlan(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned=%t", pinned), func(t *testing.T) {
			ctx := testContext(t)
			rt := freshRuntime(t, ctx, stabilized)
			earlier := rt.DeepCopy()
			earlier.UID = earlierUID
			foreign := putPlan(t, ctx, earlier, "50m")
			candidate := foreign
			if pinned {
				candidate = putPlan(t, ctx, rt, "60m")
				patch := client.MergeFrom(rt.DeepCopy())
				rt.Spec.Selection.Policy, rt.Spec.Selection.PinnedPlanDigest = v1alpha1.SelectionPinned, foreign
				if err := k8s.Patch(ctx, rt, patch); err != nil {
					t.Fatal(err)
				}
			}
			resolved(t, ctx, candidate)
			engine := newEngine()
			r, clk := newRollout(t, engine, true)
			clk.now = time.Now().Add(2 * time.Second)
			reconcileRollout(t, ctx, r)
			reconcileRollout(t, ctx, r)
			c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
			refused := c.Status == metav1.ConditionTrue && c.Reason == ReasonPlanOfAnotherRuntime &&
				strings.Contains(c.Message, foreign) && strings.Contains(c.Message, earlierUID)
			if !refused || len(rollouts(t, ctx)) != 0 || len(engine.order) != 0 {
				t.Fatalf("Stalled %+v, %d rollouts, writes %v", c, len(rollouts(t, ctx)), engine.order)
			}
		})
	}
}

// A plan rebuilt from engine objects has no recipe, so deployment validation
// cannot run against it. While validation is required, a target
// without a recipe, candidate or pinned, is never rolled out: the runtime
// stalls naming the plan and the ways forward, and nothing is written. With
// validation disabled it rolls out.
func TestRolloutRefusesAPlanWithoutARecipeToValidate(t *testing.T) {
	tests := []struct {
		name       string
		pinned     bool
		validation v1alpha1.DeploymentValidation
		refused    bool
	}{
		{"candidate", false, v1alpha1.DeploymentValidationRequired, true},
		{"pinned", true, v1alpha1.DeploymentValidationRequired, true},
		{"validation disabled", true, v1alpha1.DeploymentValidationDisabled, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
				stabilized(rt)
				rt.Spec.Validation.Deployment = tt.validation
			})
			rebuilt := putPlan(t, ctx, rt, "50m")
			rebuild(t, ctx, rebuilt)
			candidate := rebuilt
			if tt.pinned {
				candidate = putPlan(t, ctx, rt, "60m")
				patch := client.MergeFrom(rt.DeepCopy())
				rt.Spec.Selection.Policy, rt.Spec.Selection.PinnedPlanDigest = v1alpha1.SelectionPinned, rebuilt
				if err := k8s.Patch(ctx, rt, patch); err != nil {
					t.Fatal(err)
				}
			}
			resolved(t, ctx, candidate)
			engine := newEngine()
			r, clk := newRollout(t, engine, true)
			clk.now = time.Now().Add(2 * time.Second)
			reconcileRollout(t, ctx, r)
			reconcileRollout(t, ctx, r)
			if !tt.refused {
				if n := len(rollouts(t, ctx)); n != 1 {
					t.Fatalf("%d rollouts to a rebuilt plan with validation disabled, want 1", n)
				}
				return
			}
			c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled)
			refused := c.Status == metav1.ConditionTrue && c.Reason == ReasonPlanWithoutRecipe &&
				strings.Contains(c.Message, rebuilt) && strings.Contains(c.Message, "spec.validation.deployment")
			if !refused || len(rollouts(t, ctx)) != 0 || len(engine.order) != 0 {
				t.Fatalf("Stalled %+v, %d rollouts, writes %v", c, len(rollouts(t, ctx)), engine.order)
			}
		})
	}
}

// A step that is not ready within actionTimeout fails the rollout; the
// failed transition then stalls the runtime instead of being retried.
func TestRolloutFailsOnActionTimeout(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	if ro := rollouts(t, ctx)[0]; ro.Status.Phase != v1alpha1.PhaseProgressing || !strings.Contains(ro.Status.Message, "Progressing") {
		t.Fatalf("rollout = %+v", ro.Status)
	}
	clk.now = clk.now.Add(time.Hour)
	reconcileRollout(t, ctx, r)
	ro := rollouts(t, ctx)[0]
	if ro.Labels[v1alpha1.LabelOutcome] != "Failed" || !strings.Contains(ro.Status.Message, "not ready within") {
		t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
	}
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Status != metav1.ConditionTrue || c.Reason != "RolloutFailed" {
		t.Fatalf("Stalled = %+v", c)
	}
	reconcileRollout(t, ctx, r)
	if len(recorder.Events) != 1 {
		t.Fatalf("%d Events for one stall; want one Stalled", len(recorder.Events))
	}
	if e := <-recorder.Events; !strings.Contains(e, EventStalled) || !strings.Contains(e, "RolloutFailed") {
		t.Fatalf("Event = %q", e)
	}
	if n := len(rollouts(t, ctx)); n != 1 {
		t.Fatalf("a failed transition was retried without a request (%d rollouts)", n)
	}
}

// A transient error that persists while a step is in flight ends the step
// at its action timeout, naming the error, instead of retrying until the
// rollout's deadline with an empty message.
func TestPersistentTransientErrorEndsTheStep(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // step started: Ensure

	recorder := events.NewFakeRecorder(10)
	r.Events = recorder
	engine.observeErr = operrors.New(operrors.ErrCodeUnavailable, "the API server timed out")
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("a transient error within the action timeout is not retried")
	}
	if e := <-recorder.Events; !strings.Contains(e, EventReconcileFailed) || !strings.Contains(e, "the API server timed out") {
		t.Fatalf("Event = %q; a failing reconcile is visible without logs", e)
	}
	clk.now = clk.now.Add(defaults.RolloutActionTimeout + time.Minute)
	reconcileRollout(t, ctx, r)
	ro := rollouts(t, ctx)[0]
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || !strings.Contains(ro.Status.Message, "not ready within") ||
		!strings.Contains(ro.Status.Message, "the API server timed out") {

		t.Fatalf("after the action timeout: %v %+v", ro.Labels, ro.Status)
	}
}

// setHalt pauses or resumes the runtime.
func setHalt(t *testing.T, ctx context.Context, set func(*v1alpha1.ClusterRuntime, bool), on bool) {
	t.Helper()
	rt := getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	set(rt, on)
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
}

func pause(rt *v1alpha1.ClusterRuntime, on bool)         { rt.Spec.Paused = on }
func suspendEngine(rt *v1alpha1.ClusterRuntime, on bool) { rt.Spec.SuspendEngine = on }

// Time halted does not count against the step in flight: while the
// runtime is paused or its engine suspended, the step is observed, its
// release reads Suspended and nothing is written to it; on resume its
// action timeout starts again, so it fails with its real cause only a full
// timeout later.
func TestHaltedTimeDoesNotCountAgainstTheStep(t *testing.T) {
	for _, tc := range []struct {
		reason string
		set    func(*v1alpha1.ClusterRuntime, bool)
	}{{ReasonPaused, pause}, {ReasonEngineSuspended, suspendEngine}} {
		t.Run(tc.reason, func(t *testing.T) {
			ctx := testContext(t)
			rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
				rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
			})
			resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
			engine := newEngine()
			engine.state = argocd.StateProgressing
			r, clk := newRollout(t, engine, true)
			reconcileRollout(t, ctx, r) // create
			reconcileRollout(t, ctx, r) // step started: Ensure

			setHalt(t, ctx, tc.set, true)
			clk.now = clk.now.Add(45 * time.Minute)
			reconcileRollout(t, ctx, r)
			ro := rollouts(t, ctx)[0]
			if ro.Labels[v1alpha1.LabelOutcome] != "" || releaseState(&ro, "cert-manager") != v1alpha1.ReleaseSuspended ||
				!strings.Contains(ro.Status.Message, "Progressing") || !strings.Contains(ro.Status.Message, tc.reason) {

				t.Fatalf("while halted: %v %+v", ro.Labels, ro.Status)
			}

			setHalt(t, ctx, tc.set, false)
			reconcileRollout(t, ctx, r)
			ro = rollouts(t, ctx)[0]
			if ro.Labels[v1alpha1.LabelOutcome] != "" || releaseState(&ro, "cert-manager") != v1alpha1.ReleaseProgressing {
				t.Fatalf("resumed after 45m halted: %v %+v", ro.Labels, ro.Status)
			}
			clk.now = clk.now.Add(defaults.RolloutActionTimeout - time.Minute)
			reconcileRollout(t, ctx, r)
			if ro = rollouts(t, ctx)[0]; ro.Labels[v1alpha1.LabelOutcome] != "" {
				t.Fatalf("failed before a full action timeout from the resume: %+v", ro.Status)
			}
			clk.now = clk.now.Add(2 * time.Minute)
			reconcileRollout(t, ctx, r)
			if ro = rollouts(t, ctx)[0]; ro.Labels[v1alpha1.LabelOutcome] != "Failed" || !strings.Contains(ro.Status.Message, "not ready within") {
				t.Fatalf("a full action timeout after the resume: %v %+v", ro.Labels, ro.Status)
			}
			writes := slices.DeleteFunc(slices.Clone(engine.order), func(op string) bool { return strings.HasPrefix(op, "Suspend ") })
			if !slices.Equal(writes, []string{"Ensure cert-manager"}) {
				t.Fatalf("engine writes %v", engine.order)
			}
		})
	}
}

// A step that becomes ready while the runtime is paused is recorded Ready
// without an engine write, and the next step starts only on resume.
func TestHaltedStepIsStillObserved(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, twoReleases(t, ctx, rt))
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // first step started
	writes := slices.Clone(engine.order)

	setHalt(t, ctx, pause, true)
	engine.state = argocd.StateReady
	reconcileRollout(t, ctx, r)
	ro := rollouts(t, ctx)[0]
	if releaseState(&ro, "cert-manager") != v1alpha1.ReleaseReady || ro.Status.Current != nil || !slices.Equal(engine.order, writes) {
		t.Fatalf("while paused: %+v, engine writes %v", ro.Status, engine.order)
	}
	setHalt(t, ctx, pause, false)
	reconcileRollout(t, ctx, r)
	if len(engine.order) != len(writes)+1 {
		t.Fatalf("the next step did not start on resume: %v", engine.order)
	}
}

// stallResolution reports the runtime's inputs as no longer resolving, as
// the resolver does when the cluster stops matching its criteria; the
// candidate stays.
func stallResolution(t *testing.T, ctx context.Context, digest, reason string) {
	t.Helper()
	r := &ResolverReconciler{Client: k8s, Reader: k8s, Config: testConfig()}
	res := &resolution{status: metav1.ConditionFalse, reason: reason, message: reason + " in test", candidate: digest,
		inputDigest: plan.DigestOf([]byte(digest))}
	if err := r.applyStatus(ctx, getRuntime(t, ctx), res); err != nil {
		t.Fatalf("apply resolver status: %v", err)
	}
}

// A cluster that stops matching its criteria stops changing: while
// resolution is stalled, an approved rollout's step in flight is observed
// to completion and no further step starts; it continues once resolution
// recovers.
func TestResolutionStallHoldsTheRollout(t *testing.T) {
	for _, reason := range []string{discovery.ReasonHeterogeneous, discovery.ReasonCriteriaConflict} {
		t.Run(reason, func(t *testing.T) {
			ctx := testContext(t)
			rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
				rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
			})
			digest := twoReleases(t, ctx, rt)
			resolved(t, ctx, digest)
			engine := newEngine()
			engine.state = argocd.StateProgressing
			r, _ := newRollout(t, engine, true)
			reconcileRollout(t, ctx, r) // create
			reconcileRollout(t, ctx, r) // first step started
			writes := slices.Clone(engine.order)

			stallResolution(t, ctx, digest, reason)
			engine.state = argocd.StateReady
			reconcileRollout(t, ctx, r)
			reconcileRollout(t, ctx, r)
			ro := rollouts(t, ctx)[0]
			if releaseState(&ro, "cert-manager") != v1alpha1.ReleaseReady || ro.Status.Current != nil ||
				!slices.Equal(engine.order, writes) || !strings.Contains(ro.Status.Message, reason) {

				t.Fatalf("while resolution is stalled: %+v, engine writes %v", ro.Status, engine.order)
			}

			resolved(t, ctx, digest)
			reconcileRollout(t, ctx, r)
			if len(engine.order) != len(writes)+1 {
				t.Fatalf("the next step did not start once resolution recovered: %v", engine.order)
			}
		})
	}
}

// Terminal conditions end a rollout before any engine write.
func TestRolloutTerminalStates(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*v1alpha1.RuntimeRollout, *clock)
		want    v1alpha1.RolloutPhase
	}{
		{"deadline", func(_ *v1alpha1.RuntimeRollout, c *clock) { c.now = c.now.Add(24 * time.Hour) }, v1alpha1.PhaseExpired},
		{"revoked", func(ro *v1alpha1.RuntimeRollout, c *clock) {
			// Approve first: a revocation needs an existing approval.
			ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: "a", ExpiresAt: metav1.NewTime(c.now.Add(time.Hour)), Reason: "r"}
			if err := k8s.Update(context.Background(), ro); err != nil {
				panic(err)
			}
			ro.Spec.Approval.RevokedBy, ro.Spec.Approval.RevokedAt, ro.Spec.Approval.RevokeReason = "b", &metav1.Time{Time: c.now}, "stop"
		}, v1alpha1.PhaseRevoked},
		{"approval expired", func(ro *v1alpha1.RuntimeRollout, c *clock) {
			ro.Spec.Approval = &v1alpha1.Approval{ApprovedBy: "a", ExpiresAt: metav1.NewTime(c.now.Add(-time.Minute)), Reason: "r"}
		}, v1alpha1.PhaseExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
				rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
			})
			resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
			engine := newEngine()
			r, clk := newRollout(t, engine, false)
			reconcileRollout(t, ctx, r)
			ro := rollouts(t, ctx)[0]
			if tt.prepare(&ro, clk); ro.Spec.Approval != nil {
				if err := k8s.Update(ctx, &ro); err != nil {
					t.Fatalf("set approval: %v", err)
				}
			}
			reconcileRollout(t, ctx, r)
			ro = rollouts(t, ctx)[0]
			if ro.Labels[v1alpha1.LabelOutcome] != string(tt.want) || len(engine.ensured) != 0 {
				t.Fatalf("outcome = %q (want %s), ensured %v", ro.Labels[v1alpha1.LabelOutcome], tt.want, engine.ensured)
			}
		})
	}
}

// Deployment validation, which this build cannot run yet, stalls the
// rollout with a specific reason instead of acting.
func TestRolloutStallsOnUnsupportedWork(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
		rt.Spec.Validation.Deployment = v1alpha1.DeploymentValidationRequired
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	r, _ := newRollout(t, engine, true)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro := rollouts(t, ctx)[0]
	if ro.Status.Phase != v1alpha1.PhaseValidating || ro.Labels[v1alpha1.LabelOutcome] != "" {
		t.Fatalf("rollout = %+v", ro.Status)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason != ReasonValidationMissing {
		t.Fatalf("Stalled = %+v", c)
	}
	// Ready names why validation waits, not only that it does.
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); !strings.Contains(c.Message, "validation.enabled") {
		t.Fatalf("Ready = %+v", c)
	}
}

// A transition upgrades changed releases, verifies an unchanged readiness
// release whose parent changed, and removes releases the target lacks in
// reverse active order, each bound to its object's UID.
func TestSteps(t *testing.T) {
	t.Parallel()
	rel := func(id string, phase plan.Phase, parent, chart, values string) plan.Release {
		return plan.Release{ID: id, Phase: phase, ReadinessFor: parent, TargetNamespace: "ns",
			Chart:  plan.Chart{Digest: plan.DigestOf([]byte(chart))},
			Values: plan.DigestedRef{Digest: plan.DigestOf([]byte(values))}}
	}
	from := &plan.Document{Releases: []plan.Release{
		rel("cert-manager", plan.PhasePrimary, "", "c1", "v1"),
		rel("gpu-operator", plan.PhasePrimary, "", "g1", "v1"),
		rel("gpu-operator-readiness", plan.PhaseReadiness, "gpu-operator", "r1", "v1"),
		rel("nfd", plan.PhasePrimary, "", "n1", "v1"),
		rel("nvsentinel", plan.PhasePrimary, "", "s1", "v1"),
	}}
	to := &plan.Document{Releases: []plan.Release{
		rel("cert-manager", plan.PhasePrimary, "", "c1", "v1"),
		rel("gpu-operator", plan.PhasePrimary, "", "g2", "v1"),
		rel("gpu-operator-readiness", plan.PhaseReadiness, "gpu-operator", "r1", "v1"),
		rel("kai-scheduler", plan.PhasePrimary, "", "k1", "v1"),
	}}
	inventory := []argocd.InventoryItem{
		{ReleaseID: "cert-manager", UID: "u-c"}, {ReleaseID: "gpu-operator", UID: "u-g"},
		{ReleaseID: "gpu-operator-readiness", UID: "u-r"}, {ReleaseID: "nfd", UID: "u-n"},
		{ReleaseID: "nvsentinel", UID: "u-s"}, {ReleaseID: "stray", UID: "u-x"},
	}
	got := steps(to, from, inventory)
	want := []v1alpha1.RolloutStep{
		{Release: "cert-manager", Operation: v1alpha1.OperationUpgrade, Assessment: v1alpha1.AssessmentNotApplicable},
		{Release: "gpu-operator", Operation: v1alpha1.OperationUpgrade, Assessment: v1alpha1.AssessmentNotApplicable},
		{Release: "gpu-operator-readiness", Operation: v1alpha1.OperationVerify, Assessment: v1alpha1.AssessmentNotApplicable},
		{Release: "kai-scheduler", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable},
		{Release: "stray", Operation: v1alpha1.OperationRemove, Assessment: v1alpha1.AssessmentNotApplicable, UID: "u-x"},
		{Release: "nvsentinel", Operation: v1alpha1.OperationRemove, Assessment: v1alpha1.AssessmentNotApplicable, UID: "u-s"},
		{Release: "nfd", Operation: v1alpha1.OperationRemove, Assessment: v1alpha1.AssessmentNotApplicable, UID: "u-n"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("steps() =\n%+v\nwant\n%+v", got, want)
	}

	// A changed readiness release upgrades; without the active plan nothing
	// is verified.
	to.Releases[2] = rel("gpu-operator-readiness", plan.PhaseReadiness, "gpu-operator", "r2", "v1")
	if got = steps(to, from, inventory); got[2].Operation != v1alpha1.OperationUpgrade {
		t.Errorf("changed readiness release: %+v", got[2])
	}
	to.Releases[2] = rel("gpu-operator-readiness", plan.PhaseReadiness, "gpu-operator", "r1", "v1")
	if got = steps(to, nil, inventory); got[2].Operation != v1alpha1.OperationUpgrade {
		t.Errorf("no active plan: %+v", got[2])
	}
	// An unchanged parent leaves its gate alone.
	to.Releases[1] = rel("gpu-operator", plan.PhasePrimary, "", "g1", "v1")
	if got = steps(to, from, inventory); got[2].Operation != v1alpha1.OperationUpgrade {
		t.Errorf("unchanged parent: %+v", got[2])
	}
}

// succeedTo records a succeeded rollout to digest, making it the active plan.
func succeedTo(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, digest string) {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "rollout-" + strings.TrimPrefix(digest, "sha256:")[:16] + "-1",
			Labels: map[string]string{v1alpha1.LabelRuntime: rt.Name, v1alpha1.LabelOutcome: string(v1alpha1.PhaseSucceeded)},
		},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion, RuntimeUID: string(rt.UID), ToPlanDigest: digest,
			StartingInventoryDigest: digest, Deadline: metav1.NewTime(time.Now().Add(time.Hour)),
			ActionTimeout: metav1.Duration{Duration: time.Minute},
			Steps:         []v1alpha1.RolloutStep{{Release: "x", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable}},
		},
	}
	if err := k8s.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
}

// A rollout verifies an unchanged readiness release after its parent and
// removes a release the target lacks, waiting for its retirement.
func TestRolloutVerifiesAndRemoves(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	gate := testRelease("gpu-operator-readiness", plan.PhaseReadiness, "gpu-operator", `{}`)
	active := putReleases(t, ctx, rt, testRelease("gpu-operator", plan.PhasePrimary, "", `{"v":1}`), gate,
		testRelease("nvsentinel", plan.PhasePrimary, "", `{}`))
	succeedTo(t, ctx, rt, active)
	target := putReleases(t, ctx, rt, testRelease("gpu-operator", plan.PhasePrimary, "", `{"v":2}`), gate)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{
		{ReleaseID: "gpu-operator", UID: "u-g"}, {ReleaseID: "gpu-operator-readiness", UID: "u-r"}, {ReleaseID: "nvsentinel", UID: "u-s"},
	}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 8 {
		reconcileRollout(t, ctx, r)
	}
	if want := []string{"Ensure gpu-operator", "Verify gpu-operator-readiness", "Remove nvsentinel"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
	if engine.removed["nvsentinel"] != "u-s" {
		t.Fatalf("Remove UID = %q", engine.removed["nvsentinel"])
	}
	ro := rollouts(t, ctx)
	var current *v1alpha1.RuntimeRollout
	for i := range ro {
		if ro[i].Spec.ToPlanDigest == target {
			current = &ro[i]
		}
	}
	if current == nil || current.Labels[v1alpha1.LabelOutcome] != "" || releaseState(current, "nvsentinel") != v1alpha1.ReleaseDeleting {
		t.Fatalf("rollout while deleting = %+v", current)
	}

	engine.gone["nvsentinel"] = true
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		t.Fatal(err)
	}
	if current.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || releaseState(current, "nvsentinel") != v1alpha1.ReleaseRemoved {
		t.Fatalf("rollout after retirement = %v %+v", current.Labels, current.Status)
	}
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != target || cond(got, v1alpha1.ConditionReady).Status != metav1.ConditionTrue {
		t.Fatalf("runtime = %+v", got.Status)
	}
}

// A rollout that started removing a release and then ended without
// succeeding never has that release written again: the active plan still
// lists it, but nothing rolls back on its own.
func TestUnfinishedRolloutDoesNotReinstallWhatItRemoved(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	active := putReleases(t, ctx, rt, testRelease("gpu-operator", plan.PhasePrimary, "", `{"v":1}`),
		testRelease("nvsentinel", plan.PhasePrimary, "", `{}`))
	succeedTo(t, ctx, rt, active)
	target := putReleases(t, ctx, rt, testRelease("gpu-operator", plan.PhasePrimary, "", `{"v":2}`))
	resolved(t, ctx, target)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "gpu-operator", UID: "u-g"}, {ReleaseID: "nvsentinel", UID: "u-s"}}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 6 {
		reconcileRollout(t, ctx, r)
	}
	if want := []string{"Ensure gpu-operator", "Remove nvsentinel"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}

	clk.now = clk.now.Add(defaults.RolloutDeadline + time.Hour)
	engine.gone["nvsentinel"], engine.missing["nvsentinel"] = true, true
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	ro := onlyRollout(t, ctx, target)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseExpired) {
		t.Fatalf("rollout = %v %+v", ro.Labels, ro.Status)
	}
	if slices.Contains(engine.order[2:], "Ensure nvsentinel") {
		t.Fatalf("the removed release was written again: %v", engine.order)
	}
}

// A failed rollout leaves a release on its target plan; when the target is
// the active plan again, the controller rolls out to it from live state.
func TestRolloutConvergesAfterFailedRollout(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	active := putPlan(t, ctx, rt, "50m")
	failed := putPlan(t, ctx, rt, "60m")
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["cert-manager"] = failed
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: failed}}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	var converge *v1alpha1.RuntimeRollout
	for _, ro := range rollouts(t, ctx) {
		if ro.Spec.ToPlanDigest == active && ro.Spec.FromPlanDigest == active {
			converge = ro.DeepCopy()
		}
	}
	if converge == nil || converge.Spec.Steps[0].Operation != v1alpha1.OperationUpgrade {
		t.Fatalf("no rollout back to the active plan: %+v", rollouts(t, ctx))
	}
	for range 4 {
		reconcileRollout(t, ctx, r)
	}
	if engine.ensured["cert-manager"] != active || cond(getRuntime(t, ctx), v1alpha1.ConditionReady).Status != metav1.ConditionTrue {
		t.Fatalf("after converging: ensured %v, Ready %+v", engine.ensured, cond(getRuntime(t, ctx), v1alpha1.ConditionReady))
	}
	before := len(rollouts(t, ctx))
	reconcileRollout(t, ctx, r)
	if len(rollouts(t, ctx)) != before {
		t.Error("a converged runtime started another rollout")
	}
}

// A failed rollout leaves the releases it moved on its target plan. The
// rollout back to the active plan assesses each release against the plan its
// live object carries, not the active plan: moving a release back is a real
// change that needs approval, as a rollback does, and nothing is written
// before it. A release still at the active plan changes nothing; otherwise
// a convergence could downgrade components without approval.
func TestConvergenceAssessesTheLiveState(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	failed := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"60m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["cert-manager"], engine.ensured["nfd"] = failed, active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: failed},
		{ReleaseID: "nfd", UID: "u-n", PlanDigest: active}}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	var converge *v1alpha1.RuntimeRollout
	for _, ro := range rollouts(t, ctx) {
		if ro.Spec.ToPlanDigest == active && ro.Spec.FromPlanDigest == active {
			converge = ro.DeepCopy()
		}
	}
	if converge == nil {
		t.Fatalf("no rollout back to the active plan: %+v", rollouts(t, ctx))
	}
	assessments := map[string]v1alpha1.Assessment{}
	for _, s := range converge.Spec.Steps {
		assessments[s.Release] = s.Assessment
	}
	if assessments["cert-manager"] != v1alpha1.AssessmentUnknown || assessments["nfd"] != v1alpha1.AssessmentNotApplicable {
		t.Fatalf("assessments = %v; want cert-manager Unknown (60m on the failed plan back to 50m), nfd NotApplicable", assessments)
	}
	if !converge.Spec.ApprovalRequired || converge.Status.Phase != v1alpha1.PhaseAwaitingApproval || len(engine.order) != 0 {
		t.Fatalf("approval required %t, phase %s, writes %v", converge.Spec.ApprovalRequired, converge.Status.Phase, engine.order)
	}
}

// After a rollout fails, a release it already moved is judged at the plan
// its live object carries: healthy there, it keeps the runtime Available,
// while Stalled still reports the failure.
func TestReleasesMovedByAFailedRolloutStayAvailable(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	failed := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"60m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, failed)
	endedTo(t, ctx, rt, "rollout-that-failed", active, failed, v1alpha1.PhaseFailed)
	engine := newEngine()
	engine.ensured["cert-manager"], engine.ensured["nfd"] = failed, active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: failed},
		{ReleaseID: "nfd", UID: "u-n", PlanDigest: active}}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	available, stalled := cond(got, v1alpha1.ConditionAvailable), cond(got, v1alpha1.ConditionStalled)
	if available.Status != metav1.ConditionTrue || stalled.Reason != "RolloutFailed" || len(engine.order) != 0 {
		t.Fatalf("after a failed rollout: Available %+v, Stalled %+v, writes %v", available, stalled, engine.order)
	}

	engine.unhealthy["cert-manager"] = "HealthDegraded"
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionAvailable); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "HealthDegraded") {
		t.Fatalf("a degraded moved release: Available %+v", c)
	}
}

// A new engine policy changes every plan's digest but no chart or values:
// the rollout to it rewrites each release without approval.
func TestEnginePolicyChangeNeedsNoApproval(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	rel := testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`)
	active := putPolicy(t, ctx, rt, nil, "argocd.v1", rel)
	succeedTo(t, ctx, rt, active)
	target := putPolicy(t, ctx, rt, nil, argocd.PolicyVersion, rel)
	if target == active {
		t.Fatal("the engine policy is not part of the plan digest")
	}
	resolved(t, ctx, target)
	engine := newEngine()
	engine.ensured["cert-manager"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, clk := newRollout(t, engine, false)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	if ro.Spec.ApprovalRequired || len(ro.Spec.Steps) != 1 || ro.Spec.Steps[0].Operation != v1alpha1.OperationUpgrade ||
		ro.Spec.Steps[0].Assessment != v1alpha1.AssessmentNotApplicable || engine.ensured["cert-manager"] != target {

		t.Fatalf("rollout to the new policy: %+v %+v, ensured %v", ro.Spec, ro.Status, engine.ensured)
	}
}

// Every step of a readiness release syncs explicitly, an install included:
// Argo CD starts no sync for a change that touches only hooks.
func TestRolloutSyncsReadinessInstall(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	digest := putReleases(t, ctx, rt, testRelease("gpu-operator", plan.PhasePrimary, "", `{}`),
		testRelease("gpu-operator-readiness", plan.PhaseReadiness, "gpu-operator", `{}`))
	resolved(t, ctx, digest)
	engine := newEngine()
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 6 {
		reconcileRollout(t, ctx, r)
	}
	if want := []string{"Ensure gpu-operator", "Verify gpu-operator-readiness"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != digest {
		t.Fatalf("runtime = %+v", got.Status)
	}
}

// A step whose Application already carries the plan, as in a retry after a
// failed rollout, but whose sync of that revision failed before the step
// starts the sync itself: automated sync never retries a failed revision.
func TestRolloutRetriesPriorFailedSync(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	digest := putReleases(t, ctx, rt, testRelease("kai-scheduler", plan.PhasePrimary, "", `{}`))
	resolved(t, ctx, digest)
	engine := newEngine()
	engine.ensured["kai-scheduler"] = digest
	engine.priorFailed["kai-scheduler"] = true
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 6 {
		reconcileRollout(t, ctx, r)
	}
	if want := []string{"Verify kai-scheduler"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != digest {
		t.Fatalf("runtime = %+v", got.Status)
	}
}

// With no rollout in progress, an active release's Application that was
// deleted by hand or changed by someone else is written again, with an
// Event naming the managers that were overridden; paused runtimes repair
// too.
func TestRolloutRepairsEngineDrift(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) { rt.Spec.Paused = true })
	digest := putPlan(t, ctx, rt, "50m")
	succeedTo(t, ctx, rt, digest)
	resolved(t, ctx, digest)
	engine := newEngine()
	engine.ensured["cert-manager"] = digest
	r, _ := newRollout(t, engine, true)
	recorder := events.NewFakeRecorder(10)
	r.Events = recorder

	reconcileRollout(t, ctx, r)
	if len(engine.order) != 0 || cond(getRuntime(t, ctx), v1alpha1.ConditionReady).Status != metav1.ConditionTrue {
		t.Fatalf("a steady runtime was written: %v", engine.order)
	}

	engine.drifted["cert-manager"], engine.overridden = true, []string{"kubectl-edit"}
	reconcileRollout(t, ctx, r)
	if !slices.Equal(engine.order, []string{"Ensure cert-manager"}) {
		t.Fatalf("drift repair calls = %v", engine.order)
	}
	if e := <-recorder.Events; !strings.Contains(e, EventFieldOwnershipOverridden) || !strings.Contains(e, "kubectl-edit") {
		t.Fatalf("Event = %q", e)
	}

	engine.missing["cert-manager"], engine.overridden = true, nil
	reconcileRollout(t, ctx, r)
	if e := <-recorder.Events; !strings.Contains(e, EventApplicationRecreated) {
		t.Fatalf("Event = %q", e)
	}
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue || len(engine.order) != 2 {
		t.Fatalf("after repair: Ready = %+v, calls %v", c, engine.order)
	}
}

// activeAndTarget stores an active plan of cert-manager, nfd and nvsentinel,
// all live at it, and resolves a target that upgrades the first two,
// installs kai-scheduler and removes nvsentinel: four steps.
func activeAndTarget(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime) *fakeEngine {
	t.Helper()
	active := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"v":1}`),
		testRelease("nfd", plan.PhasePrimary, "", `{"v":1}`), testRelease("nvsentinel", plan.PhasePrimary, "", `{}`))
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"v":2}`),
		testRelease("nfd", plan.PhasePrimary, "", `{"v":2}`), testRelease("kai-scheduler", plan.PhasePrimary, "", `{}`)))
	engine := newEngine()
	for _, id := range []string{"cert-manager", "nfd", "nvsentinel"} {
		engine.ensured[id] = active
		engine.inventory = append(engine.inventory, argocd.InventoryItem{ReleaseID: id, UID: "u-" + id, PlanDigest: active})
	}
	return engine
}

// While a rollout is in progress, Ready names it and its step, and
// Available observes each active release at the plan its Application
// carries: a release the rollout moved is healthy at the target plan, and
// one it removes no longer counts.
func TestRolloutReportsProgress(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	engine := activeAndTarget(t, ctx, rt)
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	check := func(step string) {
		t.Helper()
		got := getRuntime(t, ctx)
		want := "rollout " + got.Status.Rollout + ": " + step
		if c := cond(got, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != ReasonRollingOut || c.Message != want {
			t.Fatalf("Ready = %+v, want RollingOut %q", c, want)
		}
		if c := cond(got, v1alpha1.ConditionAvailable); c.Status != metav1.ConditionTrue {
			t.Fatalf("Available at %q = %+v", step, c)
		}
	}

	for range 3 { // created; cert-manager upgraded; nfd written
		reconcileRollout(t, ctx, r)
	}
	if engine.ensured["cert-manager"] == engine.ensured["nvsentinel"] {
		t.Fatalf("cert-manager was not moved: %v", engine.ensured)
	}
	check("step 2/4 nfd (Upgrade)")

	for range 2 { // kai-scheduler installed; nvsentinel's removal started
		reconcileRollout(t, ctx, r)
	}
	engine.missing["nvsentinel"] = true
	reconcileRollout(t, ctx, r)
	check("step 4/4 nvsentinel (Remove)")
}

// A release the rollout moved that is unhealthy at the target plan makes
// Available False, and Available's message names it.
func TestRolloutAvailableNamesUnhealthyMovedRelease(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	engine := activeAndTarget(t, ctx, rt)
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	engine.unhealthy["cert-manager"] = "HealthDegraded"
	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	if c := cond(got, v1alpha1.ConditionAvailable); c.Status != metav1.ConditionFalse || c.Reason != ReasonNotReady ||
		c.Message != "release cert-manager: HealthDegraded" {

		t.Fatalf("Available = %+v", c)
	}
	if c := cond(got, v1alpha1.ConditionReady); c.Reason != ReasonRollingOut || !strings.Contains(c.Message, "rollout "+got.Status.Rollout+": step ") {
		t.Fatalf("Ready = %+v", c)
	}
}

// The release of the step in flight is expected to progress: it leaves
// Available True, as a rolling update leaves a Deployment available.
func TestRolloutAvailableIgnoresProgressingStepInFlight(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	engine := activeAndTarget(t, ctx, rt)
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	var ro *v1alpha1.RuntimeRollout
	ros := rollouts(t, ctx)
	for i := range ros {
		if ros[i].Name == getRuntime(t, ctx).Status.Rollout {
			ro = &ros[i]
		}
	}
	if ro == nil || ro.Status.Current == nil {
		t.Fatalf("no step in flight: %+v", ros)
	}
	engine.unhealthy[ro.Status.Current.Release] = "SyncRunning"
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionAvailable); c.Status != metav1.ConditionTrue {
		t.Fatalf("Available with %s in flight = %+v", ro.Status.Current.Release, c)
	}
}

// Ready says why the target plan is not rolled out: inside its
// stabilization window, then a rollout awaiting approval.
func TestRolloutReportsWaitingRollout(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Minute}
	})
	engine := activeAndTarget(t, ctx, rt)
	r, clk := newRollout(t, engine, false)
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); c.Reason != ReasonRollingOut || !strings.Contains(c.Message, "stabilization window") {
		t.Errorf("Ready while stabilizing = %+v", c)
	}

	clk.now = time.Now().Add(2 * time.Minute)
	reconcileRollout(t, ctx, r) // created
	reconcileRollout(t, ctx, r) // awaiting approval
	got := getRuntime(t, ctx)
	want := "rollout " + got.Status.Rollout + " awaiting approval"
	if c := cond(got, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse || c.Reason != ReasonRollingOut || c.Message != want {
		t.Fatalf("Ready = %+v, want RollingOut %q", c, want)
	}
	if c := cond(got, v1alpha1.ConditionAvailable); c.Status != metav1.ConditionTrue {
		t.Fatalf("Available = %+v", c)
	}
}

// deleteValues deletes the values ConfigMap of a plan's release, as a person
// cleaning up the operator namespace might.
func deleteValues(t *testing.T, ctx context.Context, digest, release string) string {
	t.Helper()
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	name := doc.Releases[slices.IndexFunc(doc.Releases, func(r plan.Release) bool { return r.ID == release })].Values.ConfigMap
	if err = k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
	return name
}

// A missing values ConfigMap of the active plan is reported, and status keeps
// updating, instead of every reconcile erroring on its last values.
func TestMissingActiveValuesStalls(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putPlan(t, ctx, rt, "50m")
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["cert-manager"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)

	name := deleteValues(t, ctx, active, "cert-manager")
	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	if c := cond(got, v1alpha1.ConditionStalled); c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, name) {
		t.Fatalf("Stalled = %+v; want the missing values %s named", c, name)
	}
	if c := cond(got, v1alpha1.ConditionReady); c.Status != metav1.ConditionFalse {
		t.Fatalf("Ready = %+v; the active plan cannot be observed", c)
	}
}

// A values ConfigMap missing during a rollout fails the step with that
// cause, instead of erroring until the rollout's deadline.
func TestMissingStepValuesFailsTheRollout(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, target)
	engine := newEngine()
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r)
	if len(rollouts(t, ctx)) != 1 {
		t.Fatalf("%d rollouts", len(rollouts(t, ctx)))
	}

	name := deleteValues(t, ctx, target, "cert-manager")
	reconcileRollout(t, ctx, r)
	ro := rollouts(t, ctx)[0]
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || !strings.Contains(ro.Status.Message, name) {
		t.Fatalf("rollout = %v %+v; want Failed naming %s", ro.Labels, ro.Status, name)
	}
}

// A failed engine or credentials check is reported, but never erases what
// the runtime runs: the active plan and Available stay as last
// observed, so the resolver is not woken into a new snapshot.
func TestEngineCheckFailureKeepsTheActivePlan(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putPlan(t, ctx, rt, "50m")
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["cert-manager"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != active || cond(got, v1alpha1.ConditionAvailable).Status != metav1.ConditionTrue {
		t.Fatalf("before the failure: %+v", got.Status)
	}

	engine.capabilities = operrors.New(operrors.ErrCodeUnavailable, "read AppProject aicr: timeout")
	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	if got.Status.ActivePlanDigest != active {
		t.Fatalf("a failed engine check erased the active plan: %+v", got.Status)
	}
	if c := cond(got, v1alpha1.ConditionAvailable); c.Status != metav1.ConditionTrue {
		t.Fatalf("Available = %+v; the stack was not observed to change", c)
	}
	if c := cond(got, v1alpha1.ConditionEngineAvailable); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "timeout") {
		t.Fatalf("EngineAvailable = %+v", c)
	}
}

// Without the engine nothing is written and the runtime says why; a
// resolver failure surfaces as Stalled with the resolver's reason.
func TestRolloutReportsEngineAndResolverState(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	engine := newEngine()
	engine.capabilities = operrors.New(operrors.ErrCodeUnavailable, "no Argo CD").WithContext(map[string]any{"reason": argocd.ReasonEngineMissing})
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)
	rt := getRuntime(t, ctx)
	if c := cond(rt, v1alpha1.ConditionEngineAvailable); c.Status != metav1.ConditionFalse || c.Reason != argocd.ReasonEngineMissing {
		t.Fatalf("EngineAvailable = %+v", c)
	}

	engine.capabilities = nil
	res := &resolution{status: metav1.ConditionFalse, reason: "AICRVersionMismatch", message: "mismatch"}
	if err := (&ResolverReconciler{Client: k8s, Config: testConfig()}).applyStatus(ctx, rt, res); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, ctx, r)
	rt = getRuntime(t, ctx)
	if c := cond(rt, v1alpha1.ConditionStalled); c.Status != metav1.ConditionTrue || c.Reason != "AICRVersionMismatch" {
		t.Fatalf("Stalled = %+v", c)
	}
	// Each controller kept its own conditions.
	if cond(rt, v1alpha1.ConditionResolved).Reason != "AICRVersionMismatch" || cond(rt, v1alpha1.ConditionEngineAvailable).Status != metav1.ConditionTrue {
		t.Fatalf("conditions = %+v", rt.Status.Conditions)
	}
}

func TestToRuntimeAndCacheOptions(t *testing.T) {
	t.Parallel()
	ro := &v1alpha1.RuntimeRollout{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelRuntime: "cluster"}}}
	if got := toRuntime(context.Background(), ro); len(got) != 1 || got[0].Name != "cluster" {
		t.Fatalf("toRuntime() = %v", got)
	}
	if got := toRuntime(context.Background(), &v1alpha1.RuntimeRollout{}); got != nil {
		t.Fatalf("toRuntime(unlabeled) = %v", got)
	}
	opts := CacheOptions("aicr-system", "aicr-jobs")
	if len(opts.ByObject) != 3 {
		t.Fatalf("CacheOptions() scopes %d kinds; the engine's objects have their own cache", len(opts.ByObject))
	}
	for obj, by := range opts.ByObject {
		if _, node := obj.(*corev1.Node); node {
			if by.Transform == nil {
				t.Error("Nodes are cached without the discovery transform")
			}
			continue
		}
		if len(by.Namespaces) != 1 {
			t.Errorf("%T cache is not limited to one namespace: %v", obj, by.Namespaces)
		}
	}
	if got := toSingleton(context.Background(), &corev1.Node{}); len(got) != 1 || got[0].Name != v1alpha1.SingletonName {
		t.Fatalf("toSingleton() = %v", got)
	}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}}}
	beat := n.DeepCopy()
	beat.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	relabeled := n.DeepCopy()
	relabeled.Labels["a"] = "c"
	if relevantNode.Update(event.UpdateEvent{ObjectOld: n, ObjectNew: n.DeepCopy()}) ||
		!relevantNode.Update(event.UpdateEvent{ObjectOld: n, ObjectNew: relabeled}) {

		t.Error("relevantNode does not follow discovery.Relevant")
	}
}
