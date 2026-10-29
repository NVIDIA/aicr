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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/engine/argocd"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/manager"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
)

// endedTo records a terminal rollout to digest with the outcome.
func endedTo(t *testing.T, ctx context.Context, rt *v1alpha1.ClusterRuntime, name, from, to string, outcome v1alpha1.RolloutPhase) *v1alpha1.RuntimeRollout {
	t.Helper()
	ro := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1alpha1.LabelRuntime: rt.Name}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion, RuntimeUID: string(rt.UID), FromPlanDigest: from, ToPlanDigest: to,
			StartingInventoryDigest: to, Deadline: metav1.NewTime(time.Now().Add(time.Hour)),
			ActionTimeout: metav1.Duration{Duration: time.Minute},
			Steps:         []v1alpha1.RolloutStep{{Release: "x", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable}},
		},
	}
	if outcome != "" {
		ro.Labels[v1alpha1.LabelOutcome] = string(outcome)
	}
	if err := k8s.Create(ctx, ro); err != nil {
		t.Fatal(err)
	}
	return ro
}

// A status message longer than the CRD's bound is stored cut to fit, on a
// UTF-8 boundary and marked, instead of failing the status write and
// freezing the rollout.
func TestRolloutStatusMessageFitsItsBound(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	ro := endedTo(t, ctx, rt, "rollout-long-message-1", "", plan.DigestOf([]byte("to")), "")
	r, _ := newRollout(t, newEngine(), true)
	long := strings.Repeat("drifted: release gpu-operator at plan sha256:x (was y); ", 40) + "é"
	if err := r.saveStatus(ctx, ro, long); err != nil {
		t.Fatalf("saveStatus() with a %d-byte message = %v", len(long), err)
	}
	got := &v1alpha1.RuntimeRollout{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ro), got); err != nil {
		t.Fatal(err)
	}
	if n := len(got.Status.Message); n == 0 || n > 1024 || !utf8.ValidString(got.Status.Message) || !strings.HasSuffix(got.Status.Message, "[truncated]") {
		t.Fatalf("stored message is %d bytes, valid UTF-8 %t: ...%q", n, utf8.ValidString(got.Status.Message), got.Status.Message[max(0, n-40):])
	}
}

func rolloutsTo(t *testing.T, ctx context.Context, digest string) []v1alpha1.RuntimeRollout {
	t.Helper()
	var out []v1alpha1.RuntimeRollout
	for _, ro := range rollouts(t, ctx) {
		if ro.Spec.ToPlanDigest == digest {
			out = append(out, ro)
		}
	}
	return out
}

func stabilized(rt *v1alpha1.ClusterRuntime) {
	rt.Spec.Rollout.StabilizationWindow = &metav1.Duration{Duration: time.Millisecond}
}

// Two controllers racing to create the same transition make one rollout,
// and a create whose response was lost is found again by the next list.
func TestRolloutRacingCreatorsMakeOne(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	digest := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, digest)
	engine := newEngine()
	a, clk := newRollout(t, engine, false)
	b, _ := newRollout(t, engine, false)
	b.Now = clk.Now
	clk.now = time.Now().Add(2 * time.Second)

	var wg sync.WaitGroup
	var failures atomic.Int32
	for _, r := range []*RolloutReconciler{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Reconcile(ctx, request); err != nil && !strings.Contains(err.Error(), "conflict") {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d reconciles failed", failures.Load())
	}
	if n := len(rolloutsTo(t, ctx, digest)); n != 1 {
		t.Fatalf("racing creators made %d rollouts", n)
	}
	// The create response was "lost": reconciling again finds it.
	reconcileRollout(t, ctx, a)
	if n := len(rolloutsTo(t, ctx, digest)); n != 1 {
		t.Fatalf("a retried create made %d rollouts", n)
	}
	// A creator whose list predates the winner's create, and which reads
	// live state after the winner's first step, still collides on the name.
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: digest}}
	if err := b.create(ctx, getRuntime(t, ctx), nil, "", digest, "", &runtimeView{}); err != nil {
		t.Fatal(err)
	}
	if n := len(rolloutsTo(t, ctx, digest)); n != 1 {
		t.Fatalf("a stale creator made %d rollouts", n)
	}
}

