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
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// promScrapePools renders a successful /api/v1/scrape_pools response.
func promScrapePools(pools ...string) []byte {
	if pools == nil {
		pools = []string{}
	}
	body, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"scrapePools": pools}})
	return body
}

// TestScrapePoolPrefix pins the prometheus-operator job-name shape the wait
// matches on, with the trailing slash that keeps a monitor from matching
// another whose name extends it.
func TestScrapePoolPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		kind   monitorKind
		pools  []string
		loaded bool
	}{
		{"ServiceMonitor endpoint 0", monitorKindService, []string{"serviceMonitor/ns/stub/0"}, true},
		{"PodMonitor endpoint 1", monitorKindPod, []string{"podMonitor/ns/stub/1"}, true},
		{"other kind", monitorKindService, []string{"podMonitor/ns/stub/0"}, false},
		{"name extended", monitorKindService, []string{"serviceMonitor/ns/stub-other/0"}, false},
		{"other namespace", monitorKindService, []string{"serviceMonitor/ns2/stub/0"}, false},
		{"no pools", monitorKindService, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := monitorLoaded(tt.pools, scrapePoolPrefix(tt.kind, "ns", "stub")); got != tt.loaded {
				t.Errorf("monitorLoaded(%v) = %t, want %t", tt.pools, got, tt.loaded)
			}
		})
	}
}

func TestParsePromScrapePools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		want    int
		wantErr bool
	}{
		{"two pools", string(promScrapePools("a/0", "b/0")), 2, false},
		{"no pools", string(promScrapePools()), 0, false},
		{"error status", `{"status":"error","errorType":"internal","error":"boom"}`, 0, true},
		{"malformed", `{`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pools, err := parsePromScrapePools([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tt.wantErr)
			}
			if len(pools) != tt.want {
				t.Errorf("got %d pools, want %d", len(pools), tt.want)
			}
		})
	}
}

