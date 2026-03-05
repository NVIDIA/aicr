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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

			err := checkAIServiceMetricsWithURL(ctx, promURL)

			if (err != nil) != tt.wantErr {
				t.Errorf("checkAIServiceMetricsWithURL() error = %v, wantErr %v", err, tt.wantErr)
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

func TestDiscoverPrometheusURL(t *testing.T) {
	tests := []struct {
		name        string
		recipe      *recipe.RecipeResult
		services    []corev1.Service
		wantURL     string
		wantErr     bool
		errContains string
	}{
		{
			name:        "no recipe",
			wantErr:     true,
			errContains: "recipe is not available",
		},
		{
			name: "component not in recipe",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "other", Namespace: "default"},
				},
			},
			wantErr:     true,
			errContains: "not found in recipe",
		},
		{
			name: "no matching service",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: prometheusComponentName, Namespace: "monitoring"},
				},
			},
			wantErr:     true,
			errContains: "no Prometheus service with port",
		},
		{
			name: "service discovered",
			recipe: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: prometheusComponentName, Namespace: "monitoring"},
				},
			},
			services: []corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kube-prometheus-prometheus",
						Namespace: "monitoring",
						Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{{Port: 9090}},
					},
				},
			},
			wantURL: "http://kube-prometheus-prometheus.monitoring.svc:9090",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			clientset := fake.NewSimpleClientset()
			for i := range tt.services {
				svc := &tt.services[i]
				if _, err := clientset.CoreV1().Services(svc.Namespace).Create(
					context.Background(), svc, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create service: %v", err)
				}
			}

			ctx := &checks.ValidationContext{
				Context:   context.Background(),
				Clientset: clientset,
				Recipe:    tt.recipe,
			}

			got, err := discoverPrometheusURL(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("discoverPrometheusURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContains != "" {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %v, should contain %q", err, tt.errContains)
				}
			}
			if !tt.wantErr && got != tt.wantURL {
				t.Errorf("discoverPrometheusURL() = %q, want %q", got, tt.wantURL)
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
