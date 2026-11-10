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
	stderrors "errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// terminationWaitTimeout bounds waits that are expected to return on a watch
// event the test itself sends — hitting this bound means the wait never
// observed the event, not that the test is slow.
const terminationWaitTimeout = 2 * time.Second

// expiringCtxTimeout is the deliberately short deadline used by the tests that
// assert the timeout path: no terminal event is ever sent, so this bound is the
// behavior under test rather than a safety net.
const expiringCtxTimeout = 50 * time.Millisecond

// fakeWatchReactor returns a reactor function suitable for PrependWatchReactor
// that always serves the supplied watcher.
func fakeWatchReactor(w watch.Interface) k8stesting.WatchReactionFunc {
	return func(_ k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		return true, w, nil
	}
}

func batchCond(condType string) batchv1.JobCondition {
	return batchv1.JobCondition{
		Type:   batchv1.JobConditionType(condType),
		Status: corev1.ConditionTrue,
		Reason: "Test",
	}
}

// jobWithCondition builds the canonical "test-job" Job in the "default"
// namespace with the supplied conditions. Tests in this file all reuse the
// same name; the helper exists to keep the table readable.
func jobWithCondition(conds ...batchv1.JobCondition) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
		Status:     batchv1.JobStatus{Conditions: conds},
	}
}

// terminatingJobPod builds a Running pod of Job "job" in namespace "ns" that is
// already in its deletion grace period, the case a log/result pod selector
// skips but a termination wait must still count as active.
func terminatingJobPod(name string) *corev1.Pod {
	now := metav1.Now()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{batchv1.JobNameLabel: "job"}, DeletionTimestamp: &now},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestWaitForJobPodsTermination_IncludesTerminatingPods(t *testing.T) {
	for _, disposition := range []string{"succeeded", "failed", "deleted"} {
		t.Run(disposition, func(t *testing.T) {
			p := terminatingJobPod("old")
			// A completed replacement must not hide an older terminating pod.
			newer := terminatingJobPod("newer")
			newer.DeletionTimestamp = nil
			newer.Status.Phase = corev1.PodSucceeded
			cs := fake.NewSimpleClientset(p, newer)
			cs.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if got := a.(k8stesting.ListAction).GetListRestrictions().Labels.String(); got != batchv1.JobNameLabel+"=job" {
					t.Errorf("selector=%q", got)
				}
				obj, err := cs.Tracker().List(corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithKind("Pod"), "ns")
				if err == nil {
					obj.(*corev1.PodList).ResourceVersion = "123"
				}
				return true, obj, err
			})
			ready := make(chan *watch.RaceFreeFakeWatcher, 1)
			cs.PrependWatchReactor("pods", func(a k8stesting.Action) (bool, watch.Interface, error) {
				r := a.(k8stesting.WatchAction).GetWatchRestrictions()
				if r.ResourceVersion != "123" || r.Labels.String() != batchv1.JobNameLabel+"=job" {
					t.Errorf("watch restrictions=%+v", r)
				}
				w := watch.NewRaceFreeFake()
				ready <- w
				return true, w, nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), terminationWaitTimeout)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- pod.WaitForJobPodsTermination(ctx, cs, "ns", "job") }()
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

func TestWaitForJobPodsTermination_WaitsForAllPods(t *testing.T) {
	first, second := terminatingJobPod("first"), terminatingJobPod("replacement")
	cs := fake.NewSimpleClientset(first, second)
	ready := make(chan *watch.RaceFreeFakeWatcher, 2)
	cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewRaceFreeFake()
		ready <- w
		return true, w, nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), terminationWaitTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pod.WaitForJobPodsTermination(ctx, cs, "ns", "job") }()
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

func TestWaitForJobPodsTermination_DeadlineAndCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[timeout], func(t *testing.T) {
			cs := fake.NewSimpleClientset(terminatingJobPod("pod"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.ErrCodeCanceled
			if timeout {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, expiringCtxTimeout)
				defer release()
				want = errors.ErrCodeTimeout
			}
			cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
				if !timeout {
					cancel()
				}
				return true, watch.NewRaceFreeFake(), nil
			})
			errorstest.WantReportedCode(t, pod.WaitForJobPodsTermination(ctx, cs, "ns", "job"), want)
		})
	}
}

