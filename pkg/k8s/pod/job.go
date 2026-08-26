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

package pod

import (
	"context"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// jobWatchResumeBackoff paces each watch re-establishment in rewatchJob. It is
// a package var (not the constant directly) only so tests can drop it to a
// negligible value and exercise the resume path without a real sleep;
// production code never reassigns it.
var jobWatchResumeBackoff = defaults.K8sJobWatchResumeBackoff

// jobWatchResumeJitterFactor spreads the resume backoff by up to +20% so that
// many validators whose watches were all closed by the same apiserver rollout
// do not reconnect in lockstep and produce a synchronized thundering herd.
const jobWatchResumeJitterFactor = 0.2

// resumeContext builds the structured-error context shared by the resume
// helper so a resume failure names the Job, mirroring the sibling pod-wait
// helpers.
func resumeContext(namespace, name string) map[string]any {
	return map[string]any{keyNamespace: namespace, keyName: name}
}

// isRetryableWatchError reports whether a watch.Error event is a routine,
// resumable signal (HTTP 410 Gone / ResourceExpired — the apiserver compacted
// past the watch's ResourceVersion) rather than a fatal stream error. Callers
// treat a retryable error like a channel close and re-establish the watch;
// anything else aborts the wait.
//
// Against a real apiserver this is the *common* shape of a stale-ResourceVersion
// rejection: the Watch call itself succeeds and the 410 arrives as the stream's
// first ERROR event, not as a synchronous call error.
func isRetryableWatchError(event watch.Event) bool {
	if event.Type != watch.Error {
		return false
	}
	err := apierrors.FromObject(event.Object)
	return apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
}

// resumeJobWatch reconnects a Job watch that ended — the channel closed or the
// apiserver emitted a retryable 410 — before the Job reached a terminal state.
//
// It resyncs via a field-selected List rather than a Get, which is load-bearing:
//   - The List's collection ResourceVersion (metadata.resourceVersion) is the
//     current cluster revision, so the re-established watch always starts from a
//     live RV. Re-watching from the Job's own object ResourceVersion is unsafe:
//     a Job that runs untouched past the apiserver's watch-cache window keeps
//     reporting the same aged-out object RV, so every watch from it 410s
//     immediately and the wait degrades to paced polling for the rest of the run
//     (the exact long-running-validator case issue #1966 targets).
//   - The List still returns the Job, so a terminal transition that landed while
//     the watch was down is observed here and not missed. Because the new watch
//     starts from the List's collection RV, any transition after the List is
//     replayed by the watch — there is no List-to-Watch gap (an empty
//     ResourceVersion would reopen that gap and is deliberately not used).
//
// A jittered bounded backoff precedes every attempt so a stream that keeps
// ending immediately (flapping LB, apiserver rollout) cannot make the loop spin,
// and fleets sharing one apiserver do not reconnect in lockstep. The backoff
// honors ctx cancellation, so the caller's context deadline remains the sole
// give-up authority.
//
// It returns exactly one of:
//   - terminal != nil: the Job was terminal in the resync List; the caller
//     returns it (w is nil, nothing to continue).
//   - w != nil: a fresh watch to continue consuming (terminal is nil).
//   - err != nil: give up. Transient List/Watch failures are classified as
//     ErrCodeUnavailable (vs ErrCodeTimeout when the context is done) via
//     classifyReGetError, matching the sibling pod-wait helpers.
func resumeJobWatch(ctx context.Context, client kubernetes.Interface, namespace, name string) (terminal *batchv1.Job, w watch.Interface, err error) {
	select {
	case <-time.After(wait.Jitter(jobWatchResumeBackoff, jobWatchResumeJitterFactor)):
	case <-ctx.Done():
		return nil, nil, errors.WrapWithContext(errors.ErrCodeTimeout,
			"context canceled before Job watch resume", ctx.Err(), resumeContext(namespace, name))
	}

	// The timer and ctx.Done() branches race when the backoff elapses at or near
	// cancellation: select picks a ready case at random, so a canceled context can
	// lose to an already-fired timer. Re-check so a caller that has given up
	// deterministically wins before we List/Watch against its apiserver.
	if err := ctx.Err(); err != nil {
		return nil, nil, errors.WrapWithContext(errors.ErrCodeTimeout,
			"context canceled before Job watch resume", err, resumeContext(namespace, name))
	}

	// Resync via List: its collection ResourceVersion is current, and its result
	// lets us catch a terminal transition that landed while the watch was down.
	list, listErr := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if listErr != nil {
		return nil, nil, classifyReGetError(ctx, "job watch closed and Job re-list failed", listErr)
	}
	if len(list.Items) == 0 {
		// The Job no longer exists — deleted while the watch was down.
		return nil, nil, errors.NewWithContext(errors.ErrCodeInternal,
			"Job not found during watch resume", resumeContext(namespace, name))
	}
	job := &list.Items[0]
	if isJobTerminal(job) {
		return job, nil, nil
	}

	// Re-establish the watch from the current collection ResourceVersion so it
	// starts from a live revision and replays any change after the List.
	w, watchErr := client.BatchV1().Jobs(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + name,
		ResourceVersion: list.ResourceVersion,
	})
	if watchErr != nil {
		return nil, nil, classifyReGetError(ctx, "failed to re-establish Job watch after resync", watchErr)
	}
	return nil, w, nil
}