// TestWaitForMonitorLoaded pins the wait's verdicts: a pool that appears
// after several polls passes; one that never appears fails with NotFound and
// names the monitor and the pool prefix; listings that fail are retried and,
// on timeout, reported as an undetermined load state under the listing
// error's own code, never as "not loaded"; a listing the deadline cuts short
// neither overturns an earlier completed listing nor counts as one; a check
// budget that ends first reports Timeout.
func TestWaitForMonitorLoaded(t *testing.T) {
	t.Parallel()
	const (
		ns       = "hpa-test-unit"
		interval = time.Millisecond
		timeout  = 150 * time.Millisecond
	)
	prefix := scrapePoolPrefix(monitorKindPod, ns, hpaWorkloadName)

	t.Run("pool appearing after several polls passes", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		list := func(context.Context) ([]byte, error) {
			if calls.Add(1) < 5 {
				return promScrapePools("kubelet/0"), nil
			}
			return promScrapePools("kubelet/0", prefix+"0"), nil
		}
		if err := waitForMonitorLoaded(context.Background(), list, monitorKindPod, ns, hpaWorkloadName, timeout, interval); err != nil {
			t.Fatalf("waitForMonitorLoaded: %v", err)
		}
		if calls.Load() < 5 {
			t.Errorf("returned after %d listings, want at least 5", calls.Load())
		}
	})

	t.Run("pool never appearing fails with NotFound", func(t *testing.T) {
		t.Parallel()
		list := func(context.Context) ([]byte, error) { return promScrapePools("kubelet/0"), nil }
		err := waitForMonitorLoaded(context.Background(), list, monitorKindPod, ns, hpaWorkloadName, timeout, interval)
		errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
		for _, want := range []string{"Prometheus did not load the PodMonitor " + ns + "/" + hpaWorkloadName, prefix + "*", "among the 1 configured", timeout.String()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err.Error(), want)
			}
		}
	})

	t.Run("failed listings are retried and leave the load state undetermined", func(t *testing.T) {
		t.Parallel()
		var calls atomic.Int32
		list := func(context.Context) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New(errors.ErrCodeInternal, "HTTP 502 from http://prom:9090/api/v1/scrape_pools")
		}
		err := waitForMonitorLoaded(context.Background(), list, monitorKindService, ns, aiMetricsWorkloadName, timeout, interval)
		errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		for _, want := range []string{"could not determine whether Prometheus loaded the ServiceMonitor", "last completed scrape pool listing failed", "HTTP 502"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), "did not load") {
			t.Errorf("error %q claims a verdict no listing established", err.Error())
		}
		if calls.Load() < 2 {
			t.Errorf("listing called %d times, want a retry", calls.Load())
		}
	})

	t.Run("listing cut short after pool-less listings reports not loaded", func(t *testing.T) {
		t.Parallel()
		// Every real timeout ends with a listing in flight or about to
		// start, so the call the deadline interrupts must not decide the
		// verdict: the absence conclusion rests on the last listing that
		// completed, which is sound. Reversing this made the "never loaded"
		// verdict depend on runner speed (CI run 42101378264).
		var calls atomic.Int32
		list := func(ctx context.Context) ([]byte, error) {
			if calls.Add(1) < 3 {
				return promScrapePools("kubelet/0"), nil
			}
			<-ctx.Done()
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to reach http://prom:9090/api/v1/scrape_pools", ctx.Err())
		}
		err := waitForMonitorLoaded(context.Background(), list, monitorKindPod, ns, hpaWorkloadName, timeout, interval)
		errorstest.WantReportedCode(t, err, errors.ErrCodeNotFound)
		if !strings.Contains(err.Error(), "Prometheus did not load the PodMonitor") || !strings.Contains(err.Error(), "among the 1 configured") {
			t.Errorf("error %q must rest on the last completed listing", err.Error())
		}
		if calls.Load() != 3 {
			t.Errorf("listing called %d times, want 3 (two pool-less, one cut short)", calls.Load())
		}
	})

	t.Run("no listing completed leaves the load state undetermined", func(t *testing.T) {
		t.Parallel()
		list := func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to reach http://prom:9090/api/v1/scrape_pools", ctx.Err())
		}
		err := waitForMonitorLoaded(context.Background(), list, monitorKindPod, ns, hpaWorkloadName, timeout, interval)
		errorstest.WantReportedCode(t, err, errors.ErrCodeUnavailable)
		if !strings.Contains(err.Error(), "could not determine whether Prometheus loaded the PodMonitor") || !strings.Contains(err.Error(), "no scrape pool listing completed") || strings.Contains(err.Error(), "did not load") {
			t.Errorf("error %q must report an undetermined load state", err.Error())
		}
	})

	t.Run("exhausted check budget reports Timeout", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		list := func(context.Context) ([]byte, error) { return promScrapePools(), nil }
		err := waitForMonitorLoaded(ctx, list, monitorKindPod, ns, hpaWorkloadName, time.Minute, interval)
		errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
		if !strings.Contains(err.Error(), "check budget exhausted") {
			t.Errorf("error %q does not name the budget", err.Error())
		}
	})
}

// TestPreflightScrapePools pins that the preflight keeps the lister's code
// for an unreachable Prometheus and rejects an error envelope.
func TestPreflightScrapePools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		list     func(context.Context) ([]byte, error)
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{"pools listed", func(context.Context) ([]byte, error) { return promScrapePools("kubelet/0"), nil }, "", ""},
		{"unreachable", func(context.Context) ([]byte, error) {
			return nil, errors.New(errors.ErrCodeUnavailable, "dial tcp: connection refused")
		}, errors.ErrCodeUnavailable, "did not list its scrape pools"},
		{"not served", func(context.Context) ([]byte, error) {
			return nil, errors.New(errors.ErrCodeInternal, "HTTP 404 from http://prom:9090/api/v1/scrape_pools")
		}, errors.ErrCodeInternal, "Prometheus >= v2.42"},
		{"error envelope", func(context.Context) ([]byte, error) {
			return []byte(`{"status":"error","errorType":"internal","error":"boom"}`), nil
		}, errors.ErrCodeInternal, "preflight failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := preflightScrapePools(context.Background(), "http://prom:9090", tt.list)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q lacks %q", err.Error(), tt.wantMsg)
			}
		})
	}
}
