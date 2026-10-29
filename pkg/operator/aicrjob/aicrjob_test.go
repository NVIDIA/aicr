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

package aicrjob

import (
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	"github.com/NVIDIA/aicr/pkg/operator/defaults"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

const (
	image = "ghcr.io/nvidia/aicr:v1.0.0@sha256:e91ba6f4916a5bfcd19d05933b5b4d07a4dd05a738a1ffebfab243a32cbff433"
	runID = "0123456789ab"
	uid   = "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0"
)

var namespaces = Namespaces{Jobs: "aicr-jobs", Discovery: "aicr-discovery", Validation: "aicr-validation"}

func request(stage Stage) *Request {
	r := &Request{
		Stage: stage, RunID: runID, RuntimeName: v1alpha1.SingletonName, RuntimeUID: uid,
		Image: image, Args: []string{"--output", "cm://aicr-jobs/x"}, Namespaces: namespaces, ServiceAccount: "aicr-jobs",
		Input: "0123456789abcdef",
	}
	if stage.Pushes() {
		r.CredentialSecret = "aicr-registry-" + runID
	}
	return r
}

func TestNewRunID(t *testing.T) {
	t.Parallel()
	a, err := NewRunID()
	if err != nil || !runIDPattern.MatchString(a) {
		t.Fatalf("NewRunID() = %q, %v", a, err)
	}
	b, _ := NewRunID()
	if a == b {
		t.Fatal("NewRunID() repeated a value")
	}
}

func TestBuildAppliesJobPolicy(t *testing.T) {
	t.Parallel()
	for stage, spec := range stages {
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()
			job, err := Build(request(stage))
			if err != nil {
				t.Fatalf("Build() = %v", err)
			}
			if job.Name != "aicr-"+string(stage)+"-"+runID || job.Namespace != spec.namespace(namespaces) || job.Namespace != stage.Namespace(namespaces) {
				t.Errorf("job = %s/%s", job.Namespace, job.Name)
			}
			if job.Labels[v1alpha1.LabelRunID] != runID || job.Labels[v1alpha1.LabelManagedBy] != v1alpha1.ManagedByValue ||
				job.Labels[v1alpha1.LabelStage] != string(stage) || job.Labels[v1alpha1.LabelInput] != "0123456789abcdef" {

				t.Errorf("labels = %v", job.Labels)
			}
			s := job.Spec
			if *s.BackoffLimit != 0 {
				t.Errorf("backoffLimit = %d, want 0 (a failed run is retried as a new run)", *s.BackoffLimit)
			}
			if *s.ActiveDeadlineSeconds != int64(spec.deadline.Seconds()) {
				t.Errorf("activeDeadlineSeconds = %d", *s.ActiveDeadlineSeconds)
			}
			if *s.TTLSecondsAfterFinished != int32(defaults.JobTTLAfterFinished.Seconds()) {
				t.Errorf("ttlSecondsAfterFinished = %d", *s.TTLSecondsAfterFinished)
			}
			if !*s.Suspend {
				t.Error("Jobs start suspended until their run-scoped objects exist")
			}
			if p := s.PodFailurePolicy; p == nil || len(p.Rules) != 1 || p.Rules[0].Action != batchv1.PodFailurePolicyActionIgnore ||
				len(p.Rules[0].OnPodConditions) != 1 || p.Rules[0].OnPodConditions[0].Type != corev1.DisruptionTarget {

				t.Errorf("podFailurePolicy = %+v; an evicted pod must not spend the run", p)
			}
			pod := s.Template.Spec
			if pod.RestartPolicy != corev1.RestartPolicyNever || pod.ServiceAccountName != "aicr-jobs" {
				t.Errorf("pod restart=%s sa=%s", pod.RestartPolicy, pod.ServiceAccountName)
			}
			c := pod.Containers[0]
			if c.Image != image || c.Args[0] != spec.subcommand {
				t.Errorf("container image=%s args=%v", c.Image, c.Args)
			}
			sc := c.SecurityContext
			if *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot ||
				len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
				sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {

				t.Errorf("container security context is not restricted: %+v", sc)
			}
			if c.WorkingDir != ScratchPath {
				t.Errorf("workingDir = %q, want the writable scratch volume", c.WorkingDir)
			}
			if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
				t.Errorf("terminationMessagePolicy = %s", c.TerminationMessagePolicy)
			}
			if _, ok := c.Resources.Limits[corev1.ResourceMemory]; !ok {
				t.Error("no memory limit")
			}
			hasCreds := false
			for _, v := range pod.Volumes {
				if v.Secret != nil && v.Secret.SecretName == "aicr-registry-"+runID {
					hasCreds = true
				}
			}
			if hasCreds != stage.Pushes() {
				t.Errorf("credential volume present = %v, want %v", hasCreds, stage.Pushes())
			}
		})
	}
}

