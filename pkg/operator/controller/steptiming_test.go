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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
)

// releaseStatus is the rollout's status entry for id, or a failure.
func releaseStatus(t *testing.T, ro v1alpha1.RuntimeRollout, id string) v1alpha1.ReleaseStatus {
	t.Helper()
	for _, rs := range ro.Status.Releases {
		if rs.ID == id {
			return rs
		}
	}
	t.Fatalf("rollout %s has no status for release %s: %+v", ro.Name, id, ro.Status.Releases)

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

	rs := releaseStatus(t, rollouts(t, ctx)[0], "cert-manager")
	if rs.State != v1alpha1.ReleaseProgressing || !sameSecond(rs.StartedAt, started) || rs.FinishedAt != nil ||
		rs.WaitedOn != "ReconcileStale" {

		t.Fatalf("while waiting: %+v", rs)
	}

	delete(engine.unhealthy, "cert-manager")
	clk.now = clk.now.Add(70 * time.Second)
	finished := clk.now
	reconcileRollout(t, ctx, r) // ready

	rs = releaseStatus(t, rollouts(t, ctx)[0], "cert-manager")
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
	clk.now = clk.now.Add(time.Hour)
	finished := clk.now
	reconcileRollout(t, ctx, r) // action timeout

	ro := rollouts(t, ctx)[0]
	rs := releaseStatus(t, ro, "cert-manager")
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseFailed) || rs.State != v1alpha1.ReleaseFailed ||
		!sameSecond(rs.StartedAt, started) || !sameSecond(rs.FinishedAt, finished) || rs.WaitedOn != "HealthDegraded" {

		t.Fatalf("after the action timeout: %v %+v", ro.Labels, rs)
	}
	if got := drain(recorder); len(matching(got, "Warning", EventStepFinished, "cert-manager", "Failed")) != 1 {
		t.Errorf("Events = %q; want one StepFinished warning for the failed step", got)
	}
}
