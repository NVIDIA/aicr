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

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestWaitForJobCompletion(t *testing.T) {
	tests := []struct {
		name       string
		job        *batchv1.Job
		cancel     bool
		timeout    time.Duration
		watchEvent *batchv1.Job // if non-nil, send this as a Modify event after brief delay
		wantErr    bool
	}{
		{
			name: "success via watch",
			job:  &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"}},
			watchEvent: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				}},
			},
			timeout: 5 * time.Second,
			wantErr: false,
		},
		{
			name: "failure via watch",
			job:  &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"}},
			watchEvent: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				}},
			},
			timeout: 5 * time.Second,
			wantErr: true,
		},
		{
			name:    "timeout",
			job:     &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"}},
			timeout: 100 * time.Millisecond,
			wantErr: true,
		},
		{
			name: "already complete",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				}},
			},
			timeout: 1 * time.Second,
			wantErr: false,
		},
		{
			name: "already failed",
			job: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
				Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				}},
			},
			timeout: 1 * time.Second,
			wantErr: true,
		},
		{
			name:    "context cancelled",
			job:     nil,
			cancel:  true,
			timeout: 5 * time.Second,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client *fake.Clientset //nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			if tt.job != nil {
				client = fake.NewSimpleClientset(tt.job) //nolint:staticcheck
			} else {
				client = fake.NewSimpleClientset() //nolint:staticcheck
			}

			if tt.watchEvent != nil {
				watcher := watch.NewFake()
				client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

				// FakeWatcher uses an unbuffered channel; Modify blocks until
				// WaitForJobCompletion's select reads, providing the
				// synchronization a fixed sleep was previously approximating.
				go func() {
					watcher.Modify(tt.watchEvent)
				}()
			}

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := pod.WaitForJobCompletion(ctx, client, "default", "test-job", tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("WaitForJobCompletion() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWaitForJobCompletion_WatchError(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"}}
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	go func() {
		// Unbuffered FakeWatcher channel: Error blocks until the consumer
		// reads, giving deterministic ordering without a wall-clock sleep.
		watcher.Error(&metav1.Status{
			Status:  metav1.StatusFailure,
			Reason:  metav1.StatusReasonInternalError,
			Message: "synthetic watch error",
		})
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second)
	if err == nil {
		t.Fatal("expected error from watch.Error event")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != aicrerrors.ErrCodeInternal {
		t.Errorf("error code = %v, want %v", sErr.Code, aicrerrors.ErrCodeInternal)
	}
}

// TestWaitForJobCompletion_WatchClosureResumes proves the fix for issue #1966:
// when the watch channel closes without the Job being terminal (as
// kube-apiserver does on --min-request-timeout expiry), WaitForJobCompletion
// re-establishes the watch and keeps waiting instead of returning
// ErrCodeUnavailable. Reverting the re-watch line makes this test fail.
func TestWaitForJobCompletion_WatchClosureResumes(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"}}
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

	// Close the first watcher to trigger the resume path.
	go func() {
		time.Sleep(10 * time.Millisecond)
		first.Stop()
	}()
	// Deliver the terminal event on the re-established (second) watcher.
	go func() {
		time.Sleep(40 * time.Millisecond)
		second.Modify(&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "2"},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			}},
		})
	}()

	if err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second); err != nil {
		t.Fatalf("expected nil after watch resume, got: %v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("expected at least 2 watch attempts (resume), got %d", got)
	}
}

const freshCollectionRV = "100"

// staleRVWatchReactor models a real apiserver's stale-ResourceVersion
// semantics: a watch requested from any RV other than freshCollectionRV (i.e.
// the aged-out object RV) succeeds at the call level but immediately delivers a
// 410 ERROR event, while a watch from the current collection RV becomes usable
// and delivers onFresh. It records how many watches came from each so a test can
// assert the resume used the collection RV, not the stale object RV.
func staleRVWatchReactor(fromStale, fromFresh *atomic.Int32, onFresh *batchv1.Job) k8stesting.WatchReactionFunc {
	return func(action k8stesting.Action) (bool, watch.Interface, error) {
		rv := action.(k8stesting.WatchActionImpl).WatchRestrictions.ResourceVersion
		fw := watch.NewFake()
		if rv == freshCollectionRV {
			fromFresh.Add(1)
			go func() { fw.Modify(onFresh) }()
		} else {
			fromStale.Add(1)
			go func() {
				expired := apierrors.NewResourceExpired("too old resource version: " + rv)
				fw.Error(&expired.ErrStatus)
			}()
		}
		return true, fw, nil
	}
}

