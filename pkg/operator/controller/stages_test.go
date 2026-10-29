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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// tags is a registry that names digests by run tag and counts lookups.
type tags struct {
	digests map[string]string
	lookups int
}

func (r *tags) Resolve(_ context.Context, _ registry.Repository, tag string) (string, error) {
	r.lookups++
	if d, ok := r.digests[tag]; ok {
		return d, nil
	}
	return "", operrors.New(operrors.ErrCodeNotFound, "no tag "+tag)
}

// A pushing run's digest comes from its termination message on AICR v1.1.0
// and later, with no registry lookup; on v1.0.x, or when the message holds
// no digest, from the run's tag.
func TestPushedDigest(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	cfg := testConfig()
	st := &stages{client: k8s, reader: k8s, config: cfg}
	repo := cfg.Registry().Child("bundles/" + rt.Name)
	reported, tagged := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	newJob := func(runID string) *batchv1.Job {
		t.Helper()
		job, err := aicrjob.Build(&aicrjob.Request{
			Stage: aicrjob.StageRender, RunID: runID, RuntimeName: rt.Name, RuntimeUID: string(rt.UID), Image: aicrImage,
			Namespaces: cfg.Namespaces(), ServiceAccount: cfg.JobServiceAccount, CredentialSecret: "creds", Args: []string{"--output", "x"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = k8s.Create(ctx, job); err != nil {
			t.Fatal(err)
		}
		return job
	}

	reporting := newJob("0123456789a1")
	succeedReporting(t, ctx, reporting, reported)
	silent := newJob("0123456789a2")
	succeed(t, ctx, silent, nil)
	reg := &tags{digests: map[string]string{"run-0123456789a1": tagged, "run-0123456789a2": tagged}}
	for _, tc := range []struct {
		version string
		job     *batchv1.Job
		want    string
		lookups int
	}{
		{"v1.1.2", reporting, reported, 0},
		{"v1.1.2", silent, tagged, 1},
		{"v1.0.0", reporting, tagged, 1},
	} {
		cfg.AICRVersion, reg.lookups = tc.version, 0
		got, err := st.pushed(ctx, reg, aicrjob.StageRender, tc.job, repo)
		if err != nil || got != tc.want || reg.lookups != tc.lookups {
			t.Errorf("%s run %s: pushed() = %s, %v after %d lookups; want %s after %d",
				tc.version, tc.job.Labels[v1alpha1.LabelRunID], got, err, reg.lookups, tc.want, tc.lookups)
		}
	}
	delete(reg.digests, "run-0123456789a2")
	if _, err := st.pushed(ctx, reg, aicrjob.StageRender, silent, repo); operrors.CodeOf(err) != operrors.ErrCodeNotFound {
		t.Errorf("a run that pushed nothing: pushed() = %v, want NotFound", err)
	}
}

// Render runs ask AICR v1.1.0 and later to report the bundle digest; the
// flag is I/O, not render shape, so the plan's recorded flags omit it.
func TestRenderRunReportsDigest(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	r, _ := newResolver(t)
	rs := &renderStage{stage: aicrjob.StageRender, spec: &rt.Spec}
	req := &aicrjob.Request{Stage: aicrjob.StageRender, RunID: "0123456789ab", Namespaces: r.Config.Namespaces()}
	refs := []string{"--image-refs", aicrjob.TerminationMessagePath}
	for version, want := range map[string]bool{"v1.0.0": false, "v1.1.2": true} {
		r.Config.AICRVersion = version
		args, err := r.renderRun(rs, "input", nil, r.Config.Registry().Child("bundles/"+rt.Name)).args(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Equal(args[max(0, len(args)-2):], refs); got != want {
			t.Errorf("%s render args %v: --image-refs %v, want %v", version, args, got, want)
		}
	}
}

// A drain, scale-down or node repair evicts a Job's pod with a
// DisruptionTarget condition; the Job ignores that failure and replaces the
// pod, and the run's outcome is the replacement's, whatever the pods' names.
func TestObserveReadsTheReplacementPod(t *testing.T) {
	ctx := testContext(t)
	st := &stages{client: k8s, reader: k8s, config: testConfig()}
	job, err := aicrjob.Build(&aicrjob.Request{
		Stage: aicrjob.StageRecipe, RunID: "0123456789ab", RuntimeName: "cluster", RuntimeUID: "u", Image: aicrImage,
		Namespaces: testConfig().Namespaces(), ServiceAccount: "aicr-jobs", Input: "0123456789abcdef", Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = k8s.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	pod := func(name string, exitCode int32, message string, disrupted bool) {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: job.Namespace,
				Labels: map[string]string{"batch.kubernetes.io/job-name": job.Name}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "aicr", Image: aicrImage}}},
		}
		if createErr := k8s.Create(ctx, p); createErr != nil {
			t.Fatal(createErr)
		}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "aicr", Image: aicrImage, ImageID: aicrImage,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}}}}
		if disrupted {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "EvictionByEvictionAPI"}}
		}
		if updateErr := k8s.Status().Update(ctx, p); updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	pod(job.Name+"-replacement", 0, "the replacement's result", false)
	pod(job.Name+"-zz-evicted", 143, "", true)
	finish(t, ctx, job, batchv1.JobComplete, batchv1.JobSuccessCriteriaMet)
	o, err := st.observe(ctx, aicrjob.StageRecipe, job)
	if err != nil || o.State != aicrjob.StateSucceeded || o.Message != "the replacement's result" {
		t.Fatalf("observe() = %+v, %v", o, err)
	}
}

