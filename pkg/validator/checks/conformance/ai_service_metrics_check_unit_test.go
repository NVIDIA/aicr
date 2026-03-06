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

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestCheckAIServiceMetrics(t *testing.T) {
	tests := []struct {
		name        string
		clientset   bool
		wantErr     bool
		errContains string
	}{
		{
			name:        "no clientset",
			clientset:   false,
			wantErr:     true,
			errContains: "kubernetes client is not available",
		},
		{
			name:        "fake client lacks REST client — custom metrics API fails",
			clientset:   true,
			wantErr:     true,
			errContains: "discovery REST client is not available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctx *checks.ValidationContext

			if tt.clientset {
				//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
				ctx = &checks.ValidationContext{
					Context:   context.Background(),
					Clientset: fake.NewSimpleClientset(),
				}
			} else {
				ctx = &checks.ValidationContext{
					Context: context.Background(),
				}
			}

			err := CheckAIServiceMetrics(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckAIServiceMetrics() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %v, should contain %q", err, tt.errContains)
				}
			}
		})
	}
}

// TestCheckAIServiceMetricsDataPath validates the positive path: custom metrics
// API returns resource list with required metrics AND data queries return items.
// Uses httptest to create a real REST client since fake.NewSimpleClientset()
// returns nil for Discovery().RESTClient().
func TestCheckAIServiceMetricsDataPath(t *testing.T) {
	// Custom metrics API resource list with GPU metrics registered.
	resourceList := map[string]interface{}{
		"groupVersion": "custom.metrics.k8s.io/v1beta1",
		"resources": []map[string]interface{}{
			{"name": "pods/gpu_utilization", "namespaced": true},
			{"name": "namespaces/gpu_utilization", "namespaced": false},
			{"name": "pods/gpu_memory_used", "namespaced": true},
			{"name": "namespaces/gpu_memory_used", "namespaced": false},
			{"name": "pods/gpu_power_usage", "namespaced": true},
			{"name": "namespaces/gpu_power_usage", "namespaced": false},
		},
	}

	// Metric data response with actual samples.
	metricData := map[string]interface{}{
		"kind":       "MetricValueList",
		"apiVersion": "custom.metrics.k8s.io/v1beta1",
		"items": []map[string]interface{}{
			{
				"describedObject": map[string]interface{}{
					"kind":      "Pod",
					"namespace": "gpu-operator",
					"name":      "dcgm-exporter-abc",
				},
				"metricName": "gpu_utilization",
				"value":      "42",
			},
		},
	}

	// Empty data response (no samples).
	emptyData := map[string]interface{}{
		"kind":       "MetricValueList",
		"apiVersion": "custom.metrics.k8s.io/v1beta1",
		"items":      []interface{}{},
	}

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantErr     bool
		errContains string
	}{
		{
			name: "all metrics have data — passes",
			handler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.HasSuffix(path, "custom.metrics.k8s.io/v1beta1") {
					json.NewEncoder(w).Encode(resourceList) //nolint:errcheck
					return
				}
				// All pod-scoped queries return data.
				if strings.Contains(path, "/pods/") {
					json.NewEncoder(w).Encode(metricData) //nolint:errcheck
					return
				}
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr: false,
		},
		{
			name: "metrics registered but no samples — fails",
			handler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.HasSuffix(path, "custom.metrics.k8s.io/v1beta1") {
					json.NewEncoder(w).Encode(resourceList) //nolint:errcheck
					return
				}
				// All data queries return empty items.
				json.NewEncoder(w).Encode(emptyData) //nolint:errcheck
			},
			wantErr:     true,
			errContains: "no samples flowing",
		},
		{
			name: "metrics not registered — fails",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Return empty resource list.
				json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
					"groupVersion": "custom.metrics.k8s.io/v1beta1",
					"resources":    []interface{}{},
				})
			},
			wantErr:     true,
			errContains: "not registered in custom metrics API",
		},
		{
			name: "custom metrics API unavailable — fails",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			wantErr:     true,
			errContains: "custom metrics API not available",
		},
		{
			name: "namespace-scoped only — still passes",
			handler: func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if strings.HasSuffix(path, "custom.metrics.k8s.io/v1beta1") {
					// Only namespace-scoped metrics registered.
					json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
						"groupVersion": "custom.metrics.k8s.io/v1beta1",
						"resources": []map[string]interface{}{
							{"name": "namespaces/gpu_utilization", "namespaced": false},
							{"name": "namespaces/gpu_memory_used", "namespaced": false},
							{"name": "namespaces/gpu_power_usage", "namespaced": false},
						},
					})
					return
				}
				// Pod-scoped queries return 404, namespace-scoped return data.
				if strings.Contains(path, "/pods/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if strings.Contains(path, "/metrics/") {
					json.NewEncoder(w).Encode(metricData) //nolint:errcheck
					return
				}
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatalf("failed to create clientset: %v", err)
			}

			ctx := &checks.ValidationContext{
				Context:   context.Background(),
				Clientset: clientset,
			}

			err = CheckAIServiceMetrics(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("CheckAIServiceMetrics() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %v, should contain %q", err, tt.errContains)
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

func TestRequiredCustomMetrics(t *testing.T) {
	if len(requiredCustomMetrics) == 0 {
		t.Fatal("requiredCustomMetrics must not be empty")
	}
	expected := map[string]bool{
		"gpu_utilization": true,
		"gpu_memory_used": true,
		"gpu_power_usage": true,
	}
	for _, m := range requiredCustomMetrics {
		if !expected[m] {
			t.Errorf("unexpected required metric: %s", m)
		}
	}
}

// metricDataPathTemplate is the expected URL path pattern for metric data queries.
// This test ensures the path shape stays consistent with the standard custom metrics API.
func TestMetricDataPathShape(t *testing.T) {
	var queriedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		queriedPaths = append(queriedPaths, path)

		if strings.HasSuffix(path, "custom.metrics.k8s.io/v1beta1") {
			json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
				"groupVersion": "custom.metrics.k8s.io/v1beta1",
				"resources": []map[string]interface{}{
					{"name": "pods/gpu_utilization", "namespaced": true},
					{"name": "pods/gpu_memory_used", "namespaced": true},
					{"name": "pods/gpu_power_usage", "namespaced": true},
				},
			})
			return
		}
		// Return data for all queries.
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"items": []map[string]interface{}{
				{"metricName": "test", "value": "1"},
			},
		})
	}))
	defer server.Close()

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("failed to create clientset: %v", err)
	}

	ctx := &checks.ValidationContext{
		Context:   context.Background(),
		Clientset: clientset,
	}

	if err := CheckAIServiceMetrics(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify data query paths match standard custom metrics API shape.
	for _, path := range queriedPaths {
		if strings.Contains(path, "/pods/") {
			// Must match: /apis/custom.metrics.k8s.io/v1beta1/namespaces/{ns}/pods/*/{metric}
			expected := fmt.Sprintf("/apis/custom.metrics.k8s.io/v1beta1/namespaces/%s/pods/", "gpu-operator")
			if !strings.HasPrefix(path, expected) {
				t.Errorf("pod-scoped path %q does not start with expected prefix %q", path, expected)
			}
			// Must end with */{metric_name}
			parts := strings.Split(path, "/")
			if len(parts) < 2 || parts[len(parts)-2] != "*" {
				t.Errorf("pod-scoped path %q missing wildcard selector (expected .../pods/*/<metric>)", path)
			}
		}
	}
}
