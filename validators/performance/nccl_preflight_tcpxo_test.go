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

	"github.com/NVIDIA/aicr/pkg/recipe"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGKETCPXOPreflightApplies(t *testing.T) {
	tests := []struct {
		name        string
		variant     ncclVariant
		accelerator recipe.CriteriaAcceleratorType
		service     recipe.CriteriaServiceType
		want        bool
	}{
		{
			"default + H100 + GKE → check required",
			variantDefault, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, true,
		},
		{
			"default + H100 + EKS → not required (EFA, not TCPXO)",
			variantDefault, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceEKS, false,
		},
		{
			"NET + H100 + GKE → not required (GKE has no NET variant template)",
			variantNET, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceGKE, false,
		},
		{
			"default + GB200 + GKE → not required (no GKE GB200 template)",
			variantDefault, recipe.CriteriaAcceleratorGB200, recipe.CriteriaServiceGKE, false,
		},
		{
			"default + H100 + OKE → not required",
			variantDefault, recipe.CriteriaAcceleratorH100, recipe.CriteriaServiceOKE, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gkeTCPXOPreflightApplies(tt.variant, tt.accelerator, tt.service); got != tt.want {
				t.Errorf("gkeTCPXOPreflightApplies() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTailLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"fewer than n", "a\nb", 5, "a\nb"},
		{"exactly n", "a\nb\nc", 3, "a\nb\nc"},
		{"more than n keeps tail", "a\nb\nc\nd", 2, "c\nd"},
		{"single line", "only", 3, "only"},
		{"empty", "", 3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailLines(tt.in, tt.n); got != tt.want {
				t.Errorf("tailLines(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestCollectNCCLWorkerDiagnostics(t *testing.T) {
	const ns = "aicr-test"

	workerLabels := map[string]string{
		"jobset.sigs.k8s.io/jobset-name":        ncclTrainJobName,
		"jobset.sigs.k8s.io/replicatedjob-name": nodeJobName,
	}

	failedWorker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nccl-all-reduce-tj-node-0-0-abcde",
			Namespace: ns,
			Labels:    workerLabels,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			// tcpxo-daemon is a native sidecar (init container).
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "tcpxo-daemon",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Reason:   "Error",
						ExitCode: 137,
						Message:  "fastrak init failed",
					},
				},
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: nodeJobName,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "CrashLoopBackOff",
						Message: "back-off restarting",
					},
				},
			}},
		},
	}

	tests := []struct {
		name         string
		pods         []runtime.Object
		wantContains []string
	}{
		{
			name:         "no worker pods",
			pods:         nil,
			wantContains: []string{"no NCCL worker pods found"},
		},
		{
			name: "worker with failed and waiting containers",
			pods: []runtime.Object{failedWorker},
			wantContains: []string{
				failedWorker.Name,
				"phase=Failed",
				"container tcpxo-daemon: terminated reason=Error exitCode=137",
				"fastrak init failed",
				"container node: waiting reason=CrashLoopBackOff",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.pods...)
			got := collectNCCLWorkerDiagnostics(context.Background(), client, ns)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("diagnostics missing %q\nfull output:\n%s", want, got)
				}
			}
		})
	}
}
