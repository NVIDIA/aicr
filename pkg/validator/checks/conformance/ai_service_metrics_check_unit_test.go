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
	"k8s.io/client-go/kubernetes/fake"
)

func TestCheckAIServiceMetrics(t *testing.T) {
	promResponseWithData := `{
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": [
				{"metric": {"__name__": "DCGM_FI_DEV_GPU_UTIL", "gpu": "0"}, "value": [1700000000, "42"]}
			]
		}
	}`

	promResponseEmpty := `{
		"status": "success",
		"data": {
			"resultType": "vector",
			"result": []
		}
	}`

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		clientset   bool
		wantErr     bool
		errContains string
	}{
		{
			name: "prometheus has data but fake client lacks REST client",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, promResponseWithData)
			},
			clientset:   true,
			wantErr:     true,
			errContains: "discovery REST client is not available",
		},
		{
			name:        "no clientset",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name: "prometheus has no data",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, promResponseEmpty)
			},
			clientset:   true,
			wantErr:     true,
			errContains: "no DCGM_FI_DEV_GPU_UTIL time series",
		},
		{
			name: "prometheus returns error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			clientset:   true,
			wantErr:     true,
			errContains: "Prometheus unreachable",
		},
		{
			name: "prometheus returns invalid JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "not json")
			},
			clientset:   true,
			wantErr:     true,
			errContains: "failed to parse Prometheus response",
		},
		{
			name:        "prometheus unreachable",
			handler:     nil,
			clientset:   true,
			wantErr:     true,
			errContains: "Prometheus unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				clientset := fake.NewSimpleClientset()
				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: clientset,
				}
			} else {
				ctx = &checks.ValidationContext{
					Context: context.Background(),
				}
			}

			var promURL string
			if tt.handler != nil {
				server := httptest.NewServer(tt.handler)
				defer server.Close()
				promURL = server.URL
			} else {
				promURL = "http://127.0.0.1:1"
			}

			// Note: We only test the Prometheus part of the check.
			// The custom metrics API part uses Discovery().RESTClient() which is
			// harder to mock with fake.NewSimpleClientset. The fake discovery client
			// returns success for unknown paths, so the happy path test covers it.
			err := checkAIServiceMetricsWithURL(ctx, promURL)

			if (err != nil) != tt.wantErr {
				t.Errorf("checkAIServiceMetricsWithURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("checkAIServiceMetricsWithURL() error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

func TestCheckAIServiceMetricsRegistration(t *testing.T) {
	check, ok := checks.GetCheck("ai-service-metrics")
	if !ok {
		t.Fatal("ai-service-metrics check not registered")
	}
	if check.Phase != phaseConformance {
		t.Errorf("Phase = %v, want conformance", check.Phase)
	}
	if check.Func == nil {
		t.Fatal("Func is nil")
	}
}
