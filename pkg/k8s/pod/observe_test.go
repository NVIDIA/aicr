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

package pod_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func observedPod(name string, state corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			Labels: map[string]string{"batch.kubernetes.io/job-name": "job"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "validator", State: state}}},
	}
}

// eventuallyObserved polls until the observer holds a pod named want.
func eventuallyObserved(t *testing.T, o *pod.JobPodObserver, want string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := o.LastForTest(); got != nil && got.Name == want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("observer never held pod %s (has %v)", want, o.LastForTest())
	return nil
}

func TestJobPodObserver(t *testing.T) {
	restore := pod.SetJobWatchResumeBackoffForTest(time.Millisecond)
	defer restore()

	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Message: "terminated by SIGTERM"}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}

	t.Run("terminated pod already present is found by the initial list", func(t *testing.T) {
		cs := fake.NewSimpleClientset(observedPod("a", done))
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		eventuallyObserved(t, o, "a")
		if got := o.Stop(); got == nil || got.Name != "a" {
			t.Errorf("Stop() = %v, want pod a", got)
		}
	})

	t.Run("running pod and other job's pod are not recorded", func(t *testing.T) {
		other := observedPod("x", done)
		other.Labels["batch.kubernetes.io/job-name"] = "other"
		cs := fake.NewSimpleClientset(observedPod("a", running), other)
		listed := make(chan struct{})
		var once sync.Once
		cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			fields := action.(k8stesting.ListAction).GetListRestrictions().Fields.String()
			if fields != "status.phase!=Pending,status.phase!=Running" {
				t.Errorf("list field selector = %q, want the terminal-phase selector", fields)
			}
			once.Do(func() { close(listed) })
			return false, nil, nil
		})
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		defer o.Stop()
		select {
		case <-listed:
		case <-time.After(5 * time.Second):
			t.Fatal("observer never listed pods")
		}
		// Stop joins the observer goroutine, which records the listed pods
		// before it checks for cancellation.
		if got := o.Stop(); got != nil {
			t.Errorf("Stop() = %s, want none", got.Name)
		}
	})

	t.Run("deletion keeps the last terminated status", func(t *testing.T) {
		fakeWatch := watch.NewFake()
		cs := fake.NewSimpleClientset()
		cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			return true, fakeWatch, nil
		})
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		defer o.Stop()
		// The fake watch is unbuffered; Modify blocks until the observer reads.
		fakeWatch.Modify(observedPod("a", running))
		fakeWatch.Modify(observedPod("a", done))
		fakeWatch.Delete(observedPod("a", done))
		got := eventuallyObserved(t, o, "a")
		if got.Status.ContainerStatuses[0].State.Terminated.Message != "terminated by SIGTERM" {
			t.Errorf("terminated status lost: %+v", got.Status)
		}
	})

	t.Run("closed watch is re-established and keeps observing", func(t *testing.T) {
		first, second := watch.NewFake(), watch.NewFake()
		watches := make(chan watch.Interface, 2)
		watches <- first
		watches <- second
		cs := fake.NewSimpleClientset()
		cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			select {
			case w := <-watches:
				return true, w, nil
			default:
				return true, watch.NewFake(), nil
			}
		})
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		defer o.Stop()
		first.Stop()
		second.Add(observedPod("b", done))
		eventuallyObserved(t, o, "b")
	})

	t.Run("expired watch is re-listed and keeps observing", func(t *testing.T) {
		expired := watch.NewFake()
		watches := make(chan watch.Interface, 1)
		watches <- expired
		cs := fake.NewSimpleClientset()
		cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			select {
			case w := <-watches:
				return true, w, nil
			default:
				return true, watch.NewFake(), nil
			}
		})
		lists := 0
		cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			lists++
			if lists == 1 {
				return true, &corev1.PodList{}, nil
			}
			return true, &corev1.PodList{Items: []corev1.Pod{*observedPod("b", done)}}, nil
		})
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		defer o.Stop()
		// The unbuffered fake blocks until the observer reads the 410.
		expired.Error(&apierrors.NewResourceExpired("too old resource version").ErrStatus)
		eventuallyObserved(t, o, "b")
	})

	t.Run("repeated list failures are retried until they clear", func(t *testing.T) {
		cs := fake.NewSimpleClientset(observedPod("a", done))
		failures := 0
		cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			if failures < 3 {
				failures++
				return true, nil, context.DeadlineExceeded
			}
			return false, nil, nil
		})
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		defer o.Stop()
		eventuallyObserved(t, o, "a")
	})

	t.Run("canceled context stops observation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		o := pod.ObserveJobPods(ctx, fake.NewSimpleClientset(), "ns", "job", "validator")
		cancel()
		done := make(chan struct{})
		go func() { o.Stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop did not return after context cancellation")
		}
	})
}

