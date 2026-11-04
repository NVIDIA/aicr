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
	stderrors "errors"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func terminatingJobPod(name string) *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{jobNameLabel: "job"}, DeletionTimestamp: &now},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestWaitForPodTerminationIncludesTerminatingPods(t *testing.T) {
	for _, disposition := range []string{"succeeded", "failed", "deleted"} {
		t.Run(disposition, func(t *testing.T) {
			p := terminatingJobPod("old")
			// A completed replacement must not hide an older terminating pod.
			newer := terminatingJobPod("newer")
			newer.DeletionTimestamp = nil
			newer.Status.Phase = corev1.PodSucceeded
			cs := fake.NewSimpleClientset(p, newer)
			cs.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				if got := a.(clienttesting.ListAction).GetListRestrictions().Labels.String(); got != jobNameLabel+"=job" {
					t.Errorf("selector=%q", got)
				}
				obj, err := cs.Tracker().List(corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithKind("Pod"), "ns")
				if err == nil {
					obj.(*corev1.PodList).ResourceVersion = "123"
				}
				return true, obj, err
			})
			ready := make(chan *watch.RaceFreeFakeWatcher, 1)
			cs.PrependWatchReactor("pods", func(a clienttesting.Action) (bool, watch.Interface, error) {
				r := a.(clienttesting.WatchAction).GetWatchRestrictions()
				if r.ResourceVersion != "123" || r.Labels.String() != jobNameLabel+"=job" {
					t.Errorf("watch restrictions=%+v", r)
				}
				w := watch.NewRaceFreeFake()
				ready <- w
				return true, w, nil
			})
			d := &Deployer{config: Config{Clientset: cs, Namespace: "ns"}, jobName: "job"}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- d.WaitForPodTermination(ctx) }()
			var w *watch.RaceFreeFakeWatcher
			select {
			case w = <-ready:
			case err := <-done:
				t.Fatalf("terminating Running pod was mistaken for absent: %v", err)
			case <-ctx.Done():
				t.Fatal("wait did not open a watch")
			}
			select {
			case err := <-done:
				t.Fatalf("wait returned before terminal/deleted observation: %v", err)
			default:
			}
			if disposition == "deleted" {
				if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "ns", p.Name); err != nil {
					t.Fatal(err)
				}
				w.Delete(p)
			} else {
				p = p.DeepCopy()
				p.Status.Phase = corev1.PodSucceeded
				if disposition == "failed" {
					p.Status.Phase = corev1.PodFailed
				}
				if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, "ns"); err != nil {
					t.Fatal(err)
				}
				w.Modify(p)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("wait did not observe terminal/deleted pod")
			}
		})
	}
}

func TestWaitForPodTerminationWaitsForAllPods(t *testing.T) {
	first, second := terminatingJobPod("first"), terminatingJobPod("replacement")
	cs := fake.NewSimpleClientset(first, second)
	ready := make(chan *watch.RaceFreeFakeWatcher, 2)
	cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
		w := watch.NewRaceFreeFake()
		ready <- w
		return true, w, nil
	})
	d := &Deployer{config: Config{Clientset: cs, Namespace: "ns"}, jobName: "job"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.WaitForPodTermination(ctx) }()
	for _, p := range []*corev1.Pod{first, second} {
		var w *watch.RaceFreeFakeWatcher
		select {
		case w = <-ready:
		case err := <-done:
			t.Fatalf("returned with active pod %s: %v", p.Name, err)
		case <-ctx.Done():
			t.Fatal("wait did not watch remaining pod")
		}
		if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "ns", p.Name); err != nil {
			t.Fatal(err)
		}
		w.Delete(p)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("wait did not complete")
	}
}

func TestWaitForPodTerminationDeadlineAndCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[timeout], func(t *testing.T) {
			cs := fake.NewSimpleClientset(terminatingJobPod("pod"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.ErrCodeCanceled
			if timeout {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 50*time.Millisecond)
				defer release()
				want = errors.ErrCodeTimeout
			}
			cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
				if !timeout {
					cancel()
				}
				return true, watch.NewRaceFreeFake(), nil
			})
			d := &Deployer{config: Config{Clientset: cs, Namespace: "ns"}, jobName: "job"}
			errorstest.WantReportedCode(t, d.WaitForPodTermination(ctx), want)
		})
	}
}

func TestWaitForPodTerminationAPIErrors(t *testing.T) {
	for _, stage := range []string{"list", "watch", "stream", "closed re-list", "expired re-list"} {
		t.Run(stage, func(t *testing.T) {
			cs := fake.NewSimpleClientset(terminatingJobPod("pod"))
			cause := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", stderrors.New("denied"))
			lists := 0
			cs.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
				lists++
				if stage == "list" || lists > 1 {
					return true, nil, cause
				}
				return false, nil, nil
			})
			cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
				if stage == "watch" {
					return true, nil, cause
				}
				w := watch.NewRaceFreeFake()
				switch stage {
				case "closed re-list":
					w.Stop()
				case "expired re-list":
					w.Error(&metav1.Status{Reason: metav1.StatusReasonExpired, Code: 410})
				default:
					w.Error(&cause.ErrStatus)
				}
				return true, w, nil
			})
			d := &Deployer{config: Config{Clientset: cs, Namespace: "ns"}, jobName: "job"}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := d.WaitForPodTermination(ctx)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if !apierrors.IsForbidden(err) {
				t.Errorf("API error lost: %v", err)
			}
		})
	}
}

func TestWaitForPodTerminationResyncsClosedWatch(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			p := terminatingJobPod("pod")
			cs := fake.NewSimpleClientset(p)
			watches := 0
			cs.PrependWatchReactor("pods", func(clienttesting.Action) (bool, watch.Interface, error) {
				watches++
				w := watch.NewRaceFreeFake()
				p = p.DeepCopy()
				p.Status.Phase = phase
				if watches == 2 {
					p.Status.Phase = corev1.PodSucceeded
				}
				if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, "ns"); err != nil {
					t.Error(err)
				}
				w.Stop()
				return true, w, nil
			})
			d := &Deployer{config: Config{Clientset: cs, Namespace: "ns"}, jobName: "job"}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := d.WaitForPodTermination(ctx); err != nil {
				t.Fatal(err)
			}
			want := 1
			if phase == corev1.PodRunning {
				want = 2
			}
			if watches != want {
				t.Errorf("watches=%d want %d", watches, want)
			}
		})
	}
}