// Dropping all status (a restore) is reproduced by the next reconcile: the
// active plan from the newest succeeded outcome, terminal phases from their
// outcome labels.
func TestRolloutStatusLossIsRebuilt(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putPlan(t, ctx, rt, "50m")
	failed := putPlan(t, ctx, rt, "60m")
	endedTo(t, ctx, rt, "rollout-aaaaaaaaaaaaaaaa-1", "", active, v1alpha1.PhaseSucceeded)
	endedTo(t, ctx, rt, "rollout-bbbbbbbbbbbbbbbb-1", active, failed, v1alpha1.PhaseFailed)
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["cert-manager"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, _ := newRollout(t, engine, true)

	reconcileRollout(t, ctx, r)
	got := getRuntime(t, ctx)
	if got.Status.ActivePlanDigest != active || cond(got, v1alpha1.ConditionReady).Status != metav1.ConditionTrue {
		t.Fatalf("runtime after restore = %+v", got.Status)
	}
	for _, ro := range rollouts(t, ctx) {
		if string(ro.Status.Phase) != ro.Labels[v1alpha1.LabelOutcome] {
			t.Errorf("rollout %s phase %q, outcome %q", ro.Name, ro.Status.Phase, ro.Labels[v1alpha1.LabelOutcome])
		}
	}
	if len(rollouts(t, ctx)) != 2 {
		t.Error("restoring status created a rollout")
	}
}

// An earlier runtime's unfinished rollouts are orphaned and never block the
// recreated runtime; deleting the runtime orphans its own.
func TestRolloutOrphansEarlierRuntime(t *testing.T) {
	ctx := testContext(t)
	earlier := freshRuntime(t, ctx, stabilized)
	rt := freshRuntime(t, ctx, stabilized)
	stale := endedTo(t, ctx, earlier, "rollout-cccccccccccccccc-1", "", plan.DigestOf([]byte("old")), "")
	digest := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, digest)
	engine := newEngine()
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(stale), stale); err != nil {
		t.Fatal(err)
	}
	if stale.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseOrphaned) {
		t.Fatalf("the earlier runtime's rollout = %v", stale.Labels)
	}
	if len(rolloutsTo(t, ctx, digest)) != 1 {
		t.Fatal("the earlier runtime's rollout blocked the new one")
	}

	// The cleanup finalizer removed by hand: the runtime is gone, and its
	// unfinished rollouts are orphaned on the next reconcile.
	forceDelete(ctx)
	waitGone(t, ctx)
	reconcileRollout(t, ctx, r)
	for _, ro := range rolloutsTo(t, ctx, digest) {
		if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseOrphaned) {
			t.Fatalf("deleting the runtime left rollout %s %v", ro.Name, ro.Labels)
		}
	}
}

func waitGone(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := k8s.Get(ctx, request.NamespacedName, &v1alpha1.ClusterRuntime{}); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("runtime not deleted")
}

