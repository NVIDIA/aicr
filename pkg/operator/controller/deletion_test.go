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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// deleteRuntime requests the runtime's deletion; its cleanup finalizer
// keeps it until the controller runs the deletion policy.
func deleteRuntime(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := k8s.Delete(ctx, getRuntime(t, ctx)); err != nil {
		t.Fatal(err)
	}
}

func runtimeGone(t *testing.T, ctx context.Context) bool {
	t.Helper()
	err := k8s.Get(ctx, request.NamespacedName, &v1alpha1.ClusterRuntime{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

// Orphan (the default) ends the rollout in flight Orphaned without waiting
// for the engine, annotates the engine objects, deletes none of them, and
// lets the runtime go.
func TestDeletionOrphans(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := twoReleases(t, ctx, rt)
	resolved(t, ctx, target)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r) // finalizer, rollout
	if !controllerutil.ContainsFinalizer(getRuntime(t, ctx), v1alpha1.FinalizerCleanup) {
		t.Fatal("the runtime has no cleanup finalizer")
	}
	reconcileRollout(t, ctx, r) // first step in flight

	deleteRuntime(t, ctx)
	reconcileRollout(t, ctx, r)
	ro := onlyRollout(t, ctx, target)
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseOrphaned) || !runtimeGone(t, ctx) {
		t.Fatalf("after deletion: rollout %v, runtime gone %t", ro.Labels, runtimeGone(t, ctx))
	}
	if want := []string{"Ensure cert-manager", "Orphan " + string(rt.UID)}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
}

// refusingOrphan is an engine on which the API server refuses the
// orphaned-from annotation for one object, as an admission policy can.
type refusingOrphan struct{ *fakeEngine }

func (e refusingOrphan) Orphan(ctx context.Context, runtimeUID string) ([]string, error) {
	names, err := e.fakeEngine.Orphan(ctx, runtimeUID)
	if err != nil {
		return names, err
	}
	return names, operrors.New(operrors.ErrCodeUnauthorized, "Applications nfd refused the orphaned-from annotation").
		WithReason(argocd.ReasonOrphanAnnotationRefused)
}

// An engine object refused the orphaned-from annotation is named in a
// Warning Event and does not keep the runtime from going.
func TestDeletionOrphansPastARefusedAnnotation(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, stabilized)
	engine := newEngine()
	recorder := events.NewFakeRecorder(10)
	r, _ := newRollout(t, engine, true)
	r.Engine, r.Events = refusingOrphan{engine}, recorder
	reconcileRollout(t, ctx, r)
	if !controllerutil.ContainsFinalizer(getRuntime(t, ctx), v1alpha1.FinalizerCleanup) {
		t.Fatal("the runtime has no cleanup finalizer")
	}
	deleteRuntime(t, ctx)
	reconcileRollout(t, ctx, r)
	if !runtimeGone(t, ctx) {
		t.Fatalf("the runtime stayed Terminating: finalizers %v", getRuntime(t, ctx).Finalizers)
	}
	for {
		select {
		case e := <-recorder.Events:
			if strings.HasPrefix(e, corev1.EventTypeWarning+" "+argocd.ReasonOrphanAnnotationRefused) && strings.Contains(e, "nfd") {
				return
			}
		default:
			t.Fatal("no Warning Event names the object refused the annotation")
		}
	}
}

// A runtime created with the cleanup finalizer already present, as one
// restored from `kubectl get -o yaml` is, has that entry owned by another
// field manager. Its deletion still releases the finalizer.
func TestDeletionReleasesAFinalizerAnotherManagerOwns(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		stabilized(rt)
		rt.Finalizers = []string{v1alpha1.FinalizerCleanup}
	})
	r, _ := newRollout(t, newEngine(), true)
	reconcileRollout(t, ctx, r)
	deleteRuntime(t, ctx)
	reconcileRollout(t, ctx, r)
	if !runtimeGone(t, ctx) {
		t.Fatalf("the runtime stayed Terminating: finalizers %v", getRuntime(t, ctx).Finalizers)
	}
}

