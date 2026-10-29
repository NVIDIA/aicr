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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
	"github.com/NVIDIA/aicr/pkg/operator/resolver"
)

// fakeRegistry serves one rendered bundle and mirrors charts in memory.
type fakeRegistry struct {
	bundle   *registry.Bundle
	resolved map[string]string
	// err fails every call, as unavailable credentials do.
	err error
}

func (f *fakeRegistry) Resolve(_ context.Context, _ registry.Repository, tag string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if d, ok := f.resolved[tag]; ok {
		return d, nil
	}
	return "", operrors.New(operrors.ErrCodeNotFound, "no tag "+tag)
}

func (f *fakeRegistry) PullBundle(_ context.Context, _ registry.Repository, digest string) (*registry.Bundle, error) {
	return f.bundle, nil
}

func (f *fakeRegistry) MirrorHTTPChart(_ context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error) {
	return &registry.MirroredChart{Repository: target.Child(up.Chart), Digest: plan.DigestOf([]byte(up.Chart)),
		ArchiveDigest: plan.DigestOf([]byte("archive")), Archive: chartArchive(t0)}, nil
}

func (f *fakeRegistry) MirrorOCIChart(ctx context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error) {
	return f.MirrorHTTPChart(ctx, up, target)
}

func (f *fakeRegistry) PushWrapper(_ context.Context, target registry.Repository, files map[string][]byte) (*registry.MirroredChart, error) {
	archive, _, err := registry.PackageWrapper(files)
	if err != nil {
		return nil, err
	}
	return &registry.MirroredChart{Repository: target, Digest: plan.DigestOf(archive), ArchiveDigest: plan.DigestOf(archive), Archive: archive}, nil
}

// t0 lets the fake build an archive outside a test's helper scope.
var t0 *testing.T

func fixture(t *testing.T, path ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func certManagerBundle(t *testing.T) *registry.Bundle {
	t.Helper()
	dir := filepath.Join("..", "resolver", "testdata", "bundle-cert-manager")
	b := &registry.Bundle{Digest: plan.DigestOf([]byte("bundle")), Files: map[string][]byte{}}
	for _, f := range []string{"bundle-info.yaml", "recipe.yaml", "001-cert-manager/values.yaml", "001-cert-manager/cluster-values.yaml", "001-cert-manager/upstream.env"} {
		b.Files[f] = fixture(t, dir, f)
	}
	return b
}

func lanRecipe(t *testing.T) []byte {
	t.Helper()
	return fixture(t, "..", "aicrjob", "testdata", "v1.0.0", "recipe-h100-eks-ubuntu-training-kubeflow.json")
}

func newResolver(t *testing.T) (*ResolverReconciler, *fakeRegistry) {
	t.Helper()
	t0 = t
	reg := &fakeRegistry{bundle: certManagerBundle(t), resolved: map[string]string{}}
	return &ResolverReconciler{Client: k8s, Reader: k8s, Config: testConfig(), Registry: reg, OperatorVersion: "test"}, reg
}

func reconcileResolver(t *testing.T, ctx context.Context, r *ResolverReconciler) error {
	t.Helper()
	_, err := r.Reconcile(ctx, request)
	return err
}

// The resolver runs a recipe Job, then a render Job over a run-scoped copy
// of the recipe, then writes the plan with its recipe and values and
// reports it; the Jobs that produced it are removed.
func TestResolverProducesPlan(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)

	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatalf("Reconcile() = %v", err)
	}
	recipeJobs := liveJobs(t, ctx, "recipe")
	if len(recipeJobs) != 1 || suspended(&recipeJobs[0]) {
		t.Fatalf("recipe Jobs = %d (suspended=%v), want one running", len(recipeJobs), len(recipeJobs) > 0 && suspended(&recipeJobs[0]))
	}
	args := strings.Join(recipeJobs[0].Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--intent training") || !strings.Contains(args, "--output cm://aicr-jobs/"+recipeJobs[0].Name) {
		t.Fatalf("recipe args = %s", args)
	}
	rt := getRuntime(t, ctx)
	if c := cond(rt, v1alpha1.ConditionResolved); c.Status != metav1.ConditionFalse || c.Reason != ReasonResolving {
		t.Fatalf("Resolved = %+v, want False/Resolving", c)
	}

	succeed(t, ctx, &recipeJobs[0], lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatalf("Reconcile() after recipe = %v", err)
	}
	renderJobs := liveJobs(t, ctx, "render")
	if len(renderJobs) != 1 {
		t.Fatalf("render Jobs = %d, want 1", len(renderJobs))
	}
	render := &renderJobs[0]
	copyCM := &corev1.ConfigMap{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: jobsNS, Name: aicrjob.InputName(aicrjob.StageRender, render.Labels[v1alpha1.LabelRunID], aicrjob.InputTo)}, copyCM); err != nil {
		t.Fatalf("render input copy: %v", err)
	}
	if copyCM.OwnerReferences[0].UID != render.UID || copyCM.Data[aicrjob.PayloadKey] != string(lanRecipe(t)) {
		t.Fatal("render input copy is not the Job-owned recipe")
	}
	if a := strings.Join(render.Spec.Template.Spec.Containers[0].Args, " "); !strings.Contains(a, "--output oci://registry.test:5000/aicr/cluster/bundles/cluster:run-") ||
		!strings.Contains(a, "--plain-http") {

		t.Fatalf("render args = %s", a)
	}

	renderArgs := render.Spec.Template.Spec.Containers[0].Args

	succeed(t, ctx, render, nil)
	reg.resolved["run-"+render.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatalf("Reconcile() after render = %v", err)
	}
	rt = getRuntime(t, ctx)
	c := cond(rt, v1alpha1.ConditionResolved)
	if c.Status != metav1.ConditionTrue || rt.Status.CandidatePlanDigest == "" || rt.Status.InputDigest == "" || c.ObservedGeneration != rt.Generation {
		t.Fatalf("status = %+v, Resolved = %+v", rt.Status, c)
	}
	doc, err := readPlan(ctx, k8s, operatorNS, rt.Status.CandidatePlanDigest)
	if err != nil || len(doc.Releases) != 1 || doc.Releases[0].ID != "cert-manager" || doc.RuntimeUID != string(rt.UID) {
		t.Fatalf("plan = %+v, %v", doc, err)
	}
	if rt.Status.AICR == nil || rt.Status.AICR.Version != "v1.0.0" {
		t.Errorf("status.aicr = %+v", rt.Status.AICR)
	}
	// The plan records the render's flags: the Job's arguments between the
	// recipe and the output.
	if want := renderArgs[slices.Index(renderArgs, "--recipe")+2 : slices.Index(renderArgs, "--output")]; !slices.Equal(doc.Provenance.RenderFlags, want) {
		t.Errorf("plan renderFlags = %v, want the render Job's %v", doc.Provenance.RenderFlags, want)
	}
	if len(liveJobs(t, ctx, "recipe"))+len(liveJobs(t, ctx, "render")) != 0 {
		t.Error("the Jobs that produced the plan were not removed")
	}

	// Already resolved: another reconcile creates nothing and keeps the plan.
	if err = reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(liveJobs(t, ctx, "recipe")) != 0 || getRuntime(t, ctx).Status.CandidatePlanDigest != rt.Status.CandidatePlanDigest {
		t.Error("a resolved runtime started new work")
	}
}

