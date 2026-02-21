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

// Package server implements the AICR System Configuration Recommendation API
// as defined in api/aicr/aicr-v1.yaml
//
// This implementation follows production-grade distributed systems best practices:
//
// # Architecture
//
// The server implements a stateless HTTP API with the following key components:
//
//   - Request validation using regex patterns from OpenAPI spec
//   - Rate limiting using token bucket algorithm (golang.org/x/time/rate)
//   - Request ID tracking for distributed tracing
//   - Panic recovery for resilience
//   - Graceful shutdown handling
//   - Health and readiness probes for Kubernetes
//
// # Usage
//
// Basic server startup:
//
//	package main
//
//	import (
//	    "github.com/NVIDIA/aicr/pkg/server"
//	)
//
//	func main() {
//	    if err := server.Run(); err != nil {
//	        panic(err)
//	    }
//	}
//
// Custom configuration:
//
//	config := server.DefaultConfig()
//	config.Port = 9090
//	config.RateLimit = 200  // 200 requests/sec
//	config.RateLimitBurst = 400
//
//	if err := server.RunWithConfig(config); err != nil {
//	    panic(err)
//	}
//
// # API Endpoints
//
// GET /v1/recipe - Generate configuration recipe
//
//	Query parameters:
//	  - os: ubuntu, cos, any (default: any)
//	  - osv: OS version (e.g., 24.04, 22.04)
//	  - kernel: kernel version (e.g., 6.8, 5.15.0)
//	  - service: eks, gke, aks, self-managed, any (default: any)
//	  - k8s: Kubernetes version (e.g., 1.33, 1.32)
//	  - gpu: h100, gb200, a100, l40, any (default: any)
//	  - intent: training, inference, any (default: any)
//	  - context: true/false - include context metadata (default: false)
//
//	Example:
//	  curl "http://localhost:8080/v1/recipe?os=ubuntu&osv=24.04&gpu=h100&intent=training"
//
// GET /health - Health check (for liveness probe)
//
//	Always returns 200 OK with {"status": "healthy", "timestamp": "..."}
//
// GET /ready - Readiness check (for readiness probe)
//
//	Returns 200 OK when ready, 503 when not ready
//
// # Observability
//
// Request ID Tracking:
//
//	All requests accept an optional X-Request-Id header (UUID format).
//	If not provided, the server generates one automatically.
//	The request ID is returned in the X-Request-Id response header
//	and included in all error responses for tracing.
//
// Rate Limiting:
//
//	Response headers indicate rate limit status:
//	  X-RateLimit-Limit: Total requests allowed per window
//	  X-RateLimit-Remaining: Requests remaining in current window
//	  X-RateLimit-Reset: Unix timestamp when window resets
//
//	When rate limited, returns 429 with Retry-After header.
//
// Cache Headers:
//
//	Recommendation responses include Cache-Control headers for CDN/client caching:
//	  Cache-Control: public, max-age=300
//
// # Error Handling
//
// All errors return a consistent JSON structure:
//
//	{
//	  "code": "INVALID_PARAMETER",
//	  "message": "invalid osFamily: must be one of Ubuntu, RHEL, ALL",
//	  "details": {"request": {...}},
//	  "requestId": "550e8400-e29b-41d4-a716-446655440000",
//	  "timestamp": "2025-12-22T12:00:00Z",
//	  "retryable": false
//	}
//
// Error codes:
//   - INVALID_PARAMETER: Invalid request parameter (400)
//   - INVALID_JSON: Malformed JSON payload (400)
//   - NO_MATCHING_RULE: No recommendation found (404)
//   - RATE_LIMIT_EXCEEDED: Too many requests (429)
//   - INTERNAL_ERROR: Server error (500)
//
// # Deployment
//
// Kubernetes deployment example:
//
//	apiVersion: apps/v1
//	kind: Deployment
//	metadata:
//	  name: aicr-recommendation-api
//	spec:
//	  replicas: 3
//	  selector:
//	    matchLabels:
//	      app: aicr-recommendation-api
//	  template:
//	    metadata:
//	      labels:
//	        app: aicr-recommendation-api
//	    spec:
//	      containers:
//	      - name: api
//	        image: aicr-recommendation-api:latest
//	        ports:
//	        - containerPort: 8080
//	        env:
//	        - name: PORT
//	          value: "8080"
//	        livenessProbe:
//	          httpGet:
//	            path: /health
//	            port: 8080
//	          initialDelaySeconds: 5
//	          periodSeconds: 10
//	        readinessProbe:
//	          httpGet:
//	            path: /ready
//	            port: 8080
//	          initialDelaySeconds: 5
//	          periodSeconds: 5
//	        resources:
//	          requests:
//	            cpu: 100m
//	            memory: 128Mi
//	          limits:
//	            cpu: 1000m
//	            memory: 512Mi
//
// # Performance
//
// Benchmarks (on M1 Mac):
//
//	BenchmarkGetRecommendations-8    50000    23000 ns/op    5000 B/op    80 allocs/op
//	BenchmarkValidation-8           500000     2500 ns/op     500 B/op    10 allocs/op
//
// The server is designed to handle thousands of requests per second with
// proper horizontal scaling. Rate limiting prevents resource exhaustion.
//
// # References
//
//   - OpenAPI spec: api/aicr/aicr-v1.yaml
//   - Rate limiting: https://pkg.go.dev/golang.org/x/time/rate
//   - UUID generation: https://pkg.go.dev/github.com/google/uuid
//   - Error groups: https://pkg.go.dev/golang.org/x/sync/errgroup
//   - HTTP best practices: https://datatracker.ietf.org/doc/html/rfc7807
//   - Kubernetes probes: https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
package server