// WaitForJobCompletion waits for a Kubernetes Job to complete successfully or fail.
// Returns nil if job completes successfully, error if job fails or context deadline exceeded.
//
// Performs an initial Get to catch already-complete Jobs, then uses the
// watch API for efficient monitoring.
func WaitForJobCompletion(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: Job may already be in a terminal state.
	current, err := client.BatchV1().Jobs(namespace).Get(timeoutCtx, name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get Job", err)
	}
	if done, checkErr := checkJobStatus(current); done {
		return checkErr
	}

	watcher, err := client.BatchV1().Jobs(namespace).Watch(
		timeoutCtx,
		metav1.ListOptions{
			FieldSelector: "metadata.name=" + name,
			// Start the watch from the version observed by the fast-path Get so a
			// terminal transition landing between the Get and the watch is
			// replayed rather than missed (matching WaitForJobTerminal).
			ResourceVersion: current.ResourceVersion,
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	// Closure form so a re-established watch (reassigning watcher below) stops
	// the current stream rather than leaking the original.
	defer func() { watcher.Stop() }()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "job completion timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			// The watch stream ended when the channel closes or the apiserver
			// emits a retryable 410 (it compacted past our ResourceVersion).
			// kube-apiserver closes every watch after a server-selected timeout
			// at or above --min-request-timeout; rolling restarts and LB drops
			// close them early. Resync the Job via a field-selected List to catch
			// a terminal transition during the gap, then re-establish the watch
			// and keep waiting — a watch closure alone never ends the wait; only
			// the context deadline or a classified resync failure does.
			if !ok || isRetryableWatchError(event) {
				if ctxErr := timeoutCtx.Err(); ctxErr != nil {
					return errors.Wrap(errors.ErrCodeTimeout, "job completion timeout", ctxErr)
				}
				terminal, newWatcher, resumeErr := resumeJobWatch(timeoutCtx, client, namespace, name)
				if resumeErr != nil {
					if ctxErr := timeoutCtx.Err(); ctxErr != nil {
						return errors.Wrap(errors.ErrCodeTimeout, "job completion timeout", ctxErr)
					}
					return resumeErr
				}
				if terminal != nil {
					_, checkErr := checkJobStatus(terminal)
					return checkErr
				}
				slog.Debug("job watch closed before terminal state, re-established",
					keyNamespace, namespace, keyName, name)
				watcher.Stop()
				watcher = newWatcher
				continue
			}
			if event.Type == watch.Error {
				return errors.Wrap(errors.ErrCodeInternal, "watch stream error",
					apierrors.FromObject(event.Object))
			}

			job, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}

			if done, checkErr := checkJobStatus(job); done {
				return checkErr
			}
		}
	}
}

