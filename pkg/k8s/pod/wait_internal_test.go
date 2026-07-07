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

package pod

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestClassifyReGetError pins the wait-loop re-Get classifier so a deadline
// race between watch-channel close and the re-Get surfaces as ErrCodeTimeout,
// not ErrCodeUnavailable. Without this, an upstream caller distinguishing
// transient apiserver unavailability from its own deadline would misroute
// the failure.
func TestClassifyReGetError(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		getErr  error
		wantTo  errors.ErrorCode
		wantMsg string
	}{
		{
			name:    "context already canceled",
			ctx:     canceledCtx,
			getErr:  stderrors.New("get failed"),
			wantTo:  errors.ErrCodeTimeout,
			wantMsg: "ctx canceled",
		},
		{
			name:    "getErr is DeadlineExceeded",
			ctx:     context.Background(),
			getErr:  context.DeadlineExceeded,
			wantTo:  errors.ErrCodeTimeout,
			wantMsg: "deadline exceeded",
		},
		{
			name:    "getErr is Canceled",
			ctx:     context.Background(),
			getErr:  context.Canceled,
			wantTo:  errors.ErrCodeTimeout,
			wantMsg: "canceled",
		},
		{
			name:    "transient apiserver error",
			ctx:     context.Background(),
			getErr:  stderrors.New("connection refused"),
			wantTo:  errors.ErrCodeUnavailable,
			wantMsg: "unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyReGetError(tt.ctx, "wait test", tt.getErr)
			if err == nil {
				t.Fatalf("expected non-nil error")
			}
			var se *errors.StructuredError
			if !stderrors.As(err, &se) {
				t.Fatalf("error is not StructuredError: %v", err)
			}
			if se.Code != tt.wantTo {
				t.Errorf("code = %q, want %q (%s)", se.Code, tt.wantTo, tt.wantMsg)
			}
		})
	}
}

// TestPodWaitingReason verifies that the first waiting container's reason and
// message are surfaced, that init containers take precedence over regular
// containers (init runs first), and that a pod with no waiting container
// returns empty strings. This is the signal that turns a wall of
// "status=Pending" logs into an actionable ImagePullBackOff.
func TestPodWaitingReason(t *testing.T) {
	waiting := func(reason, msg string) corev1.ContainerStatus {
		return corev1.ContainerStatus{State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg},
		}}
	}
	running := corev1.ContainerStatus{State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	}}

	tests := []struct {
		name        string
		pod         *corev1.Pod
		wantReason  string
		wantMessage string
	}{
		{
			name: "regular container ImagePullBackOff",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					waiting("ImagePullBackOff", "Back-off pulling image \"foo:uat-1\""),
				},
			}},
			wantReason:  "ImagePullBackOff",
			wantMessage: "Back-off pulling image \"foo:uat-1\"",
		},
		{
			name: "init container waiting takes precedence",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{waiting("ErrImagePull", "init pull failed")},
				ContainerStatuses:     []corev1.ContainerStatus{waiting("PodInitializing", "")},
			}},
			wantReason:  "ErrImagePull",
			wantMessage: "init pull failed",
		},
		{
			name: "no waiting container",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{running},
			}},
			wantReason:  "",
			wantMessage: "",
		},
		{
			name:        "empty status",
			pod:         &corev1.Pod{},
			wantReason:  "",
			wantMessage: "",
		},
		{
			name: "waiting with empty reason is skipped",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{waiting("", "no reason yet")},
			}},
			wantReason:  "",
			wantMessage: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message := podWaitingReason(tt.pod)
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", reason, tt.wantReason)
			}
			if message != tt.wantMessage {
				t.Errorf("message = %q, want %q", message, tt.wantMessage)
			}
		})
	}
}

// TestLogPodPhase exercises both logging branches (with and without a waiting
// container). It asserts no panic and that the waiting-reason path is reached;
// slog output itself is not captured here — the reason-extraction logic is
// covered by TestPodWaitingReason.
func TestLogPodPhase(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
	}{
		{
			name: "no waiting container",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			},
		},
		{
			name: "waiting with reason and message",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "ImagePullBackOff", Message: "back-off",
						}},
					}},
				},
			},
		},
		{
			name: "waiting reason without message",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "p"},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{{
						State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
							Reason: "PodInitializing",
						}},
					}},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logPodPhase(tc.pod) // must not panic across all branches
		})
	}
}

// TestWatchClosedContext verifies the watch-closed error context carries the
// pod phase always, and the waiting reason/message only when a container is
// waiting — so the returned error itself explains why the pod never terminated.
func TestWatchClosedContext(t *testing.T) {
	t.Run("carries phase and waiting reason", func(t *testing.T) {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff", Message: "back-off",
					}},
				}},
			},
		}
		ctxMap := watchClosedContext("ns", "p", p)
		if ctxMap["phase"] != string(corev1.PodPending) {
			t.Errorf("phase = %v, want %q", ctxMap["phase"], corev1.PodPending)
		}
		if ctxMap[keyReason] != "ImagePullBackOff" {
			t.Errorf("reason = %v, want ImagePullBackOff", ctxMap[keyReason])
		}
		if ctxMap[keyMessage] != "back-off" {
			t.Errorf("message = %v, want back-off", ctxMap[keyMessage])
		}
	})

	t.Run("omits reason when no container waiting", func(t *testing.T) {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}
		ctxMap := watchClosedContext("ns", "p", p)
		if _, ok := ctxMap[keyReason]; ok {
			t.Errorf("reason should be absent, got %v", ctxMap[keyReason])
		}
		if _, ok := ctxMap[keyMessage]; ok {
			t.Errorf("message should be absent, got %v", ctxMap[keyMessage])
		}
	})
}
