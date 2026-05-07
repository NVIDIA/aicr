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
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

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

func TestWaitForTermination_PodDeletedFastPath(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset()

	err := pod.WaitForTermination(context.Background(), client, "default", "missing-pod")
	if err != nil {
		t.Fatalf("expected nil for already-deleted pod, got: %v", err)
	}
}

func TestWaitForTermination_PodAlreadyTerminal(t *testing.T) {
	tests := []struct {
		name  string
		phase corev1.PodPhase
	}{
		{"succeeded", corev1.PodSucceeded},
		{"failed", corev1.PodFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: tt.phase},
			}
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(p)

			if err := pod.WaitForTermination(context.Background(), client, "default", "test-pod"); err != nil {
				t.Errorf("expected nil for terminal pod, got: %v", err)
			}
		})
	}
}

func TestWaitForTermination_DeletedEvent(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)
	w := watch.NewFake()
	client.PrependWatchReactor("pods", fakeWatchReactor(w))

	go func() {
		// Unbuffered FakeWatcher channel: Delete blocks until the consumer
		// reads, so synchronization is automatic.
		w.Delete(p)
	}()

	if err := pod.WaitForTermination(context.Background(), client, "default", "test-pod"); err != nil {
		t.Errorf("expected nil on Deleted event, got: %v", err)
	}
}

func TestWaitForTermination_ModifiedToSucceeded(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default", ResourceVersion: "1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)
	w := watch.NewFake()
	client.PrependWatchReactor("pods", fakeWatchReactor(w))

	go func() {
		// Unbuffered FakeWatcher channel: Modify blocks until consumed.
		modified := p.DeepCopy()
		modified.ResourceVersion = "2"
		modified.Status.Phase = corev1.PodSucceeded
		w.Modify(modified)
	}()

	if err := pod.WaitForTermination(context.Background(), client, "default", "test-pod"); err != nil {
		t.Errorf("expected nil on phase=Succeeded, got: %v", err)
	}
}

func TestWaitForTermination_WatchClosureRetrySucceeds(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default", ResourceVersion: "1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)

	var attempts atomic.Int32
	first := watch.NewFake()
	second := watch.NewFake()
	client.PrependWatchReactor("pods", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		n := attempts.Add(1)
		switch n {
		case 1:
			return true, first, nil
		case 2:
			return true, second, nil
		default:
			return true, watch.NewFake(), nil
		}
	})

	// Close the first watcher quickly to trigger the retry path.
	go func() {
		time.Sleep(10 * time.Millisecond)
		first.Stop()
	}()
	// Send a terminal event on the second watcher after the retry occurs.
	go func() {
		time.Sleep(40 * time.Millisecond)
		modified := p.DeepCopy()
		modified.ResourceVersion = "2"
		modified.Status.Phase = corev1.PodFailed
		second.Modify(modified)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := pod.WaitForTermination(ctx, client, "default", "test-pod"); err != nil {
		t.Errorf("expected nil after watch retry, got: %v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("expected at least 2 watch attempts, got %d", got)
	}
}

func TestWaitForTermination_WatchClosureRetryFails(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)

	first := watch.NewFake()
	second := watch.NewFake()
	var attempts atomic.Int32
	client.PrependWatchReactor("pods", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		n := attempts.Add(1)
		switch n {
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
		time.Sleep(30 * time.Millisecond)
		second.Stop()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := pod.WaitForTermination(ctx, client, "default", "test-pod")
	if err == nil {
		t.Fatal("expected error after both watch attempts close without terminal state")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("error is not StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeUnavailable {
		t.Errorf("error code = %q, want %q", se.Code, errors.ErrCodeUnavailable)
	}
}

func TestWaitForTermination_ContextTimeout(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)
	w := watch.NewFake()
	client.PrependWatchReactor("pods", fakeWatchReactor(w))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := pod.WaitForTermination(ctx, client, "default", "test-pod")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("error is not StructuredError: %v", err)
	}
	if se.Code != errors.ErrCodeTimeout {
		t.Errorf("error code = %q, want %q", se.Code, errors.ErrCodeTimeout)
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
