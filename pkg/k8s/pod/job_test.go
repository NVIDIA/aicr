// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
	"time"

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

				go func() {
					time.Sleep(10 * time.Millisecond)
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
