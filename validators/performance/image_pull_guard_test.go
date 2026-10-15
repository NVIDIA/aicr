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

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testPullImage = "mirror.example.com/nvidia/ai-dynamo/vllm-runtime:1.0.0"

// pullTestPod returns a pod in the workload namespace whose main container is
// waiting with reason. An empty reason leaves the container running.
func pullTestPod(name string, labels map[string]string, reason string) *v1.Pod {
	state := v1.ContainerState{Running: &v1.ContainerStateRunning{}}
	if reason != "" {
		state = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: reason, Message: "pull access denied"}}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testWorkloadNamespace, Labels: labels},
		Status: v1.PodStatus{ContainerStatuses: []v1.ContainerStatus{
			{Name: "main", Image: testPullImage, State: state},
		}},
	}
}

// blockUntilCanceled is a wait that only returns once the guard cancels it,
// and fails the test if that does not happen promptly.
func blockUntilCanceled(t *testing.T) func(context.Context) error {
	t.Helper()
	return func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "wait canceled", ctx.Err())
		case <-time.After(10 * time.Second):
			t.Error("image pull guard did not cancel the wait")
			return errors.New(errors.ErrCodeTimeout, "wait not canceled")
		}
	}
}

func TestImagePullError(t *testing.T) {
	deleting := pullTestPod("deleting", nil, "ImagePullBackOff")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}

	initStuck := pullTestPod("init", nil, "")
	initStuck.Status.InitContainerStatuses = []v1.ContainerStatus{{
		Name:  "fetch",
		Image: testPullImage,
		State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}

	tests := []struct {
		name    string
		pod     *v1.Pod
		wantErr bool
	}{
		{"image pull back-off", pullTestPod("p", nil, "ImagePullBackOff"), true},
		{"invalid image name", pullTestPod("p", nil, "InvalidImageName"), true},
		{"init container back-off", initStuck, true},
		{"first pull attempt failed", pullTestPod("p", nil, "ErrImagePull"), false},
		{"container creating", pullTestPod("p", nil, "ContainerCreating"), false},
		{"running", pullTestPod("p", nil, ""), false},
		{"pod being deleted", deleting, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := imagePullError(tt.pod)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			for _, want := range []string{tt.pod.Name, testPullImage, "--image-pull-secret"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}

func TestWaitFailFastOnImagePull(t *testing.T) {
	const selector = "job-name=populate"
	jobLabels := map[string]string{"job-name": "populate"}

	t.Run("wait result returned when no pod is stuck", func(t *testing.T) {
		client := fake.NewClientset(pullTestPod("ok", jobLabels, ""))
		waitErr := errors.New(errors.ErrCodeUnavailable, "watch closed")
		got := waitFailFastOnImagePull(t.Context(), client, testWorkloadNamespace, selector,
			func(context.Context) error { return waitErr })
		if got != waitErr { //nolint:errorlint // identity check: the wait's error must pass through unchanged
			t.Fatalf("got %v, want the wait's error unchanged", got)
		}
	})

	t.Run("wait success", func(t *testing.T) {
		client := fake.NewClientset(pullTestPod("ok", jobLabels, ""))
		if err := waitFailFastOnImagePull(t.Context(), client, testWorkloadNamespace, selector,
			func(context.Context) error { return nil }); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("pod already stuck fails fast", func(t *testing.T) {
		client := fake.NewClientset(pullTestPod("stuck", jobLabels, "ImagePullBackOff"))
		err := waitFailFastOnImagePull(t.Context(), client, testWorkloadNamespace, selector, blockUntilCanceled(t))
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		if !strings.Contains(err.Error(), testPullImage) {
			t.Errorf("error %q does not name the image", err.Error())
		}
	})

	t.Run("pod becomes stuck during the wait", func(t *testing.T) {
		client := fake.NewClientset()
		fw := watch.NewFake()
		client.PrependWatchReactor("pods", k8stesting.DefaultWatchReactor(fw, nil))
		wait := blockUntilCanceled(t)
		err := waitFailFastOnImagePull(t.Context(), client, testWorkloadNamespace, selector,
			func(ctx context.Context) error {
				fw.Add(pullTestPod("stuck", jobLabels, ""))
				fw.Modify(pullTestPod("stuck", jobLabels, "ImagePullBackOff"))
				return wait(ctx)
			})
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	})

	t.Run("closed watch re-lists", func(t *testing.T) {
		client := fake.NewClientset()
		client.PrependWatchReactor("pods", func(k8stesting.Action) (bool, watch.Interface, error) {
			if err := client.Tracker().Add(pullTestPod("stuck", jobLabels, "ImagePullBackOff")); err != nil {
				t.Errorf("add stuck pod: %v", err)
			}
			fw := watch.NewFake()
			fw.Stop()
			return true, fw, nil
		})
		err := waitFailFastOnImagePull(t.Context(), client, testWorkloadNamespace, selector, blockUntilCanceled(t))
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
	})

	t.Run("list failure leaves the wait in charge", func(t *testing.T) {
		client := fake.NewClientset()
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New(errors.ErrCodeUnavailable, "apiserver unavailable")
		})
		if err := waitFailFastOnImagePull(t.Context(), client, testWorkloadNamespace, selector,
			func(context.Context) error { return nil }); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestWatchForImagePullFailure_SelectorScope(t *testing.T) {
	client := fake.NewClientset(pullTestPod("other", map[string]string{"job-name": "other"}, "ImagePullBackOff"))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := watchForImagePullFailure(ctx, client, testWorkloadNamespace, "job-name=populate"); err != nil {
		t.Fatalf("pod outside the selector reported: %v", err)
	}
}
