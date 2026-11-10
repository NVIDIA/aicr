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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// terminalPodFieldSelector limits the observation to pods that can hold a
// terminated validator container: with restartPolicy Never, a pod whose
// container has terminated is Succeeded or Failed. It keeps the observer's
// traffic to the few pods that matter instead of every status change.
const terminalPodFieldSelector = "status.phase!=Pending,status.phase!=Running"

// JobPodObserver watches the pods of a Job and remembers the last one whose
// named container was seen Terminated, including the final object carried by a
// DELETED event. When the Job controller deletes a pod on activeDeadlineSeconds
// expiry the pod can no longer be listed, so this is the only way to read how
// the check ended.
type JobPodObserver struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	last *corev1.Pod
}

// ObserveJobPods starts watching the pods of the named Job until Stop is
// called or ctx ends. The watch is resumed from a fresh List whenever it ends,
// so a closed channel or an expired resource version never stops observation.
func ObserveJobPods(ctx context.Context, client kubernetes.Interface, namespace, jobName, containerName string) *JobPodObserver {
	ctx, cancel := context.WithCancel(ctx)
	o := &JobPodObserver{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(o.done)
		o.run(ctx, client, namespace, jobName, containerName)
	}()
	return o
}

// Stop ends the observation and returns the last pod seen with a terminated
// container, or nil when none was seen.
func (o *JobPodObserver) Stop() *corev1.Pod {
	o.cancel()
	<-o.done
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last
}

func (o *JobPodObserver) run(ctx context.Context, client kubernetes.Interface, namespace, jobName, containerName string) {
	selector := jobNameLabel + "=" + jobName
	for ctx.Err() == nil {
		list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
			FieldSelector: terminalPodFieldSelector,
		})
		if err != nil {
			o.warn(ctx, "failed to list pods while observing Job", namespace, jobName, err)
			o.backoff(ctx)
			continue
		}
		for i := range list.Items {
			o.record(&list.Items[i], jobName, containerName)
		}

		w, err := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
			LabelSelector:   selector,
			FieldSelector:   terminalPodFieldSelector,
			ResourceVersion: list.ResourceVersion,
		})
		if err != nil {
			o.warn(ctx, "failed to watch pods while observing Job", namespace, jobName, err)
			o.backoff(ctx)
			continue
		}
		o.consume(ctx, w, jobName, containerName)
		w.Stop()
		o.backoff(ctx)
	}
}

// consume records events until the watch ends or ctx is done.
func (o *JobPodObserver) consume(ctx context.Context, w watch.Interface, jobName, containerName string) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.ResultChan():
			if !ok || event.Type == watch.Error {
				return
			}
			if p, isPod := event.Object.(*corev1.Pod); isPod {
				o.record(p, jobName, containerName)
			}
		}
	}
}

// record keeps a copy of p when it belongs to the Job, has a terminated
// container, and is not older than the pod already held.
func (o *JobPodObserver) record(p *corev1.Pod, jobName, containerName string) {
	if p.Labels[jobNameLabel] != jobName || !HasTerminatedContainer(p, containerName) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.last == nil || !p.CreationTimestamp.Before(&o.last.CreationTimestamp) {
		o.last = p.DeepCopy()
	}
}

// backoff paces re-establishment so a stream that keeps ending cannot spin.
func (o *JobPodObserver) backoff(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(wait.Jitter(jobWatchResumeBackoff, jobWatchResumeJitterFactor)):
	}
}

// warn logs a non-context failure so a silent observer does not leave
// operators wondering why a deadline-killed check lacks its own message.
func (o *JobPodObserver) warn(ctx context.Context, msg, namespace, jobName string, err error) {
	if ctx.Err() != nil {
		return
	}
	slog.Warn(msg, keyNamespace, namespace, keyJob, jobName, "error", err)
}