// Missing registry credentials are not a missing bundle: the render run is
// kept, resolution reports CredentialsUnavailable and retries, and resumes
// once the credentials are back.
func TestResolverWaitsForCredentials(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	succeed(t, ctx, &liveJobs(t, ctx, "recipe")[0], lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	render := liveJobs(t, ctx, "render")[0]
	succeed(t, ctx, &render, nil)
	reg.resolved["run-"+render.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
	reg.err = operrors.New(operrors.ErrCodeUnavailable, "cannot find registry credentials Secret aicr-system/registry-auth").
		WithReason(registry.ReasonCredentialsUnavailable)
	for range 2 {
		if err := reconcileResolver(t, ctx, r); operrors.CodeOf(err) != operrors.ErrCodeUnavailable {
			t.Fatalf("Reconcile() without credentials = %v", err)
		}
	}
	c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved)
	if jobs := liveJobs(t, ctx, "render"); len(jobs) != 1 || jobs[0].Name != render.Name ||
		c.Reason != registry.ReasonCredentialsUnavailable || !strings.Contains(c.Message, "registry-auth") {

		t.Fatalf("without credentials: %d render Jobs, Resolved = %+v", len(jobs), c)
	}
	reg.err = nil
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c = cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Status != metav1.ConditionTrue {
		t.Fatalf("with credentials back: Resolved = %+v", c)
	}
}

// A run whose output disappears is replaced by a new run; a run of earlier
// inputs is pruned and never used.
func TestResolverReplacesLostAndStaleRuns(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, _ := newResolver(t)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	first := liveJobs(t, ctx, "recipe")[0]
	succeed(t, ctx, &first, lanRecipe(t))
	if err := k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: jobsNS, Name: first.Name}}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	jobs := liveJobs(t, ctx, "recipe")
	if len(jobs) != 1 || jobs[0].Labels[v1alpha1.LabelRunID] == first.Labels[v1alpha1.LabelRunID] {
		t.Fatalf("after losing the output: %d live recipe Jobs, want a new run", len(jobs))
	}

	// Change an input: the in-flight run is pruned and a new input starts.
	stale := jobs[0]
	rt := getRuntime(t, ctx)
	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Overrides = []v1alpha1.Override{{Component: "cert-manager", Path: "replicaCount", Value: apiextensionsv1.JSON{Raw: []byte("2")}}}
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	jobs = liveJobs(t, ctx, "recipe")
	if len(jobs) != 1 || jobs[0].Labels[v1alpha1.LabelInput] == stale.Labels[v1alpha1.LabelInput] {
		t.Fatalf("after an input change: %d live recipe Jobs (input %s)", len(jobs), jobs[0].Labels[v1alpha1.LabelInput])
	}
	// A late result of the stale run changes nothing.
	succeed(t, ctx, &stale, lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if len(liveJobs(t, ctx, "render")) != 0 {
		t.Fatal("a late result from a stale run advanced resolution")
	}
}