// A Job takes the placement the installation gives AICR Jobs, and none
// without it.
func TestBuildPlacesTheJob(t *testing.T) {
	t.Parallel()
	req := request(StageRecipe)
	job, err := Build(req)
	if err != nil {
		t.Fatal(err)
	}
	if pod := job.Spec.Template.Spec; pod.NodeSelector != nil || pod.Tolerations != nil {
		t.Errorf("unplaced Job: nodeSelector %v, tolerations %v", pod.NodeSelector, pod.Tolerations)
	}
	req.NodeSelector = map[string]string{"node-role": "system"}
	req.Tolerations = []corev1.Toleration{{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	if job, err = Build(req); err != nil {
		t.Fatal(err)
	}
	if pod := job.Spec.Template.Spec; pod.NodeSelector["node-role"] != "system" || len(pod.Tolerations) != 1 {
		t.Errorf("placed Job: nodeSelector %v, tolerations %v", pod.NodeSelector, pod.Tolerations)
	}
}

func TestBuildShortensDeadline(t *testing.T) {
	t.Parallel()
	r := request(StageValidation)
	r.Deadline = 10 * time.Minute
	job, err := Build(r)
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("activeDeadlineSeconds = %d, want 600", *job.Spec.ActiveDeadlineSeconds)
	}
	r.Deadline = 24 * time.Hour
	job, _ = Build(r)
	if *job.Spec.ActiveDeadlineSeconds != int64(defaults.ValidationJobDeadline.Seconds()) {
		t.Fatal("a longer requested deadline extended the stage deadline")
	}
}

func TestBuildRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{"unknown stage", func(r *Request) { r.Stage = "install" }},
		{"bad run ID", func(r *Request) { r.RunID = "RUN-1" }},
		{"tag-only image", func(r *Request) { r.Image = "ghcr.io/nvidia/aicr:v1.0.0" }},
		{"no namespace", func(r *Request) { r.Namespaces.Jobs = "" }},
		{"no service account", func(r *Request) { r.ServiceAccount = "" }},
		{"no runtime", func(r *Request) { r.RuntimeUID = "" }},
		{"pushing without credentials", func(r *Request) { r.Stage = StageRender; r.CredentialSecret = "" }},
		{"malformed input", func(r *Request) { r.Input = "XYZ" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := request(StageRecipe)
			tt.mutate(r)
			if _, err := Build(r); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
				t.Fatalf("Build() = %v, want INVALID_REQUEST", err)
			}
		})
	}
}

func finishedJob(cond batchv1.JobConditionType, reason string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: JobName(StageRecipe, runID), Labels: map[string]string{v1alpha1.LabelRunID: runID}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: cond, Status: corev1.ConditionTrue, Reason: reason},
		}},
	}
}

func podExited(code int32, msg string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  "aicr",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}},
	}}}}
}

