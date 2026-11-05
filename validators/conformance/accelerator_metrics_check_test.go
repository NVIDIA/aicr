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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/validators"
)

const dcgmSampleLabels = `{gpu="0",UUID="GPU-aaaa",device="nvidia0",modelName="NVIDIA H100",Hostname="node-a",namespace="",pod="",container=""}`

// dcgmScrape builds an exposition body from the given DCGM metric names, each
// with a per-device label set.
func dcgmScrape(names ...string) string {
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString("# HELP " + n + " help\n# TYPE " + n + " gauge\n")
		sb.WriteString(n + dcgmSampleLabels + " 42\n")
	}
	return sb.String()
}

func TestParsePrometheusText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantCount  int
		wantName   string
		wantLabels map[string]string
	}{
		{
			name:       "labeled gauge",
			body:       "# TYPE x gauge\nDCGM_FI_DEV_GPU_UTIL{gpu=\"0\",UUID=\"GPU-1\"} 12\n",
			wantCount:  1,
			wantName:   "DCGM_FI_DEV_GPU_UTIL",
			wantLabels: map[string]string{"gpu": "0", "UUID": "GPU-1"},
		},
		{
			name:       "unlabeled sample with timestamp",
			body:       "up 1 1700000000\n",
			wantCount:  1,
			wantName:   "up",
			wantLabels: map[string]string{},
		},
		{
			name:       "quoted comma and escaped quote inside a label value",
			body:       `m{a="x,y",b="q\"z",gpu="1"} 3` + "\n",
			wantCount:  1,
			wantName:   "m",
			wantLabels: map[string]string{"a": "x,y", "b": `q\"z`, "gpu": "1"},
		},
		{
			name:      "NaN and malformed lines are dropped",
			body:      "m{gpu=\"0\"} NaN\nm{gpu=\"0\"\n\n# comment\n",
			wantCount: 0,
		},
		{
			name:       "empty label values do not identify a device",
			body:       "m{gpu=\"\",pod=\"\"} 1\n",
			wantCount:  1,
			wantName:   "m",
			wantLabels: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parsePrometheusText(tt.body)
			if len(got) != tt.wantCount {
				t.Fatalf("got %d samples, want %d: %+v", len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			if got[0].name != tt.wantName {
				t.Errorf("name = %q, want %q", got[0].name, tt.wantName)
			}
			if len(got[0].labels) != len(tt.wantLabels) {
				t.Fatalf("labels = %v, want %v", got[0].labels, tt.wantLabels)
			}
			for k, v := range tt.wantLabels {
				if got[0].labels[k] != v {
					t.Errorf("label %s = %q, want %q", k, got[0].labels[k], v)
				}
			}
		})
	}
}

// TestSummarizeAcceleratorFamilies pins the MUST/SHOULD split: utilization and
// memory must be served per device; temperature, power and interconnect are
// recorded either way and never listed as missing.
func TestSummarizeAcceleratorFamilies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		body        string
		wantMissing []string
		wantReport  []string
	}{
		{
			name: "full DCGM scrape",
			body: dcgmScrape("DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_PROF_NVLINK_TX_BYTES"),
			wantReport: []string{
				"utilization              PASS (MUST): DCGM_FI_DEV_GPU_UTIL (1 samples, device labels gpu, UUID)",
				"memory                   PASS (MUST): DCGM_FI_DEV_FB_USED",
				"temperature              PASS (SHOULD): DCGM_FI_DEV_GPU_TEMP",
				"power                    PASS (SHOULD): DCGM_FI_DEV_POWER_USAGE",
				"interconnect bandwidth   PASS (SHOULD): DCGM_FI_PROF_NVLINK_TX_BYTES",
			},
		},
		{
			name: "temperature and power absent are SHOULD observations, not failures",
			body: dcgmScrape("DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED"),
			wantReport: []string{
				"temperature              SKIP (SHOULD)",
				"power                    SKIP (SHOULD)",
				"interconnect bandwidth   SKIP (SHOULD)",
			},
		},
		{
			name:        "utilization without a device label fails the MUST",
			body:        "DCGM_FI_DEV_GPU_UTIL{Hostname=\"node-a\"} 5\n" + dcgmScrape("DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE"),
			wantMissing: []string{"utilization"},
			wantReport:  []string{"utilization              FAIL (MUST)"},
		},
		{
			name:        "memory family absent fails the MUST",
			body:        dcgmScrape("DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE"),
			wantMissing: []string{"memory"},
		},
		{
			name:        "only optional families present fails both MUSTs",
			body:        dcgmScrape("DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE"),
			wantMissing: []string{"utilization", "memory"},
		},
		{
			name: "vendor-neutral names and alternative device labels match",
			body: "accel_gpu_util{pci_bus_id=\"0000:01:00.0\"} 1\naccel_hbm_used{device_id=\"0\"} 2\n",
			wantReport: []string{
				"utilization              PASS (MUST): accel_gpu_util (1 samples, device labels pci_bus_id)",
				"memory                   PASS (MUST): accel_hbm_used (1 samples, device labels device_id)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report, missing := summarizeAcceleratorFamilies(parsePrometheusText(tt.body))
			if strings.Join(missing, ",") != strings.Join(tt.wantMissing, ",") {
				t.Errorf("missing = %v, want %v\n%s", missing, tt.wantMissing, report)
			}
			for _, want := range tt.wantReport {
				if !strings.Contains(report, want) {
					t.Errorf("report lacks %q:\n%s", want, report)
				}
			}
		})
	}
}

// TestCheckAcceleratorMetricsWithURL covers the verdicts end to end against a
// stub exporter: pass with optional families absent, fail when a required
// family is absent or the scrape is empty, and the reachability code.
func TestCheckAcceleratorMetricsWithURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		status   int
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{name: "full scrape passes", body: dcgmScrape("DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE")},
		{name: "temperature and power absent still passes", body: dcgmScrape("DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_FB_USED")},
		{
			name:     "memory absent fails",
			body:     dcgmScrape("DCGM_FI_DEV_GPU_UTIL", "DCGM_FI_DEV_GPU_TEMP"),
			wantCode: errors.ErrCodeNotFound,
			wantMsg:  "no per-device memory metric",
		},
		{
			name:     "both required families absent names both",
			body:     dcgmScrape("DCGM_FI_DEV_GPU_TEMP"),
			wantCode: errors.ErrCodeNotFound,
			wantMsg:  "no per-device utilization or memory metric",
		},
		{
			name:     "empty scrape fails",
			body:     "# nothing here\n",
			wantCode: errors.ErrCodeNotFound,
			wantMsg:  "returned no samples",
		},
		{
			name:     "non-200 is unreachable",
			status:   http.StatusServiceUnavailable,
			wantCode: errors.ErrCodeInternal,
			wantMsg:  "HTTP 503",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					return
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			err := checkAcceleratorMetricsWithURL(&validators.Context{Ctx: context.Background()}, srv.URL)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error with code %q, got nil", tt.wantCode)
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q lacks %q", err.Error(), tt.wantMsg)
			}
		})
	}
}