func TestWaitForJobPodsTermination_APIErrors(t *testing.T) {
	for _, stage := range []string{"list", "watch", "stream", "closed re-list", "expired re-list"} {
		t.Run(stage, func(t *testing.T) {
			cs := fake.NewSimpleClientset(terminatingJobPod("pod"))
			cause := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", stderrors.New("denied"))
			lists := 0
			cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				lists++
				if stage == "list" || lists > 1 {
					return true, nil, cause
				}
				return false, nil, nil
			})
			cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
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
			ctx, cancel := context.WithTimeout(t.Context(), terminationWaitTimeout)
			defer cancel()
			err := pod.WaitForJobPodsTermination(ctx, cs, "ns", "job")
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			if !apierrors.IsForbidden(err) {
				t.Errorf("API error lost: %v", err)
			}
		})
	}
}

// TestWaitForJobPodsTermination_RelistsExpiredWatch proves a Watch call that
// itself fails with 410 Expired or Gone re-lists instead of aborting the wait.
func TestWaitForJobPodsTermination_RelistsExpiredWatch(t *testing.T) {
	gone := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonGone, Message: "gone",
	}}
	for _, expired := range []error{apierrors.NewResourceExpired("too old resource version"), gone} {
		t.Run(string(apierrors.ReasonForError(expired)), func(t *testing.T) {
			p := terminatingJobPod("pod")
			cs := fake.NewSimpleClientset(p)
			watches := 0
			cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
				watches++
				if watches == 1 {
					done := p.DeepCopy()
					done.Status.Phase = corev1.PodSucceeded
					if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), done, "ns"); err != nil {
						t.Error(err)
					}
					return true, nil, expired
				}
				t.Error("watched again after the re-list found every pod terminal")
				return true, watch.NewRaceFreeFake(), nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), terminationWaitTimeout)
			defer cancel()
			if err := pod.WaitForJobPodsTermination(ctx, cs, "ns", "job"); err != nil {
				t.Fatalf("WaitForJobPodsTermination() = %v, want nil after re-list", err)
			}
		})
	}
}