// A run whose pod cannot start says why, with the Job's name: not
// scheduled, a container that cannot start, or no pod at all. A container
// the cluster can never start fails the run for good; and once the Job hits
// its deadline, the last reason recorded on it stays in the failure.
func TestObserveOutcomeNamesWhyAPodIsNotRunning(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: JobName(StageRecipe, runID), Labels: map[string]string{v1alpha1.LabelRunID: runID}}}
	pending := func(conds []corev1.PodCondition, waiting *corev1.ContainerStateWaiting) *corev1.Pod {
		p := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: conds}}
		if waiting != nil {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "aicr", State: corev1.ContainerState{Waiting: waiting}}}
		}
		return p
	}
	unscheduled := pending([]corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/9 nodes are available: 9 node(s) had untolerated taint {CriticalAddonsOnly: }"}}, nil)
	for name, tc := range map[string]struct {
		pod  *corev1.Pod
		want []string
	}{
		"unscheduled": {unscheduled, []string{job.Name, "not scheduled", "untolerated taint"}},
		"image pull":  {pending(nil, &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"}), []string{job.Name, "ImagePullBackOff", "pull access denied"}},
		"no pod":      {nil, []string{job.Name, "no pod", "Events"}},
	} {
		o, err := ObserveOutcome(StageRecipe, runID, job, tc.pod)
		if err != nil || o.State != StateRunning {
			t.Fatalf("%s: %+v, %v", name, o, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(o.Waiting, w) {
				t.Errorf("%s: waiting = %q, want %q in it", name, o.Waiting, w)
			}
		}
	}
	if o, _ := ObserveOutcome(StageRecipe, runID, job, pending(nil, &corev1.ContainerStateWaiting{Reason: "ContainerCreating"})); o.Waiting != "" {
		t.Errorf("a container being created is not waiting: %q", o.Waiting)
	}
	for _, reason := range []string{"InvalidImageName", "CreateContainerConfigError"} {
		o, err := ObserveOutcome(StageRecipe, runID, job, pending(nil, &corev1.ContainerStateWaiting{Reason: reason, Message: "bad"}))
		if err != nil || o.State != StateFailed || o.Retry() != RetryNever || !strings.Contains(o.Err.Error(), reason) {
			t.Errorf("%s: %+v, %v; want a failed run that is not retried", reason, o, err)
		}
	}

	deadline := finishedJob(batchv1.JobFailed, "DeadlineExceeded")
	deadline.Annotations = map[string]string{v1alpha1.AnnotationWaiting: "not scheduled: 0/9 nodes are available"}
	if o, _ := ObserveOutcome(StageRecipe, runID, deadline, nil); !strings.Contains(o.Err.Error(), "0/9 nodes are available") {
		t.Errorf("a deadline failure lost its last reason: %v", o.Err)
	}
}

func TestObserveOutcome(t *testing.T) {
	t.Parallel()
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelRunID: runID}}}
	if o, err := ObserveOutcome(StageRecipe, runID, running, nil); err != nil || o.State != StateRunning {
		t.Fatalf("running Job = %+v, %v", o, err)
	}

	digest := "oci://registry/bundles/cluster@sha256:aaaa"
	o, err := ObserveOutcome(StageRender, runID, finishedJob(batchv1.JobComplete, ""), podExited(0, digest+"\n"))
	if err != nil || o.State != StateSucceeded || o.Message != digest {
		t.Fatalf("succeeded Job = %+v, %v", o, err)
	}

	o, err = ObserveOutcome(StageRecipe, runID, finishedJob(batchv1.JobFailed, "BackoffLimitExceeded"), podExited(ExitInvalidInput, "unknown accelerator"))
	if err != nil || o.State != StateFailed || !operrors.IsPermanent(o.Err) || !strings.Contains(o.Err.Error(), "unknown accelerator") {
		t.Fatalf("invalid-input Job = %+v, %v", o, err)
	}

	o, _ = ObserveOutcome(StageRecipe, runID, finishedJob(batchv1.JobFailed, "DeadlineExceeded"), nil)
	if o.State != StateFailed || operrors.CodeOf(o.Err) != operrors.ErrCodeTimeout {
		t.Fatalf("deadline-exceeded Job = %+v", o)
	}

	// The log tail's last line is AICR's error; it survives the bound.
	long := strings.Repeat("x", 4*maxMessageBytes) + "\n[cli] command failed: error=[INVALID_REQUEST] bad"
	o, _ = ObserveOutcome(StageRecipe, runID, finishedJob(batchv1.JobFailed, ""), podExited(ExitInternal, long))
	if len(o.Message) > maxMessageBytes || o.Message != "[cli] command failed: error=[INVALID_REQUEST] bad" {
		t.Fatalf("message = %q (%d bytes)", o.Message, len(o.Message))
	}
}

// A result from another run never completes the current stage.
func TestObserveOutcomeRejectsStaleRun(t *testing.T) {
	t.Parallel()
	_, err := ObserveOutcome(StageRecipe, "ffffffffffff", finishedJob(batchv1.JobComplete, ""), podExited(0, ""))
	if operrors.CodeOf(err) != operrors.ErrCodeConflict {
		t.Fatalf("stale run = %v, want CONFLICT", err)
	}
}

// Every AICR exit category maps to the documented handling.
func TestErrorFromExitCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		exit      int32
		code      operrors.ErrorCode
		permanent bool
		transient bool
	}{
		{ExitInvalidInput, operrors.ErrCodeInvalidRequest, true, false},
		{ExitNotFound, operrors.ErrCodeNotFound, true, false},
		{ExitUnauthorized, operrors.ErrCodeUnauthorized, true, false},
		{ExitTimeout, operrors.ErrCodeTimeout, false, true},
		{ExitUnavailable, operrors.ErrCodeUnavailable, false, true},
		{ExitRateLimited, operrors.ErrCodeRateLimitExceeded, false, true},
		{ExitInternal, operrors.ErrCodeInternal, false, false},
		{ExitError, operrors.ErrCodeInternal, false, false},
		{ExitCanceled, operrors.ErrCodeCanceled, false, false},
		{137, operrors.ErrCodeInternal, false, false},
	}
	for _, tt := range tests {
		err := ErrorFromExitCode(StageRecipe, tt.exit, "x")
		if operrors.CodeOf(err) != tt.code || operrors.IsPermanent(err) != tt.permanent || operrors.IsTransient(err) != tt.transient {
			t.Errorf("exit %d: code=%s permanent=%v transient=%v", tt.exit, operrors.CodeOf(err), operrors.IsPermanent(err), operrors.IsTransient(err))
		}
	}
	if ErrorFromExitCode(StageRecipe, ExitSuccess, "") != nil {
		t.Error("exit 0 produced an error")
	}
}