// A run whose pod cannot be scheduled is named with the scheduler's reason,
// on the runtime's resolution and on the Job, instead of being reported as
// running until its deadline.
func TestUnscheduledRunIsNamed(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	st := &stages{client: k8s, reader: k8s, config: testConfig()}
	run := &stageRun{stage: aicrjob.StageRecipe, input: "0123456789abcdef",
		args: func(*aicrjob.Request) ([]string, error) { return []string{"recipe"}, nil }}
	if _, _, err := st.drive(ctx, rt, run); err != nil {
		t.Fatal(err)
	}
	jobs := liveJobs(t, ctx, string(aicrjob.StageRecipe))
	if len(jobs) != 1 {
		t.Fatalf("recipe Jobs = %d", len(jobs))
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobs[0].Name + "-pod", Namespace: jobs[0].Namespace,
			Labels: map[string]string{"batch.kubernetes.io/job-name": jobs[0].Name}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "aicr", Image: aicrImage}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	const reason = "0/9 nodes are available: 9 node(s) had untolerated taint {CriticalAddonsOnly: }"
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: reason}}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if job, _, err := st.drive(ctx, rt, run); err != nil || job != nil {
		t.Fatalf("drive() = %v, %v", job, err)
	}
	if msg := waitingFor(rt).retrying(st).message; !strings.Contains(msg, jobs[0].Name) || !strings.Contains(msg, reason) {
		t.Fatalf("resolution message = %q", msg)
	}
	recorded := liveJobs(t, ctx, string(aicrjob.StageRecipe))[0].Annotations[v1alpha1.AnnotationWaiting]
	if !strings.Contains(recorded, reason) {
		t.Fatalf("the Job records %q", recorded)
	}
}

