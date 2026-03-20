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

package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildNRIDeviceAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		gpuCount int
		wantDevs []string
	}{
		{
			name:     "8 GPUs (a3-megagpu-8g)",
			gpuCount: 8,
			wantDevs: []string{
				"/dev/nvidia0", "/dev/nvidia1", "/dev/nvidia2", "/dev/nvidia3",
				"/dev/nvidia4", "/dev/nvidia5", "/dev/nvidia6", "/dev/nvidia7",
				"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/dmabuf_import_helper",
			},
		},
		{
			name:     "4 GPUs",
			gpuCount: 4,
			wantDevs: []string{
				"/dev/nvidia0", "/dev/nvidia1", "/dev/nvidia2", "/dev/nvidia3",
				"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/dmabuf_import_helper",
			},
		},
		{
			name:     "1 GPU",
			gpuCount: 1,
			wantDevs: []string{
				"/dev/nvidia0",
				"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/dmabuf_import_helper",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildNRIDeviceAnnotation(tt.gpuCount)
			for _, dev := range tt.wantDevs {
				if !strings.Contains(got, "- path: "+dev) {
					t.Errorf("buildNRIDeviceAnnotation(%d) missing device %q\ngot:\n%s", tt.gpuCount, dev, got)
				}
			}
			// Verify no extra nvidia devices beyond the count.
			notWanted := fmt.Sprintf("/dev/nvidia%d", tt.gpuCount)
			if strings.Contains(got, notWanted) {
				t.Errorf("buildNRIDeviceAnnotation(%d) should not contain %q", tt.gpuCount, notWanted)
			}
		})
	}
}
