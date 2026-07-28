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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// isSkipError reports whether err came from validators.Skip. Skip wraps the
// unexported "skip" sentinel, so the rendered error always ends with ": skip".
func isSkipError(err error) bool {
	return err != nil && strings.HasSuffix(err.Error(), ": skip")
}

func TestVerifyNvidiaSMILogs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		logs    string
		wantErr string
	}{
		{
			name: "accepts legacy banner fields",
			logs: "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg,
		},
		{
			name: "accepts renamed banner fields",
			logs: "NVIDIA-SMI\nKMD Version: 580.65.06\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			// Representative table-banner layout of a renamed-field driver
			// branch (see issue #1667): single header row, pipe-delimited,
			// fields separated by padding rather than newlines.
			name: "accepts renamed banner in table layout",
			logs: "| NVIDIA-SMI 610.43.02              KMD Version: 610.43.02     CUDA UMD Version: 13.3     |\n" +
				gpuCheckSuccessMsg,
		},
		{
			name: "accepts mixed legacy and renamed banner fields",
			logs: "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			// The renamed fields are documented only via `nvidia-smi
			// --version` deprecation text, which spells them lowercase
			// ("KMD version"); no fixture pins the table banner's casing
			// (issue #1667), so matching is case-insensitive.
			name: "accepts lowercase renamed banner fields",
			logs: "NVIDIA-SMI\nKMD version: 580.65.06\nCUDA UMD version: 13.0\n" + gpuCheckSuccessMsg,
		},
		{
			name: "accepts uppercase legacy banner fields",
			logs: "NVIDIA-SMI\nDRIVER VERSION: 570.86.15\nCUDA VERSION: 12.8\n" + gpuCheckSuccessMsg,
		},
		{
			name:    "rejects logs missing both driver banner alternatives",
			logs:    "NVIDIA-SMI\nCUDA UMD Version: 13.0\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [Driver Version: or KMD Version:]",
		},
		{
			name:    "rejects logs missing both CUDA banner alternatives",
			logs:    "NVIDIA-SMI\nKMD Version: 580.65.06\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [CUDA Version: or CUDA UMD Version:]",
		},
		{
			name:    "separates multiple missing marker groups",
			logs:    "NVIDIA-SMI\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [Driver Version: or KMD Version:; CUDA Version: or CUDA UMD Version:]",
		},
		{
			name:    "rejects logs missing NVIDIA-SMI marker",
			logs:    "Driver Version: 570.86.15\nCUDA Version: 12.8\n" + gpuCheckSuccessMsg,
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [NVIDIA-SMI]",
		},
		{
			name:    "rejects logs missing success marker",
			logs:    "NVIDIA-SMI\nDriver Version: 570.86.15\nCUDA Version: 12.8\n",
			wantErr: "[INTERNAL] log verification failed for pod aicr-validation/nvidia-smi-verify-test: missing [" + gpuCheckSuccessMsg + "]",
		},
	}

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "aicr-validation", Name: "nvidia-smi-verify-test"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := verifyNvidiaSMILogs(tt.logs, pod)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyNvidiaSMILogs() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyNvidiaSMILogs() error = nil, want %q", tt.wantErr)
			}
			if err.Error() != tt.wantErr {
				t.Fatalf("verifyNvidiaSMILogs() error = %q, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestCheckNvidiaSMI_SkipScenarios covers the enumeration/disclosure paths
// that return before any pod is scheduled: no GPU nodes at all must give a
// different, more generic skip reason than every GPU node being cordoned
// (issue #1668 — a cordon narrows scope, it must never look identical to
// "nothing to check"), and non-GPU nodes must never count toward either the
// schedulable or cordoned tally.
func TestCheckNvidiaSMI_SkipScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		nodes       []runtime.Object
		wantErrSubs []string
	}{
		{
			name:        "no GPU nodes at all",
			nodes:       []runtime.Object{},
			wantErrSubs: []string{"no GPU nodes found in the cluster"},
		},
		{
			name: "all GPU nodes cordoned",
			nodes: []runtime.Object{
				cordon(gpuNode("cordoned-1", 8, -1)),
				cordon(gpuNode("cordoned-2", 8, -1)),
			},
			wantErrSubs: []string{"all 2 GPU node(s) are cordoned"},
		},
		{
			name: "cordoned GPU node plus unrelated non-GPU node",
			nodes: []runtime.Object{
				cordon(gpuNode("cordoned-1", 8, -1)),
				gpuNode("non-gpu-1", -1, -1),
			},
			wantErrSubs: []string{"all 1 GPU node(s) are cordoned"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := newDeploymentTestContext(t, tt.nodes, nil, nil)

			err := checkNvidiaSMI(ctx)
			if !isSkipError(err) {
				t.Fatalf("checkNvidiaSMI() error = %v, want a skip", err)
			}
			for _, sub := range tt.wantErrSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("checkNvidiaSMI() error = %v, want it to contain %q", err, sub)
				}
			}
		})
	}
}
