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
	"testing"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
