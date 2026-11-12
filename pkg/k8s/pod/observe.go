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

// jobObserverMaxBackoff caps the growth of the retry delay while the observer's
// List or Watch keeps failing, so a persistent failure (an RBAC gap, an
// unavailable apiserver) costs a request roughly every half minute (up to
// ~36s once jitter is applied) rather than every jobWatchResumeBackoff for the
// whole wait.
const jobObserverMaxBackoff = 30 * time.Second

// jobObserverHealthyWatch is how long an established watch must stay open
// without delivering an event to count as healthy. A watch that is established
// and then dies sooner (an immediate error event or close) is a failure, so the
// retry delay keeps growing instead of resetting every time Watch succeeds.
const jobObserverHealthyWatch = 10 * time.Second

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
	failures := 0
	for ctx.Err() == nil {
		list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
			FieldSelector: terminalPodFieldSelector,
		})
		if err != nil {
			o.warn(ctx, "failed to list pods while observing Job", namespace, jobName, failures, err)
			o.backoff(ctx, failures)
			failures++
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
			o.warn(ctx, "failed to watch pods while observing Job", namespace, jobName, failures, err)
			o.backoff(ctx, failures)
			failures++
			continue
		}
		started := time.Now()
		events := o.consume(ctx, w, jobName, containerName)
		w.Stop()
		healthy := watchWasHealthy(events, time.Since(started))
		if healthy {
			failures = 0
		}
		o.backoff(ctx, failures)
		if !healthy {
			failures++
		}
	}
}

// watchWasHealthy reports whether a watch that has ended made meaningful
// progress: it delivered at least one event, or it stayed open for
// jobObserverHealthyWatch (a healthy idle watch). Only then does the failure
// streak reset.
func watchWasHealthy(events int, open time.Duration) bool {
	return events > 0 || open >= jobObserverHealthyWatch
}

// consume records events until the watch ends or ctx is done. It returns the
// number of events delivered before the watch ended, not counting the error
// event that ends it.
func (o *JobPodObserver) consume(ctx context.Context, w watch.Interface, jobName, containerName string) int {
	events := 0
	for {
		select {
		case <-ctx.Done():
			return events
		case event, ok := <-w.ResultChan():
			if !ok || event.Type == watch.Error {
				return events
			}
			events++
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

// backoff paces re-establishment so a stream that keeps ending cannot spin. The
// delay doubles with each consecutive failure up to jobObserverMaxBackoff.
func (o *JobPodObserver) backoff(ctx context.Context, failures int) {
	delay := observerRetryDelay(jobWatchResumeBackoff, failures)
	select {
	case <-ctx.Done():
	case <-time.After(wait.Jitter(delay, jobWatchResumeJitterFactor)):
	}
}

// observerRetryDelay returns base doubled once per consecutive failure, capped
// at jobObserverMaxBackoff.
func observerRetryDelay(base time.Duration, failures int) time.Duration {
	delay := base
	for i := 0; i < failures && delay < jobObserverMaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, jobObserverMaxBackoff)
}

// warn logs a non-context failure so a silent observer does not leave
// operators wondering why a deadline-killed check lacks its own message. Only
// the first failure of a streak warns; repeats are logged at debug so a
// persistent failure does not flood the log for the length of the wait.
func (o *JobPodObserver) warn(ctx context.Context, msg, namespace, jobName string, failures int, err error) {
	if ctx.Err() != nil {
		return
	}
	level := slog.LevelWarn
	if failures > 0 {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, msg, keyNamespace, namespace, keyJob, jobName, "error", err)
}