func TestWaitForJobPodsTermination_ResyncsClosedWatch(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			p := terminatingJobPod("pod")
			cs := fake.NewSimpleClientset(p)
			watches := 0
			cs.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
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
			ctx, cancel := context.WithTimeout(t.Context(), terminationWaitTimeout)
			defer cancel()
			if err := pod.WaitForJobPodsTermination(ctx, cs, "ns", "job"); err != nil {
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

func TestWaitForJobTerminal(t *testing.T) {
	tests := []struct {
		name       string
		startObj   runtime.Object
		watchEvent runtime.Object // optional Modify event
		watchType  watch.EventType
		timeout    time.Duration
		ctxCancel  bool
		wantErr    bool
		wantCode   errors.ErrorCode
		wantPhase  string // "Complete" / "Failed" / "" if no expectation
	}{
		{
			name:      "succeeded fast path",
			startObj:  jobWithCondition(batchCond("Complete")),
			timeout:   1 * time.Second,
			wantPhase: "Complete",
		},
		{
			name:      "failed fast path returns job no error",
			startObj:  jobWithCondition(batchCond("Failed")),
			timeout:   1 * time.Second,
			wantPhase: "Failed",
		},
		{
			name:     "timeout while running",
			startObj: jobWithCondition(),
			timeout:  100 * time.Millisecond,
			wantErr:  true,
			wantCode: errors.ErrCodeTimeout,
		},
		{
			// Counterpart to "timeout while running": an already-canceled
			// context must report ErrCodeCanceled, not ErrCodeTimeout,
			// pinning the WaitForJobTerminal timeoutCtx.Done() split.
			name:      "canceled while running",
			startObj:  jobWithCondition(),
			timeout:   2 * time.Second,
			ctxCancel: true,
			wantErr:   true,
			wantCode:  errors.ErrCodeCanceled,
		},
		{
			name:       "becomes complete via watch",
			startObj:   jobWithCondition(),
			watchEvent: jobWithCondition(batchCond("Complete")),
			watchType:  watch.Modified,
			timeout:    2 * time.Second,
			wantPhase:  "Complete",
		},
		{
			name:       "becomes failed via watch",
			startObj:   jobWithCondition(),
			watchEvent: jobWithCondition(batchCond("Failed")),
			watchType:  watch.Modified,
			timeout:    2 * time.Second,
			wantPhase:  "Failed",
		},
		{
			name:       "deleted while running",
			startObj:   jobWithCondition(),
			watchEvent: jobWithCondition(),
			watchType:  watch.Deleted,
			timeout:    2 * time.Second,
			wantErr:    true,
			wantCode:   errors.ErrCodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(tt.startObj)
			if tt.watchEvent != nil {
				w := watch.NewFake()
				client.PrependWatchReactor("jobs", fakeWatchReactor(w))
				go func() {
					// FakeWatcher channel is unbuffered; the emit call blocks
					// until the consumer reads, providing automatic order.
					switch tt.watchType { //nolint:exhaustive // only Modified/Deleted exercised here; other event types fall through to the default Action path
					case watch.Modified:
						w.Modify(tt.watchEvent)
					case watch.Deleted:
						w.Delete(tt.watchEvent)
					default:
						w.Action(tt.watchType, tt.watchEvent)
					}
				}()
			}

			ctx := context.Background()
			if tt.ctxCancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			got, err := pod.WaitForJobTerminal(ctx, client, "default", "test-job", tt.timeout)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (job=%v)", got)
				}
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("error is not StructuredError: %v", err)
				}
				if se.Code != tt.wantCode {
					t.Errorf("error code = %q, want %q", se.Code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("expected non-nil job on success")
			}
			if tt.wantPhase != "" {
				found := false
				for _, c := range got.Status.Conditions {
					if string(c.Type) == tt.wantPhase {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("returned job missing condition %q: %+v", tt.wantPhase, got.Status.Conditions)
				}
			}
		})
	}
}

// TestWaitForJobTerminal_WatchClosureResumes proves the fix for issue #1966 on
// the terminal-wait path: a watch channel that closes without the Job being
// terminal (kube-apiserver --min-request-timeout expiry) is followed by a
// re-established watch that delivers the terminal event, and the observed Job is
// returned rather than ErrCodeUnavailable. Reverting the re-watch line fails it.
func TestWaitForJobTerminal_WatchClosureResumes(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default", ResourceVersion: "1"}}
	client := fake.NewSimpleClientset(job) //nolint:staticcheck

	var attempts atomic.Int32
	first := watch.NewFake()
	second := watch.NewFake()
	client.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		switch attempts.Add(1) {
		case 1:
			return true, first, nil
		case 2:
			return true, second, nil
		default:
			return true, watch.NewFake(), nil
		}
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		first.Stop()
	}()
	go func() {
		time.Sleep(40 * time.Millisecond)
		second.Modify(jobWithCondition(batchCond("Failed")))
	}()

	got, err := pod.WaitForJobTerminal(context.Background(), client, "default", "test-job", 2*time.Second)
	if err != nil {
		t.Fatalf("expected nil error after watch resume, got: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil terminal job after resume")
	}
	if n := attempts.Load(); n < 2 {
		t.Errorf("expected at least 2 watch attempts (resume), got %d", n)
	}
}

// TestWaitForJobTerminal_StaleObjectRVResyncsViaList is the terminal-wait mirror
// of the long-running-validator regression: a watch from the aged-out object RV
// 410s, so the resume must resync via a field-selected List and re-watch from
// the current collection RV. Regressing to the object RV would 410-loop until
// this test's deadline.
func TestWaitForJobTerminal_StaleObjectRVResyncsViaList(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default", ResourceVersion: "1"},
	})

	var lists, fromStale, fromFresh atomic.Int32
	client.PrependReactor("list", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return true, &batchv1.JobList{
			ListMeta: metav1.ListMeta{ResourceVersion: freshCollectionRV},
			Items:    []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default", ResourceVersion: "1"}}},
		}, nil
	})
	terminal := jobWithCondition(batchCond("Complete"))
	terminal.ResourceVersion = "101"
	client.PrependWatchReactor("jobs", staleRVWatchReactor(&fromStale, &fromFresh, terminal))

	got, err := pod.WaitForJobTerminal(context.Background(), client, "default", "test-job", 3*time.Second)
	if err != nil {
		t.Fatalf("expected terminal job via collection-RV watch, got: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil terminal job after List resync")
	}
	if lists.Load() == 0 {
		t.Error("expected a resync List after the stale-RV watch 410ed")
	}
	if fromFresh.Load() == 0 {
		t.Error("expected a watch re-established from the fresh collection RV")
	}
}

