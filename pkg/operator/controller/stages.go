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
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	"github.com/NVIDIA/aicr/pkg/operator/config"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// maxStageRuns bounds the runs of one stage for one input; after that many
// failures the stage stalls until its input changes.
const maxStageRuns = 3

// stageRun describes one AICR stage for one resolution input.
type stageRun struct {
	stage aicrjob.Stage
	input string
	// serviceAccount runs the Job; empty is the Job ServiceAccount.
	serviceAccount string
	// deadline shortens the stage deadline when positive.
	deadline time.Duration
	// maxRuns bounds the runs for the input; zero is maxStageRuns.
	maxRuns int
	// args renders the stage arguments for a new run.
	args func(*aicrjob.Request) ([]string, error)
	// inputs renders the run's input copies once its Job exists.
	inputs func(*batchv1.Job) ([]client.Object, error)
}

// stages drives AICR Jobs. Every read that decides an action is uncached.
type stages struct {
	client client.Client
	reader client.Reader
	config *config.Config
	// creds supplies pushing Jobs' registry credentials.
	creds registry.Provider
	// now is the clock for retry backoff; nil is time.Now.
	now func() time.Time
	// retrying is the transient failure a stage waits to retry, if any.
	retrying string
	// waiting says why the newest run's pod is not running, if it is not.
	waiting string
	// events records each failed run on the runtime; nil records none.
	events events.EventRecorder
}

// drive advances one stage. It returns the newest run's Job once it has
// succeeded, or nil while a run is in progress or waits to be retried (with
// a requeue delay), or a terminal error once the stage cannot succeed for
// this input. Only the newest run counts, so results from earlier runs are
// never used. A failed run is retried by its class: invalid input
// and not found never; internal failures and runs that failed without an
// exit up to maxStageRuns times; unauthorized up to maxStageRuns times, each
// after a backoff; AICR's transient exits without a bound, after a backoff
// that doubles per run. A stage with its own run bound fails on that many
// failed runs, whatever their class.
func (s *stages) drive(ctx context.Context, rt *v1alpha1.ClusterRuntime, run *stageRun) (*batchv1.Job, time.Duration, error) {
	jobs, err := s.jobs(ctx, rt, run.stage, run.input)
	if err != nil {
		return nil, 0, err
	}
	next := 1
	if len(jobs) > 0 {
		newest := &jobs[0]
		next = attempt(newest) + 1
		outcome, err := s.observe(ctx, run.stage, newest)
		if err != nil {
			return nil, 0, err
		}
		switch outcome.State {
		case aicrjob.StateSucceeded:
			return newest, 0, s.record(ctx, rt, run.stage, newest, outcome)
		case aicrjob.StateRunning:
			if suspended(newest) {
				// The operator stopped between creating the Job and resuming
				// it: finish the start.
				return nil, pollInterval, s.resume(ctx, newest, run)
			}
			s.waiting = outcome.Waiting
			return nil, pollInterval, s.recordWaiting(ctx, newest, outcome.Waiting)
		case aicrjob.StateFailed:
			if err := s.record(ctx, rt, run.stage, newest, outcome); err != nil {
				return nil, 0, err
			}
			if wait, err := s.retry(ctx, run, jobs, outcome); err != nil || wait > 0 {
				return nil, wait, err
			}
		}
	}
	return nil, pollInterval, s.start(ctx, rt, run, next)
}

// retryClasses names each retry class in metrics and Events.
var retryClasses = map[aicrjob.Retry]string{
	aicrjob.RetryNever: "never", aicrjob.RetryBounded: "bounded",
	aicrjob.RetryTransient: "transient", aicrjob.RetryAuthorization: "authorization",
}

// record marks a Job the first time its run is seen finished, then counts
// the run and its duration and, when it failed, reports it on the runtime as
// a Warning Event and a log line. The mark comes first, so a failed mark
// counts nothing and the next observation records the run once; a Job
// already gone is counted, since nothing will see it again.
func (s *stages) record(ctx context.Context, rt *v1alpha1.ClusterRuntime, stage aicrjob.Stage, job *batchv1.Job,
	outcome aicrjob.Outcome) error {

	if job.Annotations[v1alpha1.AnnotationRecorded] != "" {
		return nil
	}
	result, class := "succeeded", ""
	if outcome.State == aicrjob.StateFailed {
		result, class = "failed", retryClasses[outcome.Retry()]
	}
	patch := client.MergeFrom(job.DeepCopy())
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[v1alpha1.AnnotationRecorded] = result
	if err := s.client.Patch(ctx, job, patch); client.IgnoreNotFound(err) != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "record run of Job "+job.Name)
	}
	aicrJobRuns.WithLabelValues(string(stage), result, class).Inc()
	observeJob(job)
	if outcome.State == aicrjob.StateFailed {
		runID := job.Labels[v1alpha1.LabelRunID]
		log.FromContext(ctx).Info("AICR run failed", "stage", stage, "runID", runID, "job", job.Name,
			"attempt", attempt(job), "retry", class, "error", errorMessage(outcome.Err))
		if s.events != nil {
			emit(s.events, rt, corev1.EventTypeWarning, EventAICRRunFailed, "Run",
				"aicr %s run %s (attempt %d, retry %s) failed: %s", stage, runID, attempt(job), class, errorMessage(outcome.Err))
		}
	}
	return nil
}

