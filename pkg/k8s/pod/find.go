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

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// jobNameLabel is the standard label Kubernetes applies to Pods created by a
// Job (batch/v1) starting in 1.27. It supersedes the legacy job-name label.
const jobNameLabel = "batch.kubernetes.io/job-name"

// GetPodForJob returns the live pod owned by the named Job, located via the
// standard `batch.kubernetes.io/job-name=<jobName>` label selector.
//
// Selection is tiered so the result matches what callers expect in every
// terminal state:
//
//  1. Skip pods being deleted (DeletionTimestamp != nil) — never inspect
//     an orphan from a delete-then-create flow.
//  2. Prefer a non-Failed pod (Running/Pending/Succeeded), youngest first
//     by CreationTimestamp — so a stale Failed pod cannot outrank the
//     live Job's pod under the same selector.
//  3. Fall back to a Failed pod, youngest first, when no non-Failed
//     candidate remains. Callers like ExtractResult intentionally inspect
//     a Failed pod to capture the exit code and termination reason after
//     WaitForCompletion observes a Job-level failure.
//
// Returns an ErrCodeNotFound StructuredError when every candidate is
// terminating (the Job's pod has not yet been created or every match is
// in the deletion grace window). Returns an ErrCodeInternal StructuredError
// if the List call itself fails.
func GetPodForJob(ctx context.Context, client kubernetes.Interface, namespace, jobName string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: jobNameLabel + "=" + jobName,
	})
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal, "failed to list pods for Job", err,
			map[string]any{keyNamespace: namespace, "job": jobName})
	}

	var (
		bestActive *corev1.Pod
		bestFailed *corev1.Pod
	)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			if bestFailed == nil || p.CreationTimestamp.After(bestFailed.CreationTimestamp.Time) {
				bestFailed = p
			}
			continue
		}
		if bestActive == nil || p.CreationTimestamp.After(bestActive.CreationTimestamp.Time) {
			bestActive = p
		}
	}
	if bestActive != nil {
		return bestActive, nil
	}
	if bestFailed != nil {
		return bestFailed, nil
	}
	return nil, errors.NewWithContext(errors.ErrCodeNotFound, "pod for job not found",
		map[string]any{keyNamespace: namespace, "job": jobName})
}
