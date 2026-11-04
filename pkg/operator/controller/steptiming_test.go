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
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
)

// certManager is the rollout's status entry for the test plan's release.
func certManager(t *testing.T, ro v1alpha1.RuntimeRollout) v1alpha1.ReleaseStatus {
	t.Helper()
	for _, rs := range ro.Status.Releases {
		if rs.ID == "cert-manager" {
			return rs
		}
	}
	t.Fatalf("rollout %s has no status for cert-manager: %+v", ro.Name, ro.Status.Releases)

	return v1alpha1.ReleaseStatus{}
}

// drain returns the Events recorded so far.
func drain(recorder *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// matching returns the Events whose text contains every one of parts.
func matching(all []string, parts ...string) []string {
	var out []string
	for _, e := range all {
		hit := true
		for _, p := range parts {
			hit = hit && strings.Contains(e, p)
		}
		if hit {
			out = append(out, e)
		}
	}

	return out
}

// sameSecond compares a recorded time with the clock, at the one-second
// resolution a status timestamp keeps.
func sameSecond(got *metav1.Time, want time.Time) bool {
	return got != nil && got.Unix() == want.Unix()
}

// A step records when it started, when it ended and what it last waited on,
// and each transition is an Event, so a slow rollout can be read back after
// it finishes: which steps were slow, and what each waited on.
func TestRolloutRecordsStepTiming(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.unhealthy = map[string]string{"cert-manager": "ReconcileStale"}
	r, clk := newRollout(t, engine, true)
	recorder := events.NewFakeRecorder(32)
	r.Events = recorder

	reconcileRollout(t, ctx, r) // create
	started := clk.now
	reconcileRollout(t, ctx, r) // step started, Ensure
	clk.now = clk.now.Add(10 * time.Second)
	reconcileRollout(t, ctx, r) // waits: ReconcileStale
	clk.now = clk.now.Add(10 * time.Second)
	reconcileRollout(t, ctx, r) // still ReconcileStale

	rs := certManager(t, rollouts(t, ctx)[0])
	if rs.State != v1alpha1.ReleaseProgressing || !sameSecond(rs.StartedAt, started) || rs.FinishedAt != nil ||
		rs.WaitedOn != "ReconcileStale" {

		t.Fatalf("while waiting: %+v", rs)
	}

	delete(engine.unhealthy, "cert-manager")
	clk.now = clk.now.Add(70 * time.Second)
	finished := clk.now
	reconcileRollout(t, ctx, r) // ready

	rs = certManager(t, rollouts(t, ctx)[0])
	if rs.State != v1alpha1.ReleaseReady || !sameSecond(rs.StartedAt, started) || !sameSecond(rs.FinishedAt, finished) ||
		rs.WaitedOn != "ReconcileStale" {

		t.Fatalf("after ready: %+v; a finished step keeps its start, its end and what it waited on", rs)
	}

	got := drain(recorder)
	if n := len(matching(got, EventStepStarted, "cert-manager")); n != 1 {
		t.Errorf("%d StepStarted Events for cert-manager, want 1: %q", n, got)
	}
	if n := len(matching(got, EventStepWaiting, "cert-manager", "ReconcileStale")); n != 1 {
		t.Errorf("%d StepWaiting Events for one wait reason, want 1: %q", n, got)
	}
	if n := len(matching(got, "Normal", EventStepFinished, "cert-manager", "Ready", "1m30s", "ReconcileStale")); n != 1 {
		t.Errorf("%d StepFinished Events naming the outcome, the duration and the wait, want 1: %q", n, got)
	}
}

// A step that fails records its end too, and its Event is a warning.
func TestRolloutRecordsAFailedStepsTiming(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.unhealthy = map[string]string{"cert-manager": "HealthDegraded"}
	r, clk := newRollout(t, engine, true)
	recorder := events.NewFakeRecorder(32)
	r.Events = recorder

	reconcileRollout(t, ctx, r) // create
	started := clk.now
	reconcileRollout(t, ctx, r) // step started, Ensure
	reconcileRollout(t, ctx, r) // waits: HealthDegraded
	engine.unhealthy["cert-manager"] = "HealthMissing"
	clk.now = clk.now.Add(time.Hour)
	finished := clk.now
	reconcileRollout(t, ctx, r) // action timeout, now waiting on HealthMissing

	ro := rollouts(t, ctx)[0]
	rs := certManager(t, ro)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || rs.State != v1alpha1.ReleaseFailed ||
		!sameSecond(rs.StartedAt, started) || !sameSecond(rs.FinishedAt, finished) || rs.WaitedOn != "HealthMissing" {

		t.Fatalf("after the action timeout: %v %+v; the step records the reason its failure names", ro.Labels, rs)
	}
	if got := drain(recorder); len(matching(got, "Warning", EventStepFinished, "cert-manager", "Failed")) != 1 {
		t.Errorf("Events = %q; want one StepFinished warning for the failed step", got)
	}
}

// A step whose Application already carries the plan, as a release the
// failed rollout had not reached does in a recovery that pins the previous
// plan, is not written again, so no Ensure asks Argo CD to compare it.
func TestRolloutRefreshesAStaleComparison(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	digest := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, digest)
	engine := newEngine()
	engine.ensured["cert-manager"] = digest
	engine.unhealthy = map[string]string{"cert-manager": argocd.ReasonReconcileStale}
	r, _ := newRollout(t, engine, true)

	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // step started; already written, so observed
	if engine.refreshed["cert-manager"] != 1 {
		t.Fatalf("refreshes at the step start = %d, want 1: nothing written asks Argo CD to compare",
			engine.refreshed["cert-manager"])
	}
	reconcileRollout(t, ctx, r) // observed: ReconcileStale
	if slices.Contains(engine.order, "Ensure cert-manager") {
		t.Fatalf("engine calls = %v; an already written step writes nothing", engine.order)
	}
	if engine.refreshed["cert-manager"] == 0 {
		t.Fatal("a step waiting on a stale comparison did not ask Argo CD to compare again")
	}

	engine.unhealthy["cert-manager"] = "HealthProgressing"
	before := engine.refreshed["cert-manager"]
	reconcileRollout(t, ctx, r)
	if engine.refreshed["cert-manager"] != before {
		t.Error("a step waiting on health asked for a refresh, which cannot help it")
	}
}