// keep gives a terminal stage's Jobs the stalled TTL: they stay as its
// record and its run bound until the input changes and they are pruned.
func (s *stages) keep(ctx context.Context, jobs []batchv1.Job) error {
	ttl := int32(defaults.JobTTLStalled.Seconds())
	for i := range jobs {
		j := &jobs[i]
		if j.Spec.TTLSecondsAfterFinished != nil && *j.Spec.TTLSecondsAfterFinished >= ttl {
			continue
		}
		patch := client.MergeFrom(j.DeepCopy())
		j.Spec.TTLSecondsAfterFinished = &ttl
		if err := s.client.Patch(ctx, j, patch); client.IgnoreNotFound(err) != nil {
			return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "keep Job "+j.Name)
		}
	}
	return nil
}

// recordWaiting keeps on the Job why its pod is not running, when that
// changes, so a run that hits its deadline after the Job controller removed
// the pod still names the reason.
func (s *stages) recordWaiting(ctx context.Context, job *batchv1.Job, waiting string) error {
	if waiting == "" || job.Annotations[v1alpha1.AnnotationWaiting] == waiting {
		return nil
	}
	patch := client.MergeFrom(job.DeepCopy())
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[v1alpha1.AnnotationWaiting] = waiting
	if err := s.client.Patch(ctx, job, patch); client.IgnoreNotFound(err) != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "record why Job "+job.Name+" waits")
	}
	return nil
}