// A deleted active plan ConfigMap is rebuilt from the engine objects with
// the same digest; a rebuild to another digest stalls with PlanMissing.
func TestRolloutRebuildsLostActivePlan(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putPlan(t, ctx, rt, "50m")
	endedTo(t, ctx, rt, "rollout-dddddddddddddddd-1", "", active, v1alpha1.PhaseSucceeded)
	resolved(t, ctx, active)
	doc, err := readPlan(ctx, k8s, operatorNS, active)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := plan.Name(active)
	if err = k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
	engine := newEngine()
	engine.ensured["cert-manager"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	rebuilt := *doc
	rebuilt.Provenance = plan.Provenance{Source: plan.SourceRebuilt}
	rebuilt.Recipe = nil
	engine.rebuilt = &rebuilt
	r, _ := newRollout(t, engine, true)

	reconcileRollout(t, ctx, r)
	restored, err := readPlan(ctx, k8s, operatorNS, active)
	if err != nil || restored.DesiredStateDigest != active || restored.Provenance.Source != plan.SourceRebuilt {
		t.Fatalf("restored plan = %+v, %v", restored, err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v", c)
	}

	if err = k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
	other := rebuilt
	other.DesiredStateDigest = plan.DigestOf([]byte("other"))
	engine.rebuilt = &other
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason != ReasonPlanMissing || !strings.Contains(c.Message, other.DesiredStateDigest) {
		t.Fatalf("Stalled = %+v", c)
	}
}

// The two controllers apply status in alternation; neither removes the
// other's conditions or fields.
func TestStatusAppliedInAlternation(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	digest := putPlan(t, ctx, rt, "50m")
	r, _ := newRollout(t, newEngine(), true)
	for i := range 3 {
		resolved(t, ctx, digest)
		reconcileRollout(t, ctx, r)
		got := getRuntime(t, ctx)
		for _, c := range []string{v1alpha1.ConditionResolved, v1alpha1.ConditionReady, v1alpha1.ConditionStalled, v1alpha1.ConditionEngineAvailable} {
			if cond(got, c).Type == "" {
				t.Fatalf("round %d: condition %s is gone: %+v", i, c, got.Status.Conditions)
			}
		}
		if got.Status.CandidatePlanDigest != digest {
			t.Fatalf("round %d: the rollout controller removed the candidate", i)
		}
	}
}

// A newer target supersedes a rollout only at a step boundary: the step in
// flight finishes first.
func TestRolloutSupersedesAtStepBoundary(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	first := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
		testRelease("nfd", plan.PhasePrimary, "", `{}`))
	resolved(t, ctx, first)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // first step started: Ensure
	if engine.ensured["cert-manager"] != first {
		t.Fatalf("Ensure = %v", engine.ensured)
	}

	second := putPlan(t, ctx, rt, "40m")
	resolved(t, ctx, second)
	clk.now = time.Now().Add(4 * time.Second)
	reconcileRollout(t, ctx, r)
	ro := rolloutsTo(t, ctx, first)[0]
	if ro.Labels[v1alpha1.LabelOutcome] != "" || ro.Status.Current == nil {
		t.Fatalf("a step in flight was abandoned: %v %+v", ro.Labels, ro.Status)
	}

	engine.state = argocd.StateReady
	reconcileRollout(t, ctx, r) // the step finishes; the boundary supersedes
	ro = rolloutsTo(t, ctx, first)[0]
	if ro.Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSuperseded) || engine.ensured["nfd"] != "" {
		t.Fatalf("rollout after the step = %v %+v (nfd ensured %q)", ro.Labels, ro.Status, engine.ensured["nfd"])
	}
	reconcileRollout(t, ctx, r)
	if len(rolloutsTo(t, ctx, second)) != 1 {
		t.Fatal("no rollout to the newer target")
	}
}

// A failed transition waits for a retry token; each token makes one new
// rollout from live state.
func TestRolloutRetryToken(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	target := putPlan(t, ctx, rt, "50m")
	endedTo(t, ctx, rt, "rollout-eeeeeeeeeeeeeeee-1", "", target, v1alpha1.PhaseFailed)
	resolved(t, ctx, target)
	r, clk := newRollout(t, newEngine(), true)
	clk.now = time.Now().Add(2 * time.Second)

	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason != "RolloutFailed" || len(rolloutsTo(t, ctx, target)) != 1 {
		t.Fatalf("without a retry: Stalled = %+v, %d rollouts", c, len(rolloutsTo(t, ctx, target)))
	}
	rt = getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Annotations = map[string]string{v1alpha1.AnnotationRetry: "try-1"}
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, ctx, r)
	retried := 0
	for _, ro := range rolloutsTo(t, ctx, target) {
		if ro.Annotations[v1alpha1.AnnotationRetry] == "try-1" {
			retried++
		}
	}
	if retried != 1 {
		t.Fatalf("retry rollouts = %d", retried)
	}
}

