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
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

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
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "job completion timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := timeoutCtx.Err(); ctxErr != nil {
					return errors.Wrap(errors.ErrCodeTimeout, "job completion timeout", ctxErr)
				}
				return errors.New(errors.ErrCodeInternal, "watch channel closed unexpectedly")
			}
			if event.Type == watch.Error {
				if statusErr, isErr := event.Object.(error); isErr {
					return errors.Wrap(errors.ErrCodeInternal, "watch stream error", statusErr)
				}
				return errors.New(errors.ErrCodeInternal, "watch stream error")
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
// Job is deleted while being watched. Returns ErrCodeTimeout on context
// deadline exceeded. Returns ErrCodeUnavailable if the watch channel closes
// without a terminal state being observed (after one re-Get fast-path retry).
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
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// If the parent context already expired, classify the
				// failure as a timeout rather than a generic recheck error.
				if ctxErr := timeoutCtx.Err(); ctxErr != nil {
					return nil, errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", ctxErr)
				}
				// Watch channel closed — re-check Job status directly before giving up.
				recheck, recheckErr := client.BatchV1().Jobs(namespace).Get(timeoutCtx, name, metav1.GetOptions{})
				if recheckErr != nil {
					if ctxErr := timeoutCtx.Err(); ctxErr != nil {
						return nil, errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", ctxErr)
					}
					return nil, errors.Wrap(errors.ErrCodeInternal,
						"watch closed and Job re-check failed", recheckErr)
				}
				if isJobTerminal(recheck) {
					return recheck, nil
				}
				return nil, errors.New(errors.ErrCodeUnavailable,
					"watch channel closed before Job reached terminal state")
			}
			if event.Type == watch.Error {
				if statusErr, isErr := event.Object.(error); isErr {
					return nil, errors.Wrap(errors.ErrCodeInternal, "watch stream error", statusErr)
				}
				return nil, errors.New(errors.ErrCodeInternal, "watch stream error")
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
			return true, errors.NewWithContext(errors.ErrCodeInternal, "job failed", map[string]interface{}{
				keyNamespace: job.Namespace,
				keyName:      job.Name,
				keyReason:    condition.Reason,
				keyMessage:   condition.Message,
			})
		}
	}
	return false, nil
}