// Each failed run is recorded once, however often it is observed: a Warning
// Event on the runtime and a counter increment by stage, result and retry
// class. A stage that turns terminal keeps its failed Jobs beyond their
// usual TTL, so the TTL cannot reset the run bound or erase the record.
func TestFailedRunsAreRecordedAndKept(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	recorder := events.NewFakeRecorder(10)
	st := &stages{client: k8s, reader: k8s, config: testConfig(), events: recorder}
	run := &stageRun{stage: aicrjob.StageRecipe, input: "0123456789abcdef",
		args: func(*aicrjob.Request) ([]string, error) { return []string{"recipe"}, nil }}
	failures := func() float64 {
		return testutil.ToFloat64(aicrJobRuns.WithLabelValues(string(aicrjob.StageRecipe), "failed", "bounded"))
	}
	before := failures()
	var err error
	for i := range maxStageRuns {
		if _, _, err = st.drive(ctx, rt, run); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		job := liveJobs(t, ctx, string(aicrjob.StageRecipe))[0]
		failReporting(t, ctx, &job, 1, "[INTERNAL] boom")
	}
	_, _, err = st.drive(ctx, rt, run)
	if !isTerminal(err) {
		t.Fatalf("after %d failed runs: %v", maxStageRuns, err)
	}
	if _, _, err = st.drive(ctx, rt, run); !isTerminal(err) {
		t.Fatal(err)
	}
	if got := failures() - before; got != maxStageRuns {
		t.Errorf("failed runs counted %v times, want %d", got, maxStageRuns)
	}
	for range maxStageRuns {
		if e := <-recorder.Events; !strings.Contains(e, EventAICRRunFailed) || !strings.Contains(e, "boom") {
			t.Errorf("Event = %q", e)
		}
	}
	select {
	case e := <-recorder.Events:
		t.Errorf("a run was recorded twice: %q", e)
	default:
	}
	for _, j := range liveJobs(t, ctx, string(aicrjob.StageRecipe)) {
		if ttl := j.Spec.TTLSecondsAfterFinished; ttl == nil || time.Duration(*ttl)*time.Second < defaults.JobTTLStalled {
			t.Errorf("failed Job %s keeps TTL %v after its stage stalled", j.Name, ttl)
		}
	}
}

// jobObservations is how many Job durations the histogram holds for a stage
// and result.
func jobObservations(t *testing.T, stage, result string) uint64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(jobSeconds); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["stage"] == stage && labels["result"] == result {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// A finished run is marked on its Job before it is counted: when the mark
// fails, nothing is counted and the next observation counts the run, and
// its duration, once. The duration needs no deletion of the Job, which its
// TTL may do first.
func TestRunIsCountedOnceItsMarkLands(t *testing.T) {
	ctx := testContext(t)
	rt := freshRuntime(t, ctx, nil)
	base, err := client.NewWithWatch(restConfig, client.Options{Scheme: k8s.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	refuseNextPatch := false
	st := &stages{reader: k8s, config: testConfig(), client: interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if refuseNextPatch {
				refuseNextPatch = false
				return apierrors.NewServiceUnavailable("apiserver overloaded")
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})}
	run := &stageRun{stage: aicrjob.StageRecipe, input: "0123456789abcdef",
		args: func(*aicrjob.Request) ([]string, error) { return []string{"recipe"}, nil }}
	if _, _, err = st.drive(ctx, rt, run); err != nil {
		t.Fatal(err)
	}
	job := liveJobs(t, ctx, string(aicrjob.StageRecipe))[0]
	succeed(t, ctx, &job, nil)
	runs := func() float64 {
		return testutil.ToFloat64(aicrJobRuns.WithLabelValues(string(aicrjob.StageRecipe), "succeeded", ""))
	}
	beforeRuns, beforeDurations := runs(), jobObservations(t, string(aicrjob.StageRecipe), "succeeded")

	refuseNextPatch = true
	if _, _, err = st.drive(ctx, rt, run); err == nil {
		t.Fatal("drive() recorded the run although its mark failed")
	}
	if got := runs() - beforeRuns; got != 0 {
		t.Fatalf("a run whose mark failed was counted %v times", got)
	}
	for range 2 {
		if done, _, driveErr := st.drive(ctx, rt, run); driveErr != nil || done == nil {
			t.Fatalf("drive() = %v, %v", done, driveErr)
		}
	}
	if got := runs() - beforeRuns; got != 1 {
		t.Errorf("the run was counted %v times, want once", got)
	}
	if got := jobObservations(t, string(aicrjob.StageRecipe), "succeeded") - beforeDurations; got != 1 {
		t.Errorf("the run's duration was observed %d times, want once", got)
	}
}