// A step whose Application already carries the target plan, as after a
// crash between the engine write and its record, observes without writing.
func TestRolloutStepAfterCrashDoesNotWriteAgain(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	digest := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, digest)
	engine := newEngine()
	engine.ensured["cert-manager"] = digest // written before the "crash"
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: digest}}
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	if len(engine.order) != 0 {
		t.Fatalf("engine writes = %v, want none", engine.order)
	}
	if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != digest {
		t.Fatalf("runtime = %+v", got.Status)
	}
}

// Retention keeps the newest succeeded rollout, unfinished ones and the ten
// newest terminal ones, and plans the rules name plus five others.
func TestRolloutRetention(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	active := putPlan(t, ctx, rt, "50m")
	endedTo(t, ctx, rt, "rollout-ffffffffffffffff-1", "", active, v1alpha1.PhaseSucceeded)
	for i := range 14 {
		endedTo(t, ctx, rt, fmt.Sprintf("rollout-%016x-1", i+1), active, plan.DigestOf([]byte(fmt.Sprint(i))), v1alpha1.PhaseFailed)
	}
	for i := range 8 {
		putPlan(t, ctx, rt, fmt.Sprintf("%dm", 60+i))
	}
	resolved(t, ctx, active)
	engine := newEngine()
	engine.ensured["cert-manager"] = active
	engine.inventory = []argocd.InventoryItem{{ReleaseID: "cert-manager", UID: "u-c", PlanDigest: active}}
	r, _ := newRollout(t, engine, true)
	reconcileRollout(t, ctx, r)

	ros := rollouts(t, ctx)
	succeeded := 0
	for _, ro := range ros {
		if ro.Labels[v1alpha1.LabelOutcome] == string(v1alpha1.PhaseSucceeded) {
			succeeded++
		}
	}
	if len(ros) != 11 || succeeded != 1 {
		t.Fatalf("rollouts kept = %d (succeeded %d), want 11", len(ros), succeeded)
	}
	plans := &corev1.ConfigMapList{}
	if err := k8s.List(ctx, plans, client.InNamespace(operatorNS), client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RolePlan}); err != nil {
		t.Fatal(err)
	}
	if len(plans.Items) != 6 {
		t.Fatalf("plans kept = %d, want the active one and five others", len(plans.Items))
	}
}

