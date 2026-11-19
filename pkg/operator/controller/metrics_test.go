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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "aicr.run/api/operator/v1alpha1"
)

func stalledGauge(reason string) float64 {
	return testutil.ToFloat64(runtimeStalled.WithLabelValues(reason))
}

func resolvedRuntime(generation, observed int64, status metav1.ConditionStatus) *v1alpha1.ClusterRuntime {
	rt := &v1alpha1.ClusterRuntime{ObjectMeta: metav1.ObjectMeta{Generation: generation}}
	rt.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionResolved, Status: status, ObservedGeneration: observed}}
	return rt
}

// The stall gauge reports only the current reason, moves with it and drops
// to zero when the stall clears or the runtime is deleted. The reasons are
// this test's own, so other tests' stalls do not interfere.
func TestRuntimeGaugesStall(t *testing.T) {
	t.Parallel()
	g := newRuntimeGauges()
	rt := resolvedRuntime(1, 1, metav1.ConditionTrue)
	first, second := "MetricsTestFirst", "MetricsTestSecond"

	g.record(rt, &runtimeView{stalled: true, stalledReason: first}, time.Now())
	if stalledGauge(first) != 1 || stalledGauge(second) != 0 {
		t.Fatalf("first stall: %g, %g", stalledGauge(first), stalledGauge(second))
	}
	g.record(rt, &runtimeView{stalled: true, stalledReason: second}, time.Now())
	if stalledGauge(first) != 0 || stalledGauge(second) != 1 {
		t.Fatalf("new reason: %g, %g", stalledGauge(first), stalledGauge(second))
	}
	// A reason with Stalled=False, as a view that recorded one before
	// recovering, is not a stall.
	g.record(rt, &runtimeView{stalledReason: second}, time.Now())
	if stalledGauge(second) != 0 {
		t.Fatalf("cleared stall: %g", stalledGauge(second))
	}
	g.record(rt, &runtimeView{stalled: true, stalledReason: first}, time.Now())
	g.clear()
	if stalledGauge(first) != 0 {
		t.Fatalf("deleted runtime: %g", stalledGauge(first))
	}
}

// The approval wait counts from when the rollout began waiting, and reads
// zero while nothing waits.
func TestRuntimeGaugesApprovalWait(t *testing.T) {
	t.Parallel()
	g := newRuntimeGauges()
	now := time.Unix(10_000, 0)
	rt := resolvedRuntime(1, 1, metav1.ConditionTrue)
	if got := g.approvalWait(now); got != 0 {
		t.Fatalf("before any rollout: %g", got)
	}
	g.record(rt, &runtimeView{awaitingApprovalSince: now.Add(-90 * time.Second)}, now)
	if got := g.approvalWait(now); got != 90 {
		t.Fatalf("waiting: %g, want 90", got)
	}
	if got := g.approvalWait(now.Add(-time.Hour)); got != 0 {
		t.Fatalf("a clock behind the rollout: %g, want 0", got)
	}
	g.record(rt, &runtimeView{approvalUnknown: true}, now)
	if got := g.approvalWait(now); got != 90 {
		t.Fatalf("rollouts unreadable: %g, want the last wait, 90", got)
	}
	g.record(rt, &runtimeView{}, now)
	if got := g.approvalWait(now); got != 0 {
		t.Fatalf("approved: %g", got)
	}
	g.record(rt, &runtimeView{awaitingApprovalSince: now}, now)
	g.clear()
	if got := g.approvalWait(now.Add(time.Minute)); got != 0 {
		t.Fatalf("deleted runtime: %g", got)
	}
}