// Out-of-range and mismatched AICR versions stall resolution with
// AICRVersionMismatch before any plan exists.
func TestResolverVersionGate(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, _ := newResolver(t)
	r.Config.AICRVersion = "v2.0.0"
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != aicrjob.ReasonVersionMismatch || c.Status != metav1.ConditionFalse {
		t.Fatalf("out of range: Resolved = %+v", c)
	}
	if len(liveJobs(t, ctx, "recipe")) != 0 {
		t.Fatal("an out-of-range version started a Job")
	}

	r.Config.AICRVersion = "v1.0.1"
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	job := liveJobs(t, ctx, "recipe")[0]
	succeed(t, ctx, &job, lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	rt := getRuntime(t, ctx)
	if c := cond(rt, v1alpha1.ConditionResolved); c.Reason != aicrjob.ReasonVersionMismatch || rt.Status.CandidatePlanDigest != "" {
		t.Fatalf("mismatch: Resolved = %+v, candidate %q", c, rt.Status.CandidatePlanDigest)
	}
	if len(liveJobs(t, ctx, "render")) != 0 {
		t.Fatal("a mismatched artifact reached the render stage")
	}
}

// AICR's permanent verdicts stall at once; transient failures are retried
// by new runs, up to the bound.
func TestResolverJobFailures(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, _ := newResolver(t)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	job := liveJobs(t, ctx, "recipe")[0]
	fail(t, ctx, &job, 2)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != "AICRInvalidRequest" || !strings.Contains(c.Message, "INVALID_REQUEST") {
		t.Fatalf("Resolved = %+v, want AICRInvalidRequest", c)
	}
	// The stall is checked again on the resync, without a new run.
	if res, err := r.Reconcile(ctx, request); err != nil || res.RequeueAfter != resyncInterval {
		t.Fatalf("a stalled resolution is not rechecked: %+v, %v", res, err)
	}
	if n := len(liveJobs(t, ctx, "recipe")); n != 1 {
		t.Fatalf("a permanent failure started a new run (%d Jobs)", n)
	}

	freshRuntime(t, ctx, nil)
	for run := 1; run <= maxStageRuns; run++ {
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		jobs := liveJobs(t, ctx, "recipe")
		if len(jobs) != run {
			t.Fatalf("run %d: %d Jobs", run, len(jobs))
		}
		fail(t, ctx, &jobs[0], -1)
	}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != "AICRTimeout" {
		t.Fatalf("after %d failed runs: Resolved = %+v, want AICRTimeout", maxStageRuns, c)
	}
}

// faultingRegistry passes the resolver's registry writes through a fault
// plan.
type faultingRegistry struct {
	*fakeRegistry
	f *faults
}

func (r *faultingRegistry) MirrorHTTPChart(ctx context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error) {
	var m *registry.MirroredChart
	err := r.f.write("mirror "+up.Chart, func() (err error) { m, err = r.fakeRegistry.MirrorHTTPChart(ctx, up, target); return err })
	return m, err
}

func (r *faultingRegistry) MirrorOCIChart(ctx context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error) {
	return r.MirrorHTTPChart(ctx, up, target)
}

func (r *faultingRegistry) PushWrapper(ctx context.Context, target registry.Repository, files map[string][]byte) (*registry.MirroredChart, error) {
	var m *registry.MirroredChart
	err := r.f.write("push "+target.Path, func() (err error) { m, err = r.fakeRegistry.PushWrapper(ctx, target, files); return err })
	return m, err
}

// A fault at every write resolution makes, API or registry, before it lands
// or after, changes nothing about the outcome: the same plan,
// written once with its content, and no run left behind. AICR's Jobs
// finish as soon as the operator has started them.
func TestResolverSurvivesAFaultAtEveryWrite(t *testing.T) {
	ctx := testContext(t)
	run := func(n int, landed bool) int {
		rt := freshRuntime(t, ctx, nil)
		f := &faults{n: n, landed: landed}
		r, reg := newResolver(t)
		r.Client = faultingClient(t, f)
		r.Registry = &faultingRegistry{fakeRegistry: reg, f: f}
		where := fmt.Sprintf("fault at write %d (landed %t)", n, landed)
		for range 20 {
			_, _ = r.Reconcile(ctx, request)
			for _, stage := range []aicrjob.Stage{aicrjob.StageRecipe, aicrjob.StageRender} {
				for _, j := range liveJobs(t, ctx, string(stage)) {
					if suspended(&j) || len(j.Status.Conditions) > 0 {
						continue
					}
					if stage == aicrjob.StageRecipe {
						succeed(t, ctx, &j, lanRecipe(t))
						continue
					}
					succeed(t, ctx, &j, nil)
					reg.resolved["run-"+j.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
				}
			}
			if cond(getRuntime(t, ctx), v1alpha1.ConditionResolved).Status == metav1.ConditionTrue {
				break
			}
		}
		got := getRuntime(t, ctx)
		plans := &corev1.ConfigMapList{}
		if err := k8s.List(ctx, plans, client.InNamespace(operatorNS), client.MatchingLabels{v1alpha1.LabelRole: v1alpha1.RolePlan}); err != nil {
			t.Fatal(err)
		}
		var mine []string
		for _, cm := range plans.Items {
			if cm.Annotations[v1alpha1.AnnotationRuntimeUID] == string(rt.UID) {
				mine = append(mine, cm.Name)
			}
		}
		name, err := plan.Name(got.Status.CandidatePlanDigest)
		if c := cond(got, v1alpha1.ConditionResolved); c.Status != metav1.ConditionTrue || err != nil || !slices.Equal(mine, []string{name}) {
			t.Fatalf("%s: Resolved %+v, plans %v, candidate %s", where, c, mine, got.Status.CandidatePlanDigest)
		}
		if n := len(liveJobs(t, ctx, "recipe")) + len(liveJobs(t, ctx, "render")); n != 0 {
			t.Fatalf("%s: %d runs left", where, n)
		}
		return f.writes
	}
	total := run(0, false)
	for n := 1; n <= total; n++ {
		for _, landed := range []bool{false, true} {
			run(n, landed)
		}
	}
	t.Logf("a fault at each of %d writes, before and after it landed, kept the outcome", total)
}

// AICR's transient exits get new runs without a bound, each after a backoff
// that doubles per run; earlier transient failures are removed, and while
// it waits the runtime names the failure. An internal failure
// still stalls after maxStageRuns.
// A transient error while a resolved runtime re-resolves for changed inputs
// of the same generation (such as the snapshot refresh) keeps it Resolved
// with its candidate and discovery, and says what it retries.
func TestResolverTransientErrorKeepsTheResolution(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	candidate := resolveCandidate(t, ctx, r, reg)
	discovered := &v1alpha1.DiscoveryStatus{AcceleratedNodes: 2, SystemNodes: 1}
	if err := r.applyStatus(ctx, getRuntime(t, ctx), &resolution{status: metav1.ConditionTrue, reason: ReasonResolved,
		message: "plan " + candidate + " resolved", candidate: candidate, inputDigest: getRuntime(t, ctx).Status.InputDigest, discovery: discovered}); err != nil {
		t.Fatal(err)
	}

	// The same generation's inputs change, and creating the new run fails.
	r.Config.AICRImage = strings.Replace(r.Config.AICRImage, "@sha256:", "-refreshed@sha256:", 1)
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, job := obj.(*batchv1.Job); job {
				return apierrors.NewServiceUnavailable("apiserver overloaded")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	if err := reconcileResolver(t, ctx, r); err == nil {
		t.Fatal("the transient error was not returned for a retry")
	}
	got := getRuntime(t, ctx)
	c := cond(got, v1alpha1.ConditionResolved)
	if c.Status != metav1.ConditionTrue || got.Status.CandidatePlanDigest != candidate || !strings.Contains(c.Message, "overloaded") {
		t.Fatalf("Resolved = %+v, candidate %s; want True with %s and the retried error", c, got.Status.CandidatePlanDigest, candidate)
	}
	if got.Status.Discovery == nil || got.Status.Discovery.AcceleratedNodes != 2 {
		t.Fatalf("status.discovery = %+v; the transient error dropped it", got.Status.Discovery)
	}
}

func TestResolverRetriesTransientFailures(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, _ := newResolver(t)
	now := time.Now()
	r.Now = func() time.Time { return now }
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= maxStageRuns+2; run++ {
		jobs := liveJobs(t, ctx, "recipe")
		if attempt(&jobs[0]) != run || len(jobs) > 2 {
			t.Fatalf("run %d: newest is attempt %d of %d live Jobs", run, attempt(&jobs[0]), len(jobs))
		}
		fail(t, ctx, &jobs[0], aicrjob.ExitUnavailable)
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved)
		if n := len(liveJobs(t, ctx, "recipe")); n != len(jobs) || c.Reason != ReasonResolving ||
			!strings.Contains(c.Message, "retrying after a transient failure") || !strings.Contains(c.Message, "exited 6") {

			t.Fatalf("run %d: a new run within the backoff (%d Jobs) or Resolved = %+v", run, n, c)
		}
		now = now.Add(backoff(run) + time.Second)
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if backoff(1) != defaults.StageRetryBackoff || backoff(2) != 2*defaults.StageRetryBackoff || backoff(100) != defaults.StageRetryMaxBackoff {
		t.Fatalf("backoff = %s, %s, %s", backoff(1), backoff(2), backoff(100))
	}

	for run := 1; run <= maxStageRuns; run++ {
		jobs := liveJobs(t, ctx, "recipe")
		fail(t, ctx, &jobs[0], aicrjob.ExitInternal)
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != "AICRInternal" {
		t.Fatalf("after %d internal failures: Resolved = %+v", maxStageRuns, c)
	}
}

// An unauthorized exit gets new runs after the backoff, since a credential
// can be rejected for a moment (a ServiceAccount recreated seconds before),
// and stalls once maxStageRuns runs were refused.
func TestResolverRetriesUnauthorizedUpToTheBound(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, _ := newResolver(t)
	now := time.Now()
	r.Now = func() time.Time { return now }
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	for run := 1; run < maxStageRuns; run++ {
		jobs := liveJobs(t, ctx, "recipe")
		if attempt(&jobs[0]) != run {
			t.Fatalf("run %d: newest is attempt %d", run, attempt(&jobs[0]))
		}
		fail(t, ctx, &jobs[0], aicrjob.ExitUnauthorized)
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved)
		if n := len(liveJobs(t, ctx, "recipe")); n != len(jobs) || c.Reason != ReasonResolving {
			t.Fatalf("run %d: a new run within the backoff (%d Jobs) or Resolved = %+v", run, n, c)
		}
		now = now.Add(backoff(run) + time.Second)
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	jobs := liveJobs(t, ctx, "recipe")
	fail(t, ctx, &jobs[0], aicrjob.ExitUnauthorized)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != "AICRUnauthorized" {
		t.Fatalf("after %d unauthorized runs: Resolved = %+v", maxStageRuns, c)
	}
}

// Overrides AICR cannot express stall before any Job runs a render.
func TestResolverRejectsInvalidOverride(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
		rt.Spec.Overrides = []v1alpha1.Override{{Component: "nfd", Path: "enabled", Value: apiextensionsv1.JSON{Raw: []byte(`"no"`)}}}
	})
	r, _ := newResolver(t)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	job := liveJobs(t, ctx, "recipe")[0]
	succeed(t, ctx, &job, lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != "InvalidOverride" {
		t.Fatalf("Resolved = %+v, want InvalidOverride", c)
	}
}

// With value overrides, a base render without them runs alongside the
// render. An override path that neither the base render nor the chart
// defaults know stalls resolution with OverridePathUnknown and writes no
// plan; marked additive, it resolves.
func TestResolverChecksOverridePaths(t *testing.T) {
	ctx := testContext(t)
	val := func(raw string) apiextensionsv1.JSON { return apiextensionsv1.JSON{Raw: []byte(raw)} }
	overrides := []v1alpha1.Override{
		{Component: "cert-manager", Path: "enabled", Value: val("true")},
		{Component: "cert-manager", Path: "crds.enabled", Value: val("false")},
		{Component: "cert-manager", Path: "replicaCount", Value: val("2")},
		{Component: "cert-manager", Path: "replicaCnt", Value: val("2")},
		{Component: "cert-manager", Path: "extra.flag", Value: val("true"), Additive: true},
	}
	freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) { rt.Spec.Overrides = overrides })
	r, reg := newResolver(t)

	renders := func() (render, base *batchv1.Job) {
		t.Helper()
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		recipe := liveJobs(t, ctx, "recipe")[0]
		succeed(t, ctx, &recipe, lanRecipe(t))
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		rj, bj := liveJobs(t, ctx, "render"), liveJobs(t, ctx, "base-render")
		if len(rj) != 1 || len(bj) != 1 || suspended(&rj[0]) || suspended(&bj[0]) {
			t.Fatalf("render Jobs = %d, base render Jobs = %d, want one of each running", len(rj), len(bj))
		}
		return &rj[0], &bj[0]
	}
	pushed := func(job *batchv1.Job) {
		succeed(t, ctx, job, nil)
		reg.resolved["run-"+job.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
	}

	render, base := renders()
	renderArgs := strings.Join(render.Spec.Template.Spec.Containers[0].Args, " ")
	baseArgs := strings.Join(base.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(renderArgs, "cert-manager:replicaCnt=2") || strings.Contains(baseArgs, "--set-json") ||
		!strings.Contains(baseArgs, "--set cert-manager:enabled=true") {

		t.Fatalf("render args = %s\nbase render args = %s", renderArgs, baseArgs)
	}
	pushed(render)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	if c := cond(getRuntime(t, ctx), v1alpha1.ConditionResolved); c.Reason != ReasonResolving {
		t.Fatalf("with the base render running: Resolved = %+v, want Resolving", c)
	}
	pushed(base)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	rt := getRuntime(t, ctx)
	c := cond(rt, v1alpha1.ConditionResolved)
	if c.Reason != resolver.ReasonOverridePathUnknown || !strings.Contains(c.Message, "cert-manager:replicaCnt") ||
		strings.Contains(c.Message, "replicaCount") || strings.Contains(c.Message, "extra.flag") || rt.Status.CandidatePlanDigest != "" {

		t.Fatalf("Resolved = %+v, candidate %q", c, rt.Status.CandidatePlanDigest)
	}

	patch := client.MergeFrom(rt.DeepCopy())
	rt.Spec.Overrides[3].Additive = true
	if err := k8s.Patch(ctx, rt, patch); err != nil {
		t.Fatal(err)
	}
	render, base = renders()
	pushed(render)
	pushed(base)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	rt = getRuntime(t, ctx)
	if c = cond(rt, v1alpha1.ConditionResolved); c.Status != metav1.ConditionTrue || rt.Status.CandidatePlanDigest == "" {
		t.Fatalf("with the path additive: Resolved = %+v", c)
	}
	if len(liveJobs(t, ctx, "base-render")) != 0 {
		t.Error("the base render Job outlived resolution")
	}
}

func TestReasonOf(t *testing.T) {
	t.Parallel()
	tests := map[string]error{
		"Internal":            context.Canceled,
		"AICRVersionMismatch": aicrjob.CheckSupportedVersion("v3.0.0"),
		"AICRInvalidRequest":  aicrjob.ErrorFromExitCode(aicrjob.StageRecipe, 2, "x"),
		"Conflict":            operrors.New(operrors.ErrCodeConflict, "x"),
	}
	for want, err := range tests {
		if got := reasonOf(err); got != want {
			t.Errorf("reasonOf(%v) = %s, want %s", err, got, want)
		}
	}
	if got := errorMessage(operrors.New(operrors.ErrCodeInternal, strings.Repeat("x", 2000))); len(got) != maxConditionMessage {
		t.Errorf("errorMessage() length = %d", len(got))
	}
}

// chartArchive is a minimal cert-manager chart archive with a CRD template.
func chartArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"cert-manager/Chart.yaml":          "apiVersion: v2\nname: cert-manager\nversion: v1.20.2\n",
		"cert-manager/values.yaml":         "replicaCount: 1\n",
		"cert-manager/templates/crds.yaml": "kind: CustomResourceDefinition\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A plan whose release targets a namespace outside the installation's
// component namespaces stalls resolution with NamespaceNotAllowed and is
// never a candidate.
func TestResolverRefusesNamespacesOutsideTheAllowlist(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	r.Config.RestrictNamespaces, r.Config.ComponentNamespaces = true, []string{"gpu-operator"}
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	succeed(t, ctx, &liveJobs(t, ctx, "recipe")[0], lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	render := &liveJobs(t, ctx, "render")[0]
	succeed(t, ctx, render, nil)
	reg.resolved["run-"+render.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	rt := getRuntime(t, ctx)
	c := cond(rt, v1alpha1.ConditionResolved)
	refused := c.Status == metav1.ConditionFalse && c.Reason == resolver.ReasonNamespaceNotAllowed && strings.Contains(c.Message, "cert-manager in cert-manager")
	if !refused || rt.Status.CandidatePlanDigest != "" {
		t.Fatalf("Resolved = %+v, candidate %q", c, rt.Status.CandidatePlanDigest)
	}
}

// A permanent failure at the snapshot and base-render stages stalls
// resolution with AICR's exit category and writes no plan; a transient one
// waits to retry.
func TestResolverStageFailures(t *testing.T) {
	ctx := testContext(t)
	putNode(t, ctx, "sys-a", "system", "")
	putNode(t, ctx, "gpu-a", "gpu", "NVIDIA-H100-80GB-HBM3")
	for _, tc := range []struct {
		stage aicrjob.Stage
		exit  int32
		want  string
	}{
		{aicrjob.StageSnapshot, aicrjob.ExitInvalidInput, "AICRInvalidRequest"},
		{aicrjob.StageSnapshot, aicrjob.ExitUnavailable, ReasonResolving},
		{aicrjob.StageBaseRender, aicrjob.ExitInvalidInput, "AICRInvalidRequest"},
		{aicrjob.StageBaseRender, aicrjob.ExitRateLimited, ReasonResolving},
	} {
		where := fmt.Sprintf("%s exiting %d", tc.stage, tc.exit)
		freshRuntime(t, ctx, func(rt *v1alpha1.ClusterRuntime) {
			rt.Spec.Overrides = []v1alpha1.Override{{Component: "cert-manager", Path: "replicaCount", Value: apiextensionsv1.JSON{Raw: []byte("2")}}}
		})
		r, _ := discoveryResolver(t)
		if tc.stage != aicrjob.StageSnapshot {
			r.Config.SkipDiscovery = true
		}
		failed := false
		for range 3 {
			if err := reconcileResolver(t, ctx, r); err != nil {
				t.Fatal(err)
			}
			if jobs := liveJobs(t, ctx, string(tc.stage)); len(jobs) > 0 {
				fail(t, ctx, &jobs[0], tc.exit)
				failed = true
				break
			}
			if recipe := liveJobs(t, ctx, "recipe"); len(recipe) > 0 && len(recipe[0].Status.Conditions) == 0 {
				succeed(t, ctx, &recipe[0], lanRecipe(t))
			}
		}
		if !failed {
			t.Fatalf("%s: the stage never ran", where)
		}
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		got := getRuntime(t, ctx)
		if c := cond(got, v1alpha1.ConditionResolved); c.Status != metav1.ConditionFalse || c.Reason != tc.want || got.Status.CandidatePlanDigest != "" {
			t.Fatalf("%s: Resolved = %+v, candidate %q", where, c, got.Status.CandidatePlanDigest)
		}
	}
}

// A deleted candidate plan is resolved again to the same digest.
// resolveCandidate drives the resolver and its recipe and render Jobs until
// the runtime is resolved, and returns the candidate.
func resolveCandidate(t *testing.T, ctx context.Context, r *ResolverReconciler, reg *fakeRegistry) string {
	t.Helper()
	for range 4 {
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		for _, j := range liveJobs(t, ctx, "recipe") {
			if len(j.Status.Conditions) == 0 {
				succeed(t, ctx, &j, lanRecipe(t))
			}
		}
		for _, j := range liveJobs(t, ctx, "render") {
			if len(j.Status.Conditions) == 0 {
				succeed(t, ctx, &j, nil)
				reg.resolved["run-"+j.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
			}
		}
	}
	rt := getRuntime(t, ctx)
	if cond(rt, v1alpha1.ConditionResolved).Status != metav1.ConditionTrue {
		t.Fatalf("not resolved: %+v", rt.Status)
	}
	return rt.Status.CandidatePlanDigest
}

func TestResolverRewritesDeletedCandidate(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	first := resolveCandidate(t, ctx, r, reg)
	name, err := plan.Name(first)
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: operatorNS, Name: name}}); err != nil {
		t.Fatal(err)
	}
	if again := resolveCandidate(t, ctx, r, reg); again != first {
		t.Fatalf("a deleted candidate %s resolved again to %s", first, again)
	}
	if _, err = readPlan(ctx, k8s, operatorNS, first); err != nil {
		t.Fatalf("the candidate was not written again: %v", err)
	}
}

// A plan belongs to the runtime UID it records. A recreated runtime
// that resolves the desired state of an earlier runtime's plan, which
// outlived it in the operator namespace, replaces that plan rather than
// reusing it: the earlier UID would go on the engine objects, and this
// runtime's inventory would never see them.
func TestResolverReplacesAnEarlierRuntimesPlan(t *testing.T) {
	ctx := testContext(t)
	earlier := freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	digest := resolveCandidate(t, ctx, r, reg)
	name, err := plan.Name(digest)
	if err != nil {
		t.Fatal(err)
	}
	left := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, left); err != nil {
		t.Fatal(err)
	}

	// freshRuntime deletes the operator's ConfigMaps: put the earlier plan
	// back as the deleted runtime left it.
	rt := freshRuntime(t, ctx, nil)
	if rt.UID == earlier.UID {
		t.Fatal("the recreated runtime kept its UID")
	}
	stale := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: left.Namespace, Name: left.Name, Labels: left.Labels, Annotations: left.Annotations},
		Immutable:  left.Immutable, Data: left.Data,
	}
	if err = k8s.Create(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if again := resolveCandidate(t, ctx, r, reg); again != digest {
		t.Fatalf("the recreated runtime resolved %s, not %s", again, digest)
	}
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	got := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, got); err != nil {
		t.Fatal(err)
	}
	if doc.RuntimeUID != string(rt.UID) || got.Annotations[v1alpha1.AnnotationRuntimeUID] != string(rt.UID) {
		t.Fatalf("the plan records runtime %s (annotation %s), want %s; the earlier runtime was %s",
			doc.RuntimeUID, got.Annotations[v1alpha1.AnnotationRuntimeUID], rt.UID, earlier.UID)
	}
}