// Delete supersedes the rollout in flight at its step boundary, then runs a
// removal rollout over the live objects in reverse order of the active
// plan, which requires approval; only when everything is retired does the
// runtime go.
func TestDeletionRemovesInReverseOrder(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		stabilized(rt)
		rt.Spec.DeletionPolicy = v1alpha1.DeletionDelete
	})
	active := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	succeedTo(t, ctx, rt, active)
	target := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"60m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	resolved(t, ctx, target)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{
		{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}, {ReleaseID: "nfd", UID: "u-n", PlanDigest: active},
	}
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r) // finalizer, rollout to target
	reconcileRollout(t, ctx, r) // its first step in flight

	deleteRuntime(t, ctx)
	reconcileRollout(t, ctx, r)
	if ro := onlyRollout(t, ctx, target); ro.Labels[v1alpha1.LabelOutcome] != "" {
		t.Fatalf("the step in flight was abandoned: %v", ro.Labels)
	}
	engine.state = argocd.StateReady
	reconcileRollout(t, ctx, r) // the step finishes; the boundary supersedes
	if ro := onlyRollout(t, ctx, target); ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSuperseded) {
		t.Fatalf("the rollout in progress was not superseded: %v %+v", ro.Labels, ro.Status)
	}

	reconcileRollout(t, ctx, r) // removal rollout
	removal := onlyRollout(t, ctx, removalDigest(string(rt.UID)))
	steps := removal.Spec.Steps
	ordered := len(steps) == 2 && steps[0].Release == "nfd" && steps[0].UID == "u-n" &&
		steps[1].Release == "cert-manager" && steps[1].Operation == v1alpha1.OperationRemove
	if !removal.Spec.ApprovalRequired || !ordered {
		t.Fatalf("removal rollout = %+v", removal.Spec)
	}
	engine.gone["nfd"], engine.gone["cert-manager"] = true, true
	for range 6 {
		if runtimeGone(t, ctx) {
			break
		}
		reconcileRollout(t, ctx, r)
	}
	removal = onlyRollout(t, ctx, removalDigest(string(rt.UID)))
	if removal.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) || !runtimeGone(t, ctx) {
		t.Fatalf("after the removal: %v %+v, runtime gone %t", removal.Labels, removal.Status, runtimeGone(t, ctx))
	}
	if want := []string{"Ensure cert-manager", "Remove nfd", "Remove cert-manager"}; !slices.Equal(engine.order, want) {
		t.Fatalf("engine calls = %v, want %v", engine.order, want)
	}
}

// A removal that fails keeps the finalizer and stalls the runtime; nothing
// removes the finalizer silently, and a retry makes a new removal rollout.
func TestDeletionFailureKeepsFinalizer(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		stabilized(rt)
		rt.Spec.DeletionPolicy = v1alpha1.DeletionDelete
	})
	active := putPlan(t, ctx, rt, "50m")
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, clk := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r) // finalizer
	deleteRuntime(t, ctx)
	reconcileRollout(t, ctx, r) // removal rollout
	reconcileRollout(t, ctx, r) // Remove step started
	clk.now = clk.now.Add(time.Hour)
	reconcileRollout(t, ctx, r) // the step times out
	reconcileRollout(t, ctx, r)
	rt = getRuntime(t, ctx)
	if c := cond(rt, v1alpha1.ConditionStalled); c.Reason != ReasonRemovalFailed || !controllerutil.ContainsFinalizer(rt, v1alpha1.FinalizerCleanup) {
		t.Fatalf("after a failed removal: Stalled %+v, finalizers %v", c, rt.Finalizers)
	}

	patch := client.MergeFrom(rt.DeepCopy())
	rt.Annotations = map[string]string{v1alpha1.AnnotationRetry: "remove-again"}
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	engine.gone["cert-manager"] = true
	for range 5 {
		if runtimeGone(t, ctx) {
			break
		}
		reconcileRollout(t, ctx, r)
	}
	if !runtimeGone(t, ctx) || len(rolloutsTo(t, ctx, removalDigest(string(rt.UID)))) != 2 {
		t.Fatalf("retry: runtime gone %t, %d removal rollouts", runtimeGone(t, ctx), len(rolloutsTo(t, ctx, removalDigest(string(rt.UID)))))
	}
}

// With the finalizer removed by hand, nothing cascades: no engine object is
// removed or annotated.
func TestFinalizerRemovedByHandDeletesNothing(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putPlan(t, ctx, rt, "50m")
	succeedTo(t, ctx, rt, active)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)
	forceDelete(ctx)
	waitGone(t, ctx)
	reconcileRollout(t, ctx, r)
	if len(engine.order) != 0 || len(engine.removed) != 0 {
		t.Fatalf("engine calls after a forced deletion: %v", engine.order)
	}
}