func TestWaitForJobTerminal_WatchError(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"}}
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	go func() {
		// Unbuffered FakeWatcher channel: Error blocks until consumed.
		watcher.Error(&metav1.Status{
			Status:  metav1.StatusFailure,
			Reason:  metav1.StatusReasonInternalError,
			Message: "synthetic watch error",
		})
	}()

	gotJob, err := pod.WaitForJobTerminal(context.Background(), client, "default", "j", 2*time.Second)
	if err == nil {
		t.Fatal("expected error from watch.Error event")
	}
	if gotJob != nil {
		t.Errorf("expected nil job on error, got %v", gotJob)
	}
	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeInternal {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeInternal)
	}
}

// TestWaitForJobTerminal_RetryableWatchErrorResumes mirrors the F1 fix on the
// terminal-wait path: a mid-stream 410 (Gone) watch.Error resumes the watch
// instead of aborting.
func TestWaitForJobTerminal_RetryableWatchErrorResumes(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default", ResourceVersion: "1"}}
	client := fake.NewSimpleClientset(job) //nolint:staticcheck

	var attempts atomic.Int32
	first := watch.NewFake()
	second := watch.NewFake()
	client.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		switch attempts.Add(1) {
		case 1:
			return true, first, nil
		case 2:
			return true, second, nil
		default:
			return true, watch.NewFake(), nil
		}
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		expired := apierrors.NewResourceExpired("compacted")
		first.Error(&expired.ErrStatus)
		time.Sleep(30 * time.Millisecond)
		second.Modify(jobWithCondition(batchCond("Complete")))
	}()

	got, err := pod.WaitForJobTerminal(context.Background(), client, "default", "test-job", 2*time.Second)
	if err != nil {
		t.Fatalf("expected nil error after retryable watch error resume, got: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil terminal job after resume")
	}
	if n := attempts.Load(); n < 2 {
		t.Errorf("expected at least 2 watch attempts (resume after 410 error), got %d", n)
	}
}

// TestWaitForJobTerminal_ResyncListCatchesTerminal covers the closure-race
// branch on the terminal-wait path: the Job goes terminal while the watch is
// down, so the resync List observes it and no watch is re-established.
func TestWaitForJobTerminal_ResyncListCatchesTerminal(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default", ResourceVersion: "1"},
	})

	terminal := jobWithCondition(batchCond("Complete"))
	terminal.ResourceVersion = "2"
	client.PrependReactor("list", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, &batchv1.JobList{
			ListMeta: metav1.ListMeta{ResourceVersion: "100"},
			Items:    []batchv1.Job{*terminal},
		}, nil
	})

	var watches atomic.Int32
	first := watch.NewFake()
	client.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		if watches.Add(1) == 1 {
			return true, first, nil
		}
		t.Error("watch must not be re-established once the resync List observes terminal")
		return true, watch.NewFake(), nil
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		first.Stop()
	}()

	got, err := pod.WaitForJobTerminal(context.Background(), client, "default", "test-job", 2*time.Second)
	if err != nil {
		t.Fatalf("expected nil when resync List catches terminal, got: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil terminal job from resync List")
	}
	if n := watches.Load(); n != 1 {
		t.Errorf("expected exactly 1 watch attempt (no rewatch), got %d", n)
	}
}

func TestWaitForJobTerminal_WatchClosedAfterTimeout(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"}}
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	go func() {
		time.Sleep(50 * time.Millisecond)
		watcher.Stop()
	}()

	_, err := pod.WaitForJobTerminal(context.Background(), client, "default", "j", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var sErr *errors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != errors.ErrCodeTimeout {
		t.Errorf("error code = %v, want %v", sErr.Code, errors.ErrCodeTimeout)
	}
}
