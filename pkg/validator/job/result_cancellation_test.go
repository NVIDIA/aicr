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
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWaitFailureMessageAndHandleTimeout(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		code  errors.ErrorCode
		want  string
	}{
		{"deadline", errors.WrapCtxErr(context.DeadlineExceeded, errors.ErrCodeTimeout, "wait"), errors.ErrCodeTimeout, "timeout: validator did not complete within"},
		{"canceled", errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "wait"), errors.ErrCodeCanceled, "canceled: validation run was interrupted"},
		{"unavailable", errors.New(errors.ErrCodeUnavailable, "connection refused"), errors.ErrCodeUnavailable, "validation failed:"},
		// WaitForJobTerminal wraps its initial Get/Watch failures as internal
		// without consulting ctx, so cancellation sits below the outer code.
		{"initial Get canceled", errors.Wrap(errors.ErrCodeInternal, "failed to get Job", context.Canceled), errors.ErrCodeInternal, "canceled: validation run was interrupted"},
		{"nested structured cancellation", errors.Wrap(errors.ErrCodeUnavailable, "infrastructure failed", errors.New(errors.ErrCodeCanceled, "inner")), errors.ErrCodeUnavailable, "canceled: validation run was interrupted"},
		{"initial Get deadline stays timeout", errors.Wrap(errors.ErrCodeInternal, "failed to get Job", context.DeadlineExceeded), errors.ErrCodeInternal, "timeout: validator did not complete within"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorstest.WantReportedCode(t, tt.cause, tt.code)
			if msg := waitFailureMessage(tt.cause, time.Minute); !strings.HasPrefix(msg, tt.want) {
				t.Errorf("message=%q", msg)
			}
			for _, podState := range []string{"absent", "running", "no validator", "terminated"} {
				t.Run(podState, func(t *testing.T) {
					cs := fake.NewSimpleClientset()
					if podState != "absent" {
						status := corev1.ContainerStatus{Name: ValidatorContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
						if podState == "no validator" {
							status.Name = "sidecar"
						}
						if podState == "terminated" {
							status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Message: "terminated by SIGTERM"}}
						}
						_, err := cs.CoreV1().Pods("ns").Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", Labels: map[string]string{"batch.kubernetes.io/job-name": "job"}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}}}, metav1.CreateOptions{})
						if err != nil {
							t.Fatal(err)
						}
					}
					d := NewDeployer(Config{Clientset: cs, Namespace: "ns", Entry: catalog.ValidatorEntry{Name: "check", Phase: "deployment", Timeout: time.Minute}})
					d.jobName = "job"
					result := d.HandleTimeout(t.Context(), tt.cause)
					want := tt.want
					if podState == "terminated" {
						want = "terminated by SIGTERM"
					}
					if !strings.HasPrefix(result.TerminationMsg, want) {
						t.Errorf("message=%q want prefix %q", result.TerminationMsg, want)
					}
					if result.CTRFStatus() != ctrf.StatusOther {
						t.Errorf("status=%s", result.CTRFStatus())
					}
				})
			}
		})
	}
}