// TestWaitForJobCompletion_StaleObjectRVResyncsViaList is the regression for the
// long-running-validator defect: a Job whose object ResourceVersion has aged out
// of the watch cache must resync via a field-selected List and re-watch from the
// current *collection* ResourceVersion. Watching from the stale object RV would
// 410 forever and degrade the wait to paced polling. If the fix regresses, the
// wait never re-establishes a usable watch and this test times out.
func TestWaitForJobCompletion_StaleObjectRVResyncsViaList(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})

	var lists atomic.Int32
	client.PrependReactor("list", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		// The resync List reports the still-non-terminal Job and the CURRENT
		// collection RV — the value a correct resume must watch from.
		return true, &batchv1.JobList{
			ListMeta: metav1.ListMeta{ResourceVersion: freshCollectionRV},
			Items:    []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"}}},
		}, nil
	})

	var fromStale, fromFresh atomic.Int32
	client.PrependWatchReactor("jobs", staleRVWatchReactor(&fromStale, &fromFresh, terminalJobRV("101", batchv1.JobComplete)))

	if err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 3*time.Second); err != nil {
		t.Fatalf("expected completion via collection-RV watch, got: %v", err)
	}
	if lists.Load() == 0 {
		t.Error("expected a resync List after the stale-RV watch 410ed")
	}
	if fromFresh.Load() == 0 {
		t.Error("expected a watch re-established from the fresh collection RV")
	}
}

// terminalJobRV builds the "j" Job at a given ResourceVersion carrying a single
// terminal condition. Distinct from the file-level jobWithCondition helper,
// which uses the "test-job" name and omits the ResourceVersion the resume tests
// need.
func terminalJobRV(rv string, condType batchv1.JobConditionType) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: rv},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: condType, Status: corev1.ConditionTrue},
		}},
	}
}

// TestWaitForJobCompletion_ResyncListCatchesTerminal covers the closure-race
// branch: the Job goes terminal while the watch is down, so the resync List
// observes the terminal (Complete) state and the wait returns without
// re-establishing a watch at all.
func TestWaitForJobCompletion_ResyncListCatchesTerminal(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})

	client.PrependReactor("list", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, &batchv1.JobList{
			ListMeta: metav1.ListMeta{ResourceVersion: freshCollectionRV},
			Items:    []batchv1.Job{*terminalJobRV("2", batchv1.JobComplete)},
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

	if err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second); err != nil {
		t.Fatalf("expected nil when resync List catches terminal, got: %v", err)
	}
	if got := watches.Load(); got != 1 {
		t.Errorf("expected exactly 1 watch attempt (no rewatch), got %d", got)
	}
}

// TestWaitForJobCompletion_ResyncListCatchesFailed asserts the Failed terminal
// disposition observed by the resync List is surfaced as an error, not swallowed.
func TestWaitForJobCompletion_ResyncListCatchesFailed(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})

	client.PrependReactor("list", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, &batchv1.JobList{
			ListMeta: metav1.ListMeta{ResourceVersion: freshCollectionRV},
			Items:    []batchv1.Job{*terminalJobRV("3", batchv1.JobFailed)},
		}, nil
	})

	first := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(first, nil))
	go func() {
		time.Sleep(10 * time.Millisecond)
		first.Stop()
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second)
	if err == nil {
		t.Fatal("expected job-failed error from resync-List-observed terminal state")
	}
	if _, ok := stderrors.AsType[*aicrerrors.StructuredError](err); !ok {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
}