// A rebuilt plan has no recipe. A resolution that reproduces its
// digest replaces it with the resolved plan, so its recipe and provenance
// come back, even for inputs this runtime resolved before.
func TestResolverRestoresARebuiltPlansRecipe(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	digest := resolveCandidate(t, ctx, r, reg)
	rebuild(t, ctx, digest)
	if again := resolveCandidate(t, ctx, r, reg); again != digest {
		t.Fatalf("resolved %s, not %s", again, digest)
	}
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Provenance.Source != plan.SourceResolved || doc.Recipe == nil {
		t.Fatalf("the plan is %s with recipe %+v, want Resolved with its recipe", doc.Provenance.Source, doc.Recipe)
	}
}

// A plan ConfigMap is written once per digest. A resolved plan replaces the
// rebuilt plan of its digest, and a rebuilt plan never replaces a resolved
// one: adoption and a lost-plan restore keep what resolution wrote.
func TestCreatePlanOnceKeepsTheResolvedPlan(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	digest := putPlan(t, ctx, rt, "50m")
	resolvedDoc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltDoc := *resolvedDoc
	rebuiltDoc.Provenance, rebuiltDoc.Recipe, rebuiltDoc.InputDigest = plan.Provenance{Source: plan.SourceRebuilt}, nil, ""
	write := func(doc *plan.Document) plan.Source {
		t.Helper()
		cm, cmErr := plan.ToConfigMap(doc, operatorNS, rt.Name)
		if cmErr != nil {
			t.Fatal(cmErr)
		}
		if cmErr = createPlanOnce(ctx, k8s, k8s, cm); cmErr != nil {
			t.Fatal(cmErr)
		}
		stored, cmErr := readPlan(ctx, k8s, operatorNS, digest)
		if cmErr != nil {
			t.Fatal(cmErr)
		}
		return stored.Provenance.Source
	}
	if got := write(&rebuiltDoc); got != plan.SourceResolved {
		t.Fatalf("a rebuilt write left a %s plan, want the resolved plan kept", got)
	}
	rebuild(t, ctx, digest)
	if got := write(resolvedDoc); got != plan.SourceResolved {
		t.Fatalf("a resolved write left a %s plan, want it to replace the rebuilt plan", got)
	}
}

