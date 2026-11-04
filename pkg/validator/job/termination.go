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

package job

import (
	"context"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

const jobNameLabel = "batch.kubernetes.io/job-name"

// WaitForPodTermination waits until all pods of the Job are terminal or absent,
// including pods in their deletion grace period and overlapping replacements.
// A deleted Job alone does not establish that its pods have stopped using RBAC.
// The supplied context bounds the entire wait; API failures remain errors.
func (d *Deployer) WaitForPodTermination(ctx context.Context) error {
	selector := jobNameLabel + "=" + d.jobName
	for {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "waiting for Job pod termination")
		}
		pods, err := d.config.Clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return errors.Wrap(apiErrorCode(ctx, err), "failed to list Job pods for termination", err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, "waiting for Job pod termination")
		}
		active := false
		for i := range pods.Items {
			if pods.Items[i].Status.Phase != corev1.PodSucceeded && pods.Items[i].Status.Phase != corev1.PodFailed {
				active = true
				break
			}
		}
		if !active {
			return nil
		}

		closed, err := d.waitForPodTerminationChange(ctx, selector, pods.ResourceVersion)
		if err != nil {
			return err
		}
		if closed {
			// Pace reconnects on a flapping stream. Re-list with a current
			// collection RV: an old pod's RV can already have been compacted.
			select {
			case <-ctx.Done():
				return errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting for Job pod termination")
			case <-time.After(defaults.K8sJobWatchResumeBackoff):
			}
		}
	}
}

// waitForPodTerminationChange watches from the List's collection RV so a
// transition between List and Watch is replayed. Every terminal/deleted event
// triggers a re-list to account for other pods, including replacements.
func (d *Deployer) waitForPodTerminationChange(ctx context.Context, selector, resourceVersion string) (closed bool, err error) {
	watcher, err := d.config.Clientset.CoreV1().Pods(d.config.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: selector, ResourceVersion: resourceVersion,
	})
	if err != nil {
		return false, errors.Wrap(apiErrorCode(ctx, err), "failed to watch Job pods for termination", err)
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "waiting for Job pod termination")
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return true, nil
			}
			if event.Type == watch.Error {
				cause := apierrors.FromObject(event.Object)
				if apierrors.IsResourceExpired(cause) || apierrors.IsGone(cause) {
					return true, nil
				}
				return false, errors.Wrap(apiErrorCode(ctx, cause), "Job pod termination watch error", cause)
			}
			p, ok := event.Object.(*corev1.Pod)
			if ok && (event.Type == watch.Deleted || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed) {
				return false, nil
			}
		}
	}
}