// The engine watch waits for the engine kind and registers once.
func TestEngineWatchRegistersOnceServed(t *testing.T) {
	t.Parallel()
	var polls, registered atomic.Int32
	w := &engineWatch{
		interval: time.Millisecond,
		served:   func() bool { return polls.Add(1) > 3 },
		register: func() error {
			if registered.Add(1) == 1 {
				return errors.New("not yet")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil || registered.Load() != 2 || polls.Load() < 4 {
		t.Fatalf("Start() = %v after %d polls, %d registrations", err, polls.Load(), registered.Load())
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	never := &engineWatch{interval: time.Millisecond, served: func() bool { return false }, register: func() error { return nil }}
	if err := never.Start(stopped); err != nil {
		t.Fatalf("Start(stopped) = %v", err)
	}
}

// A missing engine CRD never stops the manager: with the real
// cache options and no Argo CD installed, the manager builds and runs, and
// the rollout controller registers with the engine watch still waiting.
func TestManagerRunsWithoutTheEngineCRDs(t *testing.T) {
	scheme, err := manager.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	o := manager.DefaultOptions()
	o.MetricsBindAddress, o.HealthProbeBindAddress, o.LeaderElection = "0", "127.0.0.1:0", false
	mgr, err := manager.New(restConfig, o, scheme, CacheOptions(operatorNS, jobsNS, discoveryNS))
	if err != nil {
		t.Fatalf("manager.New() without the Argo CD CRDs = %v", err)
	}
	// The engine watch polls the real REST mapper and must keep waiting.
	gvk := argocd.ApplicationGVK
	var registered atomic.Bool
	w := &engineWatch{
		interval: 10 * time.Millisecond,
		served: func() bool {
			_, mapErr := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
			return mapErr == nil
		},
		register: func() error { registered.Store(true); return nil },
	}
	if err = mgr.Add(w); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	select {
	case err = <-done:
		t.Fatalf("the manager stopped without the Argo CD CRDs: %v", err)
	case <-time.After(3 * time.Second):
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatalf("the manager stopped with %v", err)
	}
	if registered.Load() {
		t.Fatal("the engine watch registered although the Application kind is not served")
	}
}

// Once the engine's kind is served, its cache is built from
// engineCacheOptions and sees exactly the operator's objects in the engine
// namespace: another manager's Application, or one in another
// namespace, never reaches the rollout controller.
func TestEngineCacheSeesOnlyTheOperatorsApplications(t *testing.T) {
	ctx := testContext(t)
	crds, err := envtest.InstallCRDs(restConfig, envtest.CRDInstallOptions{
		Paths: []string{filepath.Join("..", "..", "..", "tests", "operator", "chart", "testdata", "argocd-crds.yaml")},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Other tests need the CRDs gone, and a CRD's deletion completes
	// asynchronously.
	t.Cleanup(func() {
		_ = envtest.UninstallCRDs(restConfig, envtest.CRDInstallOptions{CRDs: crds})
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			crd := &unstructured.Unstructured{}
			crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
			if getErr := k8s.Get(context.Background(), client.ObjectKey{Name: "applications.argoproj.io"}, crd); apierrors.IsNotFound(getErr) {
				return
			}
		}
		t.Error("the Argo CD CRDs were not removed")
	})
	scheme, err := manager.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	o := manager.DefaultOptions()
	o.MetricsBindAddress, o.HealthProbeBindAddress, o.LeaderElection = "0", "127.0.0.1:0", false
	mgr, err := manager.New(restConfig, o, scheme, CacheOptions(operatorNS, jobsNS, discoveryNS))
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"argocd", "elsewhere"} {
		if err = k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); client.IgnoreAlreadyExists(err) != nil {
			t.Fatal(err)
		}
	}
	app := func(name, ns string, managed bool) *unstructured.Unstructured {
		a := &unstructured.Unstructured{}
		a.SetGroupVersionKind(argocd.ApplicationGVK)
		a.SetName(name)
		a.SetNamespace(ns)
		if managed {
			a.SetLabels(map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue, v1alpha1.LabelRuntime: "cluster"})
		}
		_ = unstructured.SetNestedField(a.Object, map[string]any{"destination": map[string]any{"server": "https://kubernetes.default.svc"},
			"project": "default", "source": map[string]any{"repoURL": "oci://r/c", "targetRevision": "1"}}, "spec")
		return a
	}
	for _, a := range []*unstructured.Unstructured{app("ours", "argocd", true), app("theirs", "argocd", false), app("away", "elsewhere", true)} {
		if err = k8s.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	engineCache, err := cache.New(mgr.GetConfig(), engineCacheOptions(mgr, "argocd"))
	if err != nil {
		t.Fatalf("building the engine cache once the kind is served: %v", err)
	}
	cacheCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = engineCache.Start(cacheCtx) }()
	if !engineCache.WaitForCacheSync(ctx) {
		t.Fatal("the engine cache did not start")
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(argocd.ApplicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err = engineCache.List(ctx, list, client.InNamespace("argocd")); err != nil {
		t.Fatal(err)
	}
	if names := func() []string {
		out := make([]string, 0, len(list.Items))
		for _, i := range list.Items {
			out = append(out, i.GetName())
		}
		return out
	}(); !slices.Equal(names, []string{"ours"}) {
		t.Fatalf("the engine cache holds %v; want only the operator's Application in the engine namespace", names)
	}
}

// Finished Jobs record their duration by stage and result; unfinished ones
// record nothing.
func TestObserveJobDuration(t *testing.T) {
	t.Parallel()
	start, end := metav1.NewTime(time.Unix(100, 0)), metav1.NewTime(time.Unix(130, 0))
	job := func(stage string, cond batchv1.JobConditionType) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelStage: stage}},
			Status: batchv1.JobStatus{StartTime: &start, CompletionTime: &end}}
		if cond != "" {
			j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, LastTransitionTime: end}}
		}
		return j
	}
	before := testutil.CollectAndCount(jobSeconds)
	observeJob(job("metrics-test-a", batchv1.JobComplete))
	observeJob(job("metrics-test-b", batchv1.JobFailed))
	observeJob(job("metrics-test-c", ""))
	observeJob(&batchv1.Job{})
	if got := testutil.CollectAndCount(jobSeconds); got != before+2 {
		t.Fatalf("series = %d, want %d", got, before+2)
	}
	if len(Collectors()) != 4 {
		t.Fatal("Collectors() misses a metric")
	}
}

