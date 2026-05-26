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

package mirror

import (
	"bytes"
	"strings"
	"testing"
)

func TestRenderHauler(t *testing.T) {
	tests := []struct {
		name        string
		list        *MirrorList
		wantErr     bool
		wantParts   []string
		wantNoParts []string
	}{
		{
			name: "valid list with images and charts",
			list: testMirrorList(),
			wantParts: []string{
				"apiVersion: content.hauler.cattle.io/v1",
				"kind: Images",
				"name: aicr-images",
				"name: nvcr.io/nvidia/gpu-operator:v25.3.0",
				"name: registry.k8s.io/sig-storage/csi-provisioner:v5.2.0",
				"---",
				"kind: Charts",
				"name: aicr-charts",
				"name: gpu-operator",
				"repoURL: oci://ghcr.io/nvidia",
				"version: v25.3.0",
				"name: aws-ebs-csi-driver",
				"repoURL: https://kubernetes-sigs.github.io/aws-ebs-csi-driver",
				"version: 2.40.0",
			},
		},
		{
			name: "images only, no charts",
			list: &MirrorList{
				Images: []string{"nginx:latest"},
			},
			wantParts: []string{
				"kind: Images",
				"name: nginx:latest",
			},
			wantNoParts: []string{
				"---",
				"kind: Charts",
			},
		},
		{
			name:    "nil list",
			list:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := renderHauler(&buf, tt.list)
			if (err != nil) != tt.wantErr {
				t.Errorf("renderHauler() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			output := buf.String()
			for _, part := range tt.wantParts {
				if !strings.Contains(output, part) {
					t.Errorf("renderHauler() output missing %q\nGot:\n%s", part, output)
				}
			}
			for _, noPart := range tt.wantNoParts {
				if strings.Contains(output, noPart) {
					t.Errorf("renderHauler() output should not contain %q\nGot:\n%s", noPart, output)
				}
			}
		})
	}
}

func TestRenderHaulerChartNameStripsPathPrefix(t *testing.T) {
	tests := []struct {
		name        string
		chart       ChartRef
		wantParts   []string
		wantNoParts []string
	}{
		{
			name: "chart with path prefix",
			chart: ChartRef{
				Name:       "gpu-operator",
				Repository: "oci://ghcr.io/nvidia",
				Chart:      "nvidia/gpu-operator",
				Version:    "v25.3.0",
			},
			wantParts: []string{
				"name: gpu-operator",
			},
			wantNoParts: []string{
				"name: nvidia/gpu-operator",
			},
		},
		{
			name: "chart without path prefix unchanged",
			chart: ChartRef{
				Name:       "gpu-operator",
				Repository: "oci://ghcr.io/nvidia",
				Chart:      "gpu-operator",
				Version:    "v25.3.0",
			},
			wantParts: []string{
				"name: gpu-operator",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list := &MirrorList{
				Images: []string{"placeholder:v1"},
				Charts: []ChartRef{tt.chart},
			}
			var buf bytes.Buffer
			if err := renderHauler(&buf, list); err != nil {
				t.Fatalf("renderHauler() error = %v", err)
			}
			output := buf.String()
			for _, part := range tt.wantParts {
				if !strings.Contains(output, part) {
					t.Errorf("output missing %q\nGot:\n%s", part, output)
				}
			}
			for _, noPart := range tt.wantNoParts {
				if strings.Contains(output, noPart) {
					t.Errorf("output should not contain %q\nGot:\n%s", noPart, output)
				}
			}
		})
	}
}

func TestRenderHaulerAPIVersion(t *testing.T) {
	list := &MirrorList{
		Images: []string{"test:v1"},
		Charts: []ChartRef{
			{Name: "test", Repository: "https://example.com", Chart: "test", Version: "1.0"},
		},
	}

	var buf bytes.Buffer
	if err := renderHauler(&buf, list); err != nil {
		t.Fatalf("renderHauler() error = %v", err)
	}

	output := buf.String()
	count := strings.Count(output, "apiVersion: content.hauler.cattle.io/v1")
	if count != 2 {
		t.Errorf("expected apiVersion to appear exactly 2 times (Images + Charts), got %d\nOutput:\n%s", count, output)
	}
}
