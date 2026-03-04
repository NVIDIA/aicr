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

func TestWaitForJobCompletion_Success(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	go func() {
		time.Sleep(50 * time.Millisecond)
		completedJob := job.DeepCopy()
		completedJob.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}
		watcher.Modify(completedJob)
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "test-job", 5*time.Second)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestWaitForJobCompletion_Failure(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	go func() {
		time.Sleep(50 * time.Millisecond)
		failedJob := job.DeepCopy()
		failedJob.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}
		watcher.Modify(failedJob)
	}()

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "test-job", 5*time.Second)
	if err == nil {
		t.Error("expected error for failed job")
	}
}

func TestWaitForJobCompletion_Timeout(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "default"},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(job)

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	err := pod.WaitForJobCompletion(context.Background(), client, "default", "test-job", 100*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestWaitForJobCompletion_ContextCancelled(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset()

	watcher := watch.NewFake()
	client.PrependWatchReactor("jobs", k8stesting.DefaultWatchReactor(watcher, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pod.WaitForJobCompletion(ctx, client, "default", "test-job", 5*time.Second)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}