// After a downgrade past a newer operator, a rollout with a schema this
// operator does not read stops the runtime: Stalled with PlanUnreadable,
// the active plan kept, and no engine write, rollout or suspension, even
// with a step in flight, a new candidate and a suspension request.
// Upgrading again, here removing the newer rollout, resumes.
func TestNewerRolloutSchemaStallsWithZeroWrites(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, stabilized)
	first := putPlan(t, ctx, rt, "50m")
	resolved(t, ctx, first)
	engine := newEngine()
	engine.state = argocd.StateProgressing
	r, clk := newRollout(t, engine, true)
	clk.now = time.Now().Add(2 * time.Second)
	reconcileRollout(t, ctx, r) // create
	reconcileRollout(t, ctx, r) // step started: Ensure
	inFlight := rolloutsTo(t, ctx, first)[0]

	newer := &v1alpha1.RuntimeRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "rollout-from-a-newer-operator", Labels: map[string]string{v1alpha1.LabelRuntime: rt.Name}},
		Spec: v1alpha1.RuntimeRolloutSpec{
			SchemaVersion: v1alpha1.RolloutSchemaVersion + 1, RuntimeUID: string(rt.UID), ToPlanDigest: first,
			StartingInventoryDigest: first, Deadline: metav1.NewTime(time.Now().Add(time.Hour)),
			ActionTimeout: metav1.Duration{Duration: time.Minute},
			Steps:         []v1alpha1.RolloutStep{{Release: "cert-manager", Operation: v1alpha1.OperationInstall, Assessment: v1alpha1.AssessmentNotApplicable}},
		},
	}
	if err := k8s.Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	second := putPlan(t, ctx, rt, "40m")
	resolved(t, ctx, second)
	patch := client.MergeFrom(getRuntime(t, ctx))
	suspend := getRuntime(t, ctx)
	suspend.Spec.SuspendEngine = true
	if err := k8s.Patch(ctx, suspend, patch); err != nil {
		t.Fatal(err)
	}
	engine.state = argocd.StateReady
	before := slices.Clone(engine.order)
	clk.now = time.Now().Add(4 * time.Second)
	for range 3 {
		reconcileRollout(t, ctx, r)
	}
	got := getRuntime(t, ctx)
	stalled := cond(got, v1alpha1.ConditionStalled)
	if stalled.Reason != plan.ReasonPlanUnreadable || !strings.Contains(stalled.Message, "rollout-from-a-newer-operator (schema 2)") ||
		cond(got, v1alpha1.ConditionReady).Reason != plan.ReasonPlanUnreadable || got.Status.Rollout != inFlight.Name {

		t.Fatalf("runtime status = %+v", got.Status)
	}
	if !slices.Equal(engine.order, before) || len(rolloutsTo(t, ctx, second)) != 0 {
		t.Fatalf("engine writes %v after %v, rollouts to the new candidate %d", engine.order, before, len(rolloutsTo(t, ctx, second)))
	}
	if ro := rolloutsTo(t, ctx, first)[0]; ro.Labels[v1alpha1.LabelOutcome] != "" || ro.ResourceVersion != inFlight.ResourceVersion {
		t.Fatalf("the in-flight rollout was written: %v", ro.Labels)
	}

	if err := k8s.Delete(ctx, newer); err != nil {
		t.Fatal(err)
	}
	reconcileRollout(t, ctx, r)
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionStalled); c.Reason == plan.ReasonPlanUnreadable || !engine.suspended {
		t.Fatalf("not resumed: Stalled %+v, suspended %t", c, engine.suspended)
	}
}

// faults fails the nth write: before it lands (a partition) or after (a
// crash or a lost response). n of 0 never fails; writes counts them.
type faults struct {
	n      int
	landed bool
	writes int
}

