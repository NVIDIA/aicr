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
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/client-go/kubernetes/fake"
)

// promResponse builds a Prometheus API JSON response with the given result count.
func promResponse(resultCount int) []byte {
	type result struct {
		Metric map[string]string `json:"metric"`
		Value  []interface{}     `json:"value"`
	}
	resp := struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string   `json:"resultType"`
			Result     []result `json:"result"`
		} `json:"data"`
	}{Status: "success"}
	resp.Data.ResultType = "vector"
	for i := range resultCount {
		resp.Data.Result = append(resp.Data.Result, result{
			Metric: map[string]string{"gpu": fmt.Sprintf("%d", i)},
			Value:  []interface{}{1234567890.0, "42"},
		})
	}
	b, _ := json.Marshal(resp)
	return b
}

func TestCheckAIServiceMetrics_RetryThenSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			// First two calls: empty results (DCGM not scraped yet).
			w.Write(promResponse(0))
			return
		}
		// Third call: metrics available.
		w.Write(promResponse(1))
	}))
	defer srv.Close()

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: fake.NewClientset(),
	}

	err := checkAIServiceMetricsWithURL(ctx, srv.URL)
	// The custom metrics API call will fail (fake clientset), but we only care
	// that the retry loop succeeded — the error should NOT be about Prometheus
	// being unreachable or metrics not found.
	if err != nil && strings.Contains(err.Error(), "DCGM_FI_DEV_GPU_UTIL") {
		t.Fatalf("retry loop should have found metrics, got: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("should not report Prometheus unreachable, got: %v", err)
	}
	if got := calls.Load(); got < 3 {
		t.Errorf("expected at least 3 Prometheus calls (2 empty + 1 success), got %d", got)
	}
}

func TestCheckAIServiceMetrics_TimeoutExpiry(t *testing.T) {
	t.Parallel()

	// Always return empty results so the retry loop times out.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(promResponse(0))
	}))
	defer srv.Close()

	ctx := &validators.Context{
		Ctx:       context.Background(),
		Clientset: fake.NewClientset(),
	}

	// Use a very short timeout by canceling the parent context quickly.
	shortCtx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	ctx.Ctx = shortCtx

	err := checkAIServiceMetricsWithURL(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error on timeout, got nil")
	}
	// Should be ErrCodeNotFound (metrics wait timeout) or ErrCodeTimeout (parent canceled),
	// NOT ErrCodeUnavailable (which would mean misclassification as connectivity failure).
	var se *errors.StructuredError
	if stderrors.As(err, &se) && se.Code == errors.ErrCodeUnavailable {
		t.Fatalf("timeout should not be classified as connectivity failure, got: %v", err)
	}
}

func TestCheckAIServiceMetrics_ParentContextCanceled(t *testing.T) {
	t.Parallel()

	// Always return empty results.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(promResponse(0))
	}))
	defer srv.Close()

	parentCtx, parentCancel := context.WithCancel(context.Background())
	// Cancel immediately to simulate upstream shutdown.
	parentCancel()

	ctx := &validators.Context{
		Ctx:       parentCtx,
		Clientset: fake.NewClientset(),
	}

	err := checkAIServiceMetricsWithURL(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error on parent cancellation, got nil")
	}
	var se *errors.StructuredError
	if !stderrors.As(err, &se) {
		t.Fatalf("expected StructuredError, got: %T %v", err, err)
	}
	if se.Code != errors.ErrCodeTimeout && se.Code != errors.ErrCodeUnavailable {
		// When parent is already canceled, httpGet may fail immediately. The key
		// assertion is that it does NOT return ErrCodeNotFound with the fixed
		// wait-time message (which would misattribute the failure).
		if se.Code == errors.ErrCodeNotFound && strings.Contains(err.Error(), "after 2m0s") {
			t.Fatalf("parent cancellation should not report as retry timeout: %v", err)
		}
	}
}