// retry decides on the newest run's failure: a terminal error, a wait, or
// neither, when a new run starts now. Before a run that follows a transient
// failure, the earlier transient failures are removed: they count toward no
// bound, and an outage would otherwise leave a Job per retry.
func (s *stages) retry(ctx context.Context, run *stageRun, jobs []batchv1.Job, outcome aicrjob.Outcome) (time.Duration, error) {
	newest := &jobs[0]
	failed, bounded, unauthorized := 0, 0, 0
	var transient []*batchv1.Job
	for i := range jobs {
		o, err := s.observe(ctx, run.stage, &jobs[i])
		if err != nil || o.State != aicrjob.StateFailed {
			continue
		}
		failed++
		switch o.Retry() {
		case aicrjob.RetryBounded:
			bounded++
		case aicrjob.RetryAuthorization:
			unauthorized++
		case aicrjob.RetryTransient:
			if i > 0 {
				transient = append(transient, &jobs[i])
			}
		case aicrjob.RetryNever:
		}
	}
	switch {
	case run.maxRuns > 0 && failed >= run.maxRuns,
		outcome.Retry() == aicrjob.RetryNever,
		operrors.IsPermanent(outcome.Err) && outcome.Retry() != aicrjob.RetryAuthorization,
		outcome.Retry() == aicrjob.RetryBounded && bounded >= maxStageRuns,
		outcome.Retry() == aicrjob.RetryAuthorization && unauthorized >= maxStageRuns:
		if err := s.keep(ctx, jobs); err != nil {
			return 0, err
		}
		return 0, terminal(outcome.Err)
	case outcome.Retry() == aicrjob.RetryTransient, outcome.Retry() == aicrjob.RetryAuthorization:
		s.retrying = errorMessage(outcome.Err)
		if wait := failedAt(newest).Add(backoff(attempt(newest))).Sub(s.clock()); wait > 0 {
			return wait, nil
		}
	}
	for _, j := range transient {
		if err := s.remove(ctx, j); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// backoff is the wait after a run's transient failure: StageRetryBackoff,
// doubling per run, at most StageRetryMaxBackoff.
func backoff(attempt int) time.Duration {
	wait := defaults.StageRetryBackoff
	for i := 1; i < attempt && wait < defaults.StageRetryMaxBackoff; i++ {
		wait *= 2
	}
	return min(wait, defaults.StageRetryMaxBackoff)
}

// failedAt is when a Job failed; a Job without the condition failed now.
func failedAt(job *batchv1.Job) time.Time {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

func (s *stages) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// attempt is a Job's run number; Jobs without one sort first.
func attempt(job *batchv1.Job) int {
	n, err := strconv.Atoi(job.Annotations[v1alpha1.AnnotationAttempt])
	if err != nil {
		return 0
	}
	return n
}

// jobs lists the runtime's live Jobs for a stage and input, newest first.
func (s *stages) jobs(ctx context.Context, rt *v1alpha1.ClusterRuntime, stage aicrjob.Stage, input string) ([]batchv1.Job, error) {
	list := &batchv1.JobList{}
	labels := client.MatchingLabels{v1alpha1.LabelRuntime: rt.Name, v1alpha1.LabelStage: string(stage), v1alpha1.LabelInput: input}
	if err := s.reader.List(ctx, list, client.InNamespace(stage.Namespace(s.config.Namespaces())), labels); err != nil {
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list AICR Jobs")
	}
	// A Job being deleted is a run the operator gave up on; its result is
	// never used.
	jobs := make([]batchv1.Job, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			jobs = append(jobs, list.Items[i])
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool { return attempt(&jobs[i]) > attempt(&jobs[j]) })
	return jobs, nil
}

// tagResolver resolves a registry tag to the manifest digest it names.
type tagResolver interface {
	Resolve(ctx context.Context, r registry.Repository, tag string) (string, error)
}

// pushed is the digest a pushing run published to repo: the one its
// termination message reports on AICR v1.1.0 and later, otherwise the one
// its run tag names in the registry. A run that pushed nothing is
// NotFound.
func (s *stages) pushed(ctx context.Context, reg tagResolver, stage aicrjob.Stage, job *batchv1.Job, repo registry.Repository) (string, error) {
	if aicrjob.SupportsV11(s.config.AICRVersion) {
		outcome, err := s.observe(ctx, stage, job)
		if err != nil {
			return "", err
		}
		if dgst, ok := aicrjob.PushedDigest(s.config.AICRVersion, outcome); ok {
			return dgst, nil
		}
	}
	return reg.Resolve(ctx, repo, runTag(job.Labels[v1alpha1.LabelRunID]))
}

// observe reads a Job's pod and classifies the run.
func (s *stages) observe(ctx context.Context, stage aicrjob.Stage, job *batchv1.Job) (aicrjob.Outcome, error) {
	pods := &corev1.PodList{}
	err := s.reader.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name})
	if err != nil {
		return aicrjob.Outcome{}, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list pods of "+job.Name)
	}
	return aicrjob.ObserveOutcome(stage, job.Labels[v1alpha1.LabelRunID], job, decidingPod(pods.Items))
}

// decidingPod is the pod whose result is the run's: the newest one a
// disruption did not end, since the Job replaced any that one did;
// otherwise the newest.
func decidingPod(pods []corev1.Pod) *corev1.Pod {
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].CreationTimestamp.Before(&pods[j].CreationTimestamp) })
	for i := len(pods) - 1; i >= 0; i-- {
		if !disrupted(&pods[i]) {
			return &pods[i]
		}
	}
	if len(pods) == 0 {
		return nil
	}
	return &pods[len(pods)-1]
}

func disrupted(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// start creates a new run: the suspended Job, its Job-owned objects, then
// the resume.
func (s *stages) start(ctx context.Context, rt *v1alpha1.ClusterRuntime, run *stageRun, attempt int) error {
	runID, err := aicrjob.NewRunID()
	if err != nil {
		return err
	}
	req := &aicrjob.Request{
		Stage: run.stage, RunID: runID, RuntimeName: rt.Name, RuntimeUID: string(rt.UID),
		Image: s.config.AICRImage, Namespaces: s.config.Namespaces(), ServiceAccount: s.config.JobServiceAccount,
		Input: run.input, Attempt: attempt, Deadline: run.deadline,
		NodeSelector: s.config.JobNodeSelector, Tolerations: s.config.JobTolerations,
	}
	if run.serviceAccount != "" {
		req.ServiceAccount = run.serviceAccount
	}
	if run.stage.Pushes() {
		jc, credErr := s.creds.ForJob(ctx)
		if credErr != nil {
			return credErr
		}
		req.CredentialSecret = jc.SecretName
		if req.CredentialSecret == "" {
			req.CredentialSecret = perRunSecretName(aicrjob.JobName(run.stage, runID))
		}
	}
	if req.Args, err = run.args(req); err != nil {
		return err
	}
	job, err := aicrjob.Build(req)
	if err != nil {
		return err
	}
	if err = s.client.Create(ctx, job); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create Job "+job.Name)
	}
	return s.resume(ctx, job, run)
}

