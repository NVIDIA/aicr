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

package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTP request metrics
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aicr_http_requests_total",
			Help: "Total number of HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aicr_http_request_duration_seconds",
			Help:    "HTTP request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)

	httpRequestsInFlight = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "aicr_http_requests_in_flight",
			Help: "Current number of HTTP requests being processed",
		},
	)

	// Rate limiting metrics
	rateLimitRejects = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "aicr_rate_limit_rejects_total",
			Help: "Total number of requests rejected due to rate limiting",
		},
	)

	// Panic recovery metrics
	panicRecoveries = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "aicr_panic_recoveries_total",
			Help: "Total number of panics recovered in HTTP handlers",
		},
	)
)

// metricsMiddleware instruments HTTP requests with Prometheus metrics.
// It tracks request rate, errors, and duration (RED metrics) for observability.
func (s *Server) metricsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		httpRequestsInFlight.Inc()
		defer httpRequestsInFlight.Dec()

		// Wrap response writer to capture status code
		wrapped := newResponseWriter(w)

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start).Seconds()
		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(wrapped.Status())

		httpRequestsTotal.WithLabelValues(method, path, status).Inc()
		httpRequestDuration.WithLabelValues(method, path).Observe(duration)
	}
}