// WaitForJobTerminal waits for a Kubernetes Job to reach a terminal state —
// Complete OR Failed — and returns the observed Job without classifying the
// terminal disposition as an error. This differs from WaitForJobCompletion
// which returns an error for Failed Jobs.
//
// Use this helper when the caller wants to make its own pass/fail decision
// from the Job's status (e.g., the validator orchestrator extracts the exit
// code from the underlying pod and treats both Complete and Failed Jobs as
// legitimate completions).
//
// Returns ErrCodeInternal if the initial Get or Watch call fails, or if the
// Job is deleted while being watched. Returns ErrCodeUnavailable when the
// resync List or its replacement Watch fails transiently while resuming a
// closed watch, and ErrCodeTimeout on context deadline exceeded. A watch
// closure alone never ends the wait; only the context deadline or a classified
// resync failure does.
//
// When the watch ends without the Job being terminal (routine on kube-apiserver
// --min-request-timeout expiry, rolling restarts, and LB drops), this resyncs
// via a field-selected List and re-establishes the watch from the current
// collection ResourceVersion, waiting through the closure rather than failing.
//
// Performs an initial Get to catch already-terminal Jobs, then uses the watch
// API for efficient monitoring.
func WaitForJobTerminal(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) (*batchv1.Job, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: Job may already be terminal.
	current, err := client.BatchV1().Jobs(namespace).Get(timeoutCtx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get Job", err)
	}
	if isJobTerminal(current) {
		return current, nil
	}

	watcher, err := client.BatchV1().Jobs(namespace).Watch(
		timeoutCtx,
		metav1.ListOptions{
			FieldSelector:   "metadata.name=" + name,
			ResourceVersion: current.ResourceVersion,
		},
	)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	// Closure form so a re-established watch (reassigning watcher below) stops
	// the current stream rather than leaking the original.
	defer func() { watcher.Stop() }()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			// The watch stream ended when the channel closes or the apiserver
			// emits a retryable 410 (it compacted past our ResourceVersion) —
			// routine on --min-request-timeout expiry, rolling restarts, and LB
			// drops. Resync the Job via a field-selected List, then re-establish
			// the watch and keep waiting rather than declaring failure; a watch
			// closure alone never ends the wait, only the context deadline or a
			// classified resync failure does.
			if !ok || isRetryableWatchError(event) {
				// If the parent context already expired, classify the
				// failure as a timeout rather than a generic recheck error.
				if ctxErr := timeoutCtx.Err(); ctxErr != nil {
					return nil, errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", ctxErr)
				}
				terminal, newWatcher, resumeErr := resumeJobWatch(timeoutCtx, client, namespace, name)
				if resumeErr != nil {
					if ctxErr := timeoutCtx.Err(); ctxErr != nil {
						return nil, errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", ctxErr)
					}
					return nil, resumeErr
				}
				if terminal != nil {
					return terminal, nil
				}
				slog.Debug("job watch closed before terminal state, re-established",
					keyNamespace, namespace, keyName, name)
				watcher.Stop()
				watcher = newWatcher
				continue
			}
			if event.Type == watch.Error {
				return nil, errors.Wrap(errors.ErrCodeInternal, "watch stream error",
					apierrors.FromObject(event.Object))
			}
			if event.Type == watch.Deleted {
				return nil, errors.New(errors.ErrCodeInternal, "Job was deleted before reaching terminal state")
			}
			job, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}
			if isJobTerminal(job) {
				return job, nil
			}
		}
	}
}

// isJobTerminal reports whether a Job has a terminal condition set
// (Complete=True or Failed=True). Unlike checkJobStatus this does not
// distinguish between Complete and Failed.
func isJobTerminal(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}

// checkJobStatus returns (true, nil) for Complete, (true, error) for Failed,
// and (false, nil) when the Job is still running.
func checkJobStatus(job *batchv1.Job) (bool, error) {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true, nil
		}
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true, errors.NewWithContext(errors.ErrCodeInternal, "job failed", map[string]any{
				keyNamespace: job.Namespace,
				keyName:      job.Name,
				keyReason:    condition.Reason,
				keyMessage:   condition.Message,
			})
		}
	}
	return false, nil
}
