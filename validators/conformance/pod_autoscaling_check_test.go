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
	"errors"
	"testing"
	"time"
)

// TestWaitForMetricsAPI covers the bounded-retry helper that wraps the
// pod-autoscaling metric-API probes (custom/external.metrics.k8s.io), which can
// race the prometheus-adapter relist right after the deployment phase. A tiny
// interval keeps the test fast.
func TestWaitForMetricsAPI(t *testing.T) {
	t.Run("immediate success runs the probe once", func(t *testing.T) {
		calls := 0
		err := waitForMetricsAPI(context.Background(), time.Second, time.Millisecond,
			func(context.Context) error { calls++; return nil })
		if err != nil {
			t.Fatalf("want nil, got %v", err)
		}
		if calls != 1 {
			t.Errorf("want 1 probe call, got %d", calls)
		}
	})

	t.Run("retries until the probe succeeds", func(t *testing.T) {
		calls := 0
		err := waitForMetricsAPI(context.Background(), 5*time.Second, time.Millisecond,
			func(context.Context) error {
				calls++
				if calls < 3 {
					return errors.New("not ready")
				}
				return nil
			})
		if err != nil {
			t.Fatalf("want success after retries, got %v", err)
		}
		if calls < 3 {
			t.Errorf("want >=3 probe calls, got %d", calls)
		}
	})

	t.Run("returns the last probe error on timeout (fail closed)", func(t *testing.T) {
		sentinel := errors.New("still not ready")
		err := waitForMetricsAPI(context.Background(), 30*time.Millisecond, time.Millisecond,
			func(context.Context) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("want last probe error %v, got %v", sentinel, err)
		}
	})

	t.Run("returns when the parent context is already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := waitForMetricsAPI(ctx, time.Second, time.Millisecond,
			func(context.Context) error { calls++; return errors.New("not ready") })
		if err == nil {
			t.Fatal("want non-nil error when parent context is canceled")
		}
	})
}
