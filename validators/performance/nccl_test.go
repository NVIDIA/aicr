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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTemplatePath(t *testing.T) {
	tests := []struct {
		name        string
		accelerator recipe.CriteriaAcceleratorType
		service     recipe.CriteriaServiceType
		filename    string
		expected    string
	}{
		{
			name:        "eks h100 runtime",
			accelerator: recipe.CriteriaAcceleratorH100,
			service:     recipe.CriteriaServiceEKS,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "h100", "eks", "runtime.yaml"),
		},
		{
			name:        "eks h100 trainjob",
			accelerator: recipe.CriteriaAcceleratorH100,
			service:     recipe.CriteriaServiceEKS,
			filename:    "trainjob.yaml",
			expected:    filepath.Join("testdata", "h100", "eks", "trainjob.yaml"),
		},
		{
			name:        "gke gb200",
			accelerator: recipe.CriteriaAcceleratorGB200,
			service:     recipe.CriteriaServiceGKE,
			filename:    "runtime.yaml",
			expected:    filepath.Join("testdata", "gb200", "gke", "runtime.yaml"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := templatePath(tt.accelerator, tt.service, tt.filename)
			if got != tt.expected {
				t.Errorf("templatePath() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestParseThreshold(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    float64
		wantErr bool
	}{
		{
			name:    "simple integer",
			value:   "450",
			want:    450,
			wantErr: false,
		},
		{
			name:    "float with units",
			value:   "100.5 GB/s",
			want:    100.5,
			wantErr: false,
		},
		{
			name:    "with leading whitespace",
			value:   "  200 GB/s",
			want:    200,
			wantErr: false,
		},
		{
			name:    "invalid format",
			value:   "abc GB/s",
			wantErr: true,
		},
		{
			name:    "empty string",
			value:   "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseThreshold(tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseThreshold(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseThreshold(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseBandwidthFromLogs(t *testing.T) {
	// Realistic NCCL all-reduce output snippet with 16G row (EKS).
	eksLogs := `# nThread 1 nGpus 1 minBytes 1024 maxBytes 17179869184 step: 2(factor) warmup iters: 5 iters: 20 agg iters: 1 validation: 1 graph: 0
#
# Using devices
#  Rank  0 Group  0 Pid 123 on node1 device  0 [0x00] NVIDIA H100 80GB HBM3
#
#                                                              out-of-place                       in-place
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
        1024           256     float     sum      -1    28.50    0.04    0.07      0    28.20    0.04    0.07      0
 17179869184    4294967296     float     sum      -1  123456   139.20  450.30      0  123456   139.20  450.30      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 225.15`

	// Realistic NCCL all-reduce output with 8G max (GKE TCPXO).
	gkeLogs := `# nccl-tests version 2.17.6 nccl-headers=22807 nccl-library=22807
#                                                              out-of-place                       in-place
#       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
  4294967296    1073741824     float     sum      -1  24547.5  174.97  328.06      0  24635.5  174.34  326.89      0
  8589934592    2147483648     float     sum      -1  48292.9  177.87  333.51      0  48298.2  177.85  333.47      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 87.0675`

	noMatchLogs := `some random output
no bandwidth data here
completed successfully`

	tests := []struct {
		name    string
		logs    string
		want    float64
		wantErr bool
	}{
		{
			name: "EKS 16G max message size",
			logs: eksLogs,
			want: 450.30,
		},
		{
			name: "GKE 8G max message size",
			logs: gkeLogs,
			want: 333.51,
		},
		{
			name:    "no match in logs",
			logs:    noMatchLogs,
			wantErr: true,
		},
		{
			name:    "empty logs",
			logs:    "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBandwidthFromLogs(tt.logs)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseBandwidthFromLogs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseBandwidthFromLogs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCRDEstablished(t *testing.T) {
	tests := []struct {
		name string
		obj  *unstructured.Unstructured
		want bool
	}{
		{
			name: "established true",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Established",
								"status": "True",
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "established false",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "Established",
								"status": "False",
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "no established condition",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{
						"conditions": []any{
							map[string]any{
								"type":   "NamesAccepted",
								"status": "True",
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "missing conditions",
			obj: &unstructured.Unstructured{
				Object: map[string]any{
					"status": map[string]any{},
				},
			},
			want: false,
		},
		{
			name: "empty object",
			obj: &unstructured.Unstructured{
				Object: map[string]any{},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCRDEstablished(tt.obj)
			if got != tt.want {
				t.Errorf("isCRDEstablished() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSanitizeTarPath(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		targetDir string
		entryPath string
		wantErr   bool
		wantSub   string
	}{
		{
			name:      "valid relative path",
			targetDir: tmpDir,
			entryPath: "trainer-2.1.0/manifests/base/kustomization.yaml",
			wantErr:   false,
		},
		{
			name:      "path traversal with dot-dot",
			targetDir: tmpDir,
			entryPath: "../../../etc/passwd",
			wantErr:   true,
			wantSub:   "path traversal",
		},
		{
			name:      "dot-dot mid-path traversal",
			targetDir: tmpDir,
			entryPath: "legit/../../../../../../etc/shadow",
			wantErr:   true,
			wantSub:   "path traversal",
		},
		{
			name:      "nested valid path",
			targetDir: tmpDir,
			entryPath: "a/b/c/d.txt",
			wantErr:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeTarPath(tt.targetDir, tt.entryPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("sanitizeTarPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.wantSub != "" {
				if !strings.Contains(err.Error(), tt.wantSub) {
					t.Errorf("sanitizeTarPath() error = %q, want substring %q", err.Error(), tt.wantSub)
				}
			}
			if !tt.wantErr {
				if !strings.HasPrefix(got, filepath.Clean(tt.targetDir)+string(os.PathSeparator)) {
					t.Errorf("sanitizeTarPath() = %q, want prefix %q", got, tt.targetDir)
				}
			}
		})
	}
}
