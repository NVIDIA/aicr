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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
