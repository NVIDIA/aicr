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

// GetPodForJob returns the best pod owned by the named Job, located via the
// standard `batch.kubernetes.io/job-name=<jobName>` label selector.
//
// Returns an ErrCodeNotFound StructuredError when the listing succeeds but
// matches zero pods (Job's pod has not yet been created or was deleted).
// Returns an ErrCodeInternal StructuredError if the List call itself fails.
func GetPodForJob(ctx context.Context, client kubernetes.Interface, namespace, jobName string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: jobNameLabel + "=" + jobName,
	})
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal, "failed to list pods for Job", err,
			map[string]any{keyNamespace: namespace, keyJob: jobName})
	}

	if len(pods.Items) == 0 {
		return nil, errors.NewWithContext(errors.ErrCodeNotFound, "pod for job not found",
			map[string]any{keyNamespace: namespace, keyJob: jobName})
	}

	selected := selectPodForJob(pods.Items)
	if selected == nil {
		return nil, errors.NewWithContext(errors.ErrCodeNotFound, "pod for job not found",
			map[string]any{keyNamespace: namespace, keyJob: jobName})
	}

	return selected, nil
}

func selectPodForJob(pods []corev1.Pod) *corev1.Pod {
	var newestNonFailed *corev1.Pod
	var newestFailed *corev1.Pod

	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			if isNewerPod(p, newestFailed) {
				newestFailed = p
			}
			continue
		}
		if isNewerPod(p, newestNonFailed) {
			newestNonFailed = p
		}
	}

	if newestNonFailed != nil {
		return newestNonFailed
	}
	return newestFailed
}

func isNewerPod(candidate, current *corev1.Pod) bool {
	return current == nil || candidate.CreationTimestamp.After(current.CreationTimestamp.Time)
}