// Transient exits are retried with backoff, permanent ones never,
// unauthorized ones with backoff up to a bound, and internal exits and runs
// that failed without an exit up to a bound.
func TestOutcomeRetry(t *testing.T) {
	t.Parallel()
	for exit, want := range map[int32]Retry{
		ExitTimeout: RetryTransient, ExitUnavailable: RetryTransient, ExitRateLimited: RetryTransient, ExitCanceled: RetryTransient,
		ExitInvalidInput: RetryNever, ExitNotFound: RetryNever, ExitUnauthorized: RetryAuthorization,
		ExitError: RetryBounded, ExitInternal: RetryBounded, 137: RetryBounded,
	} {
		o, err := ObserveOutcome(StageRender, runID, finishedJob(batchv1.JobFailed, ""), podExited(exit, "x"))
		if err != nil || o.Retry() != want {
			t.Errorf("exit %d: Retry() = %d, want %d (%v)", exit, o.Retry(), want, err)
		}
	}
	o, err := ObserveOutcome(StageRender, runID, finishedJob(batchv1.JobFailed, "DeadlineExceeded"), nil)
	if err != nil || o.Retry() != RetryBounded {
		t.Errorf("a run without an exit: Retry() = %d (%v)", o.Retry(), err)
	}
}

func TestVersionGate(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"v1.0.0", "v1.1.0", "v1.99.3"} {
		if err := CheckSupportedVersion(v); err != nil {
			t.Errorf("CheckSupportedVersion(%s) = %v", v, err)
		}
	}
	for _, v := range []string{"v0.22.0", "v2.0.0", "v1.1.0-rc.1", "1.0", "", "latest"} {
		if err := CheckSupportedVersion(v); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("CheckSupportedVersion(%q) = %v, want INVALID_REQUEST", v, err)
		}
	}
	if err := CheckArtifactVersion("v1.0.0", "v1.0.0"); err != nil {
		t.Errorf("matching versions = %v", err)
	}
	if err := CheckArtifactVersion("v1.0.0", "1.0.0"); err != nil {
		t.Errorf("artifact version without v prefix = %v", err)
	}
	for _, reported := range []string{"v1.1.0", ""} {
		err := CheckArtifactVersion("v1.0.0", reported)
		var se *operrors.StructuredError
		if s, ok := err.(*operrors.StructuredError); ok { //nolint:errorlint // returned directly
			se = s
		}
		if se == nil || se.Context["reason"] != ReasonVersionMismatch {
			t.Errorf("CheckArtifactVersion(v1.0.0, %q) = %v, want %s", reported, err, ReasonVersionMismatch)
		}
	}
	if err := CheckArtifactVersion("v2.0.0", "v2.0.0"); err == nil {
		t.Error("an out-of-range declared version passed the gate")
	}
}

// AICR v1.1.0 and later report a pushing run's digest through the
// termination message; anything else falls back
// to the run's registry tag.
func TestPushedDigest(t *testing.T) {
	t.Parallel()
	dgst := "sha256:" + strings.Repeat("ab", 32)
	if got := ImageRefsArgs("v1.0.0"); got != nil {
		t.Errorf("ImageRefsArgs(v1.0.0) = %v", got)
	}
	if got := ImageRefsArgs("1.1.2"); len(got) != 2 || got[0] != "--image-refs" || got[1] != TerminationMessagePath {
		t.Errorf("ImageRefsArgs(1.1.2) = %v", got)
	}
	for _, tc := range []struct {
		version, message string
		want             bool
	}{
		{"v1.1.0", dgst, true},
		{"v1.2.0", dgst, true},
		{"v1.0.0", dgst, false},
		{"v1.1.0", "", false},
		{"v1.1.0", "[INVALID_REQUEST] refused", false},
		{"v1.1.0", dgst + "\n", false},
		{"v1.1.0", "sha512:" + strings.Repeat("ab", 64), false},
	} {
		got, ok := PushedDigest(tc.version, Outcome{State: StateSucceeded, Message: tc.message})
		if ok != tc.want || (ok && got != dgst) {
			t.Errorf("PushedDigest(%s, %q) = %q, %v; want %v", tc.version, tc.message, got, ok, tc.want)
		}
	}
}