// A runtime whose candidate is already an earlier runtime's plan, as one
// resolved before plans were replaced, resolves again instead of keeping it.
func TestResolverReresolvesACandidateOfAnotherRuntime(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	digest := resolveCandidate(t, ctx, r, reg)
	name, err := plan.Name(digest)
	if err != nil {
		t.Fatal(err)
	}
	own := &corev1.ConfigMap{}
	if err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: name}, own); err != nil {
		t.Fatal(err)
	}
	const earlier = "00000000-0000-4000-8000-000000000001"
	foreign := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: own.Namespace, Name: own.Name, Labels: own.Labels, Annotations: map[string]string{}},
		Immutable:  own.Immutable, Data: map[string]string{},
	}
	for k, v := range own.Annotations {
		foreign.Annotations[k] = strings.ReplaceAll(v, string(rt.UID), earlier)
	}
	for k, v := range own.Data {
		foreign.Data[k] = strings.ReplaceAll(v, string(rt.UID), earlier)
	}
	if err = k8s.Delete(ctx, own); err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}

	if again := resolveCandidate(t, ctx, r, reg); again != digest {
		t.Fatalf("resolved %s, not %s", again, digest)
	}
	doc, err := readPlan(ctx, k8s, operatorNS, digest)
	if err != nil {
		t.Fatal(err)
	}
	if doc.RuntimeUID != string(rt.UID) {
		t.Fatalf("the candidate records runtime %s, want %s", doc.RuntimeUID, rt.UID)
	}
}

