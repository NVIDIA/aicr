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
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

func TestImagePullTimeoutError(t *testing.T) {
	const selector = "job-name=populate"
	jobLabels := map[string]string{"job-name": "populate"}
	timeoutErr := errors.New(errors.ErrCodeTimeout, "wait timed out")

	deleting := pullTestPod("deleting", jobLabels, "ImagePullBackOff")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Finalizers = []string{"test/hold"}

	initBackOff := pullTestPod("init", jobLabels, "")
	initBackOff.Status.InitContainerStatuses = []v1.ContainerStatus{{
		Name:  "fetch",
		Image: testPullImage,
		State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}

	tests := []struct {
		name     string
		pod      *v1.Pod
		waitErr  error
		listFail bool
		wantErr  bool
	}{
		{"image pull back-off", pullTestPod("p", jobLabels, "ImagePullBackOff"), timeoutErr, false, true},
		{"pull attempt failed", pullTestPod("p", jobLabels, "ErrImagePull"), timeoutErr, false, true},
		{"invalid image name", pullTestPod("p", jobLabels, "InvalidImageName"), timeoutErr, false, true},
		{"init container back-off", initBackOff, timeoutErr, false, true},
		{"wait error is not a timeout", pullTestPod("p", jobLabels, "ImagePullBackOff"),
			errors.New(errors.ErrCodeUnavailable, "watch closed"), false, false},
		{"container creating", pullTestPod("p", jobLabels, "ContainerCreating"), timeoutErr, false, false},
		{"running", pullTestPod("p", jobLabels, ""), timeoutErr, false, false},
		{"pod being deleted", deleting, timeoutErr, false, false},
		{"pod outside the selector", pullTestPod("p", map[string]string{"job-name": "other"}, "ImagePullBackOff"),
			timeoutErr, false, false},
		{"pod list fails", pullTestPod("p", jobLabels, "ImagePullBackOff"), timeoutErr, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.pod)
			if tt.listFail {
				client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New(errors.ErrCodeUnavailable, "apiserver unavailable")
				})
			}
			err := imagePullTimeoutError(t.Context(), client, testWorkloadNamespace, selector, tt.waitErr)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
			for _, want := range []string{tt.pod.Name, testPullImage, "--image-pull-secret", "wait timed out"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
		})
	}
}