// TestWaitForJobCompletion_ResumesTwice proves the loop resumes across more than
// one closure: two successive watches close before terminal, and only the third
// delivers the completion event.
func TestWaitForJobCompletion_ResumesTwice(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})

	var watches atomic.Int32
	first := watch.NewFake()
	second := watch.NewFake()
	third := watch.NewFake()
	client.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		switch watches.Add(1) {
		case 1:
			return true, first, nil
		case 2:
			return true, second, nil
		case 3:
			return true, third, nil
		default:
			return true, watch.NewFake(), nil
		}
	})

	go func() {
		time.Sleep(10 * time.Millisecond)
		first.Stop()
		time.Sleep(20 * time.Millisecond)
		second.Stop()
		time.Sleep(20 * time.Millisecond)
		third.Modify(terminalJobRV("4", batchv1.JobComplete))
	}()

	if err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 3*time.Second); err != nil {
		t.Fatalf("expected nil after two resumes, got: %v", err)
	}
	if got := watches.Load(); got < 3 {
		t.Errorf("expected at least 3 watch attempts (two resumes), got %d", got)
	}
}

// TestWaitForJobCompletion_RetryableWatchErrorResumes covers the F1 path: a
// mid-stream watch.Error carrying a 410 (Gone) status is treated as a resumable
// signal — the watch is re-established rather than the wait failing.
func TestWaitForJobCompletion_RetryableWatchErrorResumes(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})

	var watches atomic.Int32
	first := watch.NewFake()
	second := watch.NewFake()
	client.PrependWatchReactor("jobs", func(_ k8stesting.Action) (bool, watch.Interface, error) {
		switch watches.Add(1) {
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
		// Emit a 410 Gone as a watch.Error event rather than closing the channel.
		expired := apierrors.NewResourceExpired("compacted")
		first.Error(&expired.ErrStatus)
		time.Sleep(30 * time.Millisecond)
		second.Modify(terminalJobRV("2", batchv1.JobComplete))
	}()

	if err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second); err != nil {
		t.Fatalf("expected nil after retryable watch error resume, got: %v", err)
	}
	if got := watches.Load(); got < 2 {
		t.Errorf("expected at least 2 watch attempts (resume after 410 error), got %d", got)
	}
}

// TestWaitForJobCompletion_FatalWatchErrorReturns confirms a non-retryable
// watch.Error still aborts the wait with ErrCodeInternal rather than resuming.
func TestWaitForJobCompletion_FatalWatchErrorReturns(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})
	w := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(w, nil))

	go func() {
		time.Sleep(10 * time.Millisecond)
		internal := apierrors.NewInternalError(stderrors.New("boom"))
		w.Error(&internal.ErrStatus)
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second)
	if err == nil {
		t.Fatal("expected error from fatal watch stream error")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != aicrerrors.ErrCodeInternal {
		t.Errorf("error code = %v, want %v", sErr.Code, aicrerrors.ErrCodeInternal)
	}
}

// TestWaitForJobCompletion_ResumeReListTransientUnavailable pins F5: when the
// post-close resync List fails with a transient (non-context) error, the wait
// surfaces ErrCodeUnavailable — distinguishing an apiserver hiccup from the
// caller's own deadline.
func TestWaitForJobCompletion_ResumeReListTransientUnavailable(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&batchv1.Job{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default", ResourceVersion: "1"},
	})

	client.PrependReactor("list", "jobs", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver down")
	})

	w := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(w, nil))
	go func() {
		time.Sleep(10 * time.Millisecond)
		w.Stop()
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 2*time.Second)
	if err == nil {
		t.Fatal("expected error from transient resync List failure")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != aicrerrors.ErrCodeUnavailable {
		t.Errorf("error code = %v, want %v", sErr.Code, aicrerrors.ErrCodeUnavailable)
	}
}

func TestWaitForJobCompletion_WatchClosedAfterTimeout(t *testing.T) {
	t.Parallel()

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"}}
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	go func() {
		time.Sleep(50 * time.Millisecond)
		watcher.Stop()
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "j", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var sErr *aicrerrors.StructuredError
	if !stderrors.As(err, &sErr) {
		t.Fatalf("expected *errors.StructuredError, got %T", err)
	}
	if sErr.Code != aicrerrors.ErrCodeTimeout {
		t.Errorf("error code = %v, want %v", sErr.Code, aicrerrors.ErrCodeTimeout)
	}
}
