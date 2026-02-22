// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package conformance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

func TestCheckAcceleratorMetrics(t *testing.T) {
	allMetrics := `# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-abc"} 42
# HELP DCGM_FI_DEV_FB_USED Framebuffer memory used
# TYPE DCGM_FI_DEV_FB_USED gauge
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-abc"} 1024
# HELP DCGM_FI_DEV_GPU_TEMP GPU temperature
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-abc"} 65
# HELP DCGM_FI_DEV_POWER_USAGE Power draw
# TYPE DCGM_FI_DEV_POWER_USAGE gauge
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-abc"} 200
`

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantErr     bool
		errContains string
	}{
		{
			name: "all metrics present",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, allMetrics)
			},
			wantErr: false,
		},
		{
			name: "missing one metric",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Only 3 of 4 metrics
				fmt.Fprint(w, `DCGM_FI_DEV_GPU_UTIL{gpu="0"} 42
DCGM_FI_DEV_FB_USED{gpu="0"} 1024
DCGM_FI_DEV_GPU_TEMP{gpu="0"} 65
`)
			},
			wantErr:     true,
			errContains: "DCGM_FI_DEV_POWER_USAGE",
		},
		{
			name: "missing all metrics",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "# no metrics here\n")
			},
			wantErr:     true,
			errContains: "DCGM metrics missing",
		},
		{
			name: "server returns 500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr:     true,
			errContains: "HTTP 500",
		},
		{
			name:        "server unreachable",
			handler:     nil, // No server started
			wantErr:     true,
			errContains: "DCGM exporter metrics endpoint unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var url string
			if tt.handler != nil {
				server := httptest.NewServer(tt.handler)
				defer server.Close()
				url = server.URL + "/metrics"
			} else {
				// Use an unreachable URL
				url = "http://127.0.0.1:1/metrics"
			}

			ctx := &checks.ValidationContext{
				Context: context.Background(),
			}

			err := checkAcceleratorMetricsWithURL(ctx, url)

			if (err != nil) != tt.wantErr {
				t.Errorf("checkAcceleratorMetricsWithURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("checkAcceleratorMetricsWithURL() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckAcceleratorMetricsRegistration(t *testing.T) {
	check, ok := checks.GetCheck("accelerator-metrics")
	if !ok {
		t.Fatal("accelerator-metrics check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}

func TestContainsAllMetrics(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		required []string
		want     []string
	}{
		{
			name:     "all present",
			text:     "DCGM_FI_DEV_GPU_UTIL 42\nDCGM_FI_DEV_FB_USED 1024",
			required: []string{"DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED"},
			want:     nil,
		},
		{
			name:     "one missing",
			text:     "DCGM_FI_DEV_GPU_UTIL 42",
			required: []string{"DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED"},
			want:     []string{"DCGM_FI_DEV_FB_USED"},
		},
		{
			name:     "all missing",
			text:     "no metrics here",
			required: []string{"DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED"},
			want:     []string{"DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED"},
		},
		{
			name:     "empty text",
			text:     "",
			required: []string{"DCGM_FI_DEV_GPU_UTIL"},
			want:     []string{"DCGM_FI_DEV_GPU_UTIL"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsAllMetrics(tt.text, tt.required)
			if len(got) != len(tt.want) {
				t.Errorf("containsAllMetrics() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("containsAllMetrics()[%d] = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}
