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
// The validator's delete-then-create Job pattern means an in-flight cleanup
// of a prior run can leave a terminating pod under the same label until
// kubelet drains it. To avoid reading from that orphan, pods with a
// DeletionTimestamp are filtered out and the youngest remaining pod (by
// CreationTimestamp) is returned — this matches the live Job's pod even
// when the apiserver lists the older one first.
//
// Returns an ErrCodeNotFound StructuredError when no non-terminating pod
// matches (the Job's pod has not yet been created, or all matches are
// terminating). Returns an ErrCodeInternal StructuredError if the List
// call itself fails.
func GetPodForJob(ctx context.Context, client kubernetes.Interface, namespace, jobName string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: jobNameLabel + "=" + jobName,
	})
	if err != nil {
		return nil, errors.WrapWithContext(errors.ErrCodeInternal, "failed to list pods for Job", err,
			map[string]any{keyNamespace: namespace, "job": jobName})
	}

	var best *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	if best == nil {
		return nil, errors.NewWithContext(errors.ErrCodeNotFound, "pod for job not found",
			map[string]any{keyNamespace: namespace, "job": jobName})
	}

	return best, nil
}