// captured is one Event a capture recorder saw.
type captured struct{ eventType, reason, action, note string }

// capture records every Event with the fields the events API groups by.
type capture struct {
	mu     sync.Mutex
	events []captured
}

func (c *capture) Eventf(_, _ runtime.Object, eventType, reason, action, note string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, captured{eventType, reason, action, fmt.Sprintf(note, args...)})
}

// actions returns the actions of the Events with reason, in order.
func (c *capture) actions(reason string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.reason == reason {
			out = append(out, e.action)
		}
	}

	return out
}

// The events API folds Events with the same object, reason, action and
// related object into one series whatever their notes, so each step Event
// names its release and transition in its action.
func TestStepEventsAreDistinctPerTransition(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.unhealthy = map[string]string{"cert-manager": "HealthProgressing"}
	r, _ := newRollout(t, engine, true)
	rec := &capture{}
	r.Events = rec

	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // step started
	reconcileRollout(t, ctx, r) // waits: HealthProgressing
	engine.unhealthy["cert-manager"] = argocd.ReasonReconcileStale
	reconcileRollout(t, ctx, r) // waits: ReconcileStale
	delete(engine.unhealthy, "cert-manager")
	reconcileRollout(t, ctx, r) // ready

	for reason, want := range map[string][]string{
		EventStepStarted:  {"cert-manager/Install"},
		EventStepWaiting:  {"cert-manager/HealthProgressing", "cert-manager/ReconcileStale"},
		EventStepFinished: {"cert-manager/Ready"},
	} {
		if got := rec.actions(reason); !slices.Equal(got, want) {
			t.Errorf("%s actions = %q, want %q", reason, got, want)
		}
	}
}