// Once the resolver has reported, the resolution time advances only while
// the runtime is resolved for its current generation, and survives the
// runtime's deletion.
func TestRuntimeGaugesResolved(t *testing.T) {
	t.Parallel()
	g := newRuntimeGauges()
	start := time.Unix(10_000, 0)
	if got := g.resolved(); got != 0 {
		t.Fatalf("before any status: %g", got)
	}
	g.record(resolvedRuntime(2, 2, metav1.ConditionTrue), &runtimeView{}, start)
	if got := g.resolved(); got != 0 {
		t.Fatalf("Resolved=True before the resolver reported: %g", got)
	}
	g.resolution(&resolution{status: metav1.ConditionTrue}, start)
	for _, tc := range []struct {
		name string
		rt   *v1alpha1.ClusterRuntime
	}{
		{"resolving", resolvedRuntime(2, 2, metav1.ConditionFalse)},
		{"resolved for an earlier generation", resolvedRuntime(3, 2, metav1.ConditionTrue)},
		{"no Resolved condition", &v1alpha1.ClusterRuntime{}},
	} {
		g.record(tc.rt, &runtimeView{}, start.Add(time.Hour))
		if got := g.resolved(); got != float64(start.Unix()) {
			t.Errorf("%s: %g, want %d", tc.name, got, start.Unix())
		}
	}
	g.clear()
	if got := g.resolved(); got != float64(start.Unix()) {
		t.Errorf("deleted runtime: %g, want %d", got, start.Unix())
	}
}

// A resolution the resolver runs or retries stops the resolution time, even
// while it keeps the runtime Resolved=True; the next completed resolution
// starts it again.
func TestRuntimeGaugesResolvedStopsWhileResolving(t *testing.T) {
	t.Parallel()
	g := newRuntimeGauges()
	rt := resolvedRuntime(2, 2, metav1.ConditionTrue)
	start := time.Unix(10_000, 0)
	g.resolution(&resolution{status: metav1.ConditionTrue}, start)
	if got := g.resolved(); got != float64(start.Unix()) {
		t.Fatalf("completed: %g, want %d", got, start.Unix())
	}
	for _, tc := range []struct {
		name string
		res  *resolution
	}{
		{"refresh failing", &resolution{status: metav1.ConditionTrue, refreshing: true}},
		{"started, not yet reported", nil},
		{"first resolution running", &resolution{status: metav1.ConditionFalse, reason: ReasonResolving}},
	} {
		if tc.res == nil {
			g.resolutionStarted()
		} else {
			g.resolution(tc.res, start.Add(time.Minute))
		}
		g.record(rt, &runtimeView{}, start.Add(2*time.Hour))
		if got := g.resolved(); got != float64(start.Unix()) {
			t.Errorf("%s: %g, want %d", tc.name, got, start.Unix())
		}
	}
	done := start.Add(3 * time.Hour)
	g.resolution(&resolution{status: metav1.ConditionTrue}, done)
	if got := g.resolved(); got != float64(done.Unix()) {
		t.Fatalf("resolved again: %g, want %d", got, done.Unix())
	}
	g.record(rt, &runtimeView{}, done.Add(time.Hour))
	if got := g.resolved(); got != float64(done.Add(time.Hour).Unix()) {
		t.Errorf("resolved and idle: %g, want %d", got, done.Add(time.Hour).Unix())
	}
}

// Every reason the troubleshooting guide sends a Stalled runtime to starts at
// zero, so an alert on one reads zero before its first stall.
func TestStallReasonsCoverTheTroubleshootingGuide(t *testing.T) {
	t.Parallel()
	guide, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "operator", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(guide), "| Reason | Runbook |")
	if !ok {
		t.Fatal("troubleshooting.md has no reason table")
	}
	table, _, _ = strings.Cut(table, "\n\n")
	// Condition types follow "on", as in "Unauthorized on Resolved";
	// Terminating is the runtime's deletion state.
	notReasons := []string{v1alpha1.ConditionResolved, v1alpha1.ConditionEngineAvailable, v1alpha1.ConditionReady, "Terminating"}
	known := stallReasons()
	reason := regexp.MustCompile("`([A-Za-z]+)`")
	seen := 0
	for _, line := range strings.Split(table, "\n") {
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "| "), " |")
		for _, m := range reason.FindAllStringSubmatch(cell, -1) {
			if slices.Contains(notReasons, m[1]) {
				continue
			}
			seen++
			if !slices.Contains(known, m[1]) {
				t.Errorf("stall reason %s is in the troubleshooting guide but not in stallReasons", m[1])
			}
		}
	}
	if seen < 30 {
		t.Fatalf("read %d reasons from the troubleshooting guide; the table moved", seen)
	}
}