func TestObserverRetryDelay(t *testing.T) {
	const base = 2 * time.Second
	maxDelay := pod.JobObserverMaxBackoffForTest

	tests := []struct {
		name     string
		failures int
		want     time.Duration
	}{
		{"no failures uses the base", 0, base},
		{"first failure doubles", 1, 2 * base},
		{"grows with each consecutive failure", 3, 8 * base},
		{"capped at the maximum", 4, maxDelay},
		{"stays capped on long streaks", 1000, maxDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pod.ObserverRetryDelayForTest(base, tt.failures); got != tt.want {
				t.Errorf("delay(%v, %d) = %v, want %v", base, tt.failures, got, tt.want)
			}
		})
	}
}

func TestWatchWasHealthy(t *testing.T) {
	healthyIdle := pod.JobObserverHealthyWatchForTest

	tests := []struct {
		name   string
		events int
		open   time.Duration
		want   bool
	}{
		{"immediate end without events is a failure", 0, 0, false},
		{"end just under the threshold is a failure", 0, healthyIdle - time.Nanosecond, false},
		{"idle watch open past the threshold is healthy", 0, healthyIdle, true},
		{"one event then immediate close is progress", 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pod.WatchWasHealthyForTest(tt.events, tt.open); got != tt.want {
				t.Errorf("watchWasHealthy(%d, %v) = %v, want %v", tt.events, tt.open, got, tt.want)
			}
		})
	}
}

// watchesOpenedWhenEnding counts how many watches the observer opens in a
// virtual minute when every watch ends as soon as it is established, after
// delivering the given events. It uses the production 2s base delay, so it
// must not run in parallel.
func watchesOpenedWhenEnding(t *testing.T, events ...watch.Event) int32 {
	t.Helper()
	defer pod.SetJobWatchResumeBackoffForTest(2 * time.Second)()
	var opened atomic.Int32
	synctest.Test(t, func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			opened.Add(1)
			w := watch.NewFakeWithChanSize(len(events)+1, false)
			for _, e := range events {
				w.Action(e.Type, e.Object)
			}
			w.Stop()
			return true, w, nil
		})
		o := pod.ObserveJobPods(t.Context(), cs, "ns", "job", "validator")
		time.Sleep(time.Minute)
		o.Stop()
	})
	return opened.Load()
}

// TestJobPodObserverImmediateWatchEndKeepsBackingOff covers a watch that is
// established and then ends at once: the retry delay must keep growing rather
// than reset each time Watch succeeds.
func TestJobPodObserverImmediateWatchEndKeepsBackingOff(t *testing.T) {
	t.Run("immediate error event backs off", func(t *testing.T) {
		errEvent := watch.Event{Type: watch.Error, Object: &apierrors.NewResourceExpired("too old resource version").ErrStatus}
		// With delays of ~2s, 4s, 8s, 16s, 30s a virtual minute opens about 5
		// watches; a delay stuck at ~2s would open about 25.
		if got := watchesOpenedWhenEnding(t, errEvent); got > 8 {
			t.Errorf("watches opened in a virtual minute = %d, want the retry delay to grow (<= 8)", got)
		}
	})

	t.Run("immediate close backs off", func(t *testing.T) {
		if got := watchesOpenedWhenEnding(t); got > 8 {
			t.Errorf("watches opened in a virtual minute = %d, want the retry delay to grow (<= 8)", got)
		}
	})

	t.Run("event then close resets and stays at the base delay", func(t *testing.T) {
		podEvent := watch.Event{Type: watch.Added, Object: observedPod("a", corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})}
		if got := watchesOpenedWhenEnding(t, podEvent); got < 20 {
			t.Errorf("watches opened in a virtual minute = %d, want the base delay each time (>= 20)", got)
		}
	})
}