// resume creates a suspended Job's run objects (AlreadyExists is success) and
// lets the Job start.
func (s *stages) resume(ctx context.Context, job *batchv1.Job, run *stageRun) error {
	var objs []client.Object
	if run.stage.WritesOutput() {
		out, err := aicrjob.Output(job)
		if err != nil {
			return err
		}
		objs = append(objs, out)
	}
	if run.stage.Pushes() && credentialSecretName(job) == perRunSecretName(job.Name) {
		// A per-run Secret holds fresh credentials and is owned by the Job;
		// an administrator's static Secret is mounted as it is.
		jc, err := s.creds.ForJob(ctx)
		if err != nil {
			return err
		}
		secret, err := aicrjob.CredentialSecret(job, perRunSecretName(job.Name), jc.DockerConfigJSON)
		if err != nil {
			return err
		}
		objs = append(objs, secret)
	}
	if run.inputs != nil {
		in, err := run.inputs(job)
		if err != nil {
			return err
		}
		objs = append(objs, in...)
	}
	for _, o := range objs {
		if err := s.client.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "create run object "+o.GetName())
		}
	}
	patch := client.MergeFrom(job.DeepCopy())
	job.Spec.Suspend = new(bool)
	if err := s.client.Patch(ctx, job, patch); err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "resume Job "+job.Name)
	}
	return nil
}

// credentialSecretName is the Secret a pushing Job mounts.
func credentialSecretName(job *batchv1.Job) string {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			return v.Secret.SecretName
		}
	}
	return ""
}

// perRunSecretName names the Job-owned credential Secret of a run.
func perRunSecretName(jobName string) string { return jobName + "-registry" }

// output reads a succeeded run's result through the uncached reader.
func (s *stages) output(ctx context.Context, job *batchv1.Job) ([]byte, error) {
	cm := &corev1.ConfigMap{}
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: job.Namespace, Name: job.Name}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, operrors.Wrap(operrors.ErrCodeNotFound, "output of "+job.Name+" is gone", err)
		}
		return nil, operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "read output of "+job.Name)
	}
	return aicrjob.ReadOutput(cm)
}

// remove deletes a Job with foreground propagation, so its pods and run
// objects go with it.
func (s *stages) remove(ctx context.Context, job *batchv1.Job) error {
	err := s.client.Delete(ctx, job, client.PropagationPolicy(aicrjob.DeletePropagation))
	if err != nil && !apierrors.IsNotFound(err) {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "delete Job "+job.Name)
	}
	return nil
}

// observeJob records a finished AICR Job's duration by stage and result.
func observeJob(job *batchv1.Job) {
	if job.Status.StartTime == nil {
		return
	}
	result, end := "", job.Status.CompletionTime
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			result = "succeeded"
		case batchv1.JobFailed:
			result, end = "failed", &c.LastTransitionTime
		case batchv1.JobSuspended, batchv1.JobFailureTarget, batchv1.JobSuccessCriteriaMet:
		}
	}
	if result == "" || end == nil {
		return
	}
	jobSeconds.WithLabelValues(job.Labels[v1alpha1.LabelStage], result).Observe(end.Sub(job.Status.StartTime.Time).Seconds())
}

// prune deletes the runtime's Jobs except those for keepInput; an empty
// keepInput deletes them all. Late results from other inputs are never read.
func (s *stages) prune(ctx context.Context, rt *v1alpha1.ClusterRuntime, keepInput string) error {
	list := &batchv1.JobList{}
	err := s.reader.List(ctx, list, client.InNamespace(s.config.JobsNamespace), client.MatchingLabels{v1alpha1.LabelRuntime: rt.Name})
	if err != nil {
		return operrors.WrapCtxErr(err, operrors.ErrCodeUnavailable, "list AICR Jobs")
	}
	for i := range list.Items {
		j := &list.Items[i]
		if !resolverStage[aicrjob.Stage(j.Labels[v1alpha1.LabelStage])] {
			continue
		}
		if j.Labels[v1alpha1.LabelInput] == keepInput && keepInput != "" {
			continue
		}
		if !j.DeletionTimestamp.IsZero() {
			continue
		}
		if err := s.remove(ctx, j); err != nil {
			return err
		}
	}
	return nil
}

// resolverStage are the stages a resolution runs in the Job namespace; prune
// leaves the rollout controller's assessment runs alone.
var resolverStage = map[aicrjob.Stage]bool{aicrjob.StageRecipe: true, aicrjob.StageRender: true, aicrjob.StageBaseRender: true}

func suspended(job *batchv1.Job) bool { return job.Spec.Suspend != nil && *job.Spec.Suspend }