// A rollout that ends with a step in flight, as at its deadline, closes the
// step's record, keeping the release's state, which repair reads.
func TestRolloutClosesTheStepInFlightWhenItEnds(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
		rt.Spec.Rollout.ActionTimeout = &metav1.Duration{Duration: 10 * time.Minute}
		rt.Spec.Rollout.Deadline = &metav1.Duration{Duration: 10 * time.Minute}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.unhealthy = map[string]string{"cert-manager": "HealthProgressing"}
	r, clk := newRollout(t, engine, true)
	rec := &capture{}
	r.Events = rec

	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // step started
	reconcileRollout(t, ctx, r) // waits: HealthProgressing
	clk.now = clk.now.Add(11 * time.Minute)
	ended := clk.now
	reconcileRollout(t, ctx, r) // deadline passed

	ro := rollouts(t, ctx)[0]
	rs := certManager(t, ro)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseExpired) || rs.State != v1alpha1.ReleaseProgressing ||
		!sameSecond(rs.FinishedAt, ended) || rs.WaitedOn != "HealthProgressing" {

		t.Fatalf("after the deadline: %v %+v", ro.Labels, rs)
	}
	if got := rec.actions(EventStepFinished); !slices.Equal(got, []string{"cert-manager/Expired"}) {
		t.Errorf("StepFinished actions = %q, want one for the expired rollout", got)
	}
}

// A step Event is sent only once the status recording its transition is
// stored: a failed status write sends nothing, and the retry sends it once.
func TestStepEventsFollowTheSavedStatus(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.unhealthy = map[string]string{"cert-manager": "HealthProgressing"}
	r, _ := newRollout(t, engine, true)
	rec := &capture{}
	r.Events = rec
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	failNext := false
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {

			if _, ok := obj.(*v1alpha1.RuntimeRollout); ok && failNext {
				failNext = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "runtimerollouts"}, obj.GetName(),
					errors.New("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})

	reconcileRollout(t, ctx, r) // create
	failNext = true
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("a failed status write was not reported")
	}
	if got := rec.actions(EventStepStarted); len(got) != 0 {
		t.Fatalf("StepStarted sent for a start that was never stored: %q", got)
	}
	reconcileRollout(t, ctx, r) // step started again, stored
	if got := rec.actions(EventStepStarted); len(got) != 1 {
		t.Errorf("StepStarted actions = %q, want one", got)
	}
}

// A rollout whose outcome label landed but whose status write was lost has its
// outcome restored on the next reconcile; that closes the step it left in
// flight too, so the step's end and its StepFinished are not lost.
func TestRestoredOutcomeClosesTheStepInFlight(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
	})
	resolved(t, ctx, putPlan(t, ctx, rt, "50m"))
	engine := newEngine()
	engine.unhealthy = map[string]string{"cert-manager": "HealthDegraded"}
	r, clk := newRollout(t, engine, true)
	rec := &capture{}
	r.Events = rec
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	failNext := false
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {

			if _, ok := obj.(*v1alpha1.RuntimeRollout); ok && failNext {
				failNext = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "runtimerollouts"}, obj.GetName(),
					errors.New("the object has been modified"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})

	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // step started
	reconcileRollout(t, ctx, r) // waits: HealthDegraded
	clk.now = clk.now.Add(time.Hour)
	failNext = true
	if _, err := r.Reconcile(ctx, request); err == nil {
		t.Fatal("a lost status write at the action timeout was not reported")
	}
	reconcileRollout(t, ctx, r) // outcome restored from the label

	ro := rollouts(t, ctx)[0]
	if rs := certManager(t, ro); ro.Status.Phase != v1alpha1.PhaseFailed || rs.FinishedAt == nil {
		t.Fatalf("after the restored outcome: phase %s, %+v", ro.Status.Phase, rs)
	}
	if got := rec.actions(EventStepFinished); len(got) != 1 {
		t.Errorf("StepFinished actions = %q, want one", got)
	}
}