func (f *faults) at(op string, after bool) error {
	if after != f.landed {
		return nil
	}
	f.writes++
	if f.writes == f.n {
		return operrors.New(operrors.ErrCodeUnavailable, fmt.Sprintf("injected fault at write %d (%s, landed %t)", f.n, op, f.landed))
	}
	return nil
}

// write runs one write between the checks before and after it lands.
func (f *faults) write(op string, do func() error) error {
	if err := f.at(op, false); err != nil {
		return err
	}
	if err := do(); err != nil {
		return err
	}
	return f.at(op, true)
}

// faultingClient is an envtest client whose writes go through f.
func faultingClient(t *testing.T, f *faults) client.Client {
	t.Helper()
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return f.write("create "+obj.GetName(), func() error { return c.Create(ctx, obj, opts...) })
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			return f.write("update "+obj.GetName(), func() error { return c.Update(ctx, obj, opts...) })
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return f.write("patch "+obj.GetName(), func() error { return c.Patch(ctx, obj, patch, opts...) })
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			return f.write("delete "+obj.GetName(), func() error { return c.Delete(ctx, obj, opts...) })
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			return f.write(sub+" "+obj.GetName(), func() error { return c.SubResource(sub).Update(ctx, obj, opts...) })
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return f.write(sub+" "+obj.GetName(), func() error { return c.SubResource(sub).Patch(ctx, obj, patch, opts...) })
		},
		SubResourceApply: func(ctx context.Context, c client.Client, sub string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
			return f.write("apply "+sub, func() error { return c.SubResource(sub).Apply(ctx, obj, opts...) })
		},
	})
}

// faultingRollout is a rollout controller whose API writes and engine
// mutations go through one fault plan.
func faultingRollout(t *testing.T, engine *fakeEngine, n int, landed bool) (*RolloutReconciler, *int) {
	t.Helper()
	f := &faults{n: n, landed: landed}
	engine.fault = f.at
	r, _ := newRollout(t, engine, true)
	r.Client = faultingClient(t, f)
	return r, &f.writes
}

// A fault at every write the rollout controller makes, API or engine,
// before it lands or after, changes nothing about the outcome:
// one rollout, Succeeded, each release written once and in order, the
// second only after the first is current-revision ready.
func TestRolloutSurvivesAFaultAtEveryWrite(t *testing.T) {
	ctx := testContext(t)
	run := func(n int, landed bool) int {
		rt := freshRuntime(t, ctx, stabilized)
		digest := putReleases(t, ctx, rt, testRelease("cert-manager", plan.PhasePrimary, "", `{"cpu":"50m"}`),
			testRelease("nfd", plan.PhasePrimary, "", `{}`))
		resolved(t, ctx, digest)
		engine := newEngine()
		r, writes := faultingRollout(t, engine, n, landed)
		for range 12 {
			_, _ = r.Reconcile(ctx, request)
		}
		ros := rolloutsTo(t, ctx, digest)
		where := fmt.Sprintf("fault at write %d (landed %t)", n, landed)
		if len(ros) != 1 || ros[0].Labels[v1alpha1.LabelOutcome] != string(v1alpha1.PhaseSucceeded) {
			t.Fatalf("%s: %d rollouts, %+v", where, len(ros), ros)
		}
		if !slices.Equal(engine.order, []string{"Ensure cert-manager", "Ensure nfd"}) {
			t.Fatalf("%s: engine writes %v", where, engine.order)
		}
		if got := getRuntime(t, ctx); got.Status.ActivePlanDigest != digest || cond(got, v1alpha1.ConditionReady).Status != metav1.ConditionTrue {
			t.Fatalf("%s: runtime %+v", where, got.Status)
		}
		return *writes
	}
	total := run(0, false)
	if total < 5 {
		t.Fatalf("a clean rollout made only %d writes", total)
	}
	for n := 1; n <= total; n++ {
		for _, landed := range []bool{false, true} {
			run(n, landed)
		}
	}
	t.Logf("a fault at each of %d writes, before and after it landed, kept the outcome", total)
}