// staleRuntimeClient serves the runtime as it was in stale, as an informer
// cache does before the watch event of the last status write lands.
func staleRuntimeClient(t *testing.T, stale *v1alpha1.ClusterRuntime) client.Client {
	t.Helper()
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if rt, ok := obj.(*v1alpha1.ClusterRuntime); ok {
				stale.DeepCopyInto(rt)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
}

// The reconcile that the resolving reconcile's pruned Jobs trigger can see
// the runtime from before the plan was reported. Deciding on that copy would
// start a recipe run for inputs already resolved, and its status apply would
// remove inputDigest and candidatePlanDigest, so the next reconcile prunes
// the run and the two alternate. The resolver decides on the API server's
// runtime instead.
func TestResolverIgnoresAStaleCachedRuntime(t *testing.T) {
	ctx := testContext(t)
	freshRuntime(t, ctx, nil)
	r, reg := newResolver(t)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	succeed(t, ctx, &liveJobs(t, ctx, "recipe")[0], lanRecipe(t))
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	render := liveJobs(t, ctx, "render")[0]
	succeed(t, ctx, &render, nil)
	reg.resolved["run-"+render.Labels[v1alpha1.LabelRunID]] = plan.DigestOf([]byte("bundle"))
	stale := getRuntime(t, ctx)
	if err := reconcileResolver(t, ctx, r); err != nil {
		t.Fatal(err)
	}
	resolved := getRuntime(t, ctx)
	if resolved.Status.InputDigest == "" || resolved.Status.CandidatePlanDigest == "" {
		t.Fatalf("not resolved: %+v", resolved.Status)
	}

	r.Client = staleRuntimeClient(t, stale)
	for i := range 3 {
		if err := reconcileResolver(t, ctx, r); err != nil {
			t.Fatal(err)
		}
		rt := getRuntime(t, ctx)
		if n := len(liveJobs(t, ctx, "recipe")); n != 0 {
			t.Fatalf("reconcile %d on a stale runtime started %d recipe runs", i, n)
		}
		if rt.Status.InputDigest != resolved.Status.InputDigest || rt.Status.CandidatePlanDigest != resolved.Status.CandidatePlanDigest ||
			cond(rt, v1alpha1.ConditionResolved).Status != metav1.ConditionTrue {

			t.Fatalf("reconcile %d on a stale runtime left inputDigest %q, candidate %q, Resolved %+v", i,
				rt.Status.InputDigest, rt.Status.CandidatePlanDigest, cond(rt, v1alpha1.ConditionResolved))
		}
	}
}
