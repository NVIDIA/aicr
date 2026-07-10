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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewestRunnablePod(t *testing.T) {
	now := time.Now()
	mk := func(name string, ageSec int, phase corev1.PodPhase, deleting bool) corev1.Pod {
		p := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				CreationTimestamp: metav1.NewTime(now.Add(-time.Duration(ageSec) * time.Second)),
			},
			Status: corev1.PodStatus{Phase: phase},
		}
		if deleting {
			ts := metav1.NewTime(now)
			p.DeletionTimestamp = &ts
		}
		return p
	}

	tests := []struct {
		name string
		pods []corev1.Pod
		want string // expected pod name, "" => nil
	}{
		{"empty", nil, ""},
		{
			name: "skips terminating and failed, picks youngest running",
			pods: []corev1.Pod{
				mk("old", 100, corev1.PodRunning, false),
				mk("young", 10, corev1.PodRunning, false),
				mk("deleting", 1, corev1.PodRunning, true),
				mk("failed", 1, corev1.PodFailed, false),
			},
			want: "young",
		},
		{
			name: "all terminating or failed yields nil",
			pods: []corev1.Pod{
				mk("deleting", 5, corev1.PodRunning, true),
				mk("failed", 5, corev1.PodFailed, false),
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newestRunnablePod(tt.pods)
			switch {
			case tt.want == "" && got != nil:
				t.Errorf("newestRunnablePod = %q, want nil", got.Name)
			case tt.want != "" && got == nil:
				t.Errorf("newestRunnablePod = nil, want %q", tt.want)
			case tt.want != "" && got.Name != tt.want:
				t.Errorf("newestRunnablePod = %q, want %q", got.Name, tt.want)
			}
		})
	}
}
